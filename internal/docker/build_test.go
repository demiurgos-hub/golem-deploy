package docker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

func abs(t *testing.T, elem ...string) string {
	t.Helper()
	root := "/data/tmp"
	if runtime.GOOS == "windows" {
		root = `C:\data\tmp`
	}
	return filepath.Join(append([]string{root}, elem...)...)
}

func validSpec(t *testing.T) docker.BuildSpec {
	return docker.BuildSpec{
		ContextDir: abs(t, "repo"), Dockerfile: abs(t, "b", "docker", "Dockerfile"), StaticDir: abs(t, "b", "static"),
		IIDFile: abs(t, "b", "iid"), MetadataFile: abs(t, "b", "metadata.json"),
		ProjectName: "arena", ProjectID: 3, Commit: strings.Repeat("a1", 20), BuildID: 9, EngineVersion: "v0.4.0-rc.1",
	}
}

func TestBuildArgs(t *testing.T) {
	s := validSpec(t)
	got, err := s.Args(dockertest.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "--file", s.Dockerfile, "--tag", "golem-deploy/arena:a1a1a1a1a1a1",
		"--label", "dev.golemengine.deploy.managed=true", "--label", "dev.golemengine.deploy.instance=" + dockertest.InstanceID,
		"--label", "dev.golemengine.deploy.project-id=3", "--label", "dev.golemengine.deploy.commit=" + s.Commit,
		"--label", "dev.golemengine.deploy.build-id=9", "--label", "dev.golemengine.deploy.engine-version=v0.4.0-rc.1",
		"--iidfile", s.IIDFile, "--metadata-file", s.MetadataFile, "--progress", "plain", "--provenance=false", "--sbom=false", "--load",
		"--build-context", "static=" + s.StaticDir, "--", s.ContextDir}
	if !slices.Equal(got, want) {
		t.Fatalf("argv\n%q\nwant\n%q", got, want)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "--secret") || strings.HasPrefix(a, "--ssh") || strings.HasPrefix(a, "--build-arg") ||
			strings.HasPrefix(a, "--allow") || strings.HasPrefix(a, "--network") {
			t.Fatalf("forbidden option %q", a)
		}
	}
}

func TestBuildArgsRejects(t *testing.T) {
	bad := map[string]func(*docker.BuildSpec){
		"relative context":     func(s *docker.BuildSpec) { s.ContextDir = "repo" },
		"unclean context":      func(s *docker.BuildSpec) { s.ContextDir = s.ContextDir + string(filepath.Separator) + ".." },
		"newline in path":      func(s *docker.BuildSpec) { s.StaticDir = s.StaticDir + "\n--allow=network.host" },
		"empty dockerfile":     func(s *docker.BuildSpec) { s.Dockerfile = "" },
		"project name option":  func(s *docker.BuildSpec) { s.ProjectName = "--push" },
		"project name upper":   func(s *docker.BuildSpec) { s.ProjectName = "Arena" },
		"project name slash":   func(s *docker.BuildSpec) { s.ProjectName = "a/b" },
		"short commit":         func(s *docker.BuildSpec) { s.Commit = "abc" },
		"commit injection":     func(s *docker.BuildSpec) { s.Commit = strings.Repeat("a", 40) + "\n" },
		"engine version space": func(s *docker.BuildSpec) { s.EngineVersion = "v0.4.0 x" },
		"engine version label": func(s *docker.BuildSpec) { s.EngineVersion = "v0.4.0,dev.golemengine.deploy.server-id=1" },
		"zero ids":             func(s *docker.BuildSpec) { s.BuildID = 0 },
	}
	for name, mutate := range bad {
		s := validSpec(t)
		mutate(&s)
		if args, err := s.Args(dockertest.InstanceID); err == nil {
			t.Errorf("%s accepted: %q", name, args)
		}
	}
	if _, err := validSpec(t).Args("nope"); err == nil {
		t.Error("bad instance accepted")
	}
}

func newBuildClient(t *testing.T) (*docker.Client, *dockertest.Fake, docker.BuildSpec) {
	t.Helper()
	fake := dockertest.New()
	dc := docker.New(docker.Options{Runner: fake, InstanceID: dockertest.InstanceID})
	dir := t.TempDir()
	s := validSpec(t)
	s.ContextDir, s.StaticDir = dir, dir
	s.Dockerfile = filepath.Join(dir, "Dockerfile")
	s.IIDFile, s.MetadataFile = filepath.Join(dir, "iid"), filepath.Join(dir, "md.json")
	_ = os.WriteFile(s.Dockerfile, []byte("FROM scratch\n"), 0o600)
	return dc, fake, s
}

