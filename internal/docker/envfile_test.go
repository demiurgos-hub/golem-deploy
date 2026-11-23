package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestD1_MechanismChoice(t *testing.T) {
	// Linux (production) uses the pipe; other platforms a temp file.
	content := []byte("A=1\n")
	var linuxPath, otherPath string
	_ = newEnvFileFunc("linux", t.TempDir())(content, func(p string, extra []*os.File) error {
		linuxPath = p
		closeFiles(extra)
		return nil
	})
	_ = newEnvFileFunc("windows", t.TempDir())(content, func(p string, extra []*os.File) error {
		otherPath = p
		return nil
	})
	if linuxPath != "/dev/fd/3" || !strings.HasSuffix(filepath.ToSlash(otherPath), "/env") {
		t.Fatalf("paths %q %q", linuxPath, otherPath)
	}
}

// The pipe delivers content larger than a pipe buffer, and never blocks
// when the child does not read.
func TestD1_PipeEnvFile(t *testing.T) {
	content := bytes.Repeat([]byte("K=0123456789abcdef\n"), 20000) // ~380 KiB
	var got []byte
	err := pipeEnvFile(content, func(path string, extra []*os.File) error {
		if path != "/dev/fd/3" || len(extra) != 1 {
			t.Fatalf("%q %d", path, len(extra))
		}
		b, err := io.ReadAll(extra[0]) // what the child reads from fd 3
		got = b
		return err
	})
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}

	done := make(chan error, 1)
	go func() {
		done <- pipeEnvFile(content, func(path string, extra []*os.File) error {
			closeFiles(extra) // the child exits without reading
			return nil
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errEnvFileNotRead) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer blocked when the child did not read")
	}
	// A runner that forgets to close the file still cannot block it.
	go func() {
		done <- pipeEnvFile(content, func(string, []*os.File) error { return errors.New("start failed") })
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "start failed" {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer blocked")
	}
}

func TestD1_TempEnvFile(t *testing.T) {
	root := t.TempDir()
	content := []byte("SECRET=x\n")
	var path string
	err := tempEnvFile(root)(content, func(p string, extra []*os.File) error {
		path = p
		if extra != nil {
			t.Fatal("extra files with a temp file")
		}
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(b, content) {
			t.Fatalf("%q %v", b, err)
		}
		if runtime.GOOS != "windows" {
			fi, _ := os.Stat(p)
			di, _ := os.Stat(filepath.Dir(p))
			if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
				t.Fatalf("modes %v %v", fi.Mode(), di.Mode())
			}
		}
		if !strings.HasPrefix(filepath.Base(filepath.Dir(p)), EnvFilePrefix) {
			t.Fatalf("dir %q", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("env file directory not removed")
	}
	// Leftovers of a crash are swept; other files are kept.
	if err := os.MkdirAll(filepath.Join(root, EnvFilePrefix+"123"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, EnvFilePrefix+"123", "env"), content, 0o600)
	_ = os.WriteFile(filepath.Join(root, "git-session-1"), nil, 0o600)
	if err := SweepEnvFiles(root); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != "git-session-1" {
		t.Fatalf("after sweep: %v", entries)
	}
}

// ExecRunner passes extra files as fd 3+ and closes its copies after start.
func TestExecRunnerClosesExtraFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os/exec does not support ExtraFiles on Windows (the temp-file mechanism is used there)")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	exe, _ := os.Executable()
	err = ExecRunner{}.Run(context.Background(), exe, Cmd{Args: []string{"-test.run=^$"}, ExtraFiles: []*os.File{r}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); err == nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read end still open in the parent: %v", err)
	}
}
