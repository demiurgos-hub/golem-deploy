// Package git runs the host's git binary for project checkouts: clone,
// pull, branch switch and remote branch listing, with Git LFS support.
//
// Every command is hardened and non-interactive: it runs through
// exec.CommandContext (never a shell) with a minimal explicit environment
// (no system or global git config, no terminal prompts, a private HOME per
// operation), -c options that disable the file and ext transports, hooks,
// fsmonitor, credential helpers and submodule recursion, and "--" or
// "--end-of-options" before positional arguments. Remote URLs and branch
// names are validated (ValidateRemoteURL, ValidateBranch) before use.
//
// Credentials never appear in argv, URLs or .git/config: HTTPS passwords
// reach git through GIT_ASKPASS pointing at the golem-deploy executable
// itself (see Askpass), SSH keys are written to a 0600 file in a
// per-operation directory that is removed afterwards. All output written to
// the operation log passes through a scrubber that redacts the password and
// URL user info.
//
// Commands have per-runner timeouts and honour context cancellation, which
// kills git's whole process tree.
package git
