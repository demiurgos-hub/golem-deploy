package build

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
)

// ConfigFile is the optional build configuration at the repository root.
const ConfigFile = "golem-deploy.yaml"

// Limits on the repository build configuration.
const (
	maxConfigBytes = 32 << 10
	maxWords       = 32
	maxWordBytes   = 1024
	maxPathBytes   = 255
	maxPathDepth   = 16
)

// Config is the parsed golem-deploy.yaml. Every path is a clean relative
// path in slash form ("." for the directory itself). Root is relative to the
// repository root; every other path is relative to Root.
type Config struct {
	// Root is the game root: the directory holding golem.yaml and go.mod.
	Root   string
	Server ServerConfig
	Web    WebConfig
	// Dockerfile, when set, is a custom Dockerfile used instead of the
	// generated one.
	Dockerfile string
	Transport  TransportConfig
}

// TransportConfig names the endpoint paths the game server serves its
// transports on, when the game changed the engine's defaults; empty means
// the default. The daemon publishes the server's URLs with them.
type TransportConfig struct {
	// WebTransportPath is golem.ServerConfig.Path of a WebTransport game
	// (default DefaultWebTransportPath).
	WebTransportPath string
	// WebSocketPath is host.GameOptions.WebSocketPath, the WebSocket
	// fallback (default DefaultWebSocketPath).
	WebSocketPath string
}

// The engine's default endpoint paths (golem/net and golem/host).
const (
	DefaultWebTransportPath = "/wt"
	DefaultWebSocketPath    = "/api/ws"
)

// ServerConfig configures the game server binary.
type ServerConfig struct {
	// Package is the Go package of the server ("." or a relative directory);
	// empty means detect.
	Package string
	// Args are passed to the server after the binary (exec form).
	Args []string
}

// WebConfig configures the browser client build.
type WebConfig struct {
	// Dir holds package.json; empty means the game root.
	Dir string
	// Build is the build command in exec form; nil means "npm run build"
	// when package.json has a build script.
	Build []string
	// Dist is the built client directory, relative to the game root; empty
	// means the default (DefaultWebDist when there is a web build).
	Dist string
}

// DefaultWebDist is where the browser client is expected after the web
// build unless web.dist says otherwise (the Golem Engine examples' default
// -assets directory).
const DefaultWebDist = "web/dist"

// rawConfig mirrors the YAML document; unknown keys are rejected.
type rawConfig struct {
	Root   *string `yaml:"root"`
	Server *struct {
		Package *string   `yaml:"package"`
		Args    *[]string `yaml:"args"`
	} `yaml:"server"`
	Web *struct {
		Dir   *string  `yaml:"dir"`
		Build *command `yaml:"build"`
		Dist  *string  `yaml:"dist"`
	} `yaml:"web"`
	Dockerfile *string `yaml:"dockerfile"`
	Transport  *struct {
		WebTransportPath *string `yaml:"webtransport_path"`
		WebSocketPath    *string `yaml:"websocket_path"`
	} `yaml:"transport"`
}

// command accepts a list of words (exec form) or a string split on white
// space. No shell is involved either way.
type command []string

func (c *command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag != "!!str" {
			return errors.New("must be a string or a list of strings")
		}
		*c = strings.Fields(n.Value)
		return nil
	case yaml.SequenceNode:
		var words []string
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
				return errors.New("must be a list of strings")
			}
			words = append(words, item.Value)
		}
		*c = words
		return nil
	}
	return errors.New("must be a string or a list of strings")
}

