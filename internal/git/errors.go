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
}

func (e *Error) Error() string {
	msg := "git " + e.Op + ": " + e.Message()
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
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
