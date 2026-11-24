package servers

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// The Golem Engine deploy contract (golem-engine v0.4: golem/config_env.go,
// golem/host/env.go). The daemon sets these variables on every container;
// they override operator variables (whose names may not start with
// docker.ReservedEnvPrefix anyway).
const (
	// EnvAddr is the WebTransport (QUIC, UDP) listen address.
	EnvAddr = "GOLEM_ADDR"
	// EnvHTTPAddr is the HTTPS sidecar (TCP): realtime config, the
	// WebSocket fallback and the game's own pages.
	EnvHTTPAddr = "GOLEM_HTTP_ADDR"
	// EnvTLSCertFile, EnvTLSKeyFile and EnvTLSReloadInterval are shared by
	// the WebTransport listener and the sidecar; the engine polls the files
	// and reloads a replaced pair without a restart.
	EnvTLSCertFile       = "GOLEM_TLS_CERT_FILE"
	EnvTLSKeyFile        = "GOLEM_TLS_KEY_FILE"
	EnvTLSReloadInterval = "GOLEM_TLS_RELOAD_INTERVAL"
	// EnvPublicURL is the WebTransport URL clients connect to and
	// EnvWSFallbackURL the WebSocket fallback, both published in the
	// realtime config.
	EnvPublicURL     = "GOLEM_PUBLIC_URL"
	EnvWSFallbackURL = "GOLEM_WS_FALLBACK_URL"
	// EnvAllowedOrigins lists the browser origins allowed to connect
	// (WebTransport and WebSocket).
	EnvAllowedOrigins = "GOLEM_WT_ALLOWED_ORIGINS"
	// EnvTLSPinned makes the realtime config publish the certificate's
	// hashes for serverCertificateHashes pinning (self-signed only).
	EnvTLSPinned = "GOLEM_TLS_PINNED"
	// EnvAdminAddr and EnvAdminToken open the admin listener.
	EnvAdminAddr  = "GOLEM_ADMIN_ADDR"
	EnvAdminToken = "GOLEM_ADMIN_TOKEN"
)

// Fixed parts of the contract.
const (
	// AdminContainerPort is the container port of the admin listener; it
	// is published on 127.0.0.1 only, on a port Docker assigns.
	AdminContainerPort = 7778
	// TLSReloadInterval is how often the engine checks the certificate
	// files for a replacement.
	TLSReloadInterval = "30s"
	// envContractVersion changes whenever the variables below change
	// meaning, so running containers report restart_required.
	envContractVersion = "engine-env-3"
)

// MaxAllowedOrigins bounds a server's extra allowed origins.
const MaxAllowedOrigins = 16

// runtimeParams are the installation-wide inputs of a container besides
// the server row and its build.
type runtimeParams struct {
	// gameHost is the effective game host ("" when none is configured).
	gameHost    string
	testOrigins []string
	certMode    certs.Mode
	certDir     string
	groupAdd    []int
}

