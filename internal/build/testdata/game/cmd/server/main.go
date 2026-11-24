// Command server is the golem-deploy build fixture: the smallest Golem
// Engine game that runs through the generated Runtime.Host under the deploy
// contract (WebTransport primary, configured by GOLEM_* variables), serving
// the built web client from web/dist and a health endpoint.
package main

import (
	"context"
	"log"
	"net"
	"net/http"

	"github.com/demiurgos-hub/golem-engine/golem"

	"example.com/golem-deploy-fixture/internal/generated"
)

func main() {
	cfg := golem.ServerConfigFromEnv(golem.ServerConfig{
		TickRate:  10,
		Addr:      "127.0.0.1:8080",
		Transport: golem.TransportWebTransport,
	})
	// Without TLS files (a local run) WebTransport uses the engine's
	// development certificate on a loopback address; golem-deploy always
	// provides the files.
	if host, _, err := net.SplitHostPort(cfg.Addr); err == nil && cfg.TLSCertFile == "" && cfg.TLSKeyFile == "" {
		ip := net.ParseIP(host)
		cfg.DevSelfSignedCert = host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	srv, rt := generated.NewServer(cfg)
	api := http.NewServeMux()
	api.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ready\n"))
	})
	err := rt.Host(context.Background(), generated.HostOptions{
		Name:      "golem-deploy-fixture",
		HTTPAddr:  "127.0.0.1:8080",
		API:       api,
		AssetsDir: "web/dist",
		AfterRestore: func(bool) error {
			return srv.CreateEntity(generated.NewSyncedBeacon(0, 0, 1))
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
