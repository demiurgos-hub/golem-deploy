package build

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"go/version"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
)

// EngineModule is the Golem Engine Go module a game must require.
const EngineModule = "github.com/demiurgos-hub/golem-engine"

// MinEngineVersion is the oldest Golem Engine release Golem Deploy builds.
const MinEngineVersion = "v0.4.0-rc.1"

// Size caps of the files the daemon reads from a checkout.
const (
	maxGolemYAMLBytes  = 64 << 10
	maxGoModBytes      = 1 << 20
	maxPackageJSON     = 1 << 20
	maxDockerfileBytes = 1 << 20
	maxDockerignore    = 64 << 10
)

// BakeMode says how the build stage treats Golem Engine generated code.
type BakeMode int

const (
	// BakeGenerate runs golem-bake (generated code is not committed).
	BakeGenerate BakeMode = iota
	// BakeCheck runs golem-bake --check (generated code is committed and
	// must be up to date).
	BakeCheck
)

// Plan is everything the build needs from a checkout, validated. Paths are
// clean slash paths: Root relative to the repository, ServerPackage
// relative to Module, the others relative to Root.
type Plan struct {
	Root string
	// Module is the directory of go.mod relative to Root (server.module;
	// "." or empty for the game root). The Go build runs there.
	Module string
	// EngineVersion is the Golem Engine version go.mod requires.
	EngineVersion string
	// GoVersion selects the Go toolchain image (go directive or toolchain,
	// whichever is newer), e.g. "1.25.4".
	GoVersion string

	// Custom is set when the repository brings its own Dockerfile
	// (CustomDockerfile holds its content); the fields below are then
	// unused.
	Custom           bool
	DockerfilePath   string
	CustomDockerfile []byte

	ServerPackage string
	ServerArgs    []string
	Bake          BakeMode
	// NPM lists the directories (relative to Root) whose dependencies are
	// installed, with whether a lockfile allows "npm ci".
	NPM []NPMDir
	// WebDir, WebBuild and WebDist describe the browser client build;
	// WebBuild is nil when there is none. WebDist (relative to Root) is
	// copied into the image when non-empty.
	WebDir   string
	WebBuild []string
	WebDist  string

	// WebTransportPath and WebSocketPath are the game's transport endpoint
	// paths (transport in golem-deploy.yaml, defaults filled in); they are
	// recorded on the build for the servers' public URLs.
	WebTransportPath string
	WebSocketPath    string

	// SchemaFingerprint is the game's schema fingerprint read from the
	// committed generated Go code (generated Dockerfile with
	// golem-bake --check only); "" when it is not known.
	SchemaFingerprint string

	// Dockerignore is the repository's own .dockerignore (may be nil); the
	// daemon's mandatory exclusions are appended to it.
	Dockerignore []byte
	// Notes are human-readable lines for the build log.
	Notes []string
}

// NPMDir is a directory with a package.json.
type NPMDir struct {
	Dir      string
	Lockfile bool
}

var (
	engineVersionRE = regexp.MustCompile(`^v[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}(-[0-9A-Za-z.-]{1,100})?$`)
	goVersionRE     = regexp.MustCompile(`^1\.[0-9]{1,3}(\.[0-9]{1,3})?((rc|beta)[0-9]{1,2})?$`)
)

// ValidEngineVersion reports whether v is a version Golem Deploy accepts
// in labels, Dockerfiles and records (a semantic version without build
// metadata).
func ValidEngineVersion(v string) bool { return engineVersionRE.MatchString(v) && semver.IsValid(v) }

// Inspect reads the checkout at dir and returns the build plan. It reads
// only golem-deploy.yaml, .dockerignore, golem.yaml, go.mod, package.json
// files and a custom Dockerfile, through repoFS (contained, no symbolic
// links, size-capped), and lists directories to find the server package.
// Problems with the repository are *UserError values.
func Inspect(dir string, allowCustomDockerfile bool) (*Plan, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	return inspect(repo, allowCustomDockerfile)
}

