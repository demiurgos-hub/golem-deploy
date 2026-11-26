package build

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// monorepoFiles is a game whose golem.yaml is at the repository root and
// whose Go module lives in workers/game-server (server.module).
func monorepoFiles() map[string]string {
	return map[string]string{
		ConfigFile:                            "server:\n  module: workers/game-server\n",
		"golem.yaml":                          "integrations:\n  go-server:\n    out: workers/game-server/internal/generated/\n",
		"workers/game-server/go.mod":          strings.Replace(testGoMod, "go 1.25.4", "go 1.26.2", 1),
		"workers/game-server/cmd/server/a.go": "package main\n",
		"workers/unity/Assets/x.bytes":        "pointer",
		"package.json":                        `{"scripts":{"build":"vite build"}}`,
		"package-lock.json":                   "{}",
	}
}

func TestInspectModuleInSubdirectory(t *testing.T) {
	t.Run("go.mod and server below the module", func(t *testing.T) {
		p, err := Inspect(writeRepo(t, monorepoFiles()), false)
		if err != nil {
			t.Fatal(err)
		}
		if p.Root != "." || p.Module != "workers/game-server" || p.GoVersion != "1.26.2" || p.ServerPackage != "cmd/server" ||
			p.Bake != BakeGenerate || !reflect.DeepEqual(p.NPM, []NPMDir{{Dir: ".", Lockfile: true}}) || p.WebDist != DefaultWebDist {
			t.Fatalf("plan %+v", p)
		}
		if !strings.Contains(strings.Join(p.Notes, "\n"), "Go module: workers/game-server (server.module)") {
			t.Fatalf("notes %q", p.Notes)
		}
	})
	t.Run("golem.yaml paths stay relative to the game root", func(t *testing.T) {
		files := monorepoFiles()
		files["workers/game-server/internal/generated/golem_host.go"] = "package generated\n"
		p, err := Inspect(writeRepo(t, files), false)
		if err != nil || p.Bake != BakeCheck {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("module below a game root", func(t *testing.T) {
		files := map[string]string{
			ConfigFile:                      "root: games/arena\nserver:\n  module: server\n  package: ./cmd/arena\n",
			"games/arena/golem.yaml":        "proto:\n  out: gen/x.proto\n",
			"games/arena/server/go.mod":     testGoMod,
			"games/arena/server/cmd/arena/": "",
		}
		p, err := Inspect(writeRepo(t, files), false)
		if err != nil || p.Module != "server" || p.ServerPackage != "cmd/arena" {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("single command under the module's cmd", func(t *testing.T) {
		files := monorepoFiles()
		delete(files, "workers/game-server/cmd/server/a.go")
		files["workers/game-server/cmd/game/main.go"] = "package main\n"
		files["cmd/other/main.go"] = "package main\n" // the game root's cmd is not searched
		p, err := Inspect(writeRepo(t, files), false)
		if err != nil || p.ServerPackage != "cmd/game" {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("module directory as the package", func(t *testing.T) {
		files := monorepoFiles()
		delete(files, "workers/game-server/cmd/server/a.go")
		files["workers/game-server/main.go"] = "package main\n"
		files["main.go"] = "package main\n"
		p, err := Inspect(writeRepo(t, files), false)
		if err != nil || p.ServerPackage != "." || !strings.Contains(strings.Join(p.Notes, "\n"), "Server package: the module directory") {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("custom dockerfile stays relative to the game root", func(t *testing.T) {
		files := monorepoFiles()
		files[ConfigFile] += "dockerfile: deploy/Dockerfile\n"
		files["deploy/Dockerfile"] = "FROM scratch\n"
		p, err := Inspect(writeRepo(t, files), true)
		if err != nil || !p.Custom || p.DockerfilePath != "deploy/Dockerfile" || p.GoVersion != "1.26.2" {
			t.Fatalf("%+v %v", p, err)
		}
	})
}

func TestInspectModuleRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"go.mod missing in the module", func(f map[string]string) { delete(f, "workers/game-server/go.mod") },
			`go.mod was not found in "workers/game-server" (server.module "workers/game-server" in golem-deploy.yaml)`},
		{"module directory missing", func(f map[string]string) { f[ConfigFile] = "server:\n  module: workers/nope\n" },
			`server.module "workers/nope" in golem-deploy.yaml is not a directory`},
		{"module is a file", func(f map[string]string) { f[ConfigFile] = "server:\n  module: package.json\n" },
			"is not a directory"},
		{"go.mod only at the game root", func(f map[string]string) {
			f["go.mod"] = f["workers/game-server/go.mod"]
			delete(f, "workers/game-server/go.mod")
		}, `go.mod was not found in "workers/game-server"`},
		{"package below the game root, not the module", func(f map[string]string) {
			f[ConfigFile] += "  package: ./workers/game-server/cmd/server\n"
		}, `is not a directory in the module directory "workers/game-server" (server.module)`},
		{"no server package in the module", func(f map[string]string) {
			delete(f, "workers/game-server/cmd/server/a.go")
			f["cmd/server/main.go"] = "package main\n"
		}, `Go files in the module directory "workers/game-server" (server.module)`},
		{"module go.mod is a symlink-free regular file", func(f map[string]string) {
			delete(f, "workers/game-server/go.mod")
			f["workers/game-server/go.mod/"] = ""
		}, "not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := monorepoFiles()
			tc.mutate(files)
			_, err := Inspect(writeRepo(t, files), true)
			wantUserErr(t, err, tc.want)
		})
	}
	// The default names the game root and points at server.module.
	files := baseRepo()
	delete(files, "go.mod")
	_, err := Inspect(writeRepo(t, files), false)
	wantUserErr(t, err, `go.mod was not found in the game root "."; set server.module in golem-deploy.yaml`)
}

func TestInspectModuleRefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte(testGoMod), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeRepo(t, monorepoFiles())
	if err := os.RemoveAll(filepath.Join(dir, "workers", "game-server")); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, filepath.Join(dir, "workers", "game-server"))
	_, err := Inspect(dir, false)
	wantUserErr(t, err, "symbolic link")
}

// The generated Dockerfile builds the server in the module directory and
// keeps golem-bake, the web build and the web.dist check at the game root.
func TestDockerfileModule(t *testing.T) {
	p, err := Inspect(writeRepo(t, monorepoFiles()), false)
	if err != nil {
		t.Fatal(err)
	}
	df, err := Dockerfile(p, DockerfileParams{ProjectID: 3, InstanceID: testInstance})
	if err != nil {
		t.Fatal(err)
	}
	checkLines(t, df)
	golden(t, "module.Dockerfile", df)
	lines := strings.Split(string(df), "\n")
	wdAt := func(substr string) string {
		wd := ""
		for _, l := range lines {
			if d, ok := strings.CutPrefix(l, "WORKDIR "); ok {
				wd = d
			}
			if strings.Contains(l, substr) {
				return wd
			}
		}
		t.Fatalf("no line with %q", substr)
		return ""
	}
	for substr, want := range map[string]string{
		"golem-bake@":          "/src",
		`"npm","run","build"`:  "/src",
		"RUN test -d web/dist": "/src",
		"go build -trimpath":   "/src/workers/game-server",
	} {
		if got := wdAt(substr); got != want {
			t.Errorf("%s runs in %s, want %s", substr, got, want)
		}
	}
	if !strings.Contains(string(df), "FROM golang:1.26.2-bookworm AS build") {
		t.Fatal("Go image does not come from the module's go.mod")
	}

	// The default module renders exactly what a plan without one does.
	base := &Plan{Root: ".", EngineVersion: "v0.4.0", GoVersion: "1.25.4", ServerPackage: "cmd/server"}
	withDot := *base
	withDot.Module = "."
	a, errA := Dockerfile(base, DockerfileParams{ProjectID: 1, InstanceID: testInstance})
	b, errB := Dockerfile(&withDot, DockerfileParams{ProjectID: 1, InstanceID: testInstance})
	if errA != nil || errB != nil || string(a) != string(b) {
		t.Fatalf("default module changes the Dockerfile: %v %v", errA, errB)
	}
	for _, bad := range []string{"../x", "a b", "-x", "/abs"} {
		p := withDot
		p.Module = bad
		if _, err := Dockerfile(&p, DockerfileParams{ProjectID: 1, InstanceID: testInstance}); err == nil {
			t.Errorf("module %q accepted", bad)
		}
	}
}
