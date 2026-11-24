import { useEffect, useMemo, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { ArrowLeft, ExternalLink, Pencil, Rocket, RotateCw, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { describeError, isApiError, sentence } from "@/api/errors";
import { isJobActive, useAccount, useProjects, useServer, useServerRuntime, useServerStats, useSubjectJobs, useSuccessfulBuilds } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Backup, Job, Server, UpdateServerRequest } from "@/api/types.gen";
import {
  BackupPickerDialog,
  DeleteServerDialog,
  ResetDataDialog,
  RestoreDialog,
  isLastBackupRefusal,
  schemaMatch,
  serverBuildFingerprint,
  useBuildFingerprints,
} from "@/components/backups";
import { buildName } from "@/components/builds";
import { CertificateDetails } from "@/components/certificate";
import { EnvEditor } from "@/components/env-editor";
import { InlineJob, JobsPanel, useShownJob } from "@/components/jobs";
import { OriginsEditor, hasOriginErrors, noOriginErrors, originErrorsFrom, validateOrigins, type OriginErrors } from "@/components/origins-editor";
import { ServerActions } from "@/components/server-actions";
import { SchemaMismatchNotice, ServerDataSection } from "@/components/server-data";
import { ServerStateBadge, isContainerActive, portLocked, serverActionsBlocked } from "@/components/status-badges";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { LogViewer } from "@/components/ui/log-viewer";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { useLogStream } from "@/hooks/use-log-stream";
import { computeEnvDiff, emptyDraft, envErrorsFrom, mergeEnvErrors, noEnvErrors, type EnvDraft, type EnvErrors } from "@/lib/env-diff";
import { formatBytes, formatDateTime, formatDuration, formatPercent, formatRelative, shortCommit, shortFingerprint } from "@/lib/format";
import { safeHttpUrl } from "@/lib/url";
import { ConnectionTest } from "@/components/connection-test";
import { validatePort } from "@/lib/validators";
import { NotFoundPage } from "@/pages/not-found";
import { buildOptionLabel, buildsByProject } from "@/pages/servers";

export function ServerDetailPage() {
  const { id: rawId } = useParams({ strict: false });
  const id = Number(rawId);
  if (!Number.isSafeInteger(id) || id <= 0) return <NotFoundPage />;
  return <ServerDetail key={id} id={id} />;
}

function BackLink() {
  return (
    <Link to="/servers" className="inline-flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
      <ArrowLeft className="size-4" />
      Servers
    </Link>
  );
}

const START_KINDS = new Set(["server.start", "server.restart"]);

/** The newest start or restart job when it failed and nothing newer replaced it: the reason the server is not running. */
export function failedStart(jobs: readonly Job[] | undefined): Job | null {
  const latest = jobs?.find((j) => START_KINDS.has(j.kind));
  if (!latest || latest.status !== "error") return null;
  // A newer stop (or any active job) supersedes the failure.
  const newer = jobs?.slice(0, jobs.indexOf(latest)) ?? [];
  return newer.some((j) => j.kind === "server.stop" || isJobActive(j)) ? null : latest;
}

function ServerDetail({ id }: { id: number }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const server = useServer(id);
  const jobs = useSubjectJobs("server", id);
  const projects = useProjects();
  const account = useAccount();
  const fingerprints = useBuildFingerprints();
  const isAdmin = account.data?.role === "admin";
  const [dialog, setDialog] = useState<"edit" | "delete" | "deploy" | "pick-backup" | "reset" | null>(null);
  const [restoring, setRestoring] = useState<Backup | null>(null);
  // The server.delete job of a deletion that did not answer 204 (failed, or still running after the wait).
  const [deleteJob, setDeleteJob] = useState<Job | null>(null);
  const activeJob = jobs.data?.items.find(isJobActive) ?? null;
  const shown = useShownJob(activeJob);
  const gone = server.isError && isApiError(server.error) && server.error.code === "not_found";
  const deletedLate = gone && deleteJob !== null;
  const serverName = server.data?.name;

  // A deletion that outlasted the request finished later: the server is gone now.
  useEffect(() => {
    if (!deletedLate) return;
    toast.success(serverName ? `Server ${serverName} deleted.` : "Server deleted.");
    void queryClient.invalidateQueries({ queryKey: qk.servers.list });
    void navigate({ to: "/servers" });
  }, [deletedLate, serverName, toast, queryClient, navigate]);

  async function remove(target: Server, skipLastBackup: boolean) {
    try {
      await api.servers.remove(target.id, { skipLastBackup });
    } catch (err) {
      // A refused last backup keeps the dialog open with the daemon's message and the way out.
      const refused = isLastBackupRefusal(err);
      if (!refused) {
        setDialog(null);
        toast.error(err, `Could not delete ${target.name}`);
      }
      // DELETE runs a server.delete job; show it here so its outcome and log are visible.
      if (!(isApiError(err) && err.code === "job_in_progress")) {
        try {
          const list = await api.jobs.list({ subject_type: "server", subject_id: String(target.id), limit: 5 });
          const job = list.items.find((j) => j.kind === "server.delete");
          if (job) {
            queryClient.setQueryData(qk.jobs.detail(job.id), job);
            setDeleteJob(job);
            shown.show(job);
          }
        } catch {
          // The toast already says what failed; the Jobs section lists the job too.
        }
      }
      void queryClient.invalidateQueries({ queryKey: qk.servers.detail(target.id) });
      void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("server", target.id) });
      // The last backup may have been stored before the deletion stopped.
      void queryClient.invalidateQueries({ queryKey: qk.backups.all });
      if (refused) throw err;
      return;
    }
    const lastBackup = target.data.volume !== "" && !skipLastBackup;
    toast.success(
      `Server ${target.name} deleted: container and saved data removed, port ${target.port} freed. ${lastBackup ? "A last backup of its saved world was stored; its" : "Its"} backups are kept.`,
    );
    queryClient.removeQueries({ queryKey: qk.servers.detail(target.id) });
    void queryClient.invalidateQueries({ queryKey: qk.servers.list });
    void queryClient.invalidateQueries({ queryKey: qk.builds.all });
    void queryClient.invalidateQueries({ queryKey: qk.volumes });
    void queryClient.invalidateQueries({ queryKey: qk.backups.all });
    void queryClient.invalidateQueries({ queryKey: qk.status });
    void navigate({ to: "/servers" });
  }

  if (server.isPending) return <LoadingState label="Loading server" />;
  if (server.isError) {
    if (deletedLate) return <LoadingState label="Server deleted" />;
    if (gone) return <NotFoundPage />;
    return (
      <>
        <PageHeader title="Server" back={<BackLink />} />
        <ErrorState error={server.error} title="Could not load the server" onRetry={() => void server.refetch()} />
      </>
    );
  }

  const s = server.data;
  const currentJob = shown.job;
  const blocked = serverActionsBlocked(s);
  const project = s.project_id !== null ? projects.data?.items.find((p) => p.id === s.project_id) : undefined;
  const failed = failedStart(jobs.data?.items);
  const gameUrl = s.public_url ? safeHttpUrl(s.public_url) : null;
  const buildFp = serverBuildFingerprint(s, fingerprints);

  return (
    <>
      <PageHeader
        back={<BackLink />}
        title={
          <span className="inline-flex flex-wrap items-center gap-2">
            {s.name}
            <ServerStateBadge server={s} />
          </span>
        }
        description={
          gameUrl ? (
            <a href={gameUrl} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 font-mono text-xs hover:underline">
              {gameUrl}
              <ExternalLink aria-hidden className="size-3" />
              <span className="sr-only">(opens in a new tab)</span>
            </a>
          ) : undefined
        }
        actions={
          <>
            <ServerActions server={s} size="md" busy={activeJob !== null} onJob={shown.show} />
            <Button variant="outline" onClick={() => setDialog("edit")}>
              <Pencil />
              Change port
            </Button>
            <Button
              variant="outline"
              onClick={() => setDialog("delete")}
              disabled={blocked !== null || activeJob !== null}
              title={blocked ?? (activeJob !== null ? "Wait for the running job to finish." : undefined)}
            >
              <Trash2 />
              Delete
            </Button>
          </>
        }
      />
      <div className="flex flex-col gap-6">
        {s.restart_required && (
          <Notice tone="warning" title="Restart required">
            The running container was created from an older definition: the build, port, environment, allowed origins, the game
            host, the certificate mode or the daemon&apos;s run settings changed since it started. Changes never restart a server on
            their own: restart it to apply them (connected players are disconnected).
          </Notice>
        )}
        {s.build_id === null && (
          <Notice tone="warning" title="No build assigned">
            This server was created before servers ran builds, so it cannot start. Choose a successful build in the Build section
            below.
          </Notice>
        )}
        {s.state === "unknown" && (
          <Notice tone="warning" title="State unknown">
            The server has a container, but Docker could not be queried, so its actual state is not known. Start, stop,
            restart, delete and port changes are unavailable until the daemon reaches Docker again; this page checks every
            few seconds.
          </Notice>
        )}
        {s.data.schema_mismatch && (
          <SchemaMismatchNotice
            server={s}
            buildFingerprint={buildFp}
            isAdmin={isAdmin}
            busy={activeJob !== null}
            onDeploy={() => setDialog("deploy")}
            onPickBackup={() => setDialog("pick-backup")}
            onReset={() => setDialog("reset")}
          />
        )}
        {failed && failed.id !== currentJob?.id && (
          <Notice
            tone="danger"
            title={`The last ${failed.kind === "server.restart" ? "restart" : "start"} failed`}
            action={
              <div className="flex flex-wrap gap-2">
                <Button size="sm" variant="outline" onClick={() => shown.show(failed)}>
                  Show job log
                </Button>
                <a href="#logs" className="inline-flex h-8 items-center rounded-md border border-input px-2.5 text-xs font-medium hover:bg-accent">
                  Server log
                </a>
              </div>
            }
          >
            <span className="whitespace-pre-wrap break-words">{failed.detail || "The job failed without a message; its log has the details."}</span>
            <span className="mt-1 block text-xs">
              {formatRelative(failed.finished_at ?? failed.created_at)}, job {failed.id}.
            </span>
          </Notice>
        )}
        {currentJob && <InlineJob key={currentJob.id} job={currentJob} onDismiss={shown.dismiss} />}
        <div className="grid gap-6 lg:grid-cols-2">
          <Section title="Details">
            <DetailList
              items={[
                {
                  term: "Game URL",
                  value: gameUrl ? (
                    <span className="flex flex-col gap-1">
                      <span className="inline-flex flex-wrap items-center gap-2">
                        <a href={gameUrl} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 break-all font-mono text-xs hover:underline">
                          {gameUrl}
                          <ExternalLink aria-hidden className="size-3 shrink-0" />
                          <span className="sr-only">(opens in a new tab)</span>
                        </a>
                        <CopyButton value={gameUrl} size="icon" variant="ghost" label="Copy game URL" />
                      </span>
                      <span className="text-xs text-muted-foreground">
                        Port {s.port} is published over UDP (WebTransport) and TCP (HTTPS and the WebSocket fallback).
                      </span>
                    </span>
                  ) : (
                    <span className="text-muted-foreground">
                      No game host is configured, so the server has no address and cannot start. An administrator sets it under
                      Settings.
                    </span>
                  ),
                },
                {
                  term: "Project",
                  value:
                    s.project_id === null ? (
                      <span className="text-muted-foreground">None</span>
                    ) : (
                      <Link to="/projects/$id" params={{ id: String(s.project_id) }} className="hover:underline">
                        {project?.name ?? `project ${s.project_id}`}
                      </Link>
                    ),
                },
                {
                  term: "Build",
                  value:
                    s.build_id === null ? (
                      <span className="text-muted-foreground">None</span>
                    ) : (
                      <span className="flex flex-col gap-0.5">
                        <Link to="/builds/$id" params={{ id: String(s.build_id) }} className="hover:underline">
                          Build {buildName({ id: s.build_id, commit: s.commit })}
                        </Link>
                        {s.engine_version && <span className="text-xs text-muted-foreground">Golem Engine {s.engine_version}</span>}
                      </span>
                    ),
                },
                { term: "Desired state", value: s.desired_state },
                {
                  term: "Started",
                  value: s.started_at ? <span title={formatDateTime(s.started_at)}>{formatRelative(s.started_at)}</span> : "-",
                },
                ...(s.exit_code !== null ? [{ term: "Exit code", value: String(s.exit_code) }] : []),
                {
                  term: "Container",
                  value: s.container_id ? <code className="font-mono text-xs">{s.container_id.slice(0, 12)}</code> : "-",
                },
                { term: "Created", value: formatDateTime(s.created_at) },
              ]}
            />
          </Section>
          <RuntimeSection server={s} />
        </div>
        <ConnectionTest serverId={s.id} />
        <BuildSection server={s} onDeploy={() => setDialog("deploy")} />
        <ServerDataSection
          server={s}
          buildFingerprint={buildFp}
          busy={activeJob !== null}
          isAdmin={isAdmin}
          onJob={shown.show}
          onRestore={setRestoring}
          onPickBackup={() => setDialog("pick-backup")}
          onReset={() => setDialog("reset")}
        />
        <div className="grid gap-6 lg:grid-cols-2">
          <OriginsSection server={s} />
          <CertificateSection server={s} />
        </div>
        <StatsSection server={s} />
        <EnvSection server={s} />
        <LogsSection server={s} />
        <Section title="Jobs" description="Recent starts, stops, restarts, backups, restores, data resets and deletions.">
          <JobsPanel subjectType="server" subjectId={id} />
        </Section>
      </div>

      {dialog === "edit" && <EditServerDialog server={s} busy={activeJob !== null} onClose={() => setDialog(null)} />}
      {dialog === "deploy" && <DeployDialog server={s} busy={activeJob !== null} onJob={shown.show} onClose={() => setDialog(null)} />}
      {dialog === "pick-backup" && (
        <BackupPickerDialog
          server={s}
          onClose={() => setDialog(null)}
          onPick={(b) => {
            setDialog(null);
            setRestoring(b);
          }}
        />
      )}
      {restoring && <RestoreDialog backup={restoring} target={s} onClose={() => setRestoring(null)} onJob={(job) => shown.show(job)} />}
      {dialog === "reset" && <ResetDataDialog server={s} onClose={() => setDialog(null)} onJob={shown.show} />}
      {dialog === "delete" && (
        <DeleteServerDialog server={s} isAdmin={isAdmin} onClose={() => setDialog(null)} onDelete={(skip) => remove(s, skip)} />
      )}
    </>
  );
}

