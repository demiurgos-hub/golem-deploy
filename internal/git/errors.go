package git

import (
	"errors"
	"strings"
)

// Reason classifies a failed operation for a user-facing message.
type Reason int

// Failure reasons.
const (
	ReasonUnknown Reason = iota
	ReasonInvalidInput
	ReasonAuth
	ReasonNotFound
	ReasonBranchNotFound
	ReasonHostKey
	ReasonNetwork
	ReasonTLS
	ReasonTimeout
	ReasonCancelled
	ReasonEmptyRepository
	ReasonLFSMissing
	ReasonGitMissing
	ReasonLocal
	// ReasonLFSConfig: the checkout's .lfsconfig sets endpoint or transfer
	// settings, which are refused (the LFS endpoint is pinned).
	ReasonLFSConfig
	// ReasonSizeLimit: the checkout grew beyond Options.MaxRepoSize.
	ReasonSizeLimit
	// ReasonDiskSpace: free space on the data filesystem is below
	// Options.MinFreeSpace.
	ReasonDiskSpace
	// ReasonDeployConfig: the checkout's golem-deploy.yaml could not be
	// read or is not valid, so the Git LFS download could not be limited
	// (Options.LFSInclude).
	ReasonDeployConfig
)

// Error is returned by Runner operations. Its Error text contains no
// command output (that goes to the operation's log) and no secrets.
type Error struct {
	// Op is the operation ("clone", "pull", ...).
	Op     string
	Reason Reason
	// Unchanged reports that the operation failed before modifying the
	// checkout, which is still in its previous state.
	Unchanged bool
	Err       error
	// detail is a user-safe description (sanitised, single line) that
	// Message returns for ReasonDeployConfig.
	detail string
}

// Error returns "git <op>: <cause>", or the reason's message when there is
// no cause (callers usually show Message separately).
func (e *Error) Error() string {
	if e.Err != nil {
		return "git " + e.Op + ": " + e.Err.Error()
	}
	return "git " + e.Op + ": " + e.Message()
}

func (e *Error) Unwrap() error { return e.Err }

// Message returns a short, user-safe description of the failure.
func (e *Error) Message() string {
	switch e.Reason {
	case ReasonInvalidInput:
		return "invalid repository settings"
	case ReasonAuth:
		return "authentication with the Git remote failed; check the project's credentials"
	case ReasonNotFound:
		return "the repository was not found or access was denied"
	case ReasonBranchNotFound:
		return "the branch does not exist on the remote"
	case ReasonHostKey:
		return "SSH host key verification failed; the remote's host key does not match the recorded one"
	case ReasonNetwork:
		return "could not reach the Git remote"
	case ReasonTLS:
		return "TLS certificate verification for the Git remote failed"
	case ReasonTimeout:
		return "the Git operation timed out"
	case ReasonCancelled:
		return "the Git operation was cancelled"
	case ReasonEmptyRepository:
		return "the repository has no commits"
	case ReasonLFSMissing:
		return "the repository uses Git LFS but git-lfs is not installed on the host"
	case ReasonGitMissing:
		return "git is not installed on the host"
	case ReasonLocal:
		return "updating the checkout failed; see the job log"
	case ReasonLFSConfig:
		return "the repository's .lfsconfig sets the Git LFS server or transfer settings, which is not allowed; " +
			"the LFS server is always derived from the project's repository URL (see the job log)"
	case ReasonSizeLimit:
		return "the checkout exceeds the maximum repository size configured on the host"
	case ReasonDiskSpace:
		return "not enough free disk space on the host for the checkout"
	case ReasonDeployConfig:
		if e.detail != "" {
			return e.detail
		}
		return "the repository's " + deployConfigFile + " could not be read or is not valid; see the job log"
	}
	return "the Git operation failed; see the job log"
}

// SafeMessage returns the user-safe message of err if it is an *Error, and
// a generic message otherwise.
func SafeMessage(err error) string {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Message()
	}
	return "the Git operation failed; see the job log"
}

// IsUnchanged reports whether err left the checkout in its previous state.
func IsUnchanged(err error) bool {
	var ge *Error
	return errors.As(err, &ge) && ge.Unchanged
}

// classify maps (scrubbed, English) git output to a Reason.
func classify(output string) Reason {
	o := strings.ToLower(output)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(o, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("host key verification failed", "remote host identification has changed", "no matching host key"):
		return ReasonHostKey
	case has("'lfs' is not a git command", "git-lfs: command not found", "git-lfs was not found"):
		return ReasonLFSMissing
	case has("couldn't find remote ref", "remote branch", "not found in upstream"):
		return ReasonBranchNotFound
	case has("authentication failed", "could not read username", "could not read password",
		"terminal prompts disabled", "permission denied (publickey", "permission denied, please try again",
		"invalid username or password", "http basic: access denied", "the requested url returned error: 401",
		"the requested url returned error: 403"):
		return ReasonAuth
	case has("ssl certificate problem", "server certificate verification failed", "certificate verify failed", "schannel"):
		return ReasonTLS
	case has("could not resolve host", "connection refused", "connection timed out", "failed to connect",
		"network is unreachable", "no route to host", "could not resolve hostname", "connection reset", "operation timed out"):
		return ReasonNetwork
	case has("repository not found", "does not appear to be a git repository", "the requested url returned error: 404",
		"could not read from remote repository", "not found"):
		return ReasonNotFound
	case has("cloned an empty repository"):
		return ReasonEmptyRepository
	}
	return ReasonUnknown
}
