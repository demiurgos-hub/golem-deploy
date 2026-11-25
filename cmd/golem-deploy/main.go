// Command golem-deploy is the Golem Deploy daemon and its local admin CLI.
//
//	golem-deploy serve [flags]
//	golem-deploy admin create --username <u> [--password-stdin] [--data-dir <dir>]
//	golem-deploy admin reset-password --username <u> [--password-stdin] [--data-dir <dir>]
//	golem-deploy admin setup-link [--data-dir <dir>] [--origin <origin>] [--ttl <duration>]
//	golem-deploy admin instance-id [--data-dir <dir>]
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
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/demiurgos-hub/golem-deploy/frontend"
	"github.com/demiurgos-hub/golem-deploy/internal/daemon"
	"github.com/demiurgos-hub/golem-deploy/internal/git"
	"github.com/demiurgos-hub/golem-deploy/internal/inspector"
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
                                             set a new password (ends the account's sessions
                                             and deletes its API tokens)
  golem-deploy admin setup-link              print a one-time link that creates the first
                                             admin account in a browser
  golem-deploy admin instance-id             print the instance id this installation labels its
                                             Docker objects with
  golem-deploy version                       print the version

Run "golem-deploy <command> -h" for the flags of a command.
`

func main() {
	// git runs this binary as its GIT_ASKPASS helper; answer before anything
	// else (see internal/git.Askpass).
	if code, ok := git.Askpass(os.Args, os.Getenv, os.Stdout); ok {
		os.Exit(code)
	}
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
	httpListen := fs.String("http-listen", envDefault("GOLEM_DEPLOY_HTTP_LISTEN", ""), "HTTP redirect and ACME HTTP-01 listen address (env GOLEM_DEPLOY_HTTP_LISTEN)")
	acmeDirectory := fs.String("acme-directory", envDefault("GOLEM_DEPLOY_ACME_DIRECTORY", ""), "override the ACME directory URL (env GOLEM_DEPLOY_ACME_DIRECTORY)")
	acmeRootCA := fs.String("acme-root-ca", envDefault("GOLEM_DEPLOY_ACME_ROOT_CA", ""), "PEM roots for the ACME connection (env GOLEM_DEPLOY_ACME_ROOT_CA)")
	insecure := fs.Bool("insecure-http", insecureDefault, "allow plaintext HTTP on a non-loopback address, e.g. behind a TLS-terminating proxy (env GOLEM_DEPLOY_INSECURE_HTTP)")
	publicHost := fs.String("public-host", envDefault("GOLEM_DEPLOY_PUBLIC_HOST", ""), "comma-separated panel DNS names or IP addresses, also used for managed TLS (env GOLEM_DEPLOY_PUBLIC_HOST)")
	trustedProxy := fs.String("trusted-proxy", envDefault("GOLEM_DEPLOY_TRUSTED_PROXY", ""), "comma-separated CIDRs of reverse proxies whose X-Forwarded-For is trusted for the client address (env GOLEM_DEPLOY_TRUSTED_PROXY)")
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
	repoSizeDefault := 10240
	if v := envDefault("GOLEM_DEPLOY_MAX_REPO_SIZE_MB", ""); v != "" {
		if repoSizeDefault, err = strconv.Atoi(v); err != nil {
			fmt.Fprintf(stderr, "GOLEM_DEPLOY_MAX_REPO_SIZE_MB: invalid integer %q\n", v)
			return 2
		}
	}
	maxRepoSize := fs.Int("max-repo-size-mb", repoSizeDefault, "maximum size of each project checkout in MiB, enforced during clone, pull and Git LFS downloads; 0 means unlimited (env GOLEM_DEPLOY_MAX_REPO_SIZE_MB)")
	if os.Getenv("GOLEM_DEPLOY_SERVER_MEMORY_MB") == "" {
		memDefault = 2048
	}
	if os.Getenv("GOLEM_DEPLOY_SERVER_CPUS") == "" {
		cpuDefault = 2
	}
	staticQuotaDefault, err := envInt("GOLEM_DEPLOY_STATIC_QUOTA_MB")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if os.Getenv("GOLEM_DEPLOY_STATIC_QUOTA_MB") == "" {
		staticQuotaDefault = 5120
	}
	memoryMB := fs.Int("server-memory-mb", memDefault, "memory limit per server container in MiB (swap is not added); 0 means unlimited (env GOLEM_DEPLOY_SERVER_MEMORY_MB)")
	cpus := fs.Float64("server-cpus", cpuDefault, "CPU limit per server container; 0 means unlimited (env GOLEM_DEPLOY_SERVER_CPUS)")
	staticQuota := fs.Int("static-quota-mb", staticQuotaDefault, "total size limit of the static file store in MiB; 0 means unlimited (env GOLEM_DEPLOY_STATIC_QUOTA_MB)")
	startTimeoutDefault := 60 * time.Second
	if v := envDefault("GOLEM_DEPLOY_START_TIMEOUT", ""); v != "" {
		if startTimeoutDefault, err = time.ParseDuration(v); err != nil {
			fmt.Fprintf(stderr, "GOLEM_DEPLOY_START_TIMEOUT: invalid duration %q\n", v)
			return 2
		}
	}
	startTimeout := fs.Duration("start-timeout", startTimeoutDefault, "how long a server start waits for the game to report ready on its admin endpoint (env GOLEM_DEPLOY_START_TIMEOUT)")
	buildTimeoutDefault := 30 * time.Minute
	if v := envDefault("GOLEM_DEPLOY_BUILD_TIMEOUT", ""); v != "" {
		if buildTimeoutDefault, err = time.ParseDuration(v); err != nil {
			fmt.Fprintf(stderr, "GOLEM_DEPLOY_BUILD_TIMEOUT: invalid duration %q\n", v)
			return 2
		}
	}
	buildRetentionDefault := 5
	if v := envDefault("GOLEM_DEPLOY_BUILD_RETENTION", ""); v != "" {
		if buildRetentionDefault, err = strconv.Atoi(v); err != nil {
			fmt.Fprintf(stderr, "GOLEM_DEPLOY_BUILD_RETENTION: invalid integer %q\n", v)
			return 2
		}
	}
	backupMaxDefault, err := envInt("GOLEM_DEPLOY_BACKUP_MAX_MB")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if os.Getenv("GOLEM_DEPLOY_BACKUP_MAX_MB") == "" {
		backupMaxDefault = 1024
	}
	backupMax := fs.Int("backup-max-mb", backupMaxDefault, "largest data directory of a server a backup accepts, in MiB (env GOLEM_DEPLOY_BACKUP_MAX_MB)")
	buildTimeout := fs.Duration("build-timeout", buildTimeoutDefault, "time limit of one build, e.g. 30m (env GOLEM_DEPLOY_BUILD_TIMEOUT)")
	buildRetention := fs.Int("build-retention", buildRetentionDefault, "successful builds kept per project, with their images; builds that servers use are always kept (env GOLEM_DEPLOY_BUILD_RETENTION)")
	inspectorListen := fs.String("inspector-listen", envDefault("GOLEM_DEPLOY_INSPECTOR_LISTEN", ""),
		"Inspector listen address for browsers and the games' Inspector agents; empty means the host of --listen with port 8444, \"off\" disables the Inspector (env GOLEM_DEPLOY_INSPECTOR_LISTEN)")
	inspectorImage := fs.String("inspector-image", envDefault("GOLEM_DEPLOY_INSPECTOR_IMAGE", inspector.DefaultImage),
		"image reference of the Inspector hub, downloaded with the daemon's build Docker configuration when the host does not have it (env GOLEM_DEPLOY_INSPECTOR_IMAGE)")
	inspectorMemDefault, err := envInt("GOLEM_DEPLOY_INSPECTOR_MEMORY_MB")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if os.Getenv("GOLEM_DEPLOY_INSPECTOR_MEMORY_MB") == "" {
		inspectorMemDefault = inspector.DefaultMemoryMB
	}
	inspectorMemory := fs.Int("inspector-memory-mb", inspectorMemDefault, "memory limit of the Inspector hub container in MiB (env GOLEM_DEPLOY_INSPECTOR_MEMORY_MB)")
	inspectorPublicURL := fs.String("inspector-public-url", envDefault("GOLEM_DEPLOY_INSPECTOR_PUBLIC_URL", ""),
		"origin browsers use for the Inspector when a reverse proxy fronts its listener, e.g. https://inspector.example.com (env GOLEM_DEPLOY_INSPECTOR_PUBLIC_URL)")
	inspectorAgentHost := fs.String("inspector-agent-host", envDefault("GOLEM_DEPLOY_INSPECTOR_AGENT_HOST", ""),
		"address the games' Inspector agents connect to from their containers; empty picks it from --inspector-listen (env GOLEM_DEPLOY_INSPECTOR_AGENT_HOST)")
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
		HTTPListen:      *httpListen,
		ACMEDirectory:   *acmeDirectory,
		ACMERootCA:      *acmeRootCA,
		InsecureHTTP:    *insecure,
		PublicHosts:     splitList(*publicHost),
		TrustedProxies:  splitList(*trustedProxy),
		PortRange:       *portRange,
		PortBindAddress: *portBind,
		ServerMemoryMB:  *memoryMB,
		ServerCPUs:      *cpus,
		MaxRepoSizeMB:   *maxRepoSize,
		StaticQuotaMB:   *staticQuota,
		StartTimeout:    *startTimeout,
		BuildTimeout:    *buildTimeout,
		BuildRetention:  *buildRetention,
		BackupMaxMB:     *backupMax,
		Inspector: daemon.InspectorOptions{
			Listen:    *inspectorListen,
			Image:     *inspectorImage,
			MemoryMB:  *inspectorMemory,
			PublicURL: *inspectorPublicURL,
			AgentHost: *inspectorAgentHost,
		},
		Version: buildVersion(),
		Logger:  logger,
		SPA:     frontend.Handler(),
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
	switch sub {
	case "setup-link":
		return cmdSetupLink(args[1:], stdout, stderr)
	case "instance-id":
		return cmdInstanceID(args[1:], stdout, stderr)
	}
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
		revoked, err := daemon.ResetPassword(ctx, *dataDir, *username, password)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "password updated for %q; all of its sessions have been ended and %d API token(s) deleted\n", strings.ToLower(strings.TrimSpace(*username)), revoked)
	}
	return 0
}

// exitAdminExists is the exit code of admin setup-link when an admin account
// already exists (the installer relies on it).
const exitAdminExists = 3

// cmdSetupLink prints a one-time setup link as the only line on stdout.
// Notes for people go to stderr.
func cmdSetupLink(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin setup-link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	origin := fs.String("origin", "", "panel origin to put before the link's path, e.g. https://deploy.example.com:8443")
	ttl := fs.Duration("ttl", time.Hour, "how long the link stays valid, from 1m to 24h")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "admin setup-link: unexpected arguments %q\n", fs.Args())
		return 2
	}
	prefix := ""
	if *origin != "" {
		o, err := parseOrigin(*origin)
		if err != nil {
			fmt.Fprintf(stderr, "admin setup-link: --origin: %v\n", err)
			return 2
		}
		prefix = o
	}
	if *ttl < daemon.MinSetupLinkTTL || *ttl > daemon.MaxSetupLinkTTL {
		fmt.Fprintf(stderr, "admin setup-link: --ttl must be between %v and %v\n", daemon.MinSetupLinkTTL, daemon.MaxSetupLinkTTL)
		return 2
	}
	token, expires, err := daemon.CreateSetupLink(context.Background(), *dataDir, *ttl)
	if errors.Is(err, daemon.ErrAdminExists) {
		fmt.Fprintln(stderr, `error: an admin account already exists, so no setup link is needed. Use "golem-deploy admin create" to add an admin or "golem-deploy admin reset-password" to set a new password.`)
		return exitAdminExists
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s/setup#t=%s\n", prefix, token); err != nil {
		fmt.Fprintln(stderr, "error: could not write the link to standard output")
		return 1
	}
	fmt.Fprintf(stderr, "Open the link in a browser to create the first admin account. It works once and expires at %s. Making a new link ends this one.\n", expires.Format(time.RFC3339))
	return 0
}

// cmdInstanceID prints the installation's instance id (the value of the
// dev.golemengine.deploy.instance label on its Docker objects) as the only
// line on stdout. The uninstaller uses it to remove only this
// installation's Docker objects. It changes nothing in the data directory.
func cmdInstanceID(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin instance-id", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "admin instance-id: unexpected arguments %q\n", fs.Args())
		return 2
	}
	id, err := daemon.InstanceID(context.Background(), *dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := fmt.Fprintln(stdout, id); err != nil {
		fmt.Fprintln(stderr, "error: could not write the instance id to standard output")
		return 1
	}
	return 0
}

// parseOrigin accepts "http" or "https", a host and an optional port, and
// nothing else. It returns the origin as scheme://host[:port].
func parseOrigin(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("the scheme must be http or https")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("a host is required, e.g. https://deploy.example.com")
	}
	if u.User != nil {
		return "", errors.New("user information is not allowed")
	}
	if err := checkOriginHost(u); err != nil {
		return "", err
	}
	if u.Path != "" || u.RawPath != "" {
		return "", errors.New("a path is not allowed, not even /")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("a query is not allowed")
	}
	if u.Fragment != "" || strings.Contains(v, "#") {
		return "", errors.New("a fragment is not allowed")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("the port must be a number from 1 to 65535")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("the port must be a number from 1 to 65535")
	}
	return u.Scheme + "://" + u.Host, nil
}

// checkOriginHost accepts a host a browser can open: a DNS name, localhost,
// an IPv4 address or an IPv6 address in brackets without a zone.
func checkOriginHost(u *url.URL) error {
	h := u.Hostname()
	if strings.Contains(h, "%") {
		return errors.New("an IPv6 zone (%) is not allowed; browsers cannot open it")
	}
	if strings.HasPrefix(u.Host, "[") {
		if a, err := netip.ParseAddr(h); err != nil || !a.Is6() {
			return errors.New("the host in brackets must be an IPv6 address")
		}
		return nil
	}
	if a, err := netip.ParseAddr(h); err == nil && a.Is4() {
		return nil
	}
	errHost := errors.New("the host must be a DNS name, localhost, an IPv4 address or an IPv6 address in brackets")
	name := strings.TrimSuffix(h, ".")
	if name == "" || len(name) > 253 {
		return errHost
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' || strings.IndexFunc(l, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
		}) >= 0 {
			return errHost
		}
	}
	// Browsers read a host whose last label is a number as an IPv4
	// address, and it was not a valid one.
	last := strings.ToLower(labels[len(labels)-1])
	if strings.Trim(last, "0123456789") == "" || strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "" {
		return errHost
	}
	return nil
}

// splitList splits a comma-separated flag value, dropping empty entries.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