// authority returns host:port for URLs (IPv6 in brackets).
func authority(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// publicURL is the server's own page, or "" without a game host.
func publicURL(host string, port int) string {
	if host == "" || port == 0 {
		return ""
	}
	return "https://" + authority(host, port) + "/"
}

// transportPaths returns the WebTransport and WebSocket endpoint paths the
// build's game serves (recorded by the build from golem-deploy.yaml; the
// engine defaults for builds recorded before, or without a build).
func transportPaths(b *store.Build) (wt, ws string) {
	wt, ws = build.DefaultWebTransportPath, build.DefaultWebSocketPath
	if b != nil {
		wt, ws = cmp.Or(b.WebTransportPath, wt), cmp.Or(b.WebSocketPath, ws)
	}
	return wt, ws
}

// transportURLs uses the same paths for the engine environment and the probe.
func transportURLs(b *store.Build, host string, port int) (wt, ws string) {
	if host == "" || port == 0 {
		return "", ""
	}
	auth := authority(host, port)
	wtPath, wsPath := transportPaths(b)
	return "https://" + auth + wtPath, "wss://" + auth + wsPath
}

func (c Config) testOrigins(gameHost string) []string {
	origins := []string{}
	add := func(scheme, host string, port int) {
		if host == "" {
			return
		}
		// The daemon normalizes public hosts and the effective game host.
		auth := authority(host, port)
		if (scheme == "https" && port == 443) || (scheme == "http" && port == 80) {
			auth = strings.TrimSuffix(auth, ":"+strconv.Itoa(port))
		}
		origin := scheme + "://" + auth
		if !slices.Contains(origins, origin) {
			origins = append(origins, origin)
		}
	}
	scheme := "http"
	if c.PanelTLS {
		scheme = "https"
	}
	if c.PanelPort != 0 {
		for _, host := range c.PanelPublicHosts {
			add(scheme, host, c.PanelPort)
		}
		if len(c.PanelPublicHosts) == 0 {
			add(scheme, gameHost, c.PanelPort)
		}
		if c.PanelLoopback && !c.PanelTLS {
			add("http", "localhost", c.PanelPort)
			add("http", "127.0.0.1", c.PanelPort)
		}
	}
	// A plaintext panel can be reached through a TLS-terminating proxy.
	if !c.PanelTLS {
		for _, host := range c.PanelPublicHosts {
			add("https", host, 443)
		}
	}
	return origins
}

// engineEnv returns the daemon-injected environment of a container running
// build b. token is the per-container admin token ("" when computing the
// config hash, so the hash does not change with every new container).
func engineEnv(sv store.Server, b *store.Build, p runtimeParams, token string) map[string]string {
	port := 0
	if sv.Port != nil {
		port = *sv.Port
	}
	origins := []string{certs.Origin(p.gameHost, port)}
	for _, o := range slices.Concat(sv.AllowedOrigins, p.testOrigins) {
		if !slices.Contains(origins, o) {
			origins = append(origins, o)
		}
	}
	pinned := "0"
	if p.certMode == certs.ModeSelfSigned {
		pinned = "1"
	}
	game := ":" + strconv.Itoa(docker.GamePort)
	wt, ws := transportURLs(b, p.gameHost, port)
	env := map[string]string{
		EnvAddr:              game,
		EnvHTTPAddr:          game,
		EnvTLSCertFile:       certs.ContainerDir + "/" + certs.CertFile,
		EnvTLSKeyFile:        certs.ContainerDir + "/" + certs.KeyFile,
		EnvTLSReloadInterval: TLSReloadInterval,
		EnvPublicURL:         wt,
		EnvWSFallbackURL:     ws,
		EnvAllowedOrigins:    strings.Join(origins, ","),
		EnvTLSPinned:         pinned,
		EnvAdminAddr:         ":" + strconv.Itoa(AdminContainerPort),
	}
	if token != "" {
		env[EnvAdminToken] = token
	}
	return env
}

// configHash fingerprints everything a container is created from: the
// build and its image, the port, the daemon's run settings, the env (by a
// hash of its ciphertext, which is re-sealed with a fresh nonce on every
// change and reveals nothing about the values), the injected engine
// environment without the per-container token (game host, allowed origins,
// certificate mode), the certificate mount and the supplementary groups.
// The certificate's content is not part of it: a replaced certificate is
// picked up by the running engine.
func (s *Service) configHash(sv store.Server, b *store.Build, p runtimeParams) string {
	h := sha256.New()
	port := 0
	if sv.Port != nil {
		port = *sv.Port
	}
	var buildID int64
	image := ""
	if b != nil {
		buildID, image = b.ID, b.ImageID
	}
	envSum := sha256.Sum256(sv.Env)
	fmt.Fprintf(h, "v2\x00%s\x00%d\x00%s\x00%d\x00%s\x00%d\x00%g\x00%x\x00", envContractVersion, buildID, image, port,
		s.cfg.BindAddress, s.cfg.MemoryMB, s.cfg.CPUs, envSum)
	env := engineEnv(sv, b, p, "")
	for _, k := range docker.EnvKeys(env) {
		fmt.Fprintf(h, "%s=%s\x00", k, env[k])
	}
	fmt.Fprintf(h, "%s\x00%v", p.certDir, p.groupAdd)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ValidateOrigin checks and normalises a browser origin:
// "http(s)://host[:port]" with a DNS name or IP address as host, no
// credentials, path, query or fragment. The scheme and host are
// lower-cased and a default port (443 for https, 80 for http) is dropped,
// because browsers send origins that way.
func ValidateOrigin(o string) (string, error) {
	if len(o) > 270 || strings.ContainsAny(o, " \t\r\n,") {
		return "", fmt.Errorf("must be an origin such as https://play.example.com")
	}
	u, err := url.Parse(o)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		u.ForceQuery || strings.HasSuffix(o, "#") {
		return "", fmt.Errorf("must be an origin such as https://play.example.com (no path, query or credentials)")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("must use the https or http scheme")
	}
	host, err := certs.ValidateHost(u.Hostname())
	if err != nil || host == "" {
		return "", fmt.Errorf("must name a DNS host or an IP address")
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("has an empty port")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", fmt.Errorf("has an invalid port")
		}
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			port = ""
		}
	}
	out := scheme + "://" + net.JoinHostPort(host, port)
	if port == "" {
		out = scheme + "://" + host
		if strings.Contains(host, ":") {
			out = scheme + "://[" + host + "]"
		}
	}
	return out, nil
}

// validateOrigins validates and normalises a list (deduplicated, order
// kept); field names the request field.
func validateOrigins(field string, list []string) ([]string, error) {
	if len(list) > MaxAllowedOrigins {
		return nil, fieldErr(field, "at most %d allowed origins", MaxAllowedOrigins)
	}
	out := []string{}
	for i, o := range list {
		n, err := ValidateOrigin(o)
		if err != nil {
			return nil, fieldErr(fmt.Sprintf("%s[%d]", field, i), "origin %s", err.Error())
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}
