package gameadmin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
)

const reflectedToken = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestSessionUsesOneConnection(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("close first=", closeFirst), func(t *testing.T) {
			peers := make(chan string, 3)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				peers <- r.RemoteAddr
				if closeFirst {
					w.Header().Set("Connection", "close")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ready":true}`))
			}))
			defer srv.Close()
			c, err := gameadmin.NewSession(srv.Listener.Addr().(*net.TCPAddr).Port, reflectedToken)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx := context.Background()
			if ready, err := c.Ready(ctx); err != nil || !ready.Ready {
				t.Fatalf("ready: %+v %v", ready, err)
			}
			first := <-peers
			_, err = c.Stats(ctx)
			if closeFirst {
				if !errors.Is(err, gameadmin.ErrUnreachable) || len(peers) != 0 {
					t.Fatalf("session redialed: %v, additional requests: %d", err, len(peers))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Info(ctx); err != nil {
				t.Fatal(err)
			}
			if second, third := <-peers, <-peers; first != second || first != third {
				t.Fatalf("requests arrived on different connections: %s %s %s", first, second, third)
			}
		})
	}
}

func TestHostileAdminCannotReflectToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/ready":
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"reason": "starting " + reflectedToken})
		case "/info":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":           strings.Repeat("x", 512) + reflectedToken,
				"engine_version": reflectedToken, "go_version": reflectedToken,
				"transport": reflectedToken, "schema_fingerprint": reflectedToken,
				"http_certificate_hashes": []map[string]string{{"algorithm": "sha-256", "value": reflectedToken}},
			})
		case "/stats":
			conn, wire, err := http.NewResponseController(w).Hijack()
			if err == nil {
				defer conn.Close()
				_, _ = fmt.Fprint(wire, "not HTTP "+reflectedToken+"\r\n\r\n")
				_ = wire.Flush()
			}
		}
	}))
	defer srv.Close()
	c, err := gameadmin.New(srv.Listener.Addr().(*net.TCPAddr).Port, reflectedToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ready, err := c.Ready(ctx); err != nil || ready.Reason != "" {
		t.Fatalf("reflected reason survived: %+v %v", ready, err)
	}
	info, err := c.Info(ctx)
	if err != nil || info.Name != "" || info.EngineVersion != "" || info.GoVersion != "" ||
		info.Transport != "" || info.SchemaFingerprint != "" || len(info.HTTPCertificateHashes) != 0 {
		t.Fatalf("reflected info survived: %+v %v", info, err)
	}
	if _, err := c.Stats(ctx); err != gameadmin.ErrBadResponse {
		t.Fatalf("expected a fixed error for invalid protocol: %q", err)
	}
}
