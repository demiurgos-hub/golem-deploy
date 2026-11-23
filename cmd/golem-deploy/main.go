// Command golem-deploy is the Golem Deploy daemon and its local admin CLI.
//
//	golem-deploy serve [flags]
//	golem-deploy admin create --username <u> [--password-stdin] [--data-dir <dir>]
//	golem-deploy admin reset-password --username <u> [--password-stdin] [--data-dir <dir>]
//	golem-deploy version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/demiurgos-hub/golem-deploy/frontend"
	"github.com/demiurgos-hub/golem-deploy/internal/daemon"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

const usage = `Usage:
  golem-deploy serve [flags]                 run the daemon
  golem-deploy admin create --username <u>   create an approved admin account
  golem-deploy admin reset-password --username <u>
                                             set a new password (ends the account's sessions)
  golem-deploy version                       print the version

Run "golem-deploy <command> -h" for the flags of a command.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "admin":
		return cmdAdmin(args[1:], stdin, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, buildVersion())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func envDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: invalid boolean %q", key, v)
	}
	return b, nil
}

func envInt(key string) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q", key, v)
	}
	return n, nil
}

func envFloat(key string) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid number %q", key, v)
	}
	return f, nil
}

func dataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", envDefault("GOLEM_DEPLOY_DATA_DIR", "./data"), "data directory (env GOLEM_DEPLOY_DATA_DIR)")
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	insecureDefault, err := envBool("GOLEM_DEPLOY_INSECURE_HTTP")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	dataDir := dataDirFlag(fs)
	listen := fs.String("listen", envDefault("GOLEM_DEPLOY_LISTEN", "127.0.0.1:8000"), "listen address (env GOLEM_DEPLOY_LISTEN)")
	tlsCert := fs.String("tls-cert", envDefault("GOLEM_DEPLOY_TLS_CERT", ""), "TLS certificate file (env GOLEM_DEPLOY_TLS_CERT)")
	tlsKey := fs.String("tls-key", envDefault("GOLEM_DEPLOY_TLS_KEY", ""), "TLS private key file (env GOLEM_DEPLOY_TLS_KEY)")
	insecure := fs.Bool("insecure-http", insecureDefault, "allow plaintext HTTP on a non-loopback address, e.g. behind a TLS-terminating proxy (env GOLEM_DEPLOY_INSECURE_HTTP)")
	portRange := fs.String("port-range", envDefault("GOLEM_DEPLOY_PORT_RANGE", "7000-7099"), "host port range for game servers, published as UDP and TCP (env GOLEM_DEPLOY_PORT_RANGE)")
	portBind := fs.String("port-bind-address", envDefault("GOLEM_DEPLOY_PORT_BIND_ADDRESS", ""), "host IP game server ports are published on; empty means all interfaces (env GOLEM_DEPLOY_PORT_BIND_ADDRESS)")
	memDefault, err := envInt("GOLEM_DEPLOY_SERVER_MEMORY_MB")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cpuDefault, err := envFloat("GOLEM_DEPLOY_SERVER_CPUS")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	memoryMB := fs.Int("server-memory-mb", memDefault, "memory limit per server container in MiB; 0 means unlimited (env GOLEM_DEPLOY_SERVER_MEMORY_MB)")
	cpus := fs.Float64("server-cpus", cpuDefault, "CPU limit per server container; 0 means unlimited (env GOLEM_DEPLOY_SERVER_CPUS)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "serve: unexpected arguments %q\n", fs.Args())
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = daemon.Run(ctx, daemon.Options{
		DataDir:         *dataDir,
		Listen:          *listen,
		TLSCert:         *tlsCert,
		TLSKey:          *tlsKey,
		InsecureHTTP:    *insecure,
		PortRange:       *portRange,
		PortBindAddress: *portBind,
		ServerMemoryMB:  *memoryMB,
		ServerCPUs:      *cpus,
		Version:         buildVersion(),
		Logger:          logger,
		SPA:             frontend.Handler(),
	})
	if err != nil {
		logger.Error("golem-deploy failed", "err", err)
		return 1
	}
	return 0
}

func cmdAdmin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	if sub != "create" && sub != "reset-password" {
		fmt.Fprintf(stderr, "unknown admin command %q\n\n%s", sub, usage)
		return 2
	}
	fs := flag.NewFlagSet("admin "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	username := fs.String("username", "", "account username (required)")
	fromStdin := fs.Bool("password-stdin", false, "read the password from the first line of standard input instead of prompting")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *username == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "admin %s: --username is required and no positional arguments are accepted\n", sub)
		fs.Usage()
		return 2
	}
	password, err := readPassword(stdin, stderr, *fromStdin)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	ctx := context.Background()
	switch sub {
	case "create":
		acct, err := daemon.CreateAdmin(ctx, *dataDir, *username, password)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "created admin account %q (id %d)\n", acct.Username, acct.ID)
	case "reset-password":
		if err := daemon.ResetPassword(ctx, *dataDir, *username, password); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "password updated for %q; existing sessions have been ended\n", strings.ToLower(*username))
	}
	return 0
}

// readPassword reads the first line of stdin, or prompts twice on a
// terminal.
func readPassword(stdin io.Reader, stderr io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && line != "") {
			return "", errors.New("could not read a password from standard input")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("standard input is not a terminal; use --password-stdin to pipe the password")
	}
	fmt.Fprint(stderr, "Password: ")
	p1, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprint(stderr, "Confirm password: ")
	p2, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(p1) != string(p2) {
		return "", errors.New("passwords do not match")
	}
	return string(p1), nil
}
