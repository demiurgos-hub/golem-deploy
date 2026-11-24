// Command server is the golem-deploy build fixture: the smallest Golem
// Engine game that runs through the generated Runtime.Host, serving the
// built web client from web/dist and a health endpoint.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/demiurgos-hub/golem-engine/golem"
	"github.com/demiurgos-hub/golem-engine/golem/host"

	"example.com/golem-deploy-fixture/internal/generated"
)

func main() {
	srv, rt := generated.NewServer(golem.ServerConfigFromEnv(golem.ServerConfig{
		TickRate:        10,
		Transport:       golem.TransportWebSocket,
		StateUpdateLane: golem.StateUpdateLaneStream,
		Path:            host.DefaultWebSocketPath,
	}))
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