// ParseConfig parses and validates golem-deploy.yaml. Empty input yields
// the defaults. Errors are *UserError values safe to show to users.
func ParseConfig(data []byte) (Config, error) {
	cfg := Config{Root: "."}
	if len(data) > maxConfigBytes {
		return Config{}, userErr("%s is larger than %d KiB", ConfigFile, maxConfigBytes>>10)
	}
	if !utf8.Valid(data) {
		return Config{}, userErr("%s is not valid UTF-8", ConfigFile)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	if err := checkYAML(ConfigFile, data); err != nil {
		return Config{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return cfg, nil
		}
		return Config{}, userErr("%s is not valid: %s", ConfigFile, yamlMessage(err))
	}
	var err error
	if raw.Root != nil {
		if cfg.Root, err = cleanPath("root", *raw.Root, true); err != nil {
			return Config{}, err
		}
	}
	if s := raw.Server; s != nil {
		if s.Package != nil {
			if cfg.Server.Package, err = cleanPackage(*s.Package); err != nil {
				return Config{}, err
			}
		}
		if s.Args != nil {
			if err := checkWords("server.args", *s.Args, false); err != nil {
				return Config{}, err
			}
			cfg.Server.Args = append([]string{}, *s.Args...)
		}
	}
	if w := raw.Web; w != nil {
		if w.Dir != nil {
			if cfg.Web.Dir, err = cleanPath("web.dir", *w.Dir, true); err != nil {
				return Config{}, err
			}
		}
		if w.Build != nil {
			if err := checkWords("web.build", *w.Build, true); err != nil {
				return Config{}, err
			}
			cfg.Web.Build = append([]string{}, *w.Build...)
		}
		if w.Dist != nil {
			if cfg.Web.Dist, err = cleanPath("web.dist", *w.Dist, false); err != nil {
				return Config{}, err
			}
		}
	}
	if raw.Dockerfile != nil {
		if cfg.Dockerfile, err = cleanPath("dockerfile", *raw.Dockerfile, false); err != nil {
			return Config{}, err
		}
	}
	if t := raw.Transport; t != nil {
		if t.WebTransportPath != nil {
			if cfg.Transport.WebTransportPath, err = cleanURLPath("transport.webtransport_path", *t.WebTransportPath); err != nil {
				return Config{}, err
			}
		}
		if t.WebSocketPath != nil {
			if cfg.Transport.WebSocketPath, err = cleanURLPath("transport.websocket_path", *t.WebSocketPath); err != nil {
				return Config{}, err
			}
		}
	}
	return cfg, nil
}

// Limits of a transport endpoint path.
const (
	maxURLPathBytes    = 128
	maxURLPathSegments = 8
)

var urlSegmentRE = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// cleanURLPath validates a transport endpoint path from the build
// configuration: "/" followed by at most maxURLPathSegments segments of
// unreserved URL characters (letters, digits, '.', '_', '~', '-'; not "."
// or ".."), no empty segment, no trailing '/', at most maxURLPathBytes
// bytes. Nothing in it needs escaping in a URL or an environment value.
func cleanURLPath(field, p string) (string, error) {
	if len(p) > maxURLPathBytes {
		return "", fieldErr(field, "must be at most %d bytes", maxURLPathBytes)
	}
	if !strings.HasPrefix(p, "/") || p == "/" {
		return "", fieldErr(field, "must be an absolute URL path such as /wt (not / itself)")
	}
	segs := strings.Split(p[1:], "/")
	if len(segs) > maxURLPathSegments {
		return "", fieldErr(field, "must have at most %d path segments", maxURLPathSegments)
	}
	for _, s := range segs {
		switch {
		case s == "":
			return "", fieldErr(field, "must not contain empty segments or end with '/'")
		case s == "." || s == "..":
			return "", fieldErr(field, "must not contain '.' or '..' segments")
		case !urlSegmentRE.MatchString(s):
			return "", fieldErr(field, "may only contain letters, digits, '.', '_', '~', '-' and '/'")
		}
	}
	return p, nil
}

// yamlMessage returns a short, single-line parser message without Go type
// names (sanitised and clipped; the parser quotes at most a short excerpt
// of the document).
func yamlMessage(err error) string {
	var parts []string
	var te *yaml.TypeError
	if errors.As(err, &te) {
		parts = te.Errors
	} else {
		parts = []string{err.Error()}
	}
	for i, m := range parts {
		m = strings.TrimPrefix(m, "yaml: ")
		if j := strings.Index(m, " in type "); j >= 0 {
			m = m[:j]
		}
		m = intoTypeRE.ReplaceAllString(m, "into a $1")
		parts[i] = m
	}
	return clip(logsafe.String(oneLine(strings.Join(parts, "; "))), 200)
}

// intoTypeRE rewrites "into []string" / "into build.command" style type
// names in decode errors.
var intoTypeRE = regexp.MustCompile(`into (?:\*?\[\])?(?:[A-Za-z0-9_]+\.)?([A-Za-z0-9_]+)`)

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)

// cleanPath validates a relative path from the build configuration: slash
// separated segments of [A-Za-z0-9_.-] (not starting with '-', not "." or
// "..", not ".git"), at most maxPathDepth segments and maxPathBytes bytes. A
// leading "./" and a trailing "/" are removed. "." (or empty) is accepted
// only when allowDot. The allowlist keeps every path safe to place in a
// Dockerfile instruction and a docker argument without quoting.
func cleanPath(field, p string, allowDot bool) (string, error) {
	if strings.HasPrefix(p, "/") {
		return "", fieldErr(field, "must be a relative path with '/' separators")
	}
	orig := p
	p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
	if p == "" || p == "." {
		if allowDot {
			return ".", nil
		}
		return "", fieldErr(field, "must name a directory or file below the game root, not the root itself")
	}
	if len(orig) > maxPathBytes {
		return "", fieldErr(field, "must be at most %d bytes", maxPathBytes)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.Contains(p, ":") {
		return "", fieldErr(field, "must be a relative path with '/' separators")
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxPathDepth {
		return "", fieldErr(field, "must have at most %d path segments", maxPathDepth)
	}
	for _, s := range segs {
		switch {
		case s == "" || s == "." || s == "..":
			return "", fieldErr(field, "must not contain empty, '.' or '..' segments")
		case strings.EqualFold(s, ".git"):
			return "", fieldErr(field, "must not point into .git")
		case !segmentRE.MatchString(s):
			return "", fieldErr(field, "may only contain letters, digits, '.', '_', '-' and '/' (segments must not start with '-')")
		case strings.HasSuffix(s, "."):
			return "", fieldErr(field, "segments must not end with '.'")
		}
	}
	return p, nil
}

// cleanPackage validates server.package: "." or a relative directory,
// optionally written with a leading "./".
func cleanPackage(p string) (string, error) {
	return cleanPath("server.package", p, true)
}

// checkWords validates exec-form words: each valid UTF-8 without control
// characters, at most maxWordBytes; at most maxWords words; nonEmpty
// requires at least one word and a non-empty first word.
func checkWords(field string, words []string, nonEmpty bool) error {
	if len(words) > maxWords {
		return fieldErr(field, "must have at most %d items", maxWords)
	}
	if nonEmpty && (len(words) == 0 || words[0] == "") {
		return fieldErr(field, "must not be empty")
	}
	for _, w := range words {
		if len(w) > maxWordBytes {
			return fieldErr(field, "items must be at most %d bytes", maxWordBytes)
		}
		if !utf8.ValidString(w) {
			return fieldErr(field, "items must be valid UTF-8")
		}
		for _, r := range w {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 ||
				(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
				r == 0x061c || r == 0x200e || r == 0x200f || r == 0xfeff {
				return fieldErr(field, "items must be single-line text without control characters")
			}
		}
	}
	return nil
}

func fieldErr(field, format string, a ...any) error {
	return userErr("%s: %s %s", ConfigFile, field, fmt.Sprintf(format, a...))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
