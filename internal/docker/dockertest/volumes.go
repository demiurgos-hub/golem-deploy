package dockertest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
)

// File is one entry of a fake volume, as "docker cp" transfers it: Name is
// relative to the volume root (no leading or trailing slash), Type a tar
// type flag (tar.TypeReg, tar.TypeDir, tar.TypeSymlink, ...).
type File struct {
	Name     string
	Type     byte
	Mode     int64
	UID, GID int
	Data     []byte
	Linkname string
	ModTime  time.Time
}

// Reg, Dir and Symlink build entries for tests (owned by 65532).
func Reg(name, data string) File {
	return File{Name: name, Type: tar.TypeReg, Mode: 0o600, UID: 65532, GID: 65532, Data: []byte(data)}
}

// Dir is a directory entry.
func Dir(name string) File {
	return File{Name: name, Type: tar.TypeDir, Mode: 0o700, UID: 65532, GID: 65532}
}

// Special is an entry of another type (symlink, hard link, FIFO, device).
func Special(name string, typ byte, link string) File {
	return File{Name: name, Type: typ, Mode: 0o600, UID: 65532, GID: 65532, Linkname: link}
}

// CopyCall is a "docker cp" the fake received: Out is true for a copy out
// of a container, Volume the volume at the copied path.
type CopyCall struct {
	Out    bool
	Volume string
}

// SetCopyHook installs fn, called before every "docker cp"; an error makes
// the copy fail (after fn returns, so fn can also block or change state).
func (f *Fake) SetCopyHook(fn func(CopyCall) error) {
	f.mu.Lock()
	f.copyHook = fn
	f.mu.Unlock()
}

// SetVolumeFiles replaces a volume's content (creating an unlabelled
// volume when it does not exist).
func (f *Fake) SetVolumeFiles(name string, files ...File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.volumes[name]
	if v == nil {
		v = &Volume{Name: name, Labels: map[string]string{}}
		f.volumes[name] = v
	}
	v.Files = slices.Clone(files)
}

// VolumeFiles returns a copy of a volume's content.
func (f *Fake) VolumeFiles(name string) []File {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v := f.volumes[name]; v != nil {
		return slices.Clone(v.Files)
	}
	return nil
}

// Volumes returns the names of all volumes.
func (f *Fake) Volumes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.volumes))
	for n := range f.volumes {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// VolumeLabels returns a volume's labels (nil when it does not exist).
func (f *Fake) VolumeLabels(name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v := f.volumes[name]; v != nil {
		return v.Labels
	}
	return nil
}

// CopiedIn returns the entries of every tar stream copied into a container.
func (f *Fake) CopiedIn() [][]File {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.copiedIn)
}

func (f *Fake) volumeCreate(a []string, w io.Writer) *failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := firstOr(positional(a))
	if name == "" {
		return fail(1, "fake docker: volume create needs a name")
	}
	if f.volumes[name] == nil {
		labels := map[string]string{}
		for _, l := range flagValues(a, "--label") {
			k, v, _ := strings.Cut(l, "=")
			labels[k] = v
		}
		f.volumes[name] = &Volume{Name: name, Labels: labels}
	}
	fmt.Fprintln(w, name)
	return nil
}

// cp implements "docker cp -- <id>:<dir> -" and "docker cp -- - <id>:<dir>"
// for directories that are volume mounts.
func (f *Fake) cp(ctx context.Context, a []string, stdin io.Reader, stdout, stderr io.Writer) error {
	pos := positional(a)
	failWith := func(format string, args ...any) error {
		fmt.Fprintf(stderr, format+"\n", args...)
		return &ExitError{Code: 1}
	}
	if len(pos) != 2 {
		return failWith("fake docker: cp needs two arguments")
	}
	out := pos[1] == "-"
	spec := pos[0]
	if !out {
		spec = pos[1]
	}
	ref, dir, ok := strings.Cut(spec, ":")
	if !ok {
		return failWith("fake docker: cp needs a container path")
	}
	f.mu.Lock()
	c := f.findContainer(ref)
	if c == nil {
		f.mu.Unlock()
		return failWith("Error response from daemon: No such container: %s", ref)
	}
	vol := c.MountTargets[dir]
	readOnly := slices.Contains(c.ReadOnlyTargets, dir)
	hook := f.copyHook
	f.mu.Unlock()
	if vol == "" {
		return failWith("Error response from daemon: Could not find the file %s in container %s", dir, ref)
	}
	if hook != nil {
		if err := hook(CopyCall{Out: out, Volume: vol}); err != nil {
			return failWith("Error response from daemon: %v", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if out {
		return f.copyOut(vol, path.Base(dir), stdout)
	}
	if readOnly {
		return failWith("Error response from daemon: mounted volume is marked read-only")
	}
	return f.copyIn(vol, stdin, failWith)
}

func (f *Fake) copyOut(vol, base string, w io.Writer) error {
	f.mu.Lock()
	files := slices.Clone(f.volumes[vol].Files)
	f.mu.Unlock()
	slices.SortStableFunc(files, func(a, b File) int { return strings.Compare(a.Name, b.Name) })
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 65532, Gid: 65532}); err != nil {
		return err
	}
	for _, e := range files {
		h := &tar.Header{Name: base + "/" + e.Name, Typeflag: e.Type, Mode: e.Mode, Uid: e.UID, Gid: e.GID, Linkname: e.Linkname, ModTime: e.ModTime}
		if e.Type == tar.TypeDir {
			h.Name += "/"
		}
		if e.Type == tar.TypeReg {
			h.Size = int64(len(e.Data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Size > 0 {
			if _, err := tw.Write(e.Data); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

func (f *Fake) copyIn(vol string, r io.Reader, failWith func(string, ...any) error) error {
	if r == nil {
		return failWith("fake docker: cp - needs standard input")
	}
	tr := tar.NewReader(r)
	var got []File
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failWith("Error response from daemon: invalid tar: %v", err)
		}
		e := File{Name: strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/"), Type: h.Typeflag, Mode: h.Mode, UID: h.Uid, GID: h.Gid,
			Linkname: h.Linkname, ModTime: h.ModTime}
		if h.Typeflag == tar.TypeReg {
			if e.Data, err = io.ReadAll(tr); err != nil {
				return failWith("Error response from daemon: invalid tar: %v", err)
			}
		}
		got = append(got, e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copiedIn = append(f.copiedIn, got)
	v := f.volumes[vol]
	for _, e := range got {
		if e.Name == "" || e.Name == "." {
			continue
		}
		v.Files = slices.DeleteFunc(v.Files, func(x File) bool { return x.Name == e.Name })
		v.Files = append(v.Files, e)
	}
	return nil
}
