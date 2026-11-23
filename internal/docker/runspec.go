package docker

import (
	"net/netip"
	"path"
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
// hardening: read-only root filesystem, all capabilities dropped,
// no-new-privileges, a pids limit, a non-root user, a size-limited noexec
// /tmp, a bounded json-file log, the managed network and labels.
type RunSpec struct {
	ServerID   int64
	ServerName string
	ProjectID  *int64
	// Image is the immutable image id ("sha256:<64 hex>") of a managed image
	// (resolve references with Client.ManagedImage first).
	Image string
	// HostPort is published to GamePort as both UDP and TCP.
	HostPort int
	// BindAddress is the host IP the port is published on; empty means all
	// interfaces.
	BindAddress string
	// Env is the operator-defined environment (values stay out of argv).
	Env map[string]string
	// DaemonEnv is environment injected by the daemon; it overrides Env.
	DaemonEnv map[string]string
	// MemoryMB and CPUs are resource limits; zero means unlimited.
	MemoryMB int
	CPUs     float64
	// PidsLimit zero means DefaultPidsLimit.
	PidsLimit int
	// UID and GID zero mean DefaultUID/DefaultGID. Root is not expressible.
	UID int
	GID int
	// StopTimeout zero means DefaultStopTimeout.
	StopTimeout time.Duration
	// ConfigHash is recorded as a label so callers can tell whether the
	// running container matches the stored definition.
	ConfigHash string
	// ReadOnlyBinds are extra read-only bind mounts.
	ReadOnlyBinds []BindMount
	// DataVolume is an optional named volume mounted read-write.
	DataVolume *VolumeMount
	// AdminPort, when non-zero, is a container TCP port published on
	// 127.0.0.1:AdminHostPort only.
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

// Args validates the spec and returns the "docker run" arguments and the
// KEY=value entries that must be added to the CLI child's environment.
func (s RunSpec) Args() (args []string, secretEnv []string, err error) {
	if s.ServerID <= 0 {
		return nil, nil, invalid("server_id", "server id must be positive")
	}
	if err := ValidateServerName(s.ServerName); err != nil {
		return nil, nil, err
	}
	if !imageIDRE.MatchString(s.Image) {
		return nil, nil, invalid("image", "image must be a resolved image id")
	}
	if err := ValidatePort("port", s.HostPort); err != nil {
		return nil, nil, err
	}
	hostIP := ""
	if s.BindAddress != "" {
		ip, err := netip.ParseAddr(s.BindAddress)
		if err != nil || ip.Zone() != "" {
			return nil, nil, invalid("bind_address", "bind address must be an IP address")
		}
		hostIP = ip.String()
		if ip.Is6() && !ip.Is4In6() {
			hostIP = "[" + hostIP + "]"
		}
	}
	if err := ValidateEnv(s.Env); err != nil {
		return nil, nil, err
	}
	if err := ValidateEnv(s.DaemonEnv); err != nil {
		return nil, nil, err
	}
	if s.MemoryMB < 0 || (s.MemoryMB > 0 && s.MemoryMB < minMemoryMB) {
		return nil, nil, invalid("memory_mb", "memory limit must be 0 (unlimited) or at least %d MiB", minMemoryMB)
	}
	if s.CPUs < 0 || s.CPUs > 1024 {
		return nil, nil, invalid("cpus", "cpu limit must be between 0 (unlimited) and 1024")
	}
	if s.PidsLimit < 0 || s.UID < 0 || s.GID < 0 {
		return nil, nil, invalid("spec", "limits and ids must not be negative")
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

	args = []string{
		"run", "--detach",
		"--name", ContainerName(s.ServerName),
		"--label", LabelManaged + "=" + ManagedValue,
		"--label", LabelServerID + "=" + strconv.FormatInt(s.ServerID, 10),
		"--label", LabelServer + "=" + s.ServerName,
	}
	if s.ProjectID != nil {
		args = append(args, "--label", LabelProjectID+"="+strconv.FormatInt(*s.ProjectID, 10))
	}
	if s.ConfigHash != "" {
		if strings.ContainsAny(s.ConfigHash, "\x00\n\r") {
			return nil, nil, invalid("config_hash", "invalid config hash")
		}
		args = append(args, "--label", LabelConfigHash+"="+s.ConfigHash)
	}
	args = append(args,
		"--restart", "on-failure:5",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(pids),
	)
	if s.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(s.MemoryMB)+"m")
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64))
	}
	args = append(args,
		"--log-driver", "json-file",
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=3",
		"--stop-timeout", strconv.Itoa(int(s.stopTimeout()/time.Second)),
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--user", strconv.Itoa(uid)+":"+strconv.Itoa(gid),
		"--network", NetworkName,
	)
	for _, proto := range []string{"udp", "tcp"} {
		p := strconv.Itoa(s.HostPort) + ":" + strconv.Itoa(GamePort) + "/" + proto
		if hostIP != "" {
			p = hostIP + ":" + p
		}
		args = append(args, "--publish", p)
	}
	if s.AdminPort != 0 {
		if err := ValidatePort("admin_port", s.AdminPort); err != nil {
			return nil, nil, err
		}
		if err := ValidatePort("admin_host_port", s.AdminHostPort); err != nil {
			return nil, nil, err
		}
		args = append(args, "--publish", "127.0.0.1:"+strconv.Itoa(s.AdminHostPort)+":"+strconv.Itoa(s.AdminPort)+"/tcp")
	}
	for _, b := range s.ReadOnlyBinds {
		if !mountPathOK(b.Source, true) || !mountPathOK(b.Target, false) {
			return nil, nil, invalid("mounts", "bind mount paths must be absolute and must not contain , \" ' = or control characters")
		}
		args = append(args, "--mount", "type=bind,source="+b.Source+",target="+b.Target+",readonly")
	}
	if v := s.DataVolume; v != nil {
		if err := ValidateVolumeName(v.Name); err != nil {
			return nil, nil, err
		}
		if !mountPathOK(v.Target, false) {
			return nil, nil, invalid("mounts", "volume target must be an absolute container path")
		}
		args = append(args, "--mount", "type=volume,source="+v.Name+",target="+v.Target)
	}

	// Environment: names in argv, values in the child environment. Daemon
	// variables override operator variables of the same name.
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
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--env", k)
		secretEnv = append(secretEnv, k+"="+merged[k])
	}
	args = append(args, "--", s.Image)
	return args, secretEnv, nil
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
