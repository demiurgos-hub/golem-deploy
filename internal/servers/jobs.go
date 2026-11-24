package servers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/demiurgos-hub/golem-deploy/internal/build"
	"github.com/demiurgos-hub/golem-deploy/internal/certs"
	"github.com/demiurgos-hub/golem-deploy/internal/docker"
	"github.com/demiurgos-hub/golem-deploy/internal/gameadmin"
	"github.com/demiurgos-hub/golem-deploy/internal/jobs"
	"github.com/demiurgos-hub/golem-deploy/internal/logsafe"
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

// buildRef is what build b recorded about its image.
func buildRef(b store.Build) docker.BuildRef {
	return docker.BuildRef{ImageID: b.ImageID, BuildID: b.ID, ProjectID: b.ProjectID, Commit: b.Commit, EngineVersion: b.EngineVersion}
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Start (re)creates and starts the server's container: it checks the
// build, the schema of the saved data, the game host and the certificate,
// verifies the build's image, removes the server's previous container,
// ensures the network and the data volume, checks the host port, creates
// the container, records its id and token, starts it and waits for the
// game to report ready (see waitReady). A schema mismatch fails before the
// running container is touched.
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
	if err := s.removePending(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "recover the previous create", err)
	}
	if sv.Port == nil {
		return jobs.Fail("server has no port", nil)
	}
	port := *sv.Port
	if !s.portAllowed(port) {
		return jobs.Fail(fmt.Sprintf("port %d is no longer allowed (outside the configured range %s, browser-unsafe or the panel's own port); "+
			"assign a new port with PATCH /api/v1/servers/%d and start again", port, s.cfg.Ports, sv.ID), nil)
	}
	if sv.BuildID == nil {
		return jobs.Fail(fmt.Sprintf("the server has no build; assign a successful build with PATCH /api/v1/servers/%d (build_id) and start again", sv.ID), nil)
	}
	b, err := s.store.GetBuild(ctx, *sv.BuildID)
	if err != nil {
		return jobs.Fail("could not read the server's build", err)
	}
	if b.Status != store.BuildStatusSuccess || b.ImageID == "" {
		return jobs.Fail(fmt.Sprintf("build %d is not a successful build; assign another build and start again", b.ID), nil)
	}
	volumeGone, err := s.volumeState(ctx, sv)
	if err != nil {
		return err
	}
	if dataFingerprint := sv.DataFingerprint; !volumeGone && schemaMismatch(dataFingerprint, b.SchemaFingerprint) {
		return schemaGuard(sv, b, dataFingerprint)
	}
	if s.cfg.Certs == nil {
		return jobs.Fail("the game certificate is not configured on this daemon", nil)
	}
	p := runtimeParams{certDir: s.cfg.Certs.ServerDir(sv.ID), groupAdd: s.cfg.Certs.GroupAdd()}
	cert, err := s.cfg.Certs.Ensure(ctx, sv.ID, func(ctx context.Context) (string, error) {
		host, err := s.GameHost(ctx)
		if err != nil {
			return "", jobs.Fail("could not read the server settings", err)
		}
		if host == "" {
			return "", jobs.Fail("no game host is configured: set game_host (PATCH /api/v1/settings) or start the daemon with --public-host", nil)
		}
		p.gameHost = host
		p.testOrigins = s.cfg.testOrigins(host)
		return host, nil
	})
	var settingsFailure *jobs.SafeError
	switch {
	case errors.As(err, &settingsFailure):
		return err
	case errors.Is(err, certs.ErrExpired):
		if cert.Mode == certs.ModeACME {
			return jobs.Fail("the automatic game certificate has expired; renew it (POST /api/v1/certificates/acme/renew)", err)
		}
		return jobs.Fail("the custom game certificate has expired; upload a new one (PUT /api/v1/certificates/game)", err)
	case errors.Is(err, certs.ErrNoCertificate):
		if cert.Mode == certs.ModeACME {
			return jobs.Fail("no valid automatic game certificate is installed; renew it (POST /api/v1/certificates/acme/renew)", err)
		}
		return jobs.Fail("no valid custom game certificate is installed; upload one or switch to a self-signed certificate (PUT /api/v1/certificates/game)", err)
	case err != nil:
		s.logger.Error("could not prepare the game certificate", "job_id", jobIDFrom(ctx), "err", err)
		return jobs.Fail("could not prepare the game certificate; see the daemon log", nil)
	}
	p.certMode = cert.Mode
	env, err := s.openEnv(sv)
	if err != nil {
		return jobs.Fail("could not decrypt the server environment", err)
	}
	fmt.Fprintf(log, "starting server %q (id %d) from build %d (commit %s) on port %d (udp+tcp -> %d)\n",
		sv.Name, sv.ID, b.ID, shortID(b.Commit), port, docker.GamePort)
	wtPath, wsPath := transportPaths(&b)
	fmt.Fprintf(log, "public URL %s (WebTransport path %s, WebSocket fallback path %s)\n", publicURL(p.gameHost, port), wtPath, wsPath)
	if len(env) > 0 {
		fmt.Fprintf(log, "environment variables: %s\n", strings.Join(docker.EnvKeys(env), ", "))
	}
	fmt.Fprintf(log, "game certificate: %s, valid until %s, sha-256 %s\n", cert.Mode, cert.NotAfter.Format(time.RFC3339), cert.Fingerprint)
	if !cert.Covers(p.gameHost) {
		fmt.Fprintf(log, "warning: the game certificate does not cover the game host %s; clients will refuse it\n", p.gameHost)
	}

	img, err := s.docker.BuildImage(ctx, buildRef(b))
	switch {
	case errors.Is(err, docker.ErrNotFound):
		return jobs.Fail(fmt.Sprintf("the image of build %d no longer exists; build the project again", b.ID), nil)
	case errors.Is(err, docker.ErrNotManaged), errors.Is(err, docker.ErrNotOurs):
		s.logger.Warn("refusing to run an image without this installation's build labels", "job_id", jobIDFrom(ctx), "server_id", sv.ID,
			"build_id", b.ID, "image", b.ImageID, "err", err)
		return jobs.Fail(fmt.Sprintf("the image recorded for build %d does not carry this installation's labels for that build; refusing to run it", b.ID), nil)
	case err != nil:
		return s.failDocker(ctx, sv, "inspect the server's image", err)
	}
	if len(img.Volumes) > docker.MaxImageVolumes {
		return jobs.Fail(fmt.Sprintf("the server's image declares more than %d volumes", docker.MaxImageVolumes), nil)
	}
	fmt.Fprintf(log, "image %s of build %d verified\n", shortID(img.ID), b.ID)
	if tmpfs := slices.DeleteFunc(slices.Clone(img.Volumes), func(v string) bool { return path.Clean(v) == build.DataDir }); len(tmpfs) > 0 {
		fmt.Fprintf(log, "image volumes %s get size-limited tmpfs mounts\n", strings.Join(tmpfs, ", "))
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
	if err := s.ensureDataVolume(ctx, &sv, volumeGone, log); err != nil {
		return err
	}
	fmt.Fprintf(log, "data volume %s mounted at %s (autosave %s)\n", sv.DataVolume, build.DataDir, autosave(sv.SnapshotIntervalSeconds))
	if err := s.cfg.ProbePort(s.cfg.BindAddress, port); err != nil {
		return jobs.Fail(fmt.Sprintf("port %d is in use by another process on the host", port), nil)
	}

	token, sealedToken, err := s.newAdminToken(sv.ID)
	if err != nil {
		return jobs.Fail("could not create the admin token", err)
	}
	spec := s.runSpec(sv, b, env, img, p, token)
	launch := sv.PendingLaunch
	if launch == "" {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return jobs.Fail("could not prepare a container identity", err)
		}
		launch = hex.EncodeToString(nonce[:])
		if err := s.store.SetServerPendingLaunch(ctx, sv.ID, launch); err != nil {
			return jobs.Fail("could not record the pending container create", err)
		}
	}
	spec.LaunchID = launch
	cid, err := s.docker.CreateServer(ctx, spec)
	if err != nil {
		// Keep the launch identity. Docker may have created the container
		// even though the command did not return its id.
		return s.failDocker(ctx, sv, "create the container", err)
	}
	if err := s.store.SetServerContainer(context.WithoutCancel(ctx), sv.ID, cid, 0, sealedToken); err != nil {
		// The recorded id or the pending launch identity can recover it.
		return jobs.Fail("could not record the new container", err)
	}
	sv.ContainerID = cid
	releasePair, err := s.cfg.Certs.HoldForStart(ctx, sv.ID, p.certMode)
	if err != nil {
		s.logger.Warn("could not verify the game certificate before container start", "job_id", jobIDFrom(ctx), "err", err)
		return jobs.Fail("could not verify the game certificate before container start; see the daemon log and retry", nil)
	}
	defer releasePair()
	if err := s.docker.StartServer(ctx, ref(sv)); err != nil {
		var commandErr *docker.CmdError
		if errors.As(err, &commandErr) {
			commandErr.Detail = docker.RedactValues(docker.RedactValues(commandErr.Detail, env), spec.DaemonEnv)
		}
		s.stopQuietly(ctx, sv)
		return s.failDocker(ctx, sv, "start the container", err)
	}
	fmt.Fprintf(log, "container %s started, waiting up to %s for the game to report ready\n", shortID(cid), s.cfg.StartTimeout)
	return s.waitReady(ctx, sv, b, env, token, log)
}

