package docker_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

const launch = "aaaabbbbccccddddeeeeffff00001111"

func helperSpec(image, volume string) docker.HelperSpec {
	pid := int64(4)
	return docker.HelperSpec{LaunchID: launch, ServerID: 12, ServerName: "arena", ProjectID: &pid, BuildID: 9, Commit: "abc", EngineVersion: "v0.4.0",
		Image: image, ImageVolumes: []string{"/data", "/var/cache"}, Volume: volume, Target: "/data", ReadOnly: true}
}

func TestHelperArgvExact(t *testing.T) {
	img := dockertest.ImageID(1)
	args, err := helperSpec(img, "golem-deploy-data-12-0011223344556677").Args(instance)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create", "--pull", "never", "--name", "golem-deploy_helper_" + launch,
		"--label", "dev.golemengine.deploy.managed=true",
		"--label", "dev.golemengine.deploy.instance=" + instance,
		"--label", "dev.golemengine.deploy.server-id=12",
		"--label", "dev.golemengine.deploy.server=arena",
		"--label", "dev.golemengine.deploy.project-id=4",
		"--label", "dev.golemengine.deploy.commit=abc",
		"--label", "dev.golemengine.deploy.build-id=9",
		"--label", "dev.golemengine.deploy.engine-version=v0.4.0",
		"--label", "dev.golemengine.deploy.config-hash=",
		"--label", "dev.golemengine.deploy.launch-id=",
		"--label", "dev.golemengine.deploy.helper=" + launch,
		"--label", "dev.golemengine.deploy.inspector=",
		"--label", "dev.golemengine.deploy.inspector-hub=",
		"--entrypoint", "/golem-deploy-helper-is-never-started",
		"--restart", "no", "--no-healthcheck", "--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--pids-limit", "16", "--log-driver", "none", "--user", "65532:65532",
		"--tmpfs", "/var/cache:rw,noexec,nosuid,size=64m",
		"--mount", "type=volume,source=golem-deploy-data-12-0011223344556677,target=/data,readonly",
		"--", img,
	}
	if !slices.Equal(args, want) {
		t.Fatalf("argv\n got %q\nwant %q", args, want)
	}
	s := helperSpec(img, "v")
	s.ReadOnly = false
	if args, _ := s.Args(instance); args[len(args)-3] != "type=volume,source=v,target=/data" {
		t.Fatalf("%q", args)
	}
	for name, mut := range map[string]func(*docker.HelperSpec){
		"launch": func(s *docker.HelperSpec) { s.LaunchID = "x" },
		"image":  func(s *docker.HelperSpec) { s.Image = "busybox" },
		"volume": func(s *docker.HelperSpec) { s.Volume = "-bad" },
		"target": func(s *docker.HelperSpec) { s.Target = "/" },
		"server": func(s *docker.HelperSpec) { s.ServerID = 0 },
		"label":  func(s *docker.HelperSpec) { s.Commit = "a\nb" },
	} {
		s := helperSpec(img, "v")
		mut(&s)
		if _, err := s.Args(instance); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDataVolumeLifecycle(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	name, err := docker.NewDataVolumeName(12)
	if err != nil || !strings.HasPrefix(name, "golem-deploy-data-12-") || docker.ValidateVolumeName(name) != nil {
		t.Fatalf("%q %v", name, err)
	}
	if err := c.CreateDataVolume(ctx, 12, "arena", name); err != nil {
		t.Fatal(err)
	}
	labels := f.VolumeLabels(name)
	if labels[docker.LabelServerID] != "12" || labels[docker.LabelInstance] != instance || labels[docker.LabelServer] != "arena" {
		t.Fatalf("%v", labels)
	}
	if _, err := c.DataVolume(ctx, 13, name); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("other server: %v", err)
	}
	// A name that already exists without our labels is not adopted.
	f.AddVolume(dockertest.Volume{Name: "taken"})
	if err := c.CreateDataVolume(ctx, 12, "arena", "taken"); !errors.Is(err, docker.ErrNotManaged) {
		t.Fatalf("%v", err)
	}
	f.AddVolume(dockertest.Volume{Name: "other", Labels: dockertest.ManagedLabels(docker.LabelInstance, "ffffffffffffffffffffffffffffffff", docker.LabelServerID, "12")})
	if err := c.RemoveDataVolume(ctx, 12, "other"); !errors.Is(err, docker.ErrNotOurs) || !f.HasVolume("other") {
		t.Fatalf("%v", err)
	}
	if err := c.RemoveDataVolume(ctx, 12, "missing"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDataVolume(ctx, 12, name); err != nil || f.HasVolume(name) {
		t.Fatalf("%v", err)
	}
}

func TestHelperCopyAndRemove(t *testing.T) {
	c, f, _ := newClient(t)
	ctx := context.Background()
	img := managedImage(f, 1)
	vol := "golem-deploy-data-12-0011223344556677"
	if err := c.CreateDataVolume(ctx, 12, "arena", vol); err != nil {
		t.Fatal(err)
	}
	f.SetVolumeFiles(vol, dockertest.Reg("world.snapshot", "GLM"), dockertest.Special("l", tar.TypeSymlink, "/etc"))
	id, err := c.CreateHelper(ctx, helperSpec(img, vol))
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := f.Container(id)
	if ct.State != "created" || ct.Network != "none" || ct.MountTargets["/data"] != vol || !slices.Contains(ct.ReadOnlyTargets, "/data") {
		t.Fatalf("%+v", ct)
	}
	if len(f.CallsTo("start")) != 0 {
		t.Fatal("a helper was started")
	}
	var names []string
	err = c.CopyFromHelper(ctx, launch, id, "/data", func(r io.Reader) error {
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			names = append(names, h.Name)
		}
	})
	if err != nil || !slices.Equal(names, []string{"data/", "data/l", "data/world.snapshot"}) {
		t.Fatalf("%v %v", names, err)
	}
	// A consumer error cuts the copy off and is returned.
	boom := errors.New("too large")
	if err := c.CopyFromHelper(ctx, launch, id, "/data", func(io.Reader) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	// The read-only mount refuses writes.
	err = c.CopyToHelper(ctx, launch, id, "/data", func(w io.Writer) error { return tar.NewWriter(w).Close() })
	if err == nil {
		t.Fatal("copy into a read-only mount succeeded")
	}
	// Found again by its launch label when the id was lost; an impostor
	// with our labels but another name is not.
	f.AddContainer(dockertest.Container{Name: "impostor", Labels: dockertest.OurLabels(docker.LabelHelper, launch)})
	if got, err := c.Helper(ctx, launch, ""); err != nil || got.ID != id {
		t.Fatalf("%v %v", got, err)
	}
	if found, err := c.RemoveHelper(ctx, launch, ""); err != nil || !found {
		t.Fatal(found, err)
	}
	if _, ok := f.Container(id); ok {
		t.Fatal("helper kept")
	}
	if _, ok := f.Container("impostor"); !ok {
		t.Fatal("impostor removed")
	}
	if found, err := c.RemoveHelper(ctx, launch, ""); err != nil || found {
		t.Fatal(found, err)
	}
	// A recorded id that now names another container is not touched.
	impostor, _ := f.Container("impostor")
	if _, err := c.RemoveHelper(ctx, launch, impostor.ID); !errors.Is(err, docker.ErrNotOurs) {
		t.Fatalf("%v", err)
	}
	for _, rm := range f.CallsTo("rm") {
		if slices.Contains(rm.Args, impostor.ID) {
			t.Fatal("impostor got rm")
		}
	}
	// A writable helper takes a tar with the given owners.
	s := helperSpec(img, vol)
	s.ReadOnly = false
	s.LaunchID = strings.Repeat("1", 32)
	id, err = c.CreateHelper(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	err = c.CopyToHelper(ctx, s.LaunchID, id, "/data", func(w io.Writer) error {
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: "new.bin", Typeflag: tar.TypeReg, Size: 3, Mode: 0o600, Uid: 65532, Gid: 65532})
		_, _ = tw.Write([]byte("abc"))
		return tw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	files := f.VolumeFiles(vol)
	if i := slices.IndexFunc(files, func(x dockertest.File) bool { return x.Name == "new.bin" }); i < 0 || files[i].UID != 65532 || !bytes.Equal(files[i].Data, []byte("abc")) {
		t.Fatalf("%+v", files)
	}
	// A producer error cuts the copy off and is returned.
	if err := c.CopyToHelper(ctx, s.LaunchID, id, "/data", func(io.Writer) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	// The volume is in use while a helper mounts it.
	if err := c.RemoveDataVolume(ctx, 12, vol); !errors.Is(err, docker.ErrInUse) {
		t.Fatalf("%v", err)
	}
	for _, call := range f.CallsTo("cp") {
		if call.Args[1] != "--" {
			t.Fatalf("cp argv %q", call.Args)
		}
	}
}