function Metric({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="flex flex-col rounded-md border border-border px-3 py-2" title={hint}>
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="text-lg font-semibold tabular-nums">{value}</span>
    </div>
  );
}

/**
 * What the running game reports through its admin endpoint. 409 (not
 * running) and 503 (endpoint silent or Docker unreachable) are expected and
 * shown as text; the server poll catches up with the state.
 */
function RuntimeSection({ server }: { server: Server }) {
  const runtime = useServerRuntime(server);
  const running = server.state === "running";
  const err = runtime.error;
  const conflict = isApiError(err) && err.code === "conflict";
  const unavailable = isApiError(err) && err.code === "service_unavailable";
  const r = runtime.data;
  return (
    <Section title="Game runtime" description={running ? "Reported by the game, refreshed every few seconds while this page is visible." : undefined}>
      {!running || conflict ? (
        <p className="text-sm text-muted-foreground">
          {conflict && isApiError(err) ? `${sentence(err.message)} ` : ""}The game reports its runtime while the server is running.
        </p>
      ) : runtime.isPending ? (
        <LoadingState label="Asking the game" />
      ) : unavailable && !r ? (
        <Notice tone="warning" title="The game did not answer">
          {describeError(err)}
        </Notice>
      ) : runtime.isError && !r ? (
        <ErrorState error={err} title="Could not read the game's runtime" onRetry={() => void runtime.refetch()} />
      ) : r ? (
        <div className="flex flex-col gap-3">
          {runtime.isError && (
            <p className="text-xs text-amber-700 dark:text-amber-400" role="status">
              The last refresh failed ({describeError(err)}); showing the values sampled {formatRelative(r.sampled_at)}.
            </p>
          )}
          <div className="flex flex-wrap items-center gap-2 text-sm">
            {r.ready ? <Badge tone="success">Ready</Badge> : <Badge tone="warning">Not ready</Badge>}
            {!r.ready && r.not_ready_reason && <span className="break-words text-muted-foreground">{r.not_ready_reason}</span>}
            {r.game_name && <span className="text-xs text-muted-foreground">{r.game_name}</span>}
          </div>
          <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
            <Metric label="Players" value={r.sessions.toLocaleString()} hint="Connected sessions" />
            <Metric label="Entities" value={r.entities.toLocaleString()} />
            <Metric label="Uptime" value={formatDuration(r.uptime_seconds)} />
          </div>
          <DetailList
            items={[
              {
                term: "Tick",
                value: (
                  <span className="tabular-nums">
                    {r.tick.rate} Hz, tick {r.tick.tick.toLocaleString()}
                  </span>
                ),
              },
              {
                term: "Tick time",
                value: (
                  <span className="tabular-nums">
                    last {r.tick.last_ms.toFixed(1)} ms, max {r.tick.max_ms.toFixed(1)} ms of a {r.tick.budget_ms.toFixed(1)} ms budget
                    {r.tick.overruns > 0 ? `, ${r.tick.overruns.toLocaleString()} overruns` : ", no overruns"}
                  </span>
                ),
              },
              { term: "Engine", value: r.engine_version ? <code className="font-mono text-xs">{r.engine_version}</code> : "-" },
              { term: "Transport", value: r.transport || "unknown" },
              { term: "Schema", value: r.schema_fingerprint ? <code className="break-all font-mono text-xs">{r.schema_fingerprint}</code> : "-" },
              { term: "Certificate hashes", value: r.certificate_hashes_published ? "Published (clients pin the self-signed certificate)" : "Not published" },
              { term: "Sampled", value: formatDateTime(r.sampled_at) },
            ]}
          />
        </div>
      ) : null}
    </Section>
  );
}