func inspect(repo *repoFS, allowCustomDockerfile bool) (*Plan, error) {
	raw, err := repo.readFile(ConfigFile, maxConfigBytes)
	if err != nil && !errors.Is(err, errMissing) {
		return nil, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	if cfg.Dockerfile != "" && !allowCustomDockerfile {
		return nil, userErr("the repository requests a custom Dockerfile; ask an admin to enable allow_custom_dockerfile for this project")
	}
	p := &Plan{Root: cfg.Root, Module: cfg.Server.Module, ServerArgs: cfg.Server.Args,
		WebTransportPath: cmp.Or(cfg.Transport.WebTransportPath, DefaultWebTransportPath),
		WebSocketPath:    cmp.Or(cfg.Transport.WebSocketPath, DefaultWebSocketPath)}
	if raw != nil {
		p.Notes = append(p.Notes, "Using "+ConfigFile)
	}
	if k, err := repo.kind(cfg.Root); err != nil {
		return nil, err
	} else if k != kindDir {
		return nil, userErr("the game root %q (root in %s) is not a directory in the repository", cfg.Root, ConfigFile)
	}
	if p.Dockerignore, err = repo.readFile(".dockerignore", maxDockerignore); err != nil && !errors.Is(err, errMissing) {
		return nil, err
	}

	golemYAML, err := repo.readFile(join(cfg.Root, "golem.yaml"), maxGolemYAMLBytes)
	if errors.Is(err, errMissing) {
		return nil, userErr("golem.yaml was not found in the game root %q; set root in %s if the game lives in a subdirectory", cfg.Root, ConfigFile)
	}
	if err != nil {
		return nil, err
	}
	if err := p.readGoMod(repo); err != nil {
		return nil, err
	}
	if !p.defaultModule() {
		p.Notes = append(p.Notes, "Go module: "+p.Module+" (server.module); the server is built there")
	}

	if cfg.Dockerfile != "" {
		p.Custom = true
		p.DockerfilePath = join(cfg.Root, cfg.Dockerfile)
		p.CustomDockerfile, err = repo.readFile(p.DockerfilePath, maxDockerfileBytes)
		if errors.Is(err, errMissing) {
			return nil, userErr("the custom Dockerfile %q (dockerfile in %s) was not found in the game root", cfg.Dockerfile, ConfigFile)
		}
		if err != nil {
			return nil, err
		}
		if selectsFrontend(p.CustomDockerfile) {
			return nil, userErr("a custom Dockerfile cannot select a BuildKit frontend; remove the syntax directive")
		}
		p.Notes = append(p.Notes, "Using the repository's own Dockerfile "+p.DockerfilePath)
		return p, nil
	}

	if err := p.findServer(repo, cfg); err != nil {
		return nil, err
	}
	if err := p.planWeb(repo, cfg); err != nil {
		return nil, err
	}
	committed, err := generatedCommitted(repo, cfg.Root, golemYAML)
	if err != nil {
		return nil, err
	}
	if committed {
		p.Bake = BakeCheck
		p.Notes = append(p.Notes, "Generated Golem Engine code is committed: it is verified with golem-bake --check")
		p.SchemaFingerprint = committedFingerprint(repo, cfg.Root, golemYAML)
	} else {
		p.Bake = BakeGenerate
		p.Notes = append(p.Notes, "Generated Golem Engine code is not committed: golem-bake generates it during the build")
	}
	return p, nil
}

// BuildKit detects hash comments, C-style comments, and a JSON object,
// with an optional BOM and shebang. Reject the comment forms anywhere
// and reserve the JSON form entirely for the built-in frontend.
func selectsFrontend(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if bytes.HasPrefix(data, []byte("#!")) {
		_, data, _ = bytes.Cut(data, []byte("\n"))
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return true
	}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		for _, prefix := range []string{"#", "//"} {
			suffix, ok := bytes.CutPrefix(line, []byte(prefix))
			if key, _, found := bytes.Cut(suffix, []byte("=")); ok && found && bytes.EqualFold(bytes.TrimSpace(key), []byte("syntax")) {
				return true
			}
		}
	}
	return false
}

// moduleDir is the directory of go.mod relative to the repository root.
func (p *Plan) moduleDir() string { return join(p.Root, p.Module) }

// defaultModule reports whether go.mod is in the game root.
func (p *Plan) defaultModule() bool { return p.Module == "" || p.Module == "." }

