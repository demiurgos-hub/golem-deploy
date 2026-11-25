package docker_test

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

func hubSpec(image string) docker.HubSpec {
	return docker.HubSpec{Image: image, ImageVolumes: []string{"/var/cache"}, MemoryMB: 512, ConfigHash: "hash1",
		Env: map[string]string{"GOLEM_INSPECTOR_AGENT_TOKEN": secretMarker, "GOLEM_INSPECTOR_ADDR": ":8787"}}
}

// The hub container's exact arguments: the same hardening as servers, no
// mounts, the port on 127.0.0.1 only, every label explicit, an env file.
func TestHubSpecArgs(t *testing.T) {
	img := dockertest.ImageID(9)
	args, err := hubSpec(img).Args(instance, "/dev/fd/3")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create", "--pull", "never",
		"--name", "golem-deploy_inspector",
		"--label", "dev.golemengine.deploy.managed=true",
		"--label", "dev.golemengine.deploy.instance=" + instance,
		"--label", "dev.golemengine.deploy.server-id=",
		"--label", "dev.golemengine.deploy.server=",
		"--label", "dev.golemengine.deploy.project-id=",
		"--label", "dev.golemengine.deploy.commit=",
		"--label", "dev.golemengine.deploy.build-id=",
		"--label", "dev.golemengine.deploy.engine-version=",
		"--label", "dev.golemengine.deploy.config-hash=hash1",
		"--label", "dev.golemengine.deploy.launch-id=",
		"--label", "dev.golemengine.deploy.helper=",
		"--label", "dev.golemengine.deploy.inspector=",
		"--label", "dev.golemengine.deploy.inspector-hub=true",
		"--restart", "unless-stopped",
		"--init",
		"--no-healthcheck",
		"--stop-signal", "SIGTERM",
		"--stop-timeout", "15",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "256",
		"--memory", "512m", "--memory-swap", "512m",
		"--log-driver", "json-file",
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=3",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--tmpfs", "/var/cache:rw,noexec,nosuid,size=64m",
		"--user", "65532:65532",
		"--network", "golem-deploy",
		"--publish", "127.0.0.1::8787/tcp",
		"--env-file", "/dev/fd/3",
		"--", img,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args:\n%q\nwant:\n%q", args, want)
	}
	for name, mut := range map[string]func(*docker.HubSpec){
		"tag instead of id": func(s *docker.HubSpec) { s.Image = "ghcr.io/x/y:1" },
		"no env":            func(s *docker.HubSpec) { s.Env = nil },
		"foreign variable":  func(s *docker.HubSpec) { s.Env["PATH"] = "/x" },
		"line break":        func(s *docker.HubSpec) { s.Env["GOLEM_INSPECTOR_X"] = "a\nb" },
		"small memory":      func(s *docker.HubSpec) { s.MemoryMB = 4 },
		"label newline":     func(s *docker.HubSpec) { s.ConfigHash = "a\nb" },
		"too many volumes":  func(s *docker.HubSpec) { s.ImageVolumes = make([]string, docker.MaxImageVolumes+1) },
	} {
		s := hubSpec(img)
		s.Env = map[string]string{"GOLEM_INSPECTOR_ADDR": ":8787"}
		mut(&s)
		if _, err := s.Args(instance, "/dev/fd/3"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := hubSpec(img).Args(instance, ""); err == nil {
		t.Error("no env file accepted")
	}
}

// Creating the hub passes its variables only in the env file; the hub is
// found by its labels and name, and only it is started, read or removed.
func TestInspectorHubLifecycle(t *testing.T) {
	c, f, logs := newClient(t)
	ctx := context.Background()
	img := dockertest.ImageID(9)
	f.AddImage(dockertest.Image{ID: img, Labels: map[string]string{docker.LabelInspectorHub: "", "org.opencontainers.image.title": "hub"},
		Logs: []string{"listening"}})
	f.AddNetwork(docker.NetworkName, dockertest.OurLabels(), map[string]string{docker.NetworkICCOption: "false"})
	id, err := c.CreateInspectorHub(ctx, hubSpec(img))
	if err != nil {
		t.Fatal(err)
	}
	calls := f.CallsTo("create")
	if len(calls) != 1 || strings.Contains(strings.Join(calls[0].Args, " "), secretMarker) ||
		!strings.Contains(calls[0].EnvFile, "GOLEM_INSPECTOR_AGENT_TOKEN="+secretMarker) || strings.Contains(logs.String(), secretMarker) {
		t.Fatalf("env handling: %+v\n%s", calls, logs)
	}
	if err := c.StartInspectorHub(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Impostors: the right labels without the name, another instance, a
	// server id, and a server container (its hub label is explicit and
	// empty).
	other := strings.Repeat("b", 32)
	f.AddContainer(dockertest.Container{Name: "golem-deploy-inspector", Image: img, Labels: dockertest.OurLabels(docker.LabelInspectorHub, "true")})
	f.AddContainer(dockertest.Container{Name: docker.InspectorHubName + "x", Image: img, Labels: dockertest.OurLabels(docker.LabelInspectorHub, "true")})
	foreign := f.AddContainer(dockertest.Container{Name: "elsewhere", Image: img,
		Labels: dockertest.ManagedLabels(docker.LabelInstance, other, docker.LabelInspectorHub, "true")})
	withServer := f.AddContainer(dockertest.Container{Name: docker.InspectorHubName, Image: img,
		Labels: dockertest.OurLabels(docker.LabelInspectorHub, "true", docker.LabelServerID, "4")})
	f.SetIgnoreFilters(true)
	hubs, err := c.InspectorHubs(ctx)
	f.SetIgnoreFilters(false)
	if err != nil || len(hubs) != 1 || hubs[0].ID != id || !hubs[0].Running {
		t.Fatalf("%+v %v", hubs, err)
	}
	for _, bad := range []string{foreign, withServer} {
		if err := c.RemoveInspectorHub(ctx, bad); !errors.Is(err, docker.ErrNotOurs) {
			t.Fatalf("removing %s: %v", bad, err)
		}
		if _, err := c.InspectorHubLogTail(ctx, bad, 5); !errors.Is(err, docker.ErrNotOurs) {
			t.Fatalf("logs of %s: %v", bad, err)
		}
	}
	if len(f.CallsTo("rm")) != 0 || len(f.CallsTo("stop")) != 0 {
		t.Fatal("an impostor was touched")
	}
	out, err := c.InspectorHubLogTail(ctx, id, 5)
	if err != nil || !strings.Contains(string(out), "listening") {
		t.Fatalf("%q %v", out, err)
	}
	ct, _ := f.Container(id)
	if ct.Labels[docker.LabelInspectorHub] != "true" || ct.Labels["org.opencontainers.image.title"] != "hub" {
		t.Fatalf("labels %v", ct.Labels)
	}
	if err := c.RemoveInspectorHub(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Container(id); ok {
		t.Fatal("hub not removed")
	}
	if err := c.RemoveInspectorHub(ctx, id); err != nil {
		t.Fatalf("removing a gone hub: %v", err)
	}
	// A server container never carries the hub label.
	sp := spec(img)
	sp.Inspector = true
	args, err := sp.Args(instance, "/dev/fd/3")
	if err != nil || !slices.Contains(args, "dev.golemengine.deploy.inspector=true") || !slices.Contains(args, "dev.golemengine.deploy.inspector-hub=") {
		t.Fatalf("%q %v", args, err)
	}
}

// Pulls run with the dedicated client configuration, never the
// operator's; references are checked before they reach the CLI.
func TestPullAndResolveImage(t *testing.T) {
	f := dockertest.New()
	cfgDir := filepath.Join(t.TempDir(), "docker-build")
	c := docker.New(docker.Options{Runner: f, InstanceID: instance, TempDir: t.TempDir(), BuildConfigDir: cfgDir, Logger: slog.New(slog.DiscardHandler),
		LookupEnv: func(k string) (string, bool) { v, ok := daemonEnv[k]; return v, ok }})
	ctx := context.Background()
	ref := "ghcr.io/demiurgos-hub/golem-inspector:0.4.0-rc.1"
	if _, err := c.ResolveImage(ctx, ref); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
	f.AddRegistryImage(ref, dockertest.Image{ID: dockertest.ImageID(5)})
	if err := c.PullImage(ctx, ref); err != nil {
		t.Fatal(err)
	}
	pulls := f.CallsTo("pull")
	if len(pulls) != 1 || !slices.Equal(pulls[0].Args, []string{"pull", "--quiet", "--", ref}) || !slices.Contains(pulls[0].Env, "DOCKER_CONFIG="+cfgDir) {
		t.Fatalf("%+v", pulls)
	}
	img, err := c.ResolveImage(ctx, ref)
	if err != nil || img.ID != dockertest.ImageID(5) {
		t.Fatalf("%+v %v", img, err)
	}
	noConfig, _, _ := newClient(t)
	if err := noConfig.PullImage(ctx, ref); !errors.Is(err, docker.ErrBuildConfig) {
		t.Fatalf("pull without the dedicated configuration: %v", err)
	}
	for _, ok := range []string{ref, "golem-inspector:dev", "registry.example.com:5000/a/b:1",
		"ghcr.io/x/y@sha256:" + strings.Repeat("a", 64), "ghcr.io/x/y:1@sha256:" + strings.Repeat("a", 64)} {
		if err := docker.ValidateHubImageRef(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "--help", dockertest.ImageID(1), "abcdef123456", "a b", "x@sha256:12", "x:", "x//y:1",
		"x:1:2", "../x", "x/", strings.Repeat("a", 300)} {
		if err := docker.ValidateHubImageRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if len(f.CallsTo("pull")) != 1 {
		t.Fatal("an invalid reference reached the CLI")
	}
}

func TestManagedNetworkAddresses(t *testing.T) {
	c, _, _ := newClient(t)
	ctx := context.Background()
	if _, err := c.ManagedNetworkAddresses(ctx); !errors.Is(err, docker.ErrNotFound) {
		t.Fatalf("missing network: %v", err)
	}
	if _, err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := c.ManagedNetworkAddresses(ctx)
	if err != nil || len(a.Subnets) != 1 || a.Subnets[0] != netip.MustParsePrefix("172.30.0.0/16") ||
		len(a.Gateways) != 1 || a.Gateways[0] != netip.MustParseAddr("172.30.0.1") {
		t.Fatalf("%+v %v", a, err)
	}
	f2 := dockertest.New()
	c2 := docker.New(docker.Options{Runner: f2, InstanceID: instance, TempDir: t.TempDir()})
	f2.AddNetwork(docker.NetworkName, map[string]string{}, nil)
	if _, err := c2.ManagedNetworkAddresses(ctx); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("unmanaged: %v", err)
	}
	f2.AddNetwork(docker.NetworkName, dockertest.ManagedLabels(docker.LabelInstance, strings.Repeat("c", 32)), nil)
	if _, err := c2.ManagedNetworkAddresses(ctx); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("other instance: %v", err)
	}
}
