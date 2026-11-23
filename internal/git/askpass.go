package git

import (
	"fmt"
	"io"
	"net"
	"strings"
)

// Environment variables of the askpass protocol. They are set only on the
// git child process of an operation that uses AuthHTTPS; git passes them on
// to the helper it spawns (GIT_ASKPASS is the golem-deploy executable).
const (
	// AskpassEnv marks an invocation of the golem-deploy binary as an
	// askpass helper.
	AskpassEnv         = "GOLEM_DEPLOY_ASKPASS"
	askpassUsernameEnv = "GOLEM_DEPLOY_ASKPASS_USERNAME"
	askpassPasswordEnv = "GOLEM_DEPLOY_ASKPASS_PASSWORD"
	// askpassHostEnv is the host[:port] of the remote exactly as in the
	// project's URL; the helper answers only prompts for exactly that host,
	// so a redirect to another host never receives the credentials.
	askpassHostEnv = "GOLEM_DEPLOY_ASKPASS_HOST"
)

// Askpass implements the GIT_ASKPASS protocol when the process was started
// as a helper (AskpassEnv=1). It reports handled=false otherwise, and the
// caller continues normally. git runs the helper with the prompt as the only
// argument and reads the answer from stdout.
//
// The helper answers only the exact prompts git prints for the configured
// host and user (see askpassPrompts); any other prompt gets no output and a
// non-zero exit, which makes git fail the operation.
//
// Call it first thing in main, before flag parsing:
//
//	if code, ok := git.Askpass(os.Args, os.Getenv, os.Stdout); ok {
//		os.Exit(code)
//	}
func Askpass(args []string, getenv func(string) string, stdout io.Writer) (code int, handled bool) {
	if getenv(AskpassEnv) != "1" {
		return 0, false
	}
	if len(args) != 2 {
		return 1, true
	}
	answer, ok := askpassAnswer(args[1], getenv)
	if !ok {
		return 1, true
	}
	if _, err := io.WriteString(stdout, answer+"\n"); err != nil {
		return 1, true
	}
	return 0, true
}

func askpassAnswer(prompt string, getenv func(string) string) (string, bool) {
	host, user, pass := getenv(askpassHostEnv), getenv(askpassUsernameEnv), getenv(askpassPasswordEnv)
	if host == "" || user == "" || pass == "" {
		return "", false
	}
	prompt = strings.TrimSuffix(prompt, "\n")
	userPrompts, passPrompts := askpassPrompts(host, user)
	for _, p := range userPrompts {
		if prompt == p {
			return user, true
		}
	}
	for _, p := range passPrompts {
		if prompt == p {
			return pass, true
		}
	}
	return "", false
}

// askpassPrompts returns every prompt the helper answers for host (the
// host[:port] of the project's https URL) and user:
//
//	Username for 'https://<host>':
//	Password for 'https://<user>@<host>':
//
// <host> is the configured host[:port] and its ASCII-lower-case form; when
// the port is absent or 443 (the https default), both the form without a
// port and the form with ":443" are accepted. IPv6 literals keep their
// brackets. <user> is the user name raw and percent-encoded the way git
// does it since 2.48.1 (CVE-2024-50349), with and without '/' encoded.
// Nothing else is accepted: no path, query, other user, other scheme or
// quoting.
func askpassPrompts(host, user string) (userPrompts, passPrompts []string) {
	hosts := askpassHosts(host)
	users := uniq([]string{user, gitPercentEncode(user, true), gitPercentEncode(user, false)})
	for _, h := range hosts {
		userPrompts = append(userPrompts, fmt.Sprintf("Username for 'https://%s': ", h))
		for _, u := range users {
			passPrompts = append(passPrompts, fmt.Sprintf("Password for 'https://%s@%s': ", u, h))
		}
	}
	return userPrompts, passPrompts
}

// askpassHosts expands host[:port] into the accepted spellings (see
// askpassPrompts). A malformed value yields only itself.
func askpassHosts(host string) []string {
	name, port := host, ""
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return []string{host}
		}
		name, port = host[:end+1], host[end+1:]
		if port != "" {
			if !strings.HasPrefix(port, ":") {
				return []string{host}
			}
			port = port[1:]
		}
	} else if h, p, err := net.SplitHostPort(host); err == nil {
		name, port = h, p
	}
	var forms []string
	switch port {
	case "", "443":
		forms = []string{name, name + ":443"}
	default:
		forms = []string{name + ":" + port}
	}
	for _, f := range forms[:len(forms):len(forms)] {
		forms = append(forms, strings.ToLower(f))
	}
	return uniq(forms)
}

// gitURLUnsafe are the characters git's strbuf_add_percentencode encodes in
// user names (URL_UNSAFE_CHARS), besides controls and non-ASCII bytes.
const gitURLUnsafe = " <>\"%{}|\\^`:?#[]@"

// gitPercentEncode mirrors git's strbuf_add_percentencode for a user name.
func gitPercentEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x1f || c >= 0x7f || (c == '/' && encodeSlash) || strings.IndexByte(gitURLUnsafe, c) >= 0 {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
