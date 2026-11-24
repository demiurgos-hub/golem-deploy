// Command fakegame is a stand-in game server for integration tests against
// a real Docker daemon: tests compile it statically for Linux and run it
// from a FROM scratch image as a server container. It implements the parts
// of the Golem Engine deploy contract the daemon drives: an HTTPS sidecar
// on GOLEM_HTTP_ADDR serving the certificate pair from GOLEM_TLS_CERT_FILE
// and GOLEM_TLS_KEY_FILE (reloaded when either file's size or modification
// time changes, like golem/net), and the admin listener on GOLEM_ADMIN_ADDR
// with bearer GOLEM_ADMIN_TOKEN (/live, /ready, /info, /stats, POST
// /snapshot). With GOLEM_SNAPSHOT_DIR it keeps an entity snapshot there like
// the engine: world.snapshot (the engine's header with the game's schema
// fingerprint) and content/state.txt, written to a hidden name and renamed,
// on POST /snapshot and when it stops; at start it loads world.snapshot and
// exits when its schema differs.
//
// Test knobs (operator variables): FAKEGAME_NEVER_READY=1 keeps /ready at
// 503; FAKEGAME_POLL overrides the certificate poll interval;
// FAKEGAME_SCHEMA overrides the schema fingerprint (default itest-fp-1).
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// snapshots keeps the fake entity snapshot.
type snapshots struct {
	mu     sync.Mutex
	dir    string
	schema string
	saves  int
}

// header is the engine's world.snapshot header: magic, uvarint version,
// uvarint timestamp, uvarint length and the fingerprint.
func header(schema string) []byte {
	b := []byte{'G', 'L', 'M', 1}
	b = binary.AppendUvarint(b, 2)
	b = binary.AppendUvarint(b, uint64(time.Now().Unix()))
	b = binary.AppendUvarint(b, uint64(len(schema)))
	return append(b, schema...)
}

func writeAtomic(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, "."+filepath.Base(name)+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func (s *snapshots) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if err := os.MkdirAll(filepath.Join(s.dir, "content"), 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dir, "content"), "state.txt", []byte(fmt.Sprintf("saved %d\n", s.saves))); err != nil {
		return err
	}
	return writeAtomic(s.dir, "world.snapshot", append(header(s.schema), "entities"...))
}

// load reads the schema of an existing world.snapshot ("" without one).
func (s *snapshots) load() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "world.snapshot"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(b) < 4 || string(b[:3]) != "GLM" {
		return "", fmt.Errorf("not a snapshot")
	}
	rest := b[4:]
	for i := 0; i < 2; i++ {
		_, n := binary.Uvarint(rest)
		if n <= 0 {
			return "", fmt.Errorf("bad header")
		}
		rest = rest[n:]
	}
	l, n := binary.Uvarint(rest)
	if n <= 0 || uint64(len(rest)-n) < l {
		return "", fmt.Errorf("bad header")
	}
	return string(rest[n : n+int(l)]), nil
}

type certState struct {
	mu      sync.Mutex
	cert    *tls.Certificate
	hash    string
	certMod time.Time
	keyMod  time.Time
	certSz  int64
	keySz   int64
}

func (c *certState) reload(certFile, keyFile string) error {
	ci, err := os.Stat(certFile)
	if err != nil {
		return err
	}
	ki, err := os.Stat(keyFile)
	if err != nil {
		return err
	}
	c.mu.Lock()
	same := c.cert != nil && ci.ModTime().Equal(c.certMod) && ki.ModTime().Equal(c.keyMod) && ci.Size() == c.certSz && ki.Size() == c.keySz
	c.mu.Unlock()
	if same {
		return nil
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err // a pair caught mid-replacement: keep the old one, retry
	}
	sum := sha256.Sum256(pair.Certificate[0])
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cert, c.hash = &pair, hex.EncodeToString(sum[:])
	c.certMod, c.keyMod, c.certSz, c.keySz = ci.ModTime(), ki.ModTime(), ci.Size(), ki.Size()
	fmt.Println("certificate loaded", c.hash)
	return nil
}