func TestBuildAndVerify(t *testing.T) {
	for _, containerd := range []bool{false, true} {
		dc, fake, s := newBuildClient(t)
		fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
			return dockertest.BuildOutcome{ContainerdStore: containerd, Output: "#1 building\n"}
		})
		var out strings.Builder
		if err := dc.Build(context.Background(), s, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "#1 building") {
			t.Fatalf("output %q", out.String())
		}
		img, err := dc.BuiltImage(context.Background(), s)
		if err != nil || img.Labels[docker.LabelBuildID] != "9" || !slices.Contains(img.Tags, s.Tag()) {
			t.Fatalf("containerd=%v: %+v %v", containerd, img, err)
		}
	}
}

func TestBuiltImageLabelChecks(t *testing.T) {
	cases := map[string]map[string]string{
		"foreign namespace label": {docker.LabelServerID: "4"},
		"config hash label":       {docker.LabelConfigHash: "x"},
		"unknown namespace label": {"dev.golemengine.deploy.anything": "x"},
	}
	for name, extra := range cases {
		dc, fake, s := newBuildClient(t)
		fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome { return dockertest.BuildOutcome{ExtraLabels: extra} })
		if err := dc.Build(context.Background(), s, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
		img, err := dc.BuiltImage(context.Background(), s)
		if !errors.Is(err, docker.ErrLabelMismatch) || img.ID == "" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Labels outside the namespace are fine.
	dc, fake, s := newBuildClient(t)
	fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		return dockertest.BuildOutcome{ExtraLabels: map[string]string{"org.opencontainers.image.title": "game"}}
	})
	_ = dc.Build(context.Background(), s, &strings.Builder{})
	if _, err := dc.BuiltImage(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	// A spec for another build does not verify against this image.
	other := s
	other.BuildID = 10
	if _, err := dc.BuiltImage(context.Background(), other); !errors.Is(err, docker.ErrLabelMismatch) {
		t.Fatalf("other build: %v", err)
	}
	// A garbage iidfile is never passed to the CLI.
	_ = os.WriteFile(s.IIDFile, []byte("--format={{.}}"), 0o600)
	_ = os.Remove(s.MetadataFile)
	fake.ResetCalls()
	if _, err := dc.BuiltImage(context.Background(), s); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("garbage iid: %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("calls: %v", fake.Calls())
	}
}

func TestBuildFailureClassification(t *testing.T) {
	dc, fake, s := newBuildClient(t)
	fake.SetBuildHook(func(dockertest.BuildCall) dockertest.BuildOutcome {
		return dockertest.BuildOutcome{Fail: true, Output: "secret-ish output\n"}
	})
	err := dc.Build(context.Background(), s, &strings.Builder{})
	var ce *docker.CmdError
	if !errors.As(err, &ce) || ce.Detail != "" || strings.Contains(err.Error(), "secret-ish") {
		t.Fatalf("%v", err)
	}
	fake.SetDown(true)
	if err := dc.Build(context.Background(), s, &strings.Builder{}); !errors.Is(err, docker.ErrUnavailable) {
		t.Fatalf("down: %v", err)
	}
}

func TestImageInUse(t *testing.T) {
	fake := dockertest.New()
	dc := docker.New(docker.Options{Runner: fake, InstanceID: dockertest.InstanceID})
	id := dockertest.ImageID(5)
	fake.AddImage(dockertest.Image{ID: id})
	if used, err := dc.ContainersUsingImage(context.Background(), id); err != nil || len(used) != 0 {
		t.Fatalf("%v %v", used, err)
	}
	fake.AddContainer(dockertest.Container{Image: id, State: "exited"})
	if used, err := dc.ContainersUsingImage(context.Background(), id); err != nil || len(used) != 1 {
		t.Fatalf("%v %v", used, err)
	}
	if _, err := dc.ContainersUsingImage(context.Background(), "abc"); err == nil {
		t.Fatal("short id accepted")
	}
}