/** The build the server runs, and moving it to another successful build of the same project ("deploy"). */
function BuildSection({ server, onDeploy }: { server: Server; onDeploy: () => void }) {
  return (
    <Section
      title="Build"
      description="A server runs one successful build of its project. Deploying another build changes what the next start runs."
      actions={
        <Button size="sm" variant={server.build_id === null ? "primary" : "outline"} onClick={onDeploy}>
          <Rocket />
          {server.build_id === null ? "Choose a build" : "Deploy another build"}
        </Button>
      }
    >
      {server.build_id === null ? (
        <p className="text-sm text-muted-foreground">No build assigned. The server cannot start until it has one.</p>
      ) : (
        <p className="text-sm">
          Runs{" "}
          <Link to="/builds/$id" params={{ id: String(server.build_id) }} className="font-medium hover:underline">
            build {buildName({ id: server.build_id, commit: server.commit })}
          </Link>
          {server.commit && (
            <>
              {" "}
              (commit <code className="font-mono text-xs">{shortCommit(server.commit)}</code>
              {server.engine_version ? `, Golem Engine ${server.engine_version}` : ""})
            </>
          )}
          .
        </p>
      )}
    </Section>
  );
}

function DeployDialog({ server, busy, onJob, onClose }: { server: Server; busy: boolean; onJob: (job: Job) => void; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const builds = useSuccessfulBuilds();
  const projects = useProjects();
  const grouped = useMemo(() => buildsByProject(builds.data?.items ?? []), [builds.data]);
  // A server's project is fixed: only builds of that project (any project for a server that never had a build).
  const options = useMemo(
    () => (server.project_id !== null ? (grouped.get(server.project_id) ?? []) : [...grouped.values()].flat().sort((a, b) => b.id - a.id)),
    [grouped, server.project_id],
  );
  const others = options.filter((b) => b.id !== server.build_id);
  const [choice, setChoice] = useState<number | null>(null);
  const selected = choice ?? others[0]?.id ?? null;
  const selectedBuild = others.find((b) => b.id === selected) ?? null;
  const saved = server.data.schema_fingerprint;
  const fit = selectedBuild ? schemaMatch(saved, selectedBuild.schema_fingerprint) : "unknown";
  const [error, setError] = useState<string | null>(null);
  const running = isContainerActive(server.state);
  const projectName = (id: number) => projects.data?.items.find((p) => p.id === id)?.name ?? `project ${id}`;

  const deploy = useMutation({
    mutationFn: async (restart: boolean) => {
      const updated = await api.servers.update(server.id, { build_id: selected ?? 0 });
      queryClient.setQueryData(qk.servers.detail(updated.id), updated);
      const job = restart ? await api.servers.restart(server.id) : null;
      return { updated, job };
    },
    onSuccess: ({ updated, job }) => {
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      void queryClient.invalidateQueries({ queryKey: qk.builds.all });
      if (job) {
        queryClient.setQueryData(qk.jobs.detail(job.id), job);
        void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("server", server.id) });
        onJob(job);
        toast.success(`Deploying build #${updated.build_id}: ${updated.name} is restarting.`);
      } else {
        toast.success(
          running ? `Build #${updated.build_id} assigned. Restart ${updated.name} to run it.` : `Build #${updated.build_id} assigned. The next start runs it.`,
        );
      }
      onClose();
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "validation_failed" && err.field === "build_id") setError(sentence(err.message));
      // The build is saved even if the restart was refused: refresh to show it.
      void queryClient.invalidateQueries({ queryKey: qk.servers.detail(server.id) });
    },
  });

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && !deploy.isPending && onClose()}
      title={`Deploy a build to ${server.name}`}
      description={
        server.project_id !== null
          ? `Only successful builds of ${projectName(server.project_id)} can be deployed: a server's project is fixed when it is created.`
          : "This server has no build yet; choose any successful build. Its project then becomes the server's project."
      }
      footer={
        <>
          <Button variant="outline" onClick={onClose} disabled={deploy.isPending}>
            Cancel
          </Button>
          {running ? (
            <>
              <Button variant="outline" disabled={selected === null || busy} loading={deploy.isPending && deploy.variables === false} onClick={() => deploy.mutate(false)}>
                Assign without restarting
              </Button>
              <Button
                variant="destructive"
                disabled={selected === null || busy}
                title={busy ? "Wait for the running job to finish." : undefined}
                loading={deploy.isPending && deploy.variables === true}
                onClick={() => deploy.mutate(true)}
              >
                Deploy and restart
              </Button>
            </>
          ) : (
            <Button disabled={selected === null || busy} loading={deploy.isPending} onClick={() => deploy.mutate(false)}>
              Assign build
            </Button>
          )}
        </>
      }
    >
      {builds.isPending ? (
        <LoadingState label="Loading builds" />
      ) : builds.isError ? (
        <ErrorState error={builds.error} title="Could not load the builds" onRetry={() => void builds.refetch()} />
      ) : others.length === 0 ? (
        <Notice tone="info" title="No other successful build">
          {server.project_id !== null ? (
            <>
              Start a new build on the{" "}
              <Link to="/projects/$id" params={{ id: String(server.project_id) }} className="underline underline-offset-2">
                project page
              </Link>
              ; once it succeeds it can be deployed here.
            </>
          ) : (
            "Build a project first; once a build succeeds it can be assigned here."
          )}
        </Notice>
      ) : (
        <div className="flex flex-col gap-4">
          <Field label="Build" error={error}>
            {(p) => (
              <Select
                {...p}
                value={selected ?? ""}
                onChange={(e) => {
                  setChoice(Number(e.target.value));
                  setError(null);
                }}
              >
                {others.map((b) => (
                  <option key={b.id} value={b.id}>
                    {server.project_id === null ? `${projectName(b.project_id)} ` : ""}
                    {buildOptionLabel(b, server.build_id)}
                  </option>
                ))}
              </Select>
            )}
          </Field>
          {fit === "differs" && selectedBuild && (
            <Notice tone="warning" title="This build cannot load the saved world">
              The saved world has schema{" "}
              <code className="font-mono text-xs" title={saved}>
                {shortFingerprint(saved)}
              </code>
              ; build {buildName(selectedBuild)} has schema{" "}
              <code className="font-mono text-xs" title={selectedBuild.schema_fingerprint}>
                {shortFingerprint(selectedBuild.schema_fingerprint)}
              </code>
              . With this build the server will not start (starts and restarts fail) until it runs a build with the saved schema
              again, or an administrator restores a backup with this build&apos;s schema or resets the data. You can still assign it.
            </Notice>
          )}
          {fit === "match" && server.data.schema_mismatch && (
            <Notice tone="info" title="This build can load the saved world">
              Its schema matches the saved world, so the server can start again once it runs this build.
            </Notice>
          )}
          {running ? (
            <Notice tone="warning" title="Applying it restarts the server">
              The server is running. Deploy and restart stops the game, starts the new build and waits until it reports ready:
              every connected player is disconnected. Assign without restarting keeps the game running on the current build until
              its next restart.
            </Notice>
          ) : (
            <p className="text-sm text-muted-foreground">The server is not running: the next start runs the new build.</p>
          )}
          {busy && <p className="text-xs text-muted-foreground">A job is running for this server; the build can change once it ends.</p>}
          {deploy.isError && !(isApiError(deploy.error) && deploy.error.field === "build_id") && <ErrorText error={deploy.error} />}
        </div>
      )}
    </Dialog>
  );
}

