package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExecRunnerStopsProcessTree(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancel"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GOLEM_DEPLOY_TEST_TREE_DIR", dir)
			t.Setenv("GOLEM_DEPLOY_TEST_TREE_ROLE", "parent")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			var ctx context.Context
			var cancel context.CancelFunc
			if timeout {
				ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			errs := make(chan error, 1)
			go func() {
				errs <- (ExecRunner{}).Run(ctx, exe, Cmd{Args: []string{"-test.run=^TestProcessTreeHelper$"}, Env: os.Environ(), ProcessTree: true})
			}()
			deadline := time.Now().Add(8 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("descendant never started")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-errs:
				if !errors.Is(err, ctx.Err()) || ctx.Err() == nil {
					t.Fatalf("runner returned %v, context %v", err, ctx.Err())
				}
			case <-time.After(8 * time.Second):
				t.Fatal("runner did not wait for its process tree to terminate")
			}
			time.Sleep(time.Until(deadline.Add(-3 * time.Second)))
			if _, err := os.Stat(filepath.Join(dir, "escaped")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("descendant ran after cancellation: %v", err)
			}
		})
	}
}

func TestProcessTreeHelper(t *testing.T) {
	dir := os.Getenv("GOLEM_DEPLOY_TEST_TREE_DIR")
	switch os.Getenv("GOLEM_DEPLOY_TEST_TREE_ROLE") {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
		cmd.Env = append(os.Environ(), "GOLEM_DEPLOY_TEST_TREE_ROLE=child")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	case "child":
		if err := os.WriteFile(filepath.Join(dir, "started"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		if err := os.WriteFile(filepath.Join(dir, "escaped"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
