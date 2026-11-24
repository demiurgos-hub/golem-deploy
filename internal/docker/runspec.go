package docker

import (
	"net/netip"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Defaults applied by RunSpec.
const (
	DefaultPidsLimit   = 512
	DefaultStopTimeout = 30 * time.Second
	// DefaultUID and DefaultGID are distroless "nonroot".
	DefaultUID = 65532
	DefaultGID = 65532
	// minMemoryMB is Docker's minimum memory limit (6 MiB), rounded up.
	minMemoryMB = 8
	// RestartPolicy is applied with "docker update" once a new container
	// has settled (it runs with "--restart no" until then, so a crash
	// during the settle window keeps its real exit code).
	RestartPolicy = "on-failure:5"
	// MaxImageVolumes bounds the VOLUME declarations an image may have; each
	// gets a size-limited tmpfs instead of an anonymous volume.
	MaxImageVolumes = 8
	// ImageVolumeTmpfs are the options of the tmpfs mounted at each
	// declared image volume (and at /tmp).
	ImageVolumeTmpfs = "rw,noexec,nosuid,size=64m"
)

// BindMount is a read-only bind mount of a host path into the container.
type BindMount struct {
	Source string // absolute host path
	Target string // absolute container path
}

// VolumeMount mounts a named volume read-write.
type VolumeMount struct {
	Name   string
	Target string
}

// RunSpec describes a server container. Every run applies the same
// hardening: no registry pull, read-only root filesystem, all capabilities
// dropped, no-new-privileges, an init process, no image health check, a
// fixed stop signal, a pids limit, a non-root user, size-limited noexec
// tmpfs mounts on /tmp and on every volume the image declares (so Docker
// never creates an anonymous volume), a bounded json-file log, the managed
// network, and every dev.golemengine.deploy.* label set explicitly so no
// image-inherited value survives.
type RunSpec struct {
	ServerID   int64
	ServerName string
	ProjectID  *int64
	// BuildID, Commit and EngineVersion describe the build the image comes
	// from; they are recorded in the build-id, commit and engine-version
	// labels (explicitly, so the image's own labels are never inherited).
	BuildID       int64
	Commit        string
	EngineVersion string
	// Image is the immutable image id ("sha256:<64 hex>") of a verified
	// build image (Client.BuildImage).
	Image string
	// ImageVolumes are the paths the image declares as volumes
	// (Image.Volumes); each gets a tmpfs unless a daemon mount (DataVolume,
	// ReadOnlyBinds, /tmp) has the same target.
	ImageVolumes []string
	// HostPort is published to GamePort as both UDP and TCP.
	HostPort int
	// BindAddress is the host IP the port is published on; empty means all
	// interfaces.
	BindAddress string
	// Env is the operator-defined environment (passed in an env file).
	Env map[string]string
	// DaemonEnv is environment injected by the daemon; it overrides Env.
	DaemonEnv map[string]string
	// MemoryMB and CPUs are resource limits; zero means unlimited. A memory
	// limit also sets --memory-swap to the same value (no swap).
	MemoryMB int
	CPUs     float64
	// PidsLimit zero means DefaultPidsLimit.
	PidsLimit int
	// UID and GID zero mean DefaultUID/DefaultGID. Root is not expressible.
	UID int
	GID int
	// GroupAdd are supplementary groups of the container user (for example
	// the daemon's group, so the user can read the certificate bind mount).
	// Group 0 is refused.
	GroupAdd []int
	// StopTimeout zero means DefaultStopTimeout.
	StopTimeout time.Duration
	// ConfigHash is recorded as a label so callers can tell whether the
	// running container matches the stored definition.
	ConfigHash string
	// LaunchID is the daemon-generated identity of a pending create.
	LaunchID string
	// ReadOnlyBinds are extra read-only bind mounts.
	ReadOnlyBinds []BindMount
	// DataVolume is an optional named volume mounted read-write.
	DataVolume *VolumeMount
	// AdminPort, when non-zero, is a container TCP port published on
	// 127.0.0.1 only: on AdminHostPort, or on a port Docker assigns when
	// AdminHostPort is zero (read it back with Container.LoopbackPort).
	AdminPort     int
	AdminHostPort int
}

func (s RunSpec) stopTimeout() time.Duration {
	if s.StopTimeout <= 0 {
		return DefaultStopTimeout
	}
	return s.StopTimeout
}

// mountPathOK rejects paths that could inject options into the --mount
// CSV syntax or that are not absolute and clean.
func mountPathOK(p string, hostWindows bool) bool {
	if p == "" || strings.ContainsAny(p, ",\"'\x00\n\r=") {
		return false
	}
	if hostWindows && len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return path.IsAbs(p) && path.Clean(p) == p
}

// ValidateBindSource reports whether a host path can be used as the source
// of a read-only bind mount (RunSpec.ReadOnlyBinds).
func ValidateBindSource(p string) error {
	if !mountPathOK(p, runtime.GOOS == "windows") {
		return invalid("mounts", "bind mount paths must be absolute and must not contain , \" ' = or control characters")
	}
	return nil
}

// mergedEnv returns Env overlaid with DaemonEnv (daemon variables replace
// operator variables of the same name ignoring case).
func (s RunSpec) mergedEnv() map[string]string {
	merged := make(map[string]string, len(s.Env)+len(s.DaemonEnv))
	for k, v := range s.Env {
		merged[k] = v
	}
	for k, v := range s.DaemonEnv {
		for ok := range merged {
			if strings.EqualFold(ok, k) {
				delete(merged, ok)
			}
		}
		merged[k] = v
	}
	return merged
}

// EnvFile validates the environment and returns the env-file content for
// "docker create --env-file" (nil when there are no variables).
func (s RunSpec) EnvFile() ([]byte, error) {
	if err := ValidateEnv(s.Env); err != nil {
		return nil, err
	}
	// Daemon variables may use the reserved prefix; check them without it.
	for k, v := range s.DaemonEnv {
		if !envKeyRE.MatchString(k) {
			return nil, invalid("daemon_env", "invalid daemon variable name")
		}
		if err := ValidateEnvValue(k, v); err != nil {
			return nil, err
		}
	}
	return encodeEnvFile(s.mergedEnv()), nil
}

// tmpfsTargets returns the image volume paths that get a tmpfs: cleaned,
// deduplicated, minus targets of daemon mounts.
func (s RunSpec) tmpfsTargets() ([]string, error) {
	if len(s.ImageVolumes) > MaxImageVolumes {
		return nil, invalid("image", "the image declares more than %d volumes", MaxImageVolumes)
	}
	taken := map[string]bool{"/tmp": true}
	if s.DataVolume != nil {
		taken[s.DataVolume.Target] = true
	}
	for _, b := range s.ReadOnlyBinds {
		taken[b.Target] = true
	}
	var out []string
	for _, v := range s.ImageVolumes {
		p := path.Clean(v)
		if !mountPathOK(p, false) || strings.ContainsRune(p, ':') || p == "/" {
			return nil, invalid("image", "the image declares a volume at an unsupported path")
		}
		if taken[p] {
			continue
		}
		taken[p] = true
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

// Args validates the spec and returns the "docker run" arguments. instance
// is the installation's instance id. envFilePath, when non-empty, is passed
// to --env-file (see EnvFile); it is required when the spec has variables.
func (s RunSpec) Args(instance, envFilePath string) ([]string, error) {
	if s.ServerID <= 0 {
		return nil, invalid("server_id", "server id must be positive")
	}
	if !ValidInstanceID(instance) {
		return nil, invalid("instance", "invalid instance id")
	}
	if err := ValidateServerName(s.ServerName); err != nil {
		return nil, err
	}
	if !imageIDRE.MatchString(s.Image) {
		return nil, invalid("image", "image must be a resolved image id")
	}
	if err := ValidatePort("port", s.HostPort); err != nil {
		return nil, err
	}
	hostIP := ""
	if s.BindAddress != "" {
		ip, err := netip.ParseAddr(s.BindAddress)
		if err != nil || ip.Zone() != "" {
			return nil, invalid("bind_address", "bind address must be an IP address")
		}
		hostIP = ip.String()
		if ip.Is6() && !ip.Is4In6() {
			hostIP = "[" + hostIP + "]"
		}
	}
	envFile, err := s.EnvFile()
	if err != nil {
		return nil, err
	}
	if len(envFile) > 0 && envFilePath == "" {
		return nil, invalid("env", "an env file is required")
	}
	if strings.ContainsAny(envFilePath, "\x00\n\r") {
		return nil, invalid("env", "invalid env file path")
	}
	if s.MemoryMB < 0 || (s.MemoryMB > 0 && s.MemoryMB < minMemoryMB) {
		return nil, invalid("memory_mb", "memory limit must be 0 (unlimited) or at least %d MiB", minMemoryMB)
	}
	if s.CPUs < 0 || s.CPUs > 1024 {
		return nil, invalid("cpus", "cpu limit must be between 0 (unlimited) and 1024")
	}
	if s.PidsLimit < 0 || s.UID < 0 || s.GID < 0 {
		return nil, invalid("spec", "limits and ids must not be negative")
	}
	if s.BuildID < 0 {
		return nil, invalid("build_id", "build id must not be negative")
	}
	for _, v := range []string{s.ConfigHash, s.Commit, s.EngineVersion} {
		if strings.ContainsAny(v, "\x00\n\r") {
			return nil, invalid("labels", "invalid label value")
		}
	}
	tmpfs, err := s.tmpfsTargets()
	if err != nil {
		return nil, err
	}
	pids, uid, gid := s.PidsLimit, s.UID, s.GID
	if pids == 0 {
		pids = DefaultPidsLimit
	}
	if uid == 0 {
		uid = DefaultUID
	}
	if gid == 0 {
		gid = DefaultGID
	}
	projectID := ""
	if s.ProjectID != nil {
		projectID = strconv.FormatInt(*s.ProjectID, 10)
	}
	buildID := ""
	if s.BuildID > 0 {
		buildID = strconv.FormatInt(s.BuildID, 10)
	}
	if s.LaunchID != "" && !ValidInstanceID(s.LaunchID) {
		return nil, invalid("launch", "invalid launch id")
	}
	for _, g := range s.GroupAdd {
		if g <= 0 {
			return nil, invalid("group_add", "supplementary groups must be positive")
		}
	}

	args := []string{
		"create", "--pull", "never",
		"--name", ContainerName(s.ServerName),
		// Every label of our namespace, so none can be inherited from the
		// image.
		"--label", LabelManaged + "=" + ManagedValue,
		"--label", LabelInstance + "=" + instance,
		"--label", LabelServerID + "=" + strconv.FormatInt(s.ServerID, 10),
		"--label", LabelServer + "=" + s.ServerName,
		"--label", LabelProjectID + "=" + projectID,
		"--label", LabelCommit + "=" + s.Commit,
		"--label", LabelBuildID + "=" + buildID,
		"--label", LabelEngineVersion + "=" + s.EngineVersion,
		"--label", LabelConfigHash + "=" + s.ConfigHash,
		"--label", LabelLaunchID + "=" + s.LaunchID,
		"--restart", "no",
		"--init",
		"--no-healthcheck",
		"--stop-signal", "SIGTERM",
		"--stop-timeout", strconv.Itoa(int(s.stopTimeout() / time.Second)),
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(pids),
	}
	if s.MemoryMB > 0 {
		mem := strconv.Itoa(s.MemoryMB) + "m"
		args = append(args, "--memory", mem, "--memory-swap", mem)
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64))
	}
	args = append(args,
		"--log-driver", "json-file",
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=3",
		"--tmpfs", "/tmp:"+ImageVolumeTmpfs,
	)
	for _, p := range tmpfs {
		args = append(args, "--tmpfs", p+":"+ImageVolumeTmpfs)
	}
	args = append(args, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(gid))
	for _, g := range s.GroupAdd {
		args = append(args, "--group-add", strconv.Itoa(g))
	}
	args = append(args, "--network", NetworkName)
	for _, proto := range []string{"udp", "tcp"} {
		p := strconv.Itoa(s.HostPort) + ":" + strconv.Itoa(GamePort) + "/" + proto
		if hostIP != "" {
			p = hostIP + ":" + p
		}
		args = append(args, "--publish", p)
	}
	if s.AdminPort != 0 {
		if err := ValidatePort("admin_port", s.AdminPort); err != nil {
			return nil, err
		}
		host := ""
		if s.AdminHostPort != 0 {
			if err := ValidatePort("admin_host_port", s.AdminHostPort); err != nil {
				return nil, err
			}
			host = strconv.Itoa(s.AdminHostPort)
		}
		if s.AdminPort == GamePort {
			return nil, invalid("admin_port", "the admin port must differ from the game port")
		}
		args = append(args, "--publish", "127.0.0.1:"+host+":"+strconv.Itoa(s.AdminPort)+"/tcp")
	}
	for _, b := range s.ReadOnlyBinds {
		if !mountPathOK(b.Source, true) || !mountPathOK(b.Target, false) {
			return nil, invalid("mounts", "bind mount paths must be absolute and must not contain , \" ' = or control characters")
		}
		args = append(args, "--mount", "type=bind,source="+b.Source+",target="+b.Target+",readonly")
	}
	if v := s.DataVolume; v != nil {
		if err := ValidateVolumeName(v.Name); err != nil {
			return nil, err
		}
		if !mountPathOK(v.Target, false) {
			return nil, invalid("mounts", "volume target must be an absolute container path")
		}
		args = append(args, "--mount", "type=volume,source="+v.Name+",target="+v.Target)
	}
	if len(envFile) > 0 {
		args = append(args, "--env-file", envFilePath)
	}
	args = append(args, "--", s.Image)
	return args, nil
}

// EnvKeys returns the variable names of an env map, sorted.
func EnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
