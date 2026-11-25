package docker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/demiurgos-hub/golem-deploy/internal/backups"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/docker/dockertest"
)

// TestIntegrationDataVolumes checks, against a real Docker daemon, what the
// backup design relies on: a fresh named volume takes the owner and mode of
// the image's /data, helpers that are created and never started can copy a
// mounted volume out and in, the copy out lists links and FIFOs (which the
// normaliser leaves out), a copy in keeps the tar's uid and gid so the game
// user can write afterwards, and a read-only mount refuses writes.
func TestIntegrationDataVolumes(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	tag := buildImage(t, "gdm2h-data-"+sfx, "FROM "+BusyboxImage+"\n"+
		"RUN mkdir /data && chown 65532:65532 /data && chmod 0700 /data\n"+
		`CMD ["sh", "-c", "sleep 3600"]`+"\n", true)
	image := dockerCLI(t, "", "image", "inspect", "--format", "{{.Id}}", tag)
	id := serverID()
	name := "gdm2h-data-" + sfx
	var volumes []string
	newVolume := func() string {
		t.Helper()
		v, err := docker.NewDataVolumeName(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CreateDataVolume(ctx, id, name, v); err != nil {
			t.Fatal(err)
		}
		volumes = append(volumes, v)
		return v
	}
	var helpers []string
	t.Cleanup(func() {
		for _, h := range helpers {
			dockerQuiet("rm", "--force", h)
		}
		for _, v := range volumes {
			dockerQuiet("volume", "rm", "--force", v)
		}
	})
	helper := func(vol string, readOnly bool) (string, string) {
		t.Helper()
		launch, _ := docker.NewLaunchID()
		hid, err := c.CreateHelper(ctx, docker.HelperSpec{LaunchID: launch, ServerID: id, ServerName: name, Image: image, Volume: vol, Target: "/data", ReadOnly: readOnly})
		if err != nil {
			t.Fatal(err)
		}
		helpers = append(helpers, hid)
		return launch, hid
	}
	// The game (uid 65532) writes into a fresh volume: regular files, a
	// directory, a symbolic link, a hard link and a FIFO.
	src := newVolume()
	out := dockerCLI(t, "", "run", "--rm", "--user", "65532:65532", "--network", "none", "--mount", "type=volume,source="+src+",target=/data", tag,
		"sh", "-c", "stat -c '%u:%g %a' /data && printf snap > /data/world.snapshot && mkdir /data/d && printf hello > /data/d/f && "+
			"ln -s /etc/passwd /data/link && ln /data/d/f /data/hard && mkfifo /data/fifo && echo ok")
	if !strings.HasPrefix(out, "65532:65532 700") || !strings.HasSuffix(out, "ok") {
		t.Fatalf("fresh volume: %q", out)
	}

	// Copy out through a read-only helper that is never started.
	launch, hid := helper(src, true)
	if state := dockerCLI(t, "", "container", "inspect", "--format", "{{.State.Status}} {{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}}", hid); state != "created none true" {
		t.Fatalf("helper %q", state)
	}
	var archive bytes.Buffer
	var res backups.Result
	var raw []string
	err := c.CopyFromHelper(ctx, launch, hid, "/data", func(r io.Reader) error {
		pr, pw := io.Pipe()
		go func() {
			tr := tar.NewReader(io.TeeReader(r, pw))
			for {
				h, err := tr.Next()
				if err != nil {
					_, _ = io.Copy(pw, r)
					pw.Close()
					return
				}
				raw = append(raw, h.Name+":"+strconv.Itoa(int(h.Typeflag)))
			}
		}()
		var err error
		res, err = backups.Normalize(pr, "data", 1<<20, &archive)
		_, _ = io.Copy(io.Discard, pr)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("docker cp entries: %v", raw)
	if res.Files != 2 || res.Dirs != 1 || res.SkippedCount != 3 {
		t.Fatalf("%+v (raw %v)", res, raw)
	}
	if res.Skipped[backups.SkipSymlink] != 1 || res.Skipped[backups.SkipPipe] != 1 || res.Skipped[backups.SkipHardLink] != 1 {
		t.Fatalf("%v", res.Skipped)
	}
	// The read-only mount refuses a copy in.
	if err := c.CopyToHelper(ctx, launch, hid, "/data", func(w io.Writer) error { return tar.NewWriter(w).Close() }); err == nil {
		t.Fatal("copy into a read-only mount succeeded")
	}
	if _, err := c.RemoveHelper(ctx, launch, hid); err != nil {
		t.Fatal(err)
	}
	if _, err := dockerTry("container", "inspect", hid); err == nil {
		t.Fatal("helper kept")
	}

	// Restore into a fresh volume through a writable helper, owned by the
	// container user; the game can then change the files.
	dst := newVolume()
	launch, hid = helper(dst, false)
	err = c.CopyToHelper(ctx, launch, hid, "/data", func(w io.Writer) error {
		return backups.Rewrite(bytes.NewReader(archive.Bytes()), docker.DefaultUID, docker.DefaultGID, res.Files, res.ContentBytes, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RemoveHelper(ctx, launch, hid); err != nil {
		t.Fatal(err)
	}
	out = dockerCLI(t, "", "run", "--rm", "--user", "65532:65532", "--network", "none", "--mount", "type=volume,source="+dst+",target=/data", tag,
		"sh", "-c", "stat -c '%u:%g' /data /data/d /data/d/f /data/world.snapshot && cat /data/d/f && printf more >> /data/d/f && "+
			"touch /data/d/new && ls -a /data")
	lines := strings.Fields(out)
	if !slices.Equal(lines[:4], []string{"65532:65532", "65532:65532", "65532:65532", "65532:65532"}) || !strings.Contains(out, "hello") ||
		strings.Contains(out, "link") || strings.Contains(out, "fifo") {
		t.Fatalf("restored volume: %q", out)
	}
	// A copy in that stops half way is an error.
	launch, hid = helper(dst, false)
	boom := errors.New("archive damaged")
	if err := c.CopyToHelper(ctx, launch, hid, "/data", func(w io.Writer) error {
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte("partial"))
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if _, err := c.RemoveHelper(ctx, launch, hid); err != nil {
		t.Fatal(err)
	}
	for _, v := range volumes {
		if err := c.RemoveDataVolume(ctx, id, v); err != nil {
			t.Fatal(err)
		}
	}
}

// TestIntegrationRestoreVolumeRoot records what the restore relies on, on
// a real Docker daemon. A fresh volume mounted with volume-nocopy keeps a
// root-owned root: "docker cp" never applies the tar's "." entry to the
// mount root (with or without --archive), so uid 65532 could not write
// there and the restore cannot use volume-nocopy. Without it Docker copies
// the image's /data (owner, mode and content) into the empty volume when
// the never-started helper is created, so the restore reads the helper's
// /data back before it copies anything and refuses image content.
func TestIntegrationRestoreVolumeRoot(t *testing.T) {
	c, _ := itClient(t)
	ctx := context.Background()
	sfx := randSuffix()
	full := buildImage(t, "gdm2h-root-full-"+sfx, "FROM "+BusyboxImage+"\n"+
		"RUN mkdir /data && echo image > /data/from-image && chown -R 65532:65532 /data && chmod 0700 /data\n", true)
	empty := buildImage(t, "gdm2h-root-empty-"+sfx, "FROM "+BusyboxImage+"\n"+
		"RUN mkdir /data && chown 65532:65532 /data && chmod 0700 /data\n", true)
	imageID := func(tag string) string { return dockerCLI(t, "", "image", "inspect", "--format", "{{.Id}}", tag) }
	id := serverID()
	name := "gdm2h-root-" + sfx
	var volumes, containers []string
	t.Cleanup(func() {
		for _, h := range containers {
			dockerQuiet("rm", "--force", h)
		}
		for _, v := range volumes {
			dockerQuiet("volume", "rm", "--force", v)
		}
	})
	newVolume := func() string {
		t.Helper()
		v, err := docker.NewDataVolumeName(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.CreateDataVolume(ctx, id, name, v); err != nil {
			t.Fatal(err)
		}
		volumes = append(volumes, v)
		return v
	}
	var in bytes.Buffer
	tw := tar.NewWriter(&in)
	_ = tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o700, Uid: docker.DefaultUID, Gid: docker.DefaultGID})
	_ = tw.WriteHeader(&tar.Header{Name: "world.snapshot", Typeflag: tar.TypeReg, Mode: 0o600, Uid: docker.DefaultUID, Gid: docker.DefaultGID, Size: 4})
	_, _ = tw.Write([]byte("snap"))
	_ = tw.Close()

	// volume-nocopy: the root stays root's, whatever the tar says.
	nocopy := newVolume()
	hid := dockerCLI(t, "", "create", "--user", "65532:65532", "--network", "none", "--read-only", "--label", docker.LabelManaged+"=true",
		"--label", docker.LabelInstance+"="+dockertest.IntegrationInstanceID, "--mount", "type=volume,source="+nocopy+",target=/data,volume-nocopy", full)
	containers = append(containers, hid)
	for _, archive := range []bool{false, true} {
		args := []string{"cp"}
		if archive {
			args = append(args, "--archive")
		}
		dockerCLI(t, in.String(), append(args, "-", hid+":/data")...)
	}
	out := dockerCLI(t, "", "run", "--rm", "--user", "65532:65532", "--network", "none", "--mount", "type=volume,source="+nocopy+",target=/data", empty,
		"sh", "-c", "stat -c '%u:%g' /data; ls /data; touch /data/new 2>/dev/null || echo refused")
	if !strings.HasPrefix(out, "0:0") || !strings.Contains(out, "world.snapshot") || strings.Contains(out, "from-image") || !strings.HasSuffix(out, "refused") {
		t.Fatalf("volume-nocopy root: %q", out)
	}

	// Copy-up at create: the helper's fresh volume shows the image's /data.
	list := func(img string) []string {
		t.Helper()
		launch, _ := docker.NewLaunchID()
		h, err := c.CreateHelper(ctx, docker.HelperSpec{LaunchID: launch, ServerID: id, ServerName: name, Image: imageID(img), Volume: newVolume(), Target: "/data"})
		if err != nil {
			t.Fatal(err)
		}
		containers = append(containers, h)
		var names []string
		if err := c.CopyFromHelper(ctx, launch, h, "/data", func(r io.Reader) error {
			tr := tar.NewReader(r)
			for {
				hdr, err := tr.Next()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				names = append(names, hdr.Name)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if state := dockerCLI(t, "", "container", "inspect", "--format", "{{.State.Status}}", h); state != "created" {
			t.Fatalf("helper %q", state)
		}
		return names
	}
	if got := list(full); !slices.Contains(got, "data/from-image") {
		t.Fatalf("image content not copied into the fresh volume at create: %v", got)
	}
	if got := list(empty); !slices.Equal(got, []string{"data/"}) {
		t.Fatalf("empty image /data: %v", got)
	}
	// With copy-up the root has the image's owner: uid 65532 can create,
	// replace and delete files after a copy in.
	vol := volumes[len(volumes)-1]
	launch, _ := docker.NewLaunchID()
	h, err := c.CreateHelper(ctx, docker.HelperSpec{LaunchID: launch, ServerID: id, ServerName: name, Image: imageID(empty), Volume: vol, Target: "/data"})
	if err != nil {
		t.Fatal(err)
	}
	containers = append(containers, h)
	if err := c.CopyToHelper(ctx, launch, h, "/data", func(w io.Writer) error { _, err := w.Write(in.Bytes()); return err }); err != nil {
		t.Fatal(err)
	}
	out = dockerCLI(t, "", "run", "--rm", "--user", "65532:65532", "--network", "none", "--mount", "type=volume,source="+vol+",target=/data", empty,
		"sh", "-c", "stat -c '%u:%g %a' /data && echo n > /data/new && echo r > /data/world.snapshot && rm /data/new /data/world.snapshot && echo ok")
	if !strings.HasPrefix(out, "65532:65532 700") || !strings.HasSuffix(out, "ok") {
		t.Fatalf("copy-up root: %q", out)
	}
}
