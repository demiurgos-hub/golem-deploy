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
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Name] = s.Reason
	}
	if reasons["link"] != "symbolic link" || reasons["fifo"] != "named pipe" || reasons["hard"] != "hard link" {
		t.Fatalf("%v", reasons)
	}
	// The read-only mount refuses a copy in.
	if err := c.CopyToHelper(ctx, launch, hid, "/data", func(w io.Writer) error { return tar.NewWriter(w).Close() }); err == nil {
		t.Fatal("copy into a read-only mount succeeded")
	}
	if err := c.RemoveHelper(ctx, launch, hid); err != nil {
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
	if err := c.RemoveHelper(ctx, launch, hid); err != nil {
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
	if err := c.RemoveHelper(ctx, launch, hid); err != nil {
		t.Fatal(err)
	}
	for _, v := range volumes {
		if err := c.RemoveDataVolume(ctx, id, v); err != nil {
			t.Fatal(err)
		}
	}
}
