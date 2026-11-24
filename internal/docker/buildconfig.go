package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Docker detects the platform's credential helper when config.json contains
// no auth configuration. An empty entry for a reserved hostname opts out.
const anonymousRegistry = "golem-deploy.invalid"

type buildConfig struct {
	Auths         map[string]json.RawMessage `json:"auths"`
	CredsStore    string                     `json:"credsStore,omitempty"`
	CredHelpers   map[string]string          `json:"credHelpers,omitempty"`
	Proxies       map[string]json.RawMessage `json:"proxies,omitempty"`
	CLIPluginDirs []string                   `json:"cliPluginsExtraDirs,omitempty"`
}

// PrepareBuildConfig creates the daemon's private build configuration.
// Docker's system-wide plugin lookup still applies. Also allow buildx from
// the original client config directory's cli-plugins directory.
func (c *Client) PrepareBuildConfig() error {
	return c.prepareBuildConfig(c.buildConfigDir)
}

func (c *Client) prepareBuildConfig(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrBuildConfig
	}
	path := filepath.Join(dir, "config.json")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return ErrBuildConfig
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	cfg := buildConfig{Auths: map[string]json.RawMessage{anonymousRegistry: json.RawMessage("{}")}}
	original := envValue(c.baseEnv, "DOCKER_CONFIG")
	if original == "" {
		if home, err := os.UserHomeDir(); err == nil {
			original = filepath.Join(home, ".docker")
		}
	}
	if filepath.IsAbs(original) && original != dir {
		cfg.CLIPluginDirs = []string{filepath.Join(original, "cli-plugins")}
	}
	if err := errors.Join(json.NewEncoder(f).Encode(cfg), f.Close()); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

func envValue(env []string, key string) string {
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, key+"="); ok {
			return value
		}
	}
	return ""
}

// ErrBuildConfig is safe to report in the build detail.
var ErrBuildConfig = errors.New("the dedicated Docker build configuration must contain explicit registry settings and no proxies; see the installation guide")

var (
	errBuildContext    = errors.New("the active Docker context could not be resolved for the build")
	errBuildTLSContext = errors.New("import the active Docker TLS context into the dedicated build configuration")
)

func (c *Client) buildClientEnv(ctx context.Context, spec BuildSpec) ([]string, error) {
	dir := c.buildConfigDir
	if dir == "" {
		// Library callers can use a disposable configuration beside their
		// private output files. The daemon always sets a persistent path.
		dir = filepath.Join(filepath.Dir(spec.MetadataFile), "client-config")
	}
	if err := c.prepareBuildConfig(dir); err != nil {
		return nil, ErrBuildConfig
	}
	raw, err := readCapped(filepath.Join(dir, "config.json"), maxMetadataBytes)
	var cfg buildConfig
	if err != nil || json.Unmarshal(raw, &cfg) != nil || len(cfg.Proxies) != 0 ||
		(len(cfg.Auths) == 0 && len(cfg.CredHelpers) == 0 && cfg.CredsStore == "") {
		return nil, ErrBuildConfig
	}
	env := buildEnv()
	env = append(env, "DOCKER_CONFIG="+dir)
	for _, pair := range c.baseEnv {
		if !strings.HasPrefix(pair, "DOCKER_CONFIG=") {
			env = append(env, pair)
		}
	}
	if _, real := c.runner.(ExecRunner); !real {
		return env, nil
	}
	// Resolve the original context, so isolating the client does not send
	// a build to a different daemon (notably on Docker Desktop).
	type endpoint struct {
		Host          string
		SkipTLSVerify bool
	}
	type inspect struct {
		Endpoints   map[string]endpoint
		TLSMaterial map[string][]string
		Name        string
	}
	readContext := func(env []string) (inspect, error) {
		out := &capBuffer{max: maxMetadataBytes}
		err := c.execEnv(ctx, quickTimeout, []string{"context", "inspect", "--format", "{{json .}}"}, env, nil, out, io.Discard, false)
		var current inspect
		if err != nil {
			return inspect{}, err
		}
		if out.truncated || json.Unmarshal(out.buf.Bytes(), &current) != nil || current.Endpoints["docker"].Host == "" {
			return inspect{}, errBuildContext
		}
		return current, nil
	}
	current, err := readContext(c.baseEnv)
	if err != nil {
		return nil, err
	}
	ep := current.Endpoints["docker"]
	if current.Name == "default" || (len(current.TLSMaterial["docker"]) == 0 && !ep.SkipTLSVerify) {
		// The default context's TLS is already supplied via the allowlisted
		// Docker environment. Plain named contexts need only their endpoint.
		env = append(env, "DOCKER_CONTEXT=", "DOCKER_HOST="+ep.Host)
	} else {
		// TLS material for a named context is deliberately provisioned by
		// the operator in the isolated config, separately from auth/proxies.
		env = append(env, "DOCKER_CONTEXT="+current.Name)
		isolated, err := readContext(env)
		if err != nil || isolated.Endpoints["docker"] != ep {
			return nil, errBuildTLSContext
		}
	}
	return env, nil
}
