package docker

import (
	"errors"
	"strconv"
)

// Sentinel error kinds. Callers use errors.Is.
var (
	// ErrUnavailable: the docker CLI is missing or the daemon is unreachable.
	ErrUnavailable = errors.New("docker: daemon unreachable")
	// ErrNotFound: the object does not exist.
	ErrNotFound = errors.New("docker: not found")
	// ErrNotManaged: the object exists but does not carry the managed label.
	ErrNotManaged = errors.New("docker: object is not managed by golem-deploy")
	// ErrInUse: the object is used by a container.
	ErrInUse = errors.New("docker: object is in use")
	// ErrConflict: a name is taken or another state conflict.
	ErrConflict = errors.New("docker: conflict")
	// ErrPortBusy: a host port could not be bound.
	ErrPortBusy = errors.New("docker: host port is busy")
	// ErrNotRunning: the container is not running.
	ErrNotRunning = errors.New("docker: container is not running")
)

// CmdError describes a failed CLI call. Detail is the CLI's (capped)
// standard error, which may name containers or images but never contains
// container environment values (those are never in argv). It belongs in
// daemon and job logs, never in HTTP responses.
type CmdError struct {
	Op       string
	Kind     error
	ExitCode int
	Detail   string
	cause    error
}

func (e *CmdError) Error() string {
	s := "docker " + e.Op
	if e.Kind != nil {
		s += ": " + e.Kind.Error()
	}
	if e.ExitCode != 0 {
		s += " (exit " + strconv.Itoa(e.ExitCode) + ")"
	}
	if e.Detail != "" {
		d := e.Detail
		if len(d) > 512 {
			d = d[:512] + "…"
		}
		s += ": " + d
	}
	return s
}

// Unwrap exposes the kind (for errors.Is) and the process error.
func (e *CmdError) Unwrap() []error {
	var errs []error
	if e.Kind != nil {
		errs = append(errs, e.Kind)
	}
	if e.cause != nil {
		errs = append(errs, e.cause)
	}
	return errs
}
