package servers

import (
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
)

// JobLogPrefix starts every line of container output copied into a job log,
// so it cannot be mistaken for a message of the daemon. Container output is
// made safe to show with internal/logsafe (control and bidirectional
// characters escaped, invalid UTF-8 replaced, lines capped).
const JobLogPrefix = "container | "

// redact replaces, in container output, every line (of at least 4 bytes)
// of every env value with "[redacted]". It is best effort: a game that
// prints a value transformed (encoded, split, reversed) is not caught.
func redact(b []byte, env map[string]string) []byte {
	return []byte(docker.RedactValues(string(b), env))
}
