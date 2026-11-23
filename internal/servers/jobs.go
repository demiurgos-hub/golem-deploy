package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// The functions below are job bodies: they write progress to log and return
// jobs.Fail errors whose message is safe to show as the job detail. Callers
// run them through the job runner so jobs for one server are serialised.

// failDocker turns a docker error into a job failure with a safe message.
func failDocker(msg string, err error) error {
	if errors.Is(err, docker.ErrUnavailable) {
		return jobs.Fail("Docker is unreachable", err)
	}
	return jobs.Fail(msg, err)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Start (re)creates and starts the server's container: it removes any
// previous managed container of the server, ensures the network, checks the
// host port, runs the container and waits for it to keep running for the
// settle window.
func (s *Service) Start(ctx context.Context, id int64, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	if err := s.store.SetServerDesiredState(ctx, id, "running"); err != nil {
		return jobs.Fail("could not record the desired state", err)
	}
	return s.start(ctx, sv, log)
}

// Restart is Start with a different log line; the container is always
// recreated so definition changes apply.
func (s *Service) Restart(ctx context.Context, id int64, log io.Writer) error {
	fmt.Fprintln(log, "restart requested")
	return s.Start(ctx, id, log)
}

func (s *Service) start(ctx context.Context, sv store.Server, log io.Writer) error {
	if sv.Port == nil {
		return jobs.Fail("server has no port", nil)
	}
	port := *sv.Port
	env, err := s.openEnv(sv)
	if err != nil {
		return jobs.Fail("could not decrypt the server environment", err)
	}
	fmt.Fprintf(log, "starting server %q (id %d) on port %d (udp+tcp -> %d)\n", sv.Name, sv.ID, port, docker.GamePort)
	if len(env) > 0 {
		fmt.Fprintf(log, "environment variables: %s\n", strings.Join(docker.EnvKeys(env), ", "))
	}

	img, err := s.docker.ManagedImage(ctx, sv.Image)
	switch {
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrNotManaged):
		return jobs.Fail("the server's image is missing or not managed by golem-deploy", err)
	case err != nil:
		return failDocker("could not inspect the server's image", err)
	}
	fmt.Fprintf(log, "image %s verified (managed)\n", shortID(img.ID))

	n, err := s.docker.RemoveServerContainers(ctx, sv.ID, s.cfg.StopTimeout)
	if err != nil {
		return failDocker("could not remove the previous container", err)
	}
	if n > 0 {
		fmt.Fprintf(log, "removed %d previous container(s)\n", n)
	}
	if err := s.docker.EnsureNetwork(ctx); err != nil {
		if errors.Is(err, docker.ErrNotManaged) {
			return jobs.Fail("a Docker network named "+docker.NetworkName+" exists but was not created by golem-deploy", err)
		}
		return failDocker("could not prepare the "+docker.NetworkName+" network", err)
	}
	if err := s.cfg.ProbePort(s.cfg.BindAddress, port); err != nil {
		return jobs.Fail(fmt.Sprintf("port %d is in use by another process on the host", port), err)
	}

	cid, err := s.docker.RunServer(ctx, s.runSpec(sv, env, img.ID))
	switch {
	case errors.Is(err, docker.ErrConflict):
		return jobs.Fail(fmt.Sprintf("a container named %s already exists and is not managed by golem-deploy; rename or remove it", docker.ContainerName(sv.Name)), err)
	case errors.Is(err, docker.ErrPortBusy):
		return jobs.Fail(fmt.Sprintf("port %d is already in use", port), err)
	case err != nil:
		return failDocker("Docker could not create the container", err)
	}
	fmt.Fprintf(log, "container %s created, waiting %s for it to settle\n", shortID(cid), s.cfg.SettleWindow)
	return s.settle(ctx, sv, env, log)
}