/** Extra browser origins for WebTransport and WebSocket; the server's own address is always allowed. */
function OriginsSection({ server }: { server: Server }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [draft, setDraftState] = useState<string[] | null>(null);
  const [errors, setErrors] = useState<OriginErrors>(noOriginErrors());
  const origins = draft ?? server.allowed_origins;
  const changed = draft !== null && JSON.stringify(draft.map((o) => o.trim())) !== JSON.stringify(server.allowed_origins);
  const setDraft = (d: string[] | null) => {
    setDraftState(d);
    setErrors(noOriginErrors());
  };
  const save = useMutation({
    mutationFn: (list: string[]) => api.servers.update(server.id, { allowed_origins: list }),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      setDraft(null);
      toast.success(isContainerActive(s.state) ? "Allowed origins saved. Restart the server to apply them." : "Allowed origins saved.");
    },
    onError: (err) => {
      const routed = originErrorsFrom(err);
      if (routed) setErrors(routed);
    },
  });
  const own = server.public_url ? server.public_url.replace(/\/$/, "") : null;
  return (
    <Section
      title="Allowed origins"
      description={
        <>
          Browser pages on these origins may open WebTransport and WebSocket connections to the game. The server&apos;s own address
          {own ? (
            <>
              {" "}
              (<code className="font-mono">{own}</code>)
            </>
          ) : null}{" "}
          is always allowed. Changes apply on the next start or restart.
        </>
      }
      actions={
        <>
          {changed && (
            <Button size="sm" variant="ghost" onClick={() => setDraft(null)} disabled={save.isPending}>
              Discard
            </Button>
          )}
          <Button
            size="sm"
            disabled={!changed}
            loading={save.isPending}
            onClick={() => {
              const list = origins.map((o) => o.trim());
              const errs = validateOrigins(list);
              setErrors(errs);
              if (!hasOriginErrors(errs)) save.mutate(list);
            }}
          >
            Save origins
          </Button>
        </>
      }
    >
      <OriginsEditor origins={origins} onChange={setDraft} errors={errors} disabled={save.isPending} />
      {save.isError && !originErrorsFrom(save.error) && (
        <div className="mt-3">
          <ErrorText error={save.error} />
        </div>
      )}
    </Section>
  );
}

