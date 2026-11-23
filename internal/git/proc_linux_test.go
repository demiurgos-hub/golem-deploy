//go:build linux

package git

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// procGone reports whether pid no longer runs (absent or a zombie).
func procGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// "pid (comm) S ..." — the state follows the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] == 'Z'
}

func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !procGone(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("%s (pid %d) still running", what, pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestG9_ProcessGroupKilledAfterWait: a helper that outlives git (here a
// background sleep started by the child) is killed when Wait returns, not
// only on cancellation.
func TestG9_ProcessGroupKilledAfterWait(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 60 >/dev/null 2>&1 & echo $!")
	var out bytes.Buffer
	cmd.Stdout = &out
	tree := newProcTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tree.started(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("pid: %q", out.String())
	}
	if procGone(pid) {
		t.Fatal("background process already gone before close (test proves nothing)")
	}
	tree.close()
	waitGone(t, pid, "grandchild")
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatal("Pdeathsig not set")
	}
}

// holdingListener accepts connections and keeps them open without ever
// answering, until the test ends.
func holdingListener(t *testing.T) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done })
	return ln
}

// TestG9_PdeathsigHelper is the daemon stand-in for TestG9_PdeathsigKillsGit:
// it runs git against a server (in the parent test process, so it outlives
// the helper) that never answers, until it is killed.
func TestG9_PdeathsigHelper(t *testing.T) {
	addr := os.Getenv("GOLEM_TEST_PDEATHSIG_ADDR")
	if addr == "" {
		t.Skip("helper process only")
	}
	base := filepath.Join(os.Getenv("GOLEM_TEST_PDEATHSIG_DIR"), "data")
	r, err := New(Options{TempDir: filepath.Join(base, "tmp"), KnownHostsFile: filepath.Join(base, "known_hosts"), NetworkTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.ListRemoteBranches(context.Background(), Remote{URL: "https://" + addr + "/x.git"})
}

// TestG9_PdeathsigKillsGit: when the process running the git runner dies
// abruptly (SIGKILL, no cleanup), its git child is killed by the kernel.
func TestG9_PdeathsigKillsGit(t *testing.T) {
	requireGit(t)
	helper := exec.Command(os.Args[0], "-test.run=^TestG9_PdeathsigHelper$", "-test.v")
	ln := holdingListener(t)
	helper.Env = append(os.Environ(), "GOLEM_TEST_PDEATHSIG_ADDR="+ln.Addr().String(), "GOLEM_TEST_PDEATHSIG_DIR="+t.TempDir())
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = helper.Process.Kill(); _ = helper.Wait() }()

	// Find the helper's git child.
	gitPid := 0
	deadline := time.Now().Add(15 * time.Second)
	for gitPid == 0 && time.Now().Before(deadline) {
		gitPid = childRunning(helper.Process.Pid, "git", "ls-remote")
		time.Sleep(50 * time.Millisecond)
	}
	if gitPid == 0 {
		t.Fatal("helper never started git")
	}
	time.Sleep(300 * time.Millisecond)
	if procGone(gitPid) {
		t.Fatal("git exited on its own (test proves nothing)")
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	waitGone(t, gitPid, "git child of a killed daemon")
}

// childRunning returns the pid of a child of parent whose comm is name
// and whose command line contains arg.
func childRunning(parent int, name, arg string) int {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || end < open {
			continue
		}
		fields := strings.Fields(s[end+1:])
		if len(fields) < 2 || s[open+1:end] != name {
			continue
		}
		if ppid, _ := strconv.Atoi(fields[1]); ppid != parent {
			continue
		}
		if cmdline, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline"); strings.Contains(string(cmdline), arg) {
			return pid
		}
	}
	return 0
}