// readGoMod validates the game's go.mod (in the module directory): the
// engine requirement and its version, no local replacement of engine
// modules, and the Go version.
func (p *Plan) readGoMod(repo *repoFS) error {
	if !p.defaultModule() {
		if k, err := repo.kind(p.moduleDir()); err != nil {
			return err
		} else if k != kindDir {
			return userErr("server.module %q in %s is not a directory in the game root", p.Module, ConfigFile)
		}
	}
	data, err := repo.readFile(join(p.moduleDir(), "go.mod"), maxGoModBytes)
	if errors.Is(err, errMissing) {
		if p.defaultModule() {
			return userErr("go.mod was not found in the game root %q; set server.module in %s if the Go module lives in a subdirectory", p.Root, ConfigFile)
		}
		return userErr("go.mod was not found in %q (server.module %q in %s)", p.moduleDir(), p.Module, ConfigFile)
	}
	if err != nil {
		return err
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return userErr("go.mod could not be parsed: %s", clip(logsafe.String(oneLine(err.Error())), 200))
	}
	for _, r := range f.Require {
		if r.Mod.Path == EngineModule {
			p.EngineVersion = r.Mod.Version
		}
	}
	switch {
	case p.EngineVersion == "":
		return userErr("go.mod does not require %s; a Golem Engine game is required", EngineModule)
	case !ValidEngineVersion(p.EngineVersion):
		return userErr("go.mod requires %s at a version Golem Deploy does not accept (%s)", EngineModule, clip(logsafe.String(p.EngineVersion), 80))
	case semver.Compare(p.EngineVersion, MinEngineVersion) < 0:
		return userErr("go.mod requires %s %s; Golem Deploy needs %s or newer", EngineModule, p.EngineVersion, MinEngineVersion)
	}
	for _, r := range f.Replace {
		if r.Old.Path != EngineModule && !strings.HasPrefix(r.Old.Path, EngineModule+"/") {
			continue
		}
		if r.New.Version == "" || modfile.IsDirectoryPath(r.New.Path) {
			return userErr("go.mod replaces %s with a local path; remove the replace directive and require a released Golem Engine version", clip(logsafe.String(r.Old.Path), 120))
		}
		if r.Old.Path == EngineModule {
			return userErr("go.mod replaces %s with another module; require a released Golem Engine version instead", EngineModule)
		}
	}
	if f.Go == nil {
		return userErr("go.mod has no go directive")
	}
	goVer := f.Go.Version
	if f.Toolchain != nil {
		if tv := strings.TrimPrefix(f.Toolchain.Name, "go"); version.Compare("go"+tv, "go"+goVer) > 0 {
			goVer = tv
		}
	}
	if !goVersionRE.MatchString(goVer) {
		return userErr("go.mod asks for a Go version Golem Deploy does not accept (%s)", clip(logsafe.String(goVer), 40))
	}
	p.GoVersion = goVer
	return nil
}

// findServer resolves the server package, relative to the module
// directory: the configured one, else ./cmd/server, else the only ./cmd/*
// directory with Go files, else the module directory itself when it has Go
// files.
func (p *Plan) findServer(repo *repoFS, cfg Config) error {
	base := p.moduleDir()
	where, self := "the game root", "the game root"
	if !p.defaultModule() {
		where = fmt.Sprintf("the module directory %q (server.module)", p.Module)
		self = "the module directory"
	}
	if pkg := cfg.Server.Package; pkg != "" {
		k, err := repo.kind(join(base, pkg))
		if err != nil {
			return err
		}
		if k != kindDir {
			return userErr("server.package %q in %s is not a directory in %s", pkg, ConfigFile, where)
		}
		p.ServerPackage = pkg
		return nil
	}
	if k, err := repo.kind(join(base, "cmd/server")); err != nil {
		return err
	} else if k == kindDir {
		p.ServerPackage = "cmd/server"
		return nil
	}
	var mains []string
	entries, err := repo.readDir(join(base, "cmd"))
	if err != nil && !errors.Is(err, errMissing) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !segmentRE.MatchString(e.Name()) {
			continue
		}
		ok, err := hasGoFiles(repo, join(base, "cmd", e.Name()))
		if err != nil {
			return err
		}
		if ok {
			mains = append(mains, "cmd/"+e.Name())
		}
	}
	switch len(mains) {
	case 1:
		p.ServerPackage = mains[0]
		p.Notes = append(p.Notes, "Server package: ./"+mains[0])
		return nil
	case 0:
	default:
		slices.Sort(mains)
		return userErr("several commands were found (%s); set server.package in %s", strings.Join(clipList(mains, 5), ", "), ConfigFile)
	}
	if ok, err := hasGoFiles(repo, base); err != nil {
		return err
	} else if ok {
		p.ServerPackage = "."
		p.Notes = append(p.Notes, "Server package: "+self)
		return nil
	}
	return userErr("the server package was not found (looked for ./cmd/server, a single ./cmd/* command and Go files in %s); set server.package in %s", where, ConfigFile)
}

func clipList(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(items[:n:n], "…")
}