/** The certificate the server's game serves (its own self-signed one, or the shared custom one). */
function CertificateSection({ server }: { server: Server }) {
  const cert = server.certificate;
  return (
    <Section title="Certificate" description="The TLS certificate the game serves for WebTransport and HTTPS on its port.">
      {cert === null ? (
        <p className="text-sm text-muted-foreground">
          No certificate yet. With self-signed certificates (the default) each server gets its own when it first starts.
        </p>
      ) : (
        <div className="flex flex-col gap-3">
          <CertificateDetails cert={cert} />
          {cert.self_signed && (
            <p className="text-xs text-muted-foreground">
              Self-signed and short-lived (renewed automatically): native clients and pages that pin the published hashes connect
              fine, but browsers opening the game URL directly show a certificate warning. An administrator can install a trusted
              certificate under Settings.
            </p>
          )}
        </div>
      )}
    </Section>
  );
}

function StatsSection({ server }: { server: Server }) {
  const stats = useServerStats(server);
  const running = server.state === "running";
  // The daemon answers 409 conflict when the container is gone or stopped; the state poll catches up.
  const conflict = isApiError(stats.error) && stats.error.code === "conflict" ? stats.error : null;
  const notRunning = conflict !== null;
  return (
    <Section title="Resource usage" description={running ? "Sampled every few seconds while this page is visible." : undefined}>
      {!running || notRunning ? (
        <p className="text-sm text-muted-foreground">
          {conflict ? `${sentence(conflict.message)} ` : ""}Statistics are available while the server is running.
        </p>
      ) : stats.isPending ? (
        <LoadingState label="Loading statistics" />
      ) : stats.isError ? (
        <ErrorState error={stats.error} title="Could not load statistics" onRetry={() => void stats.refetch()} />
      ) : (
        <DetailList
          items={[
            { term: "CPU", value: formatPercent(stats.data.cpu_percent) },
            {
              term: "Memory",
              value:
                stats.data.memory_limit_bytes > 0
                  ? `${formatBytes(stats.data.memory_bytes)} of ${formatBytes(stats.data.memory_limit_bytes)}`
                  : formatBytes(stats.data.memory_bytes),
            },
            { term: "Network in", value: formatBytes(stats.data.network_rx_bytes) },
            { term: "Network out", value: formatBytes(stats.data.network_tx_bytes) },
            { term: "Processes", value: String(stats.data.pids) },
            { term: "Sampled", value: formatDateTime(stats.data.sampled_at) },
          ]}
        />
      )}
    </Section>
  );
}

