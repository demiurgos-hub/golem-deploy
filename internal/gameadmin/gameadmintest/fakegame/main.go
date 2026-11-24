// Command fakegame is a stand-in game server for integration tests against
// a real Docker daemon: tests compile it statically for Linux and run it
// from a FROM scratch image as a server container. It implements the parts
// of the Golem Engine deploy contract the daemon drives: an HTTPS sidecar
// on GOLEM_HTTP_ADDR serving the certificate pair from GOLEM_TLS_CERT_FILE
// and GOLEM_TLS_KEY_FILE (reloaded when either file's size or modification
// time changes, like golem/net), and the admin listener on GOLEM_ADMIN_ADDR
// with bearer GOLEM_ADMIN_TOKEN (/live, /ready, /info, /stats).
//
// Test knobs (operator variables): FAKEGAME_NEVER_READY=1 keeps /ready at
// 503; FAKEGAME_POLL overrides the certificate poll interval.
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

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
					"name": "fakegame", "schema_fingerprint": "itest-fp-1", "engine_version": "v0.4.0-rc.1", "go_version": "go1.26",
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
