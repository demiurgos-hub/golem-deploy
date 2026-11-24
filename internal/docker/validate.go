package docker

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Labels set on every object the daemon creates. LabelManaged=ManagedValue
// marks an object as managed; LabelInstance=<instance id> marks it as
// created by this installation. Operations refuse containers, networks and
// volumes without both (images: see Client.ManagedImage).
const (
	LabelManaged    = "dev.golemengine.deploy.managed"
	LabelInstance   = "dev.golemengine.deploy.instance"
	LabelServerID   = "dev.golemengine.deploy.server-id"
	LabelServer     = "dev.golemengine.deploy.server"
	LabelProjectID  = "dev.golemengine.deploy.project-id"
	LabelCommit     = "dev.golemengine.deploy.commit"
	LabelLaunchID   = "dev.golemengine.deploy.launch-id"
	LabelConfigHash = "dev.golemengine.deploy.config-hash"
	// LabelBuildID and LabelEngineVersion are set on images by the build
	// pipeline: the id of the builds row and the Golem Engine version.
	LabelBuildID       = "dev.golemengine.deploy.build-id"
	LabelEngineVersion = "dev.golemengine.deploy.engine-version"
	ManagedValue       = "true"
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
	// MaxEnvVarBytes bounds one variable (key plus value). It keeps every
	// env-file line well below the docker CLI's 64 KiB line limit.
	MaxEnvVarBytes = 32 << 10
)

// ReservedEnvPrefix is reserved for variables the daemon injects itself
// (the engine contract); operator variables may not use it.
const ReservedEnvPrefix = "GOLEM_"

// IsManaged reports whether labels carry the managed label.
func IsManaged(labels map[string]string) bool { return labels[LabelManaged] == ManagedValue }

var instanceIDRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

// ValidInstanceID reports whether id is a well-formed instance id (32
// lower-case hex characters).
func ValidInstanceID(id string) bool { return instanceIDRE.MatchString(id) }

// ValidationError is a rejected input. Field names the offending input and
// Msg is safe to show to users. Key, when set, is the (valid) name of the
// environment variable whose value was rejected.
type ValidationError struct {
	Field string
	Key   string
	Msg   string
}

func (e *ValidationError) Error() string {
	if e.Key != "" {
		return e.Field + "." + e.Key + ": " + e.Msg
	}
	return e.Field + ": " + e.Msg
}

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

var containerIDRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ValidContainerID reports whether id is a full container id.
func ValidContainerID(id string) bool { return containerIDRE.MatchString(id) }

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidateEnvKey checks one container variable name. Names starting with
// ReservedEnvPrefix (any case) are reserved for the daemon. Container
// variables never reach the docker CLI's own environment (they travel in an
// env file), so names such as PATH or LD_PRELOAD are ordinary container
// variables.
func ValidateEnvKey(key string) error {
	if !envKeyRE.MatchString(key) {
		return invalid("env", "variable names must match ^[A-Za-z_][A-Za-z0-9_]{0,127}$")
	}
	if strings.HasPrefix(strings.ToUpper(key), ReservedEnvPrefix) {
		return invalid("env", "variable names starting with %s are reserved for golem-deploy (%q)", ReservedEnvPrefix, key)
	}
	return nil
}

// ValidateEnvValue checks one value: valid UTF-8 without NUL, CR or LF (the
// env-file format carries one KEY=value per line, taken literally) and at
// most MaxEnvVarBytes together with its key. Errors never contain values.
func ValidateEnvValue(key, value string) error {
	bad := func(format string, a ...any) error {
		return &ValidationError{Field: "env", Key: key, Msg: fmt.Sprintf(format, a...)}
	}
	switch {
	case len(key)+len(value) > MaxEnvVarBytes:
		return bad("the variable exceeds %d bytes", MaxEnvVarBytes)
	case strings.IndexByte(value, 0) >= 0:
		return bad("the value contains a NUL byte")
	case strings.ContainsAny(value, "\r\n"):
		return bad("the value contains a line break (values must be a single line)")
	case !utf8.ValidString(value):
		return bad("the value is not valid UTF-8")
	}
	return nil
}

// ValidateEnv checks container variables: names (unique case-insensitively
// so the set is portable to Windows hosts), values (ValidateEnvValue), at
// most MaxEnvVars entries and MaxEnvBytes in total. Errors never contain
// values.
func ValidateEnv(env map[string]string) error {
	if len(env) > MaxEnvVars {
		return invalid("env", "at most %d variables are allowed", MaxEnvVars)
	}
	total := 0
	seen := make(map[string]bool, len(env))
	for _, k := range EnvKeys(env) {
		if err := ValidateEnvKey(k); err != nil {
			return err
		}
		up := strings.ToUpper(k)
		if seen[up] {
			return invalid("env", "variable names must be unique ignoring case (%q)", k)
		}
		seen[up] = true
		v := env[k]
		if err := ValidateEnvValue(k, v); err != nil {
			return err
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
