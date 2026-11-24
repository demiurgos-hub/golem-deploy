package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/store"
)

// The functions below are job bodies: they write progress to log and return
// jobs.Fail errors whose message is safe to show as the job detail. Docker's
// own output (stderr) never reaches job logs or details: it goes to the
// daemon log together with the job id. Callers run the bodies through the
// job runner so jobs for one server are serialised.

type jobIDKey struct{}

// WithJobID returns ctx carrying the id of the job a body runs in, so
// failures can be correlated with the daemon log.
func WithJobID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, jobIDKey{}, id)
}

func jobIDFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(jobIDKey{}).(int64)
	return id
}

// failDocker logs a failed docker call (with the CLI's stderr) to the
// daemon log and returns a job failure with a short, fixed message for its
// class. The failure wraps only the sentinel kind, so callers can still
// test it with errors.Is and nothing from Docker reaches the job log.
func (s *Service) failDocker(ctx context.Context, sv store.Server, action string, err error) error {
	jobID := jobIDFrom(ctx)
	s.logger.Warn("docker command failed", "job_id", jobID, "server_id", sv.ID, "action", action, "err", err)
	ref := ""
	if jobID != 0 {
		ref = fmt.Sprintf(" (details in the daemon log under job_id=%d)", jobID)
	}
	switch {
	case errors.Is(err, docker.ErrUnavailable):
		return jobs.Fail("Docker is unreachable", docker.ErrUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		return jobs.Fail("Docker did not answer in time while trying to "+action, docker.ErrUnavailable)
	case errors.Is(err, docker.ErrImageMissing):
		return jobs.Fail("the server's image no longer exists", docker.ErrImageMissing)
	case errors.Is(err, docker.ErrConflict):
		name := docker.ContainerName(sv.Name)
		return jobs.Fail(fmt.Sprintf("a container named %s already exists and this installation does not manage it; "+
			"check what it is, then remove it (docker rm -f %s) and start again", name, name), docker.ErrConflict)
	case errors.Is(err, docker.ErrPortBusy):
		port := 0
		if sv.Port != nil {
			port = *sv.Port
		}
		return jobs.Fail(fmt.Sprintf("port %d is already in use", port), docker.ErrPortBusy)
	}
	return jobs.Fail("Docker could not "+action+ref, nil)
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Start (re)creates and starts the server's container: it removes the
// server's previous container, ensures the network, checks the host port,
// runs the container, records its id and waits for it to keep running for
// the settle window.
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

// removeRecorded stops and removes the server's recorded container and
// forgets it. A recorded id that now names a container that is not ours is
// forgotten without touching that container.
func (s *Service) removeRecorded(ctx context.Context, sv store.Server, log io.Writer) error {
	if sv.ContainerID == "" {
		return nil
	}
	removed, err := s.docker.RemoveServerContainer(ctx, ref(sv), s.cfg.StopTimeout)
	switch {
	case errors.Is(err, docker.ErrNotOurs):
		s.logForeignRef(ref(sv))
	case err != nil:
		return err
	case removed:
		fmt.Fprintf(log, "removed the previous container %s\n", shortID(sv.ContainerID))
	}
	return s.store.ClearServerContainer(context.WithoutCancel(ctx), sv.ID, sv.ContainerID)
}

func (s *Service) start(ctx context.Context, sv store.Server, log io.Writer) error {
	if sv.Port == nil {
		return jobs.Fail("server has no port", nil)
	}
	port := *sv.Port
	if !s.portAllowed(port) {
		return jobs.Fail(fmt.Sprintf("port %d is no longer allowed (outside the configured range %s, browser-unsafe or the panel's own port); "+
			"assign a new port with PATCH /api/v1/servers/%d and start again", port, s.cfg.Ports, sv.ID), nil)
	}
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
		return jobs.Fail("the server's image is missing or not managed by golem-deploy", nil)
	case err != nil:
		return s.failDocker(ctx, sv, "inspect the server's image", err)
	}
	if len(img.Volumes) > docker.MaxImageVolumes {
		return jobs.Fail(fmt.Sprintf("the server's image declares more than %d volumes", docker.MaxImageVolumes), nil)
	}
	fmt.Fprintf(log, "image %s verified (managed)\n", shortID(img.ID))
	if len(img.Volumes) > 0 {
		fmt.Fprintf(log, "image volumes %s get size-limited tmpfs mounts\n", strings.Join(img.Volumes, ", "))
	}

	if err := s.removeRecorded(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "remove the previous container", err)
	}
	isolated, err := s.docker.EnsureNetwork(ctx)
	switch {
	case errors.Is(err, docker.ErrNotManaged):
		return jobs.Fail("a Docker network named "+docker.NetworkName+" exists but was not created by golem-deploy", nil)
	case errors.Is(err, docker.ErrNotOurs):
		return jobs.Fail("the Docker network "+docker.NetworkName+" belongs to another golem-deploy installation (or to an older version and still has containers attached); "+
			"remove it (docker network rm "+docker.NetworkName+") once nothing uses it", nil)
	case err != nil:
		return s.failDocker(ctx, sv, "prepare the "+docker.NetworkName+" network", err)
	}
	if !isolated {
		fmt.Fprintln(log, "warning: the "+docker.NetworkName+" network still allows traffic between servers because containers are attached; "+
			"isolation applies once every server is stopped and one is started again")
	}
	if err := s.cfg.ProbePort(s.cfg.BindAddress, port); err != nil {
		return jobs.Fail(fmt.Sprintf("port %d is in use by another process on the host", port), nil)
	}

	cid, err := s.docker.RunServer(ctx, s.runSpec(sv, env, img))
	if err != nil {
		return s.failDocker(ctx, sv, "create the container", err)
	}
	if err := s.store.SetServerContainer(context.WithoutCancel(ctx), sv.ID, cid); err != nil {
		// Without a record the container could never be reached again.
		_, _ = s.docker.RemoveServerContainer(context.WithoutCancel(ctx), docker.ServerRef{ServerID: sv.ID, ContainerID: cid}, s.cfg.StopTimeout)
		return jobs.Fail("could not record the new container", err)
	}
	sv.ContainerID = cid
	fmt.Fprintf(log, "container %s created, waiting %s for it to settle\n", shortID(cid), s.cfg.SettleWindow)
	return s.settle(ctx, sv, env, log)
}

