package git

import (
	"io"
	"net/url"
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
	// askpassHostEnv is the host[:port] of the remote; the helper answers
	// only prompts for exactly that host, so a redirect to another host
	// never receives the credentials.
	askpassHostEnv = "GOLEM_DEPLOY_ASKPASS_HOST"
)

// Askpass implements the GIT_ASKPASS protocol when the process was started
// as a helper (AskpassEnv=1). It reports handled=false otherwise, and the
// caller continues normally. git runs the helper with the prompt as the only
// argument ("Username for 'https://host': " or "Password for
// 'https://user@host': ") and reads the answer from stdout.
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
	var env string
	switch {
	case strings.HasPrefix(prompt, "Username for "):
		env = askpassUsernameEnv
	case strings.HasPrefix(prompt, "Password for "):
		env = askpassPasswordEnv
	default:
		return "", false
	}
	first, last := strings.IndexByte(prompt, '\''), strings.LastIndexByte(prompt, '\'')
	if first < 0 || last <= first {
		return "", false
	}
	u, err := url.Parse(prompt[first+1 : last])
	if err != nil || u.Scheme != "https" {
		return "", false
	}
	want := getenv(askpassHostEnv)
	if want == "" || !strings.EqualFold(u.Host, want) {
		return "", false
	}
	v := getenv(env)
	if v == "" {
		return "", false
	}
	return v, true
}