function EnvSection({ server }: { server: Server }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [draft, setDraftState] = useState<EnvDraft>(emptyDraft());
  // The daemon's env errors (value errors name the key: env_set.<KEY>), until the draft changes.
  const [serverErrors, setServerErrors] = useState<EnvErrors>(noEnvErrors());
  const result = computeEnvDiff(server.env_keys, draft);
  const setDraft = (d: EnvDraft) => {
    setDraftState(d);
    setServerErrors(noEnvErrors());
  };
  const save = useMutation({
    mutationFn: () => {
      const body: UpdateServerRequest = {};
      if (Object.keys(result.diff.env_set).length) body.env_set = result.diff.env_set;
      if (result.diff.env_unset.length) body.env_unset = result.diff.env_unset;
      return api.servers.update(server.id, body);
    },
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      setDraft(emptyDraft());
      toast.success(s.state === "running" ? "Environment saved. Restart the server to apply it." : "Environment saved.");
    },
    onError: (err) => {
      const routed = envErrorsFrom(err, draft, server.env_keys);
      if (routed) setServerErrors(routed);
    },
  });
  const routed = save.isError && envErrorsFrom(save.error, draft, server.env_keys) !== null;
  const collided = save.isError && isApiError(save.error) && save.error.code === "conflict";

  return (
    <Section
      title="Environment"
      description="Values are encrypted on the host and never shown. Changes apply on the next start or restart."
      actions={
        <>
          {result.hasChanges && (
            <Button size="sm" variant="ghost" onClick={() => setDraft(emptyDraft())} disabled={save.isPending}>
              Discard
            </Button>
          )}
          <Button size="sm" disabled={!result.hasChanges || !result.valid} loading={save.isPending} onClick={() => save.mutate()}>
            Save environment
          </Button>
        </>
      }
    >
      {server.env_keys.length === 0 && draft.added.length === 0 && (
        <p className="mb-3 text-sm text-muted-foreground">No environment variables set.</p>
      )}
      <EnvEditor
        existingKeys={server.env_keys}
        draft={draft}
        onChange={setDraft}
        errors={mergeEnvErrors(result, serverErrors)}
        disabled={save.isPending}
      />
      {save.isError && !routed && (
        <div className="mt-3">
          {collided ? (
            <ErrorText error={save.error}>
              Another change to this server&apos;s environment was saved at the same moment, so yours was not. Your edits are
              still here: save again.
            </ErrorText>
          ) : (
            <ErrorText error={save.error} />
          )}
        </div>
      )}
    </Section>
  );
}