// settle waits for the settle window and fails if the container stops or
// restarts meanwhile. The last log lines go to the job log (with env values
// redacted), never into the detail.
func (s *Service) settle(ctx context.Context, sv store.Server, env map[string]string, log io.Writer) error {
	deadline := time.Now().Add(s.cfg.SettleWindow)
	t := time.NewTicker(s.cfg.SettlePoll)
	defer t.Stop()
	for {
		ct, err := s.docker.ServerContainer(ctx, sv.ID)
		if err != nil {
			return failDocker("could not inspect the new container", err)
		}
		if !ct.Running || ct.RestartCount > 0 {
			fmt.Fprintf(log, "container is %s (exit code %d, restarts %d); last output:\n", ct.State, ct.ExitCode, ct.RestartCount)
			var buf bytes.Buffer
			if err := s.docker.ServerLogs(ctx, sv.ID, docker.LogOptions{Tail: 50}, &buf); err != nil {
				fmt.Fprintf(log, "(could not read container logs: %v)\n", err)
			}
			_, _ = log.Write(redact(buf.Bytes(), env))
			if _, err := s.docker.StopServer(ctx, sv.ID, s.cfg.StopTimeout); err != nil {
				fmt.Fprintf(log, "could not stop the failing container: %v\n", err)
			}
			return jobs.Fail(fmt.Sprintf("the container exited right after starting (exit code %d); see the job log", ct.ExitCode), nil)
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintf(log, "server %q is running\n", sv.Name)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// redact replaces env values (of at least 4 bytes) in container output.
func redact(b []byte, env map[string]string) []byte {
	vals := make([]string, 0, len(env))
	for _, v := range env {
		if len(v) >= 4 {
			vals = append(vals, v)
		}
	}
	// Longest first so a value containing another is fully replaced.
	slices.SortFunc(vals, func(a, b string) int { return len(b) - len(a) })
	for _, v := range vals {
		b = bytes.ReplaceAll(b, []byte(v), []byte("[redacted]"))
	}
	return b
}

// Stop gracefully stops the server's container and records desired state
// "stopped". The container is kept so its logs stay readable.
func (s *Service) Stop(ctx context.Context, id int64, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	if err := s.store.SetServerDesiredState(ctx, id, "stopped"); err != nil {
		return jobs.Fail("could not record the desired state", err)
	}
	fmt.Fprintf(log, "stopping server %q (graceful, timeout %s)\n", sv.Name, s.cfg.StopTimeout)
	n, err := s.docker.StopServer(ctx, id, s.cfg.StopTimeout)
	if err != nil {
		return failDocker("could not stop the container", err)
	}
	if n == 0 {
		fmt.Fprintln(log, "no running container")
	} else {
		fmt.Fprintln(log, "stopped")
	}
	return nil
}

// Delete stops and removes the server's containers, then deletes the row
// (releasing its port). Docker being unreachable aborts the deletion so no
// container is left behind without a row.
func (s *Service) Delete(ctx context.Context, id int64, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	fmt.Fprintf(log, "deleting server %q\n", sv.Name)
	n, err := s.docker.RemoveServerContainers(ctx, id, s.cfg.StopTimeout)
	if err != nil {
		return failDocker("could not remove the container", err)
	}
	fmt.Fprintf(log, "removed %d container(s)\n", n)
	if err := s.store.DeleteServer(context.WithoutCancel(ctx), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return jobs.Fail("server not found", ErrNotFound)
		}
		return jobs.Fail("could not delete the server", err)
	}
	s.logger.Info("server deleted", "server_id", id, "name", sv.Name)
	fmt.Fprintln(log, "deleted")
	return nil
}

// Reconcile runs once at startup: every server that should be running but
// is not gets start(id) (which should run Start as a job). Managed
// containers without a server row are logged, never removed.
func (s *Service) Reconcile(ctx context.Context, start func(id int64) error) error {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return err
	}
	cts, err := s.docker.ManagedContainers(ctx)
	if err != nil {
		return err
	}
	byServer := map[int64][]docker.Container{}
	for _, c := range cts {
		byServer[c.ServerID] = append(byServer[c.ServerID], c)
	}
	known := map[int64]bool{}
	for _, sv := range rows {
		known[sv.ID] = true
		if sv.DesiredState != "running" {
			continue
		}
		if c := pickContainer(byServer[sv.ID]); c != nil && c.Running {
			continue
		}
		s.logger.Info("reconcile: starting server", "server_id", sv.ID, "name", sv.Name)
		if err := start(sv.ID); err != nil {
			s.logger.Warn("reconcile: could not start server", "server_id", sv.ID, "err", err)
		}
	}
	for _, c := range cts {
		if !known[c.ServerID] {
			s.logger.Warn("managed container has no server definition; leaving it alone", "container", c.Name, "id", shortID(c.ID), "server_id", c.ServerID)
		}
	}
	return nil
}