func (c *certState) get() (*tls.Certificate, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cert, c.hash
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	fmt.Printf("fakegame started as uid=%d gid=%d groups=%v\n", os.Getuid(), os.Getgid(), groups())
	certFile, keyFile := os.Getenv("GOLEM_TLS_CERT_FILE"), os.Getenv("GOLEM_TLS_KEY_FILE")
	poll, err := time.ParseDuration(os.Getenv("GOLEM_TLS_RELOAD_INTERVAL"))
	if v := os.Getenv("FAKEGAME_POLL"); v != "" {
		poll, err = time.ParseDuration(v)
	}
	if err != nil || poll <= 0 {
		poll = 30 * time.Second
	}
	token := os.Getenv("GOLEM_ADMIN_TOKEN")
	neverReady := os.Getenv("FAKEGAME_NEVER_READY") == "1"
	started := time.Now()
	schema := "itest-fp-1"
	if v := os.Getenv("FAKEGAME_SCHEMA"); v != "" {
		schema = v
	}
	snaps := &snapshots{dir: os.Getenv("GOLEM_SNAPSHOT_DIR"), schema: schema}
	if snaps.dir != "" {
		loaded, err := snaps.load()
		switch {
		case err != nil:
			fmt.Println("cannot read the snapshot:", err)
			os.Exit(1)
		case loaded != "" && loaded != schema:
			fmt.Printf("snapshot schema %s does not match the game's schema %s\n", loaded, schema)
			os.Exit(1)
		case loaded != "":
			fmt.Println("loaded snapshot", loaded)
		default:
			fmt.Println("no snapshot; starting an empty world")
		}
	}

	certs := &certState{}
	if err := certs.reload(certFile, keyFile); err != nil {
		fmt.Println("cannot load the certificate:", err)
	}
	go func() {
		t := time.NewTicker(poll)
		defer t.Stop()
		for range t.C {
			if err := certs.reload(certFile, keyFile); err != nil {
				fmt.Println("certificate reload failed:", err)
			}
		}
	}()

	sidecar := &http.Server{
		Addr:              os.Getenv("GOLEM_HTTP_ADDR"),
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintln(w, "fakegame")
		}),
		TLSConfig: &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c, _ := certs.get()
			if c == nil {
				return nil, fmt.Errorf("no certificate loaded")
			}
			return c, nil
		}},
	}
	go func() { fmt.Println("sidecar:", sidecar.ListenAndServeTLS("", "")) }()

	admin := &http.Server{
		Addr:              os.Getenv("GOLEM_ADMIN_ADDR"),
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/live" {
				writeJSON(w, 200, map[string]bool{"live": true})
				return
			}
			want := sha256.Sum256([]byte("Bearer " + token))
			got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			if len(token) < 16 || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
				writeJSON(w, 401, map[string]string{"error": "unauthorized"})
				return
			}
			_, hash := certs.get()
			switch r.URL.Path {
			case "/snapshot":
				switch {
				case r.Method != http.MethodPost:
					writeJSON(w, 405, map[string]string{"code": "method_not_allowed", "message": "use POST"})
				case snaps.dir == "":
					writeJSON(w, 501, map[string]string{"code": "not_implemented", "message": "no snapshot directory"})
				default:
					if err := snaps.save(); err != nil {
						fmt.Println("snapshot failed:", err)
						writeJSON(w, 500, map[string]string{"code": "snapshot_failed", "message": "snapshot failed"})
						return
					}
					writeJSON(w, 200, map[string]any{"ok": true, "duration_ms": 1})
				}
			case "/ready":
				if hash == "" || neverReady {
					writeJSON(w, 503, map[string]string{"reason": "starting"})
					return
				}
				writeJSON(w, 200, map[string]bool{"ready": true})
			case "/info":
				hashes := []map[string]string{}
				if hash != "" {
					hashes = append(hashes, map[string]string{"algorithm": "sha-256", "value": hash})
				}
				writeJSON(w, 200, map[string]any{
					"name": "fakegame", "schema_fingerprint": schema, "engine_version": "v0.4.0-rc.1", "go_version": "go1.26",
					"transport": "webtransport", "tick_rate": 20, "certificate_hashes_published": os.Getenv("GOLEM_TLS_PINNED") == "1",
					"webtransport_certificate_hashes": hashes, "http_tls_reloadable": true, "http_certificate_hashes": hashes,
					"extra": map[string]string{},
				})
			case "/stats":
				writeJSON(w, 200, map[string]any{
					"uptime_seconds": time.Since(started).Seconds(), "sessions": 0, "entities": 7,
					"tick": map[string]any{"tick": int(time.Since(started).Seconds() * 20), "rate": 20, "budget_ms": 50, "last_ms": 0.1, "max_ms": 0.5},
				})
			default:
				writeJSON(w, 404, map[string]string{"error": "not_found"})
			}
		}),
	}
	go func() { fmt.Println("admin:", admin.ListenAndServe()) }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	tick := time.NewTicker(time.Second)
	for i := 1; ; i++ {
		select {
		case <-stop:
			fmt.Println("stopping")
			if snaps.dir != "" {
				if err := snaps.save(); err != nil {
					fmt.Println("final snapshot failed:", err)
				}
			}
			return
		case <-tick.C:
			fmt.Println("tick", i)
		}
	}
}

func groups() []int {
	g, _ := os.Getgroups()
	return g
}