// removePending removes only a container matching the identity recorded
// before an interrupted create. An empty result leaves that identity
// intact so a late create can still be found on the next attempt.
func (s *Service) removePending(ctx context.Context, sv store.Server, log io.Writer) error {
	if sv.PendingLaunch == "" {
		return nil
	}
	ct, err := s.docker.PendingServerContainer(ctx, sv.ID, sv.Name, sv.PendingLaunch)
	if errors.Is(err, docker.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.docker.RemoveServerContainer(ctx, docker.ServerRef{ServerID: sv.ID, ContainerID: ct.ID}, s.cfg.StopTimeout); err != nil {
		return err
	}
	fmt.Fprintf(log, "removed the container of an interrupted create %s\n", shortID(ct.ID))
	return nil
}

// waitReady polls the container and the game's admin endpoint until the
// game reports ready, the container stops or StartTimeout passes. The
// container runs without a restart policy until then, so a crash is not
// retried and keeps its real exit code; a failed start leaves the container
// stopped so its logs stay readable, and copies its last lines to the job
// log (sanitised, prefixed, with env values redacted), never into the
// detail. On ready, the game's schema fingerprint is recorded on the build
// and RestartPolicy is applied.
func (s *Service) waitReady(ctx context.Context, sv store.Server, b store.Build, env map[string]string, token string, log io.Writer) error {
	redactEnv := map[string]string{}
	for k, v := range env {
		redactEnv[k] = v
	}
	redactEnv[EnvAdminToken] = token
	probeCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	t := time.NewTicker(s.cfg.ReadyPoll)
	defer t.Stop()
	answered, oddAnswer := false, false
	logsHint := fmt.Sprintf("see the server log (GET /api/v1/servers/%d/logs) and the job log", sv.ID)
	for probeCtx.Err() == nil {
		ct, err := s.docker.ServerContainer(probeCtx, ref(sv))
		if probeCtx.Err() != nil {
			break
		}
		if err != nil {
			s.stopQuietly(ctx, sv)
			return s.failDocker(ctx, sv, "inspect the new container", err)
		}
		if !adminRunning(ct) {
			fmt.Fprintf(log, "container is %s (exit code %d); last output:\n", ct.State, ct.ExitCode)
			s.writeExcerpt(ctx, sv, redactEnv, log)
			s.stopQuietly(ctx, sv)
			return jobs.Fail(fmt.Sprintf("the container exited before the game became ready (exit code %d); %s", ct.ExitCode, logsHint), nil)
		}
		if port := ct.LoopbackPort(AdminContainerPort); port != 0 {
			client, err := gameadmin.NewSession(port, token)
			if err != nil {
				s.stopQuietly(ctx, sv)
				return jobs.Fail("could not reach the game's admin endpoint", err)
			}
			r, err := client.Ready(probeCtx)
			switch {
			case err == nil:
				answered = true
				if r.Ready && probeCtx.Err() == nil {
					defer client.Close()
					return s.ready(ctx, probeCtx, sv, b, client, log)
				}
			case errors.Is(err, gameadmin.ErrUnauthorized), errors.Is(err, gameadmin.ErrBadResponse):
				answered = true
				if !oddAnswer {
					oddAnswer = true
					fmt.Fprintln(log, "the game's admin endpoint answered outside the Golem Engine admin contract; still waiting")
					s.logger.Warn("unexpected admin answer during start", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
				}
			}
			client.Close()
		}
		select {
		case <-probeCtx.Done():
		case <-t.C:
		}
	}
	if ctx.Err() != nil {
		s.stopQuietly(ctx, sv)
		return ctx.Err()
	}
	fmt.Fprintln(log, "the game did not report ready in time; last output:")
	s.writeExcerpt(ctx, sv, redactEnv, log)
	s.stopQuietly(ctx, sv)
	if !answered {
		return jobs.Fail(fmt.Sprintf("the game did not become ready within %s and its admin endpoint never answered: the game must use "+
			"the Golem Engine deploy contract (golem.ServerConfigFromEnv and the generated Runtime.Host); %s", s.cfg.StartTimeout, logsHint), nil)
	}
	return jobs.Fail(fmt.Sprintf("the game did not report ready within %s; %s", s.cfg.StartTimeout, logsHint), nil)
}

// ready finishes a start whose game reported ready.
func (s *Service) ready(ctx, probeCtx context.Context, sv store.Server, b store.Build, client *gameadmin.Client, log io.Writer) error {
	if info, err := client.Info(probeCtx); err != nil {
		fmt.Fprintln(log, "warning: could not read the game's /info")
		s.logger.Warn("admin /info failed after ready", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
	} else {
		fmt.Fprintf(log, "game ready: engine %s, transport %s, reported schema %s\n", orDash(info.EngineVersion), orDash(info.Transport), orDash(info.SchemaFingerprint))
		if info.SchemaFingerprint != "" && info.SchemaFingerprint != b.SchemaFingerprint {
			if err := s.store.SetBuildFingerprint(context.WithoutCancel(ctx), b.ID, info.SchemaFingerprint); err != nil {
				s.logger.Warn("could not record the schema fingerprint", "build_id", b.ID, "err", err)
			}
		}
		if info.SchemaFingerprint != "" && info.SchemaFingerprint != sv.DataFingerprint && sv.DataVolume != "" {
			// The game loaded (or started) the saved world with this schema.
			if err := s.store.SetServerDataFingerprint(context.WithoutCancel(ctx), sv.ID, sv.DataVolume, info.SchemaFingerprint); err != nil {
				s.logger.Warn("could not record the saved data's schema fingerprint", "server_id", sv.ID, "err", err)
			}
		}
		if b.EngineVersion != "" && info.EngineVersion != "" && b.EngineVersion != info.EngineVersion {
			fmt.Fprintf(log, "note: the game reports engine %s, the build recorded %s\n", info.EngineVersion, b.EngineVersion)
		}
	}
	if err := s.docker.ApplyRestartPolicy(ctx, ref(sv)); err != nil {
		s.stopQuietly(ctx, sv)
		return s.failDocker(ctx, sv, "apply the restart policy", err)
	}
	if err := s.store.CompleteServerStart(ctx, sv.ID, sv.ContainerID); err != nil {
		s.stopQuietly(ctx, sv)
		return jobs.Fail("could not record the completed start", err)
	}
	fmt.Fprintf(log, "server %q is running (restart policy %s)\n", sv.Name, docker.RestartPolicy)
	return nil
}

// autosave describes the autosave period for job logs.
func autosave(seconds int) string {
	if seconds <= 0 {
		return "off"
	}
	return (time.Duration(seconds) * time.Second).String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stopQuietly stops a container whose start failed (it is kept for its
// logs), also when ctx is cancelled.
func (s *Service) stopQuietly(ctx context.Context, sv store.Server) {
	if _, err := s.docker.StopServer(context.WithoutCancel(ctx), ref(sv), s.cfg.StopTimeout); err != nil {
		s.logger.Warn("could not stop the container of a failed start", "job_id", jobIDFrom(ctx), "server_id", sv.ID, "err", err)
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
	san := logsafe.NewWriter(log, JobLogPrefix)
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
	if err := s.removePending(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "recover the previous create", err)
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

// Delete stops and removes the server's container, then its data volume,
// then deletes the row (releasing its port); its backups stay. Docker being
// unreachable aborts the deletion so no container or volume is left behind
// without a row.
func (s *Service) Delete(ctx context.Context, id int64, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	fmt.Fprintf(log, "deleting server %q\n", sv.Name)
	if err := s.removePending(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "recover the previous create", err)
	}
	if err := s.removeRecorded(ctx, sv, log); err != nil {
		return s.failDocker(ctx, sv, "remove the container", err)
	}
	if sv.DataVolume != "" {
		err := s.docker.RemoveDataVolume(ctx, sv.ID, sv.DataVolume)
		switch {
		case errors.Is(err, docker.ErrNotManaged), errors.Is(err, docker.ErrNotOurs):
			// The recorded name now belongs to something else: leave it.
			s.logger.Warn("not removing a volume that is not this server's data volume", "job_id", jobIDFrom(ctx), "server_id", id, "volume", sv.DataVolume)
		case errors.Is(err, docker.ErrInUse):
			return jobs.Fail("the server's data volume "+sv.DataVolume+" is still used by a container; stop that container and delete the server again", err)
		case err != nil:
			return s.failDocker(ctx, sv, "remove the data volume", err)
		default:
			fmt.Fprintf(log, "removed the data volume %s (backups of the server are kept)\n", sv.DataVolume)
		}
	}
	cleanupCtx := context.WithoutCancel(ctx)
	deleteRow := func() error { return s.store.DeleteServer(cleanupCtx, id) }
	var deleteErr error
	if s.cfg.Certs != nil {
		deleteErr = s.cfg.Certs.RemoveServer(cleanupCtx, id, deleteRow)
	} else {
		deleteErr = deleteRow()
	}
	if deleteErr != nil {
		if errors.Is(deleteErr, store.ErrNotFound) {
			return jobs.Fail("server not found", ErrNotFound)
		}
		s.logger.Warn("could not delete server data", "job_id", jobIDFrom(ctx), "server_id", id, "err", deleteErr)
		return jobs.Fail("could not delete the server", deleteErr)
	}
	s.logger.Info("server deleted", "server_id", id, "name", sv.Name)
	fmt.Fprintln(log, "deleted")
	return nil
}

// Reconcile queues verification and recovery under the normal server job
// lock. The job re-reads the row in case a user changed it in the meantime.
func (s *Service) Reconcile(ctx context.Context, start func(id int64) error) error {
	rows, err := s.store.ListServers(ctx)
	if err != nil {
		return err
	}
	cts, err := s.docker.ManagedContainers(ctx)
	if err != nil {
		return err
	}
	s.containers(rows, cts) // report unrecorded foreign containers
	for _, sv := range rows {
		if sv.DesiredState != "running" && sv.PendingLaunch == "" {
			continue
		}
		if err := start(sv.ID); err != nil {
			s.logger.Warn("reconcile: could not queue server recovery", "server_id", sv.ID, "err", err)
		}
	}
	return nil
}

// Recover re-verifies a completed start and redoes an incomplete or
// unhealthy start through the same health-gated path used by Start.
func (s *Service) Recover(ctx context.Context, id int64, log io.Writer) error {
	sv, err := s.load(ctx, id)
	if err != nil {
		return jobs.Fail("server not found", err)
	}
	if sv.DesiredState != "running" {
		if err := s.removePending(ctx, sv, log); err != nil {
			return s.failDocker(ctx, sv, "recover the previous create", err)
		}
		return nil
	}
	if sv.StartComplete && sv.BuildID != nil {
		adopted, err := s.verifyRunning(ctx, sv)
		if err != nil {
			return err
		}
		if adopted {
			fmt.Fprintln(log, "verified the running container and game readiness")
			return nil
		}
	}
	fmt.Fprintln(log, "recreating the container after startup verification")
	if err := s.start(ctx, sv, log); err != nil {
		if sv.ContainerID != "" && ctx.Err() == nil {
			s.stopQuietly(ctx, sv)
		}
		return err
	}
	return nil
}

func (s *Service) verifyRunning(ctx context.Context, sv store.Server) (bool, error) {
	b, err := s.store.GetBuild(ctx, *sv.BuildID)
	if err != nil {
		return false, jobs.Fail("could not read the server's build", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	ct, err := s.container(probeCtx, sv)
	if err != nil {
		return false, s.failDocker(ctx, sv, "inspect the recorded container", err)
	}
	if ct == nil || !adminRunning(*ct) || b.Status != store.BuildStatusSuccess || b.ImageID == "" || ct.ImageID != b.ImageID {
		return false, nil
	}
	port := ct.LoopbackPort(AdminContainerPort)
	if port == 0 {
		return false, nil
	}
	token, err := s.openAdminToken(sv)
	if err != nil {
		return false, nil
	}
	client, err := gameadmin.New(port, token)
	if err != nil {
		return false, nil
	}
	ready, err := client.Ready(probeCtx)
	return err == nil && ready.Ready, nil
}