// settle waits for the settle window and fails if the container stops
// meanwhile. The container runs without a restart policy until then, so its
// exit code is the real one; once settled, RestartPolicy is applied. The
// last log lines of a failed start go to the job log (sanitised, prefixed
// and with env values redacted), never into the detail.
func (s *Service) settle(ctx context.Context, sv store.Server, env map[string]string, log io.Writer) error {
	deadline := time.Now().Add(s.cfg.SettleWindow)
	t := time.NewTicker(s.cfg.SettlePoll)
	defer t.Stop()
	for {
		ct, err := s.docker.ServerContainer(ctx, ref(sv))
		if err != nil {
			return s.failDocker(ctx, sv, "inspect the new container", err)
		}
		if !ct.Running {
			fmt.Fprintf(log, "container is %s (exit code %d); last output:\n", ct.State, ct.ExitCode)
			s.writeExcerpt(ctx, sv, env, log)
			if _, err := s.docker.StopServer(ctx, ref(sv), s.cfg.StopTimeout); err != nil {
				fmt.Fprintln(log, "could not stop the failing container")
			}
			return jobs.Fail(fmt.Sprintf("the container exited right after starting (exit code %d); see the job log", ct.ExitCode), nil)
		}
		if !time.Now().Before(deadline) {
			if err := s.docker.ApplyRestartPolicy(ctx, ref(sv)); err != nil {
				_, _ = s.docker.StopServer(context.WithoutCancel(ctx), ref(sv), s.cfg.StopTimeout)
				return s.failDocker(ctx, sv, "apply the restart policy", err)
			}
			fmt.Fprintf(log, "server %q is running (restart policy %s)\n", sv.Name, docker.RestartPolicy)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// writeExcerpt copies the last lines of the container's output to the job
// log: env values redacted (best effort), then sanitised and prefixed with
// JobLogPrefix so nothing the game prints can pose as a daemon message.
func (s *Service) writeExcerpt(ctx context.Context, sv store.Server, env map[string]string, log io.Writer) {
	out, err := s.docker.ServerLogTail(ctx, ref(sv), 50)
	if err != nil {
		s.logger.Warn("could not read container logs for the job log", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
		fmt.Fprintln(log, "(could not read the container logs)")
		return
	}
	san := newSanitizer(log, JobLogPrefix)
	_, _ = io.Copy(san, bytes.NewReader(redact(out, env)))
	_ = san.Close()
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
	stopped, err := s.docker.StopServer(ctx, ref(sv), s.cfg.StopTimeout)
	if errors.Is(err, docker.ErrNotOurs) {
		s.logForeignRef(ref(sv))
		stopped, err = false, nil
	}
	if err != nil {
		return s.failDocker(ctx, sv, "stop the container", err)
	}
	if !stopped {
		fmt.Fprintln(log, "no running container")
	} else {
		fmt.Fprintln(log, "stopped")
	}
	return nil
}

// Delete stops and removes the server's container, then deletes the row
// (releasing its port). Docker being unreachable aborts the deletion so no
// container is left behind without a row.
func (s *Service) Delete(ctx context.Context, id int64, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	fmt.Fprintf(log, "deleting server %q\n", sv.Name)
	if err := s.removeRecorded(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "remove the container", err)
	}
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
// whose recorded container is not running gets start(id) (which should run
// Start as a job). Containers with our labels that are not recorded for
// any server are logged, never touched.
func (s *Service) Reconcile(ctx context.Context, start func(id int64) error) error {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return err
	}
	cts, err := s.docker.ManagedContainers(ctx)
	if err != nil {
		return err
	}
	byServer := s.containers(rows, cts)
	for _, sv := range rows {
		if sv.DesiredState != "running" {
			continue
		}
		if c := byServer[sv.ID]; c != nil && c.Running {
			continue
		}
		s.logger.Info("reconcile: starting server", "server_id", sv.ID, "name", sv.Name)
		if err := start(sv.ID); err != nil {
			s.logger.Warn("reconcile: could not start server", "server_id", sv.ID, "err", err)
		}
	}
	return nil
}
