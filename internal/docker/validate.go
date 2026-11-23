package docker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Labels set on every object the daemon creates. LabelManaged=ManagedValue
// marks an object as managed; operations refuse objects without it.
const (
	LabelManaged    = "dev.golemengine.deploy.managed"
	LabelServerID   = "dev.golemengine.deploy.server-id"
	LabelServer     = "dev.golemengine.deploy.server"
	LabelProjectID  = "dev.golemengine.deploy.project-id"
	LabelCommit     = "dev.golemengine.deploy.commit"
	LabelConfigHash = "dev.golemengine.deploy.config-hash"
	ManagedValue    = "true"
)

// Naming and fixed runtime parameters.
const (
	// NamePrefix prefixes managed container names: golem-deploy-<server>.
	NamePrefix = "golem-deploy-"
	// NetworkName is the user-defined bridge network servers join.
	NetworkName = "golem-deploy"
	// GamePort is the container port every game server listens on (UDP and
	// TCP); the server's host port is published to it.
	GamePort = 7777
)

// Limits on container environment.
const (
	MaxEnvVars  = 128
	MaxEnvBytes = 64 << 10 // keys plus values
)

// IsManaged reports whether labels carry the managed label.
func IsManaged(labels map[string]string) bool { return labels[LabelManaged] == ManagedValue }

// ValidationError is a rejected input. Field names the offending input and
// Msg is safe to show to users.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

func invalid(field, format string, a ...any) error {
	return &ValidationError{Field: field, Msg: fmt.Sprintf(format, a...)}
}

var serverNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateServerName checks a server name: ^[a-z0-9][a-z0-9-]{0,62}$.
func ValidateServerName(name string) error {
	if !serverNameRE.MatchString(name) {
		return invalid("name", "name must be 1-63 characters of a-z, 0-9 and '-', starting with a letter or digit")
	}
	return nil
}

// ContainerName is the managed container name of a server.
func ContainerName(serverName string) string { return NamePrefix + serverName }

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// reservedEnvKeys would change how the docker CLI child itself behaves if a
// container variable with that name were placed in its environment, so they
// cannot be used as container variable names (compared case-insensitively,
// because Windows environments are case-insensitive).
func reservedEnvKey(key string) bool {
	up := strings.ToUpper(key)
	if strings.HasPrefix(up, "DOCKER_") || strings.HasPrefix(up, "BUILDKIT_") {
		return true
	}
	for _, goos := range []string{"linux", "windows"} {
		for _, n := range childEnvNames(goos) {
			if strings.EqualFold(n, key) {
				return true
			}
		}
	}
	switch up {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "SSH_AUTH_SOCK":
		return true
	}
	return false
}

// ValidateEnvKey checks one container variable name.
func ValidateEnvKey(key string) error {
	if !envKeyRE.MatchString(key) {
		return invalid("env", "variable names must match ^[A-Za-z_][A-Za-z0-9_]{0,127}$")
	}
	if reservedEnvKey(key) {
		return invalid("env", "variable name %q is reserved", key)
	}
	return nil
}

// ValidateEnv checks container variables: names (unique case-insensitively
// so the set is portable to Windows hosts), values without NUL, at most
// MaxEnvVars entries and MaxEnvBytes in total. Errors never contain values.
func ValidateEnv(env map[string]string) error {
	if len(env) > MaxEnvVars {
		return invalid("env", "at most %d variables are allowed", MaxEnvVars)
	}
	total := 0
	seen := make(map[string]bool, len(env))
	for k, v := range env {
		if err := ValidateEnvKey(k); err != nil {
			return err
		}
		up := strings.ToUpper(k)
		if seen[up] {
			return invalid("env", "variable names must be unique ignoring case (%q)", k)
		}
		seen[up] = true
		if strings.IndexByte(v, 0) >= 0 {
			return invalid("env", "the value of %q contains a NUL byte", k)
		}
		total += len(k) + len(v)
	}
	if total > MaxEnvBytes {
		return invalid("env", "variables exceed %d bytes in total", MaxEnvBytes)
	}
	return nil
}

var (
	imageIDRE  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	shortIDRE  = regexp.MustCompile(`^(sha256:)?[a-f0-9]{12,64}$`)
	imageRefRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
	volumeRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
)

// ValidateImageRef checks an image reference supplied by a client (a tag,
// a full or short id, or name@digest) before it reaches the CLI.
func ValidateImageRef(ref string) error {
	if !imageRefRE.MatchString(ref) || strings.Contains(ref, "..") {
		return invalid("image", "image must be an image id or a name[:tag]")
	}
	return nil
}

// ValidateImageID checks a (possibly shortened) image id.
func ValidateImageID(id string) error {
	if !shortIDRE.MatchString(id) {
		return invalid("id", "invalid image id")
	}
	return nil
}

// ValidateVolumeName checks a Docker volume name.
func ValidateVolumeName(name string) error {
	if !volumeRE.MatchString(name) {
		return invalid("name", "invalid volume name")
	}
	return nil
}

// ValidatePort checks a TCP/UDP port number.
func ValidatePort(field string, p int) error {
	if p < 1 || p > 65535 {
		return invalid(field, "port must be between 1 and 65535")
	}
	return nil
}

func serverIDLabel(labels map[string]string) int64 {
	id, err := strconv.ParseInt(labels[LabelServerID], 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