/** The daemon serves 1 to 5000 trailing lines (default 200). */
export const TAIL_OPTIONS = [200, 1000, 5000] as const;

function LogsSection({ server }: { server: Server }) {
  const api = useApi();
  const [tail, setTail] = useState<number>(1000);
  const [follow, setFollow] = useState(true);
  // Without a container there is no log (409); while Docker is unreachable it cannot be read (503).
  const hasContainer = server.state !== "missing";
  const readable = hasContainer && server.state !== "unknown";
  // Only a running container has output to follow; a stopped one still has its last output.
  const live = follow && (server.state === "running" || server.state === "restarting");
  // A restart or start creates a new container, whose output is a new log: reopen for it.
  const stream = useLogStream(
    readable ? api.servers.logsPath(server.id) : null,
    { tail, follow: live ? 1 : undefined },
    { resetKey: server.container_id },
  );
  const status = !hasContainer
    ? "No container"
    : !readable
      ? "Docker unreachable"
      : stream.state === "connecting"
      ? "Connecting"
      : stream.state === "streaming"
        ? live
          ? "Live"
          : "Loading"
        : stream.state === "error"
          ? "Not connected"
          : live
            ? "Stream ended"
            : server.state === "running"
              ? "Loaded"
              : "Container not running: last output";
  const err = stream.state === "error" ? stream.error : null;
  const tooMany = isApiError(err) && err.code === "rate_limited";

  return (
    <Section
      title="Logs"
      description="Container output, each line prefixed with its timestamp. Control characters such as colour codes arrive escaped (\x1b[31m) and are shown dimmed. Long histories are trimmed to the newest 100,000 lines in the browser."
    >
      <div className="flex flex-col gap-2">
        <LogViewer
          buffer={stream.buffer}
          version={stream.version}
          filename={`${server.name}.log`}
          heightClass="h-[28rem]"
          status={<span>{status}</span>}
          emptyText={
            !hasContainer
              ? "The server has no container yet. Start it to see output."
              : !readable
                ? "Docker cannot be reached right now, so the container's output cannot be read."
                : "No output yet."
          }
          notice={
            err !== null ? (
              <div className="flex flex-wrap items-center justify-between gap-2" role="alert">
                <span>
                  {tooMany
                    ? `${describeError(err)} Log streams of other tabs or pages count too; close one or wait.`
                    : describeError(err)}
                  {isApiError(err) && err.requestId && (
                    <span className="ml-2 font-mono text-xs text-muted-foreground">Request id: {err.requestId}</span>
                  )}
                </span>
                <Button size="sm" variant="outline" onClick={stream.restart}>
                  <RotateCw />
                  Retry
                </Button>
              </div>
            ) : undefined
          }
          controls={
            <>
              <label className="flex items-center gap-1.5 text-xs">
                <span className="text-muted-foreground">Last</span>
                <Select className="h-8 w-24 text-xs" value={tail} onChange={(e) => setTail(Number(e.target.value))} aria-label="Lines of history">
                  {TAIL_OPTIONS.map((n) => (
                    <option key={n} value={n}>
                      {n.toLocaleString()}
                    </option>
                  ))}
                </Select>
              </label>
              <Checkbox label="Follow" checked={follow} onChange={(e) => setFollow(e.target.checked)} className="text-xs" />
              {readable && stream.state === "ended" && (
                <Button size="sm" variant="outline" onClick={stream.restart}>
                  <RotateCw />
                  Reconnect
                </Button>
              )}
            </>
          }
        />
      </div>
    </Section>
  );
}