// hasGoFiles reports whether dir directly holds a non-test .go file.
func hasGoFiles(repo *repoFS, dir string) (bool, error) {
	entries, err := repo.readDir(dir)
	if errors.Is(err, errMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			return true, nil
		}
	}
	return false, nil
}

// planWeb decides the npm installs and the web build.
func (p *Plan) planWeb(repo *repoFS, cfg Config) error {
	webDir := cfg.Web.Dir
	if webDir == "" {
		webDir = "."
	}
	if k, err := repo.kind(join(p.Root, webDir)); err != nil {
		return err
	} else if k != kindDir {
		return userErr("web.dir %q in %s is not a directory in the game root", webDir, ConfigFile)
	}
	pkg, err := readPackageJSON(repo, join(p.Root, webDir, "package.json"))
	if err != nil {
		return err
	}
	if pkg == nil && cfg.Web.Dir != "" {
		return userErr("web.dir %q in %s has no package.json", webDir, ConfigFile)
	}
	if pkg == nil && cfg.Web.Build != nil {
		return userErr("web.build is set in %s but there is no package.json in the game root; set web.dir", ConfigFile)
	}
	dirs := []string{}
	if webDir != "." {
		rootPkg, err := readPackageJSON(repo, join(p.Root, "package.json"))
		if err != nil {
			return err
		}
		if rootPkg != nil {
			dirs = append(dirs, ".")
		}
	}
	if pkg != nil {
		dirs = append(dirs, webDir)
	}
	for _, d := range dirs {
		lock := false
		for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
			k, err := repo.kind(join(p.Root, d, name))
			if err != nil {
				return err
			}
			lock = lock || k == kindFile
		}
		p.NPM = append(p.NPM, NPMDir{Dir: d, Lockfile: lock})
		if !lock {
			p.Notes = append(p.Notes, "No package-lock.json in "+displayDir(d)+": dependencies are installed with npm install (commit a lockfile for reproducible builds)")
		}
	}
	p.WebDir = webDir
	switch {
	case cfg.Web.Build != nil:
		p.WebBuild = cfg.Web.Build
	case pkg != nil && pkg.hasBuild:
		p.WebBuild = []string{"npm", "run", "build"}
	}
	p.WebDist = cfg.Web.Dist
	if p.WebDist == "" && p.WebBuild != nil {
		p.WebDist = DefaultWebDist
	}
	if pkg != nil && p.WebBuild == nil {
		p.Notes = append(p.Notes, "package.json has no build script: no web build")
	}
	return nil
}

func displayDir(d string) string {
	if d == "." {
		return "the game root"
	}
	return d
}

type packageJSON struct {
	hasBuild bool
}

// readPackageJSON returns nil when the file does not exist.
func readPackageJSON(repo *repoFS, rel string) (*packageJSON, error) {
	data, err := repo.readFile(rel, maxPackageJSON)
	if errors.Is(err, errMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, userErr("%s is not valid JSON", rel)
	}
	var s string
	b, ok := v.Scripts["build"]
	return &packageJSON{hasBuild: ok && json.Unmarshal(b, &s) == nil && strings.TrimSpace(s) != ""}, nil
}

// generatedCommitted reports whether any output golem.yaml configures
// (proto.out and every integration's out) exists in the checkout.
func generatedCommitted(repo *repoFS, root string, golemYAML []byte) (bool, error) {
	if err := checkYAML("golem.yaml", golemYAML); err != nil {
		return false, err
	}
	var gy struct {
		Proto struct {
			Out string `yaml:"out"`
		} `yaml:"proto"`
		Integrations map[string]struct {
			Out string `yaml:"out"`
		} `yaml:"integrations"`
	}
	if err := yaml.Unmarshal(golemYAML, &gy); err != nil {
		return false, userErr("golem.yaml is not valid YAML: %s", yamlMessage(err))
	}
	outs := []string{gy.Proto.Out}
	for _, in := range gy.Integrations {
		outs = append(outs, in.Out)
	}
	for _, o := range outs {
		o = path.Clean(strings.TrimSpace(o))
		if o == "" || o == "." || strings.HasPrefix(o, "/") || strings.HasPrefix(o, "../") || o == ".." || strings.ContainsAny(o, `\:`) {
			continue // golem-bake refuses outputs outside the project itself
		}
		k, err := repo.kind(join(root, o))
		if err != nil {
			var ue *UserError
			if errors.As(err, &ue) {
				return false, err
			}
			continue
		}
		switch k {
		case kindFile:
			return true, nil
		case kindDir:
			entries, err := repo.readDir(join(root, o))
			if err != nil {
				return false, err
			}
			if len(entries) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}
