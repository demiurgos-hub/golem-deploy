package docker

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Container environment values reach "docker run" only through an env file
// ("--env-file"), never through argv or the CLI's own process environment:
// a variable in the CLI's environment (LD_PRELOAD, GODEBUG, DOCKER_HOST,
// ...) would change how the CLI itself runs as the daemon user.
//
// The docker CLI parses an env file line by line (bufio.Scanner, 64 KiB
// line limit, trailing "\r" dropped): leading whitespace is trimmed, empty
// lines and lines starting with "#" are skipped, a UTF-8 BOM is trimmed from
// the first line, the key ends at the first "=" and the value is everything
// after it, taken literally (no quotes, escapes, expansion or trimming). A
// line without "=" means "copy KEY from the CLI's environment", which must
// never happen. EncodeEnvFile therefore emits exactly "KEY=value\n" per
// variable, with keys that match ^[A-Za-z_][A-Za-z0-9_]*$ (no whitespace,
// "#", BOM or "=") and values that ValidateEnvValue accepted (valid UTF-8,
// no NUL, CR or LF, bounded size), so the file means exactly the map it was
// made from.

// EnvFilePrefix names the per-operation directories that hold env files on
// platforms without the pipe mechanism; SweepEnvFiles removes leftovers.
const EnvFilePrefix = "docker-envfile-"

// EncodeEnvFile renders env (validated with ValidateEnv first) as an env
// file, sorted by key.
func EncodeEnvFile(env map[string]string) ([]byte, error) {
	if err := ValidateEnv(env); err != nil {
		return nil, err
	}
	return encodeEnvFile(env), nil
}

// encodeEnvFile renders already validated variables, one "KEY=value\n"
// line each, sorted by key; nil when env is empty.
func encodeEnvFile(env map[string]string) []byte {
	if len(env) == 0 {
		return nil
	}
	var b bytes.Buffer
	for _, k := range EnvKeys(env) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// envFileFunc makes content available to the docker CLI as a file and calls
// run with the path to pass to --env-file and the files the child must
// inherit (Cmd.ExtraFiles). The file exists only while run executes.
type envFileFunc func(content []byte, run func(path string, extra []*os.File) error) error

// newEnvFileFunc picks the mechanism: an anonymous pipe inherited as fd 3
// ("/dev/fd/3") on Linux, where the daemon runs in production, so the values
// never touch a disk; a private temporary file elsewhere (Docker Desktop on
// Windows and macOS).
func newEnvFileFunc(goos, tmpDir string) envFileFunc {
	if goos == "linux" {
		return pipeEnvFile
	}
	return tempEnvFile(tmpDir)
}

// errEnvFileNotRead means the CLI exited successfully without reading the
// whole env file.
var errEnvFileNotRead = errors.New("docker: the env file was not read completely")

// pipeEnvFile passes content through a pipe whose read end becomes the
// child's fd 3. The writer runs in its own goroutine because the content can
// exceed the pipe buffer; the read end is closed by the runner once the
// child has started and again here once run returns, so the writer can
// never block forever.
func pipeEnvFile(content []byte, run func(path string, extra []*os.File) error) error {
	r, w, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("env file pipe: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := w.Write(content)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		done <- err
	}()
	runErr := run("/dev/fd/3", []*os.File{r})
	_ = r.Close()
	werr := <-done
	if runErr != nil {
		return runErr
	}
	if werr != nil {
		return fmt.Errorf("%w: %v", errEnvFileNotRead, werr)
	}
	return nil
}

// tempEnvFile writes content to a 0600 file in a fresh 0700 directory under
// dir (the data directory's tmp/) and removes the directory as soon as run
// returns. Leftovers of a crash are removed at startup by SweepEnvFiles.
func tempEnvFile(dir string) envFileFunc {
	return func(content []byte, run func(path string, extra []*os.File) error) error {
		if dir == "" {
			dir = os.TempDir()
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		opDir, err := os.MkdirTemp(dir, EnvFilePrefix+"*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(opDir)
		p := filepath.Join(opDir, "env")
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		return run(p, nil)
	}
}

// SweepEnvFiles removes env-file directories left in dir by a crash. Call
// it once at startup, before any docker command runs.
func SweepEnvFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), EnvFilePrefix) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
