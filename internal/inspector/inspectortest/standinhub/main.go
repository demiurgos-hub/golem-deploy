// Command standinhub stands in for the Golem Inspector hub in the Docker
// integration test of internal/inspector. It reads the hub's
// GOLEM_INSPECTOR_* variables and serves the part of the hub's HTTP
// surface the daemon uses: /ready, /auth/login (bcrypt password, origin
// check, session cookie), /auth/logout, /auth/session, /api/targets,
// /ws/agent (bearer token) and /ws/client. WebSocket endpoints complete the
// upgrade and then speak a line protocol: an agent sends
// "register <target id>" and stays connected.
//
// "standinhub agent" is a minimal agent: it dials GOLEM_INSPECTOR_HUB_URL
// with GOLEM_INSPECTOR_TOKEN, registers GOLEM_INSPECTOR_TARGET_ID and
// keeps the connection open.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		if err := agent(); err != nil {
			fmt.Println("agent:", err)
			os.Exit(1)
		}
		return
	}
	if err := hub(); err != nil {
		fmt.Println("hub:", err)
		os.Exit(1)
	}
}

type target struct {
	ID          string    `json:"targetId"`
	Connected   bool      `json:"connected"`
	EntityCount int       `json:"entityCount"`
	ConnectedAt time.Time `json:"connectedAt"`
}

type state struct {
	mu       sync.Mutex
	sessions map[string]bool
	targets  map[string]*target
}

func hub() error {
	env := func(k string) string { return os.Getenv("GOLEM_INSPECTOR_" + k) }
	if env("PRODUCTION") != "true" || env("BEHIND_PROXY") != "true" || env("SESSION_MAX_AGE") == "" {
		return fmt.Errorf("production settings missing")
	}
	hash, token, origin := env("PASSWORD_HASH"), env("AGENT_TOKEN"), env("PUBLIC_ORIGINS")
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost < 12 || len(token) < 32 || !strings.HasPrefix(origin, "https://") {
		return fmt.Errorf("invalid secrets")
	}
	readOnly := "writable"
	if err := os.WriteFile("/standin-probe", []byte("x"), 0o600); err != nil {
		readOnly = "read-only"
	}
	fmt.Printf("standin hub uid=%d root=%s\n", os.Getuid(), readOnly)
	st := &state{sessions: map[string]bool{}, targets: map[string]*target{}}
	authed := func(r *http.Request) (string, bool) {
		c, err := r.Cookie("golem_inspector_session")
		if err != nil {
			return "", false
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return c.Value, st.sessions[c.Value]
	}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ready": true}) })
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin {
			writeJSON(w, 403, map[string]string{"error": "origin_not_allowed"})
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
			writeJSON(w, 401, map[string]string{"error": "invalid_credentials"})
			return
		}
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		sid := hex.EncodeToString(b)
		st.mu.Lock()
		st.sessions[sid] = true
		st.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "golem_inspector_session", Value: sid, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"authenticated": true})
	})
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		sid, ok := authed(r)
		if r.Header.Get("Origin") != origin || !ok {
			writeJSON(w, 401, map[string]string{"error": "auth_required"})
			return
		}
		st.mu.Lock()
		delete(st.sessions, sid)
		st.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"authenticated": false})
	})
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		_, ok := authed(r)
		writeJSON(w, 200, map[string]bool{"authenticated": ok})
	})
	mux.HandleFunc("/api/targets", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(r); !ok {
			writeJSON(w, 401, map[string]string{"error": "auth_required"})
			return
		}
		st.mu.Lock()
		list := []target{}
		for _, t := range st.targets {
			list = append(list, *t)
		}
		st.mu.Unlock()
		writeJSON(w, 200, map[string]any{"targets": list})
	})
	mux.HandleFunc("/ws/agent", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "auth_failed"})
			return
		}
		conn, br, ok := upgrade(w, r)
		if !ok {
			return
		}
		defer conn.Close()
		line, err := br.ReadString('\n')
		id, found := strings.CutPrefix(strings.TrimSpace(line), "register ")
		if err != nil || !found || id == "" {
			return
		}
		st.mu.Lock()
		st.targets[id] = &target{ID: id, Connected: true, EntityCount: 3, ConnectedAt: time.Now().UTC()}
		st.mu.Unlock()
		_, _ = io.Copy(io.Discard, br)
		st.mu.Lock()
		st.targets[id].Connected = false
		st.mu.Unlock()
	})
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(r); !ok || r.Header.Get("Origin") != origin {
			writeJSON(w, 401, map[string]string{"error": "auth_required"})
			return
		}
		conn, br, ok := upgrade(w, r)
		if !ok {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, br)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>standin inspector</html>")
	})
	srv := &http.Server{Addr: env("ADDR"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, bool) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "upgrade required", http.StatusBadRequest)
		return nil, nil, false
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, nil, false
	}
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, nil, false
	}
	return conn, brw.Reader, true
}

func agent() error {
	u, err := url.Parse(os.Getenv("GOLEM_INSPECTOR_HUB_URL"))
	if err != nil || u.Scheme != "ws" {
		return fmt.Errorf("bad hub URL")
	}
	for attempt := 0; ; attempt++ {
		err := dialOnce(u)
		fmt.Println("agent: connection ended:", err)
		if attempt > 30 {
			return err
		}
		time.Sleep(time.Second)
	}
}

func dialOnce(u *url.URL) error {
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	req := "GET " + u.Path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer " + os.Getenv("GOLEM_INSPECTOR_TOKEN") + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	fmt.Println("agent: connected")
	if _, err := io.WriteString(conn, "register "+os.Getenv("GOLEM_INSPECTOR_TARGET_ID")+"\n"); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, br)
	return err
}
