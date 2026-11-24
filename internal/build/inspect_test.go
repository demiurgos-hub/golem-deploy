package build

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const testGoMod = "module example.com/game\n\ngo 1.25.4\n\nrequire github.com/demiurgos-hub/golem-engine v0.4.0-rc.1\n"

// writeRepo creates a checkout from path -> content ("" content with a
// trailing slash makes a directory).
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func baseRepo() map[string]string {
	return map[string]string{
		"golem.yaml":          "integrations:\n  go-server:\n    out: internal/generated/\n",
		"go.mod":              testGoMod,
		"cmd/server/main.go":  "package main\n",
		"package.json":        `{"scripts":{"build":"vite build"}}`,
		"package-lock.json":   "{}",
		"web/index.html":      "x",
		"internal/game/a.go":  "package game\n",
		".dockerignore":       "*.log\n",
		"README.md":           "readme",
		"schemas/entities/x/": "",
	}
}

func TestInspectFixture(t *testing.T) {
	p, err := Inspect(filepath.Join("testdata", "game"), false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != "." || p.EngineVersion != "v0.4.0-rc.1" || p.GoVersion != "1.25.4" || p.ServerPackage != "cmd/server" ||
		p.Bake != BakeGenerate || p.Custom || p.WebDist != DefaultWebDist || p.WebDir != "." ||
		!reflect.DeepEqual(p.WebBuild, []string{"npm", "run", "build"}) || !reflect.DeepEqual(p.NPM, []NPMDir{{Dir: ".", Lockfile: true}}) {
		t.Fatalf("plan %+v", p)
	}
}

func TestInspectPlans(t *testing.T) {
	t.Run("committed generated code is checked", func(t *testing.T) {
		files := baseRepo()
		files["internal/generated/golem_host.go"] = "package generated\n"
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.Bake != BakeCheck {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("empty output directory is not committed code", func(t *testing.T) {
		files := baseRepo()
		files["internal/generated/"] = ""
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.Bake != BakeGenerate {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("subdirectory game with web dir, args and toolchain", func(t *testing.T) {
		files := map[string]string{
			"golem-deploy.yaml":                      "root: games/arena\nserver:\n  args: [\"-assets\", \"client/dist\"]\nweb:\n  dir: client\n",
			"games/arena/golem.yaml":                 "proto:\n  out: gen/entities.proto\n",
			"games/arena/gen/entities.proto":         "syntax",
			"games/arena/go.mod":                     strings.Replace(testGoMod, "go 1.25.4", "go 1.25.4\n\ntoolchain go1.26.2", 1),
			"games/arena/cmd/arena/main.go":          "package main\n",
			"games/arena/client/package.json":        `{"scripts":{"build":"vite build"}}`,
			"games/arena/package.json":               `{}`,
			"games/arena/package-lock.json":          `{}`,
			"games/arena/client/npm-shrinkwrap.json": `{}`,
		}
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil {
			t.Fatal(err)
		}
		if p.Root != "games/arena" || p.GoVersion != "1.26.2" || p.ServerPackage != "cmd/arena" || p.Bake != BakeCheck ||
			p.WebDir != "client" || p.WebDist != DefaultWebDist ||
			!reflect.DeepEqual(p.NPM, []NPMDir{{Dir: ".", Lockfile: true}, {Dir: "client", Lockfile: true}}) ||
			!reflect.DeepEqual(p.ServerArgs, []string{"-assets", "client/dist"}) {
			t.Fatalf("plan %+v", p)
		}
	})
	t.Run("no node at all", func(t *testing.T) {
		files := baseRepo()
		delete(files, "package.json")
		delete(files, "package-lock.json")
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || len(p.NPM) != 0 || p.WebBuild != nil || p.WebDist != "" {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("package.json without build script or lockfile", func(t *testing.T) {
		files := baseRepo()
		files["package.json"] = `{"scripts":{"test":"x"}}`
		delete(files, "package-lock.json")
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.WebBuild != nil || p.WebDist != "" || !reflect.DeepEqual(p.NPM, []NPMDir{{Dir: ".", Lockfile: false}}) {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("prebuilt dist without build", func(t *testing.T) {
		files := baseRepo()
		delete(files, "package.json")
		files["golem-deploy.yaml"] = "web:\n  dist: public\n"
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.WebDist != "public" || p.WebBuild != nil {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("single command under cmd", func(t *testing.T) {
		files := baseRepo()
		delete(files, "cmd/server/main.go")
		files["cmd/game/main.go"] = "package main\n"
		files["cmd/tools/README"] = "no go files"
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.ServerPackage != "cmd/game" {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("game root package", func(t *testing.T) {
		files := baseRepo()
		delete(files, "cmd/server/main.go")
		files["main.go"] = "package main\n"
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.ServerPackage != "." {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("pseudo-version newer than the minimum", func(t *testing.T) {
		files := baseRepo()
		files["go.mod"] = strings.Replace(testGoMod, "v0.4.0-rc.1", "v0.4.1-0.20261201000000-abcdefabcdef", 1)
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || p.EngineVersion != "v0.4.1-0.20261201000000-abcdefabcdef" {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("custom dockerfile", func(t *testing.T) {
		files := baseRepo()
		files["golem-deploy.yaml"] = "dockerfile: deploy/Dockerfile\n"
		files["deploy/Dockerfile"] = "FROM scratch\n"
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || !p.Custom || string(p.CustomDockerfile) != "FROM scratch\n" || p.DockerfilePath != "deploy/Dockerfile" || p.EngineVersion == "" {
			t.Fatalf("%+v %v", p, err)
		}
	})
}

func TestCustomDockerfileFrontendDirectives(t *testing.T) {
	for name, content := range map[string]string{
		"hash":    "# syntax=example.invalid/frontend\nFROM scratch\n",
		"case":    "# escape=`\n# SyNtAx \t= frontend\r\nFROM scratch\r\n",
		"c style": "// syntax=frontend\nFROM scratch\n",
		"json":    "{\n  \"sy\\u006etax\": \"frontend\"\n}\n",
		"shebang": "\xef\xbb\xbf#!/usr/bin/env docker\n#\u2003SYNTAX=frontend\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := baseRepo()
			files[ConfigFile] = "dockerfile: Dockerfile.custom\n"
			files["Dockerfile.custom"] = content
			_, err := Inspect(writeRepo(t, files), true)
			wantUserErr(t, err, "cannot select a BuildKit frontend")
		})
	}
}

func TestGolemYAMLRejectsComplexMappingsAndAliases(t *testing.T) {
	for _, data := range []string{
		"metadata:\n" + strings.Repeat("  key: a\n", 129),
		"ignored: &value [1]\nother: *value\n",
		"ignored: [" + strings.Repeat("0,", 1024) + "]\n",
		"ignored: " + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40),
	} {
		files := baseRepo()
		files["golem.yaml"] = data
		_, err := Inspect(writeRepo(t, files), false)
		wantUserErr(t, err, "YAML complexity limits")
	}
	_, err := ParseConfig([]byte("server: &s {package: .}\nweb: *s\n"))
	wantUserErr(t, err, "YAML complexity limits")
}

func TestCheckoutOpenRejectsReplacedDirectory(t *testing.T) {
	dir := writeRepo(t, baseRepo())
	repo, err := openRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	checked, err := repo.lstatChain("cmd")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "cmd"), filepath.Join(dir, "old-cmd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "cmd"), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := repo.openChecked("cmd", checked)
	if f != nil {
		f.Close()
	}
	wantUserErr(t, err, "changed before it could be read")
}

func TestInspectRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"no golem.yaml", func(f map[string]string) { delete(f, "golem.yaml") }, "golem.yaml was not found"},
		{"golem.yaml is a directory", func(f map[string]string) { delete(f, "golem.yaml"); f["golem.yaml/"] = "" }, "not a regular file"},
		{"no go.mod", func(f map[string]string) { delete(f, "go.mod") }, "go.mod was not found"},
		{"no engine", func(f map[string]string) { f["go.mod"] = "module x\n\ngo 1.25.4\n" }, "does not require"},
		{"old engine", func(f map[string]string) { f["go.mod"] = strings.Replace(testGoMod, "v0.4.0-rc.1", "v0.3.2", 1) }, "v0.4.0-rc.1 or newer"},
		{"older prerelease", func(f map[string]string) { f["go.mod"] = strings.Replace(testGoMod, "v0.4.0-rc.1", "v0.4.0-beta.9", 1) }, "or newer"},
		{"build metadata version", func(f map[string]string) {
			f["go.mod"] = strings.Replace(testGoMod, "v0.4.0-rc.1", "v0.4.0+incompatible", 1)
		}, ""},
		{"local replace", func(f map[string]string) {
			f["go.mod"] = testGoMod + "\nreplace github.com/demiurgos-hub/golem-engine => ../golem-engine\n"
		}, "local path"},
		{"local replace of a submodule", func(f map[string]string) {
			f["go.mod"] = testGoMod + "\nreplace github.com/demiurgos-hub/golem-engine/golem/nav => ./nav\n"
		}, "local path"},
		{"remote replace of the engine", func(f map[string]string) {
			f["go.mod"] = testGoMod + "\nreplace github.com/demiurgos-hub/golem-engine => example.com/fork v0.9.0\n"
		}, "another module"},
		{"bad go version", func(f map[string]string) { f["go.mod"] = strings.Replace(testGoMod, "go 1.25.4", "go 2.0", 1) }, "Go version"},
		{"no go directive", func(f map[string]string) { f["go.mod"] = strings.Replace(testGoMod, "go 1.25.4", "", 1) }, "no go directive"},
		{"unparsable go.mod", func(f map[string]string) { f["go.mod"] = "module x\nrequire (\n" }, "could not be parsed"},
		{"huge go.mod", func(f map[string]string) { f["go.mod"] = testGoMod + "// " + strings.Repeat("x", maxGoModBytes) }, "larger than"},
		{"no server package", func(f map[string]string) { delete(f, "cmd/server/main.go") }, "server package was not found"},
		{"several commands", func(f map[string]string) {
			delete(f, "cmd/server/main.go")
			f["cmd/a/main.go"] = "package main"
			f["cmd/b/main.go"] = "package main"
		}, "several commands"},
		{"configured package missing", func(f map[string]string) { f["golem-deploy.yaml"] = "server:\n  package: ./cmd/nope\n" }, "server.package"},
		{"root missing", func(f map[string]string) { f["golem-deploy.yaml"] = "root: games/nope\n" }, "not a directory"},
		{"web dir without package.json", func(f map[string]string) { f["golem-deploy.yaml"] = "web:\n  dir: web\n" }, "has no package.json"},
		{"web build without package.json", func(f map[string]string) {
			delete(f, "package.json")
			f["golem-deploy.yaml"] = "web:\n  build: make\n"
		}, "no package.json"},
		{"invalid package.json", func(f map[string]string) { f["package.json"] = "{" }, "not valid JSON"},
		{"invalid golem.yaml", func(f map[string]string) { f["golem.yaml"] = "integrations: [\n" }, "golem.yaml is not valid YAML"},
		{"missing custom dockerfile", func(f map[string]string) { f["golem-deploy.yaml"] = "dockerfile: Dockerfile.deploy\n" }, "was not found"},
		{"invalid config", func(f map[string]string) { f["golem-deploy.yaml"] = "root: ../..\n" }, "root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := baseRepo()
			tc.mutate(files)
			_, err := Inspect(writeRepo(t, files), true)
			wantUserErr(t, err, tc.want)
		})
	}
}

// symlink creates a symbolic link or skips the test where that needs
// privileges (Windows without developer mode).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symbolic links unavailable: %v", err)
		}
		t.Fatal(err)
	}
}

// TestInspectRefusesSymlinks pins that the daemon never follows a link
// planted in the repository, wherever it points.
func TestInspectRefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("module evil\n\ngo 1.25.4\n\nrequire github.com/demiurgos-hub/golem-engine v0.4.0-rc.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T, dir string){
		"golem-deploy.yaml to a host file": func(t *testing.T, dir string) { symlink(t, secret, filepath.Join(dir, "golem-deploy.yaml")) },
		"go.mod to a host file": func(t *testing.T, dir string) {
			_ = os.Remove(filepath.Join(dir, "go.mod"))
			symlink(t, secret, filepath.Join(dir, "go.mod"))
		},
		"golem.yaml inside the repo": func(t *testing.T, dir string) {
			_ = os.Rename(filepath.Join(dir, "golem.yaml"), filepath.Join(dir, "real.yaml"))
			symlink(t, "real.yaml", filepath.Join(dir, "golem.yaml"))
		},
		"root through a linked directory": func(t *testing.T, dir string) {
			symlink(t, outside, filepath.Join(dir, "linked"))
			_ = os.WriteFile(filepath.Join(dir, "golem-deploy.yaml"), []byte("root: linked\n"), 0o644)
		},
		"package.json": func(t *testing.T, dir string) {
			_ = os.Remove(filepath.Join(dir, "package.json"))
			symlink(t, secret, filepath.Join(dir, "package.json"))
		},
		".dockerignore": func(t *testing.T, dir string) {
			_ = os.Remove(filepath.Join(dir, ".dockerignore"))
			symlink(t, secret, filepath.Join(dir, ".dockerignore"))
		},
		"custom dockerfile": func(t *testing.T, dir string) {
			_ = os.WriteFile(filepath.Join(dir, "golem-deploy.yaml"), []byte("dockerfile: Dockerfile.x\n"), 0o644)
			symlink(t, secret, filepath.Join(dir, "Dockerfile.x"))
		},
		"server package directory": func(t *testing.T, dir string) {
			_ = os.RemoveAll(filepath.Join(dir, "cmd"))
			symlink(t, outside, filepath.Join(dir, "cmd"))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeRepo(t, baseRepo())
			plant(t, dir)
			_, err := Inspect(dir, true)
			wantUserErr(t, err, "symbolic link")
		})
	}
}

func TestRepoFSContainment(t *testing.T) {
	dir := writeRepo(t, map[string]string{"a/b.txt": "x"})
	r, err := openRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, p := range []string{"../x", "a/../../x", "/etc/passwd", "a/./b.txt"} {
		if _, err := r.readFile(p, 10); err == nil {
			t.Errorf("readFile(%q) succeeded", p)
		}
	}
	if b, err := r.readFile("a/b.txt", 10); err != nil || string(b) != "x" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := r.readFile("a/b.txt/c", 10); err != errMissing {
		t.Fatalf("file used as directory: %v", err)
	}
	// Repository-derived names are escaped in messages.
	_, err = r.readFile("../evil"+string(rune(0x1b))+"[31m"+string(rune(0x202e))+"\nnext", 10)
	wantUserErr(t, err, "not a path inside the repository")
	if !strings.Contains(err.Error(), `\x1b[31m`) || !strings.Contains(err.Error(), `\u`+"202e") || strings.ContainsRune(err.Error(), 0x202e) {
		t.Fatalf("not escaped: %q", err.Error())
	}
}