function EditServerDialog({ server, busy, onClose }: { server: Server; busy: boolean; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [port, setPort] = useState(String(server.port));
  const [error, setError] = useState<string | null>(null);
  const locked = portLocked(server);
  const update = useMutation({
    mutationFn: (body: UpdateServerRequest) => api.servers.update(server.id, body),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      toast.success(`Port changed to ${s.port}.`);
      onClose();
    },
    onError: (err) => {
      // port_in_use; conflict when the container started meanwhile (another user, an API token);
      // job_in_progress while a start or stop runs; validation errors of the port.
      if (isApiError(err) && (err.code === "port_in_use" || err.code === "conflict" || err.code === "job_in_progress" || err.field === "port")) {
        setError(describeError(err));
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const err = locked ? null : validatePort(port, { optional: false });
    setError(err);
    if (err || locked) return;
    if (Number(port) === server.port) {
      onClose();
      return;
    }
    update.mutate({ port: Number(port) });
  }

  const inline =
    isApiError(update.error) &&
    (update.error.code === "port_in_use" || update.error.code === "conflict" || update.error.code === "job_in_progress" || update.error.field === "port");
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={`Change the port of ${server.name}`}
      description="The game URL changes with the port; the next start publishes the new one."
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="edit-server" loading={update.isPending} disabled={locked !== null}>
            Save
          </Button>
        </>
      }
    >
      <form id="edit-server" onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field
          label="Port"
          error={error}
          hint={
            locked === "running"
              ? "Stop the server to change its port: a running container keeps the port it was started with."
              : locked === "unknown"
                ? "Docker cannot be reached, so the daemon cannot tell whether the server is running. The port can be changed once Docker is reachable and the server is stopped."
                : busy
                  ? "A job is running for this server: the port can change once it ends."
                  : "Published as UDP and TCP. Must be inside the daemon's port range."
          }
        >
          {(p) => (
            <Input
              {...p}
              inputMode="numeric"
              className="w-32"
              value={locked ? String(server.port) : port}
              disabled={locked !== null}
              onChange={(e) => {
                setPort(e.target.value);
                setError(null);
              }}
            />
          )}
        </Field>
        {update.isError && !inline && <ErrorText error={update.error} />}
      </form>
    </Dialog>
  );
}
