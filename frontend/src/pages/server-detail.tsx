import { useEffect, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { ArrowLeft, Pencil, RotateCw, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { describeError, isApiError, sentence } from "@/api/errors";
import { isJobActive, useImages, useProjects, useServer, useServerStats, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Job, Server, UpdateServerRequest } from "@/api/types.gen";
import { EnvEditor } from "@/components/env-editor";
import { InlineJob, JobsPanel, useShownJob } from "@/components/jobs";
import { ServerActions } from "@/components/server-actions";
import { ServerStateBadge, portLocked, serverActionsBlocked } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { LogViewer } from "@/components/ui/log-viewer";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { useLogStream } from "@/hooks/use-log-stream";
import { computeEnvDiff, emptyDraft, envErrorsFrom, mergeEnvErrors, noEnvErrors, type EnvDraft, type EnvErrors } from "@/lib/env-diff";
import { formatBytes, formatDateTime, formatPercent, formatRelative } from "@/lib/format";
import { imageLabel } from "@/lib/images";
import { validatePort } from "@/lib/validators";
import { NotFoundPage } from "@/pages/not-found";
import { NoImagesNotice, imageOptions } from "@/pages/servers";

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


function ServerDetail({ id }: { id: number }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const server = useServer(id);
  const jobs = useSubjectJobs("server", id);
  const projects = useProjects();
  const images = useImages();
  const [dialog, setDialog] = useState<"edit" | "delete" | null>(null);
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

  async function remove(target: Server) {
    try {
      await api.servers.remove(target.id);
    } catch (err) {
      setDialog(null);
      toast.error(err, `Could not delete ${target.name}`);
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
      return;
    }
    toast.success(`Server ${target.name} deleted: container removed and port ${target.port} freed.`);
    queryClient.removeQueries({ queryKey: qk.servers.detail(target.id) });
    void queryClient.invalidateQueries({ queryKey: qk.servers.list });
    void queryClient.invalidateQueries({ queryKey: qk.volumes });
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
  const address = `${window.location.hostname}:${s.port}`;

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
        actions={
          <>
            <ServerActions server={s} size="md" busy={activeJob !== null} onJob={shown.show} />
            <Button variant="outline" onClick={() => setDialog("edit")}>
              <Pencil />
              Edit
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
            The running container was created from an older definition (image, port, environment or the daemon&apos;s run
            settings changed). Changes never restart a server on their own: restart it to apply them.
          </Notice>
        )}
        {s.state === "unknown" && (
          <Notice tone="warning" title="State unknown">
            The server has a container, but Docker could not be queried, so its actual state is not known. Start, stop,
            restart, delete and port changes are unavailable until the daemon reaches Docker again; this page checks every
            few seconds.
          </Notice>
        )}
        {currentJob && (
          <InlineJob key={currentJob.id} job={currentJob} onDismiss={shown.dismiss} />
        )}
        <div className="grid gap-6 lg:grid-cols-2">
          <Section title="Details">
            <DetailList
              items={[
                {
                  term: "Address",
                  value: (
                    <span className="flex flex-col gap-1">
                      <span className="inline-flex items-center gap-2">
                        <code className="font-mono text-sm">{address}</code>
                        <CopyButton value={address} size="icon" variant="ghost" label="Copy address" />
                      </span>
                      <span className="text-xs text-muted-foreground">
                        Port {s.port} is published over both UDP and TCP. The host name is the one you use for this page;
                        players may need your public address instead.
                      </span>
                    </span>
                  ),
                },
                {
                  term: "Image",
                  value: (
                    <span className="flex flex-col gap-0.5">
                      <span>{imageLabel(s.image, images.data?.items)}</span>
                      <code className="break-all font-mono text-xs text-muted-foreground">{s.image}</code>
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
          <StatsSection server={s} />
        </div>
        <EnvSection server={s} />
        <LogsSection server={s} />
        <Section title="Jobs" description="Recent start, stop, restart and delete operations.">
          <JobsPanel subjectType="server" subjectId={id} />
        </Section>
      </div>

      {dialog === "edit" && <EditServerDialog server={s} onClose={() => setDialog(null)} />}
      <ConfirmDialog
        open={dialog === "delete"}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Delete server"
        description={
          <>
            Delete <span className="font-medium text-foreground">{s.name}</span>? The container is stopped gracefully (this can
            take up to 30 seconds) and removed, its stored environment is deleted and port {s.port} is freed.
          </>
        }
        confirmLabel="Delete server"
        destructive
        onConfirm={() => remove(s)}
      />
    </>
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

function EditServerDialog({ server, onClose }: { server: Server; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const images = useImages();
  const projects = useProjects();
  const options = imageOptions(images.data?.items ?? [], new Map((projects.data?.items ?? []).map((p) => [p.id, p])));
  const [image, setImage] = useState(server.image);
  const [port, setPort] = useState(String(server.port));
  const [errors, setErrors] = useState<Partial<Record<"image" | "port", string | null>>>({});
  const locked = portLocked(server);
  const [sentPort, setSentPort] = useState(false);
  const update = useMutation({
    mutationFn: (body: UpdateServerRequest) => api.servers.update(server.id, body),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      toast.success(s.state === "running" ? "Server updated. Restart it to apply the changes." : "Server updated.");
      onClose();
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "port_in_use") setErrors({ port: sentence(err.message) });
      // The container started meanwhile (another user, an API token): the port cannot change while it runs.
      else if (isApiError(err) && err.code === "conflict" && sentPort) setErrors({ port: sentence(err.message) });
      else if (isApiError(err) && err.code === "validation_failed" && (err.field === "image" || err.field === "port")) {
        setErrors({ [err.field]: sentence(err.message) });
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = { image: image ? null : "Choose an image.", port: locked ? null : validatePort(port, { optional: false }) };
    setErrors(errs);
    if (errs.image || errs.port) return;
    const body: UpdateServerRequest = {};
    if (image !== server.image) body.image = image;
    if (!locked && Number(port) !== server.port) body.port = Number(port);
    setSentPort(body.port !== undefined);
    if (Object.keys(body).length === 0) {
      onClose();
      return;
    }
    update.mutate(body);
  }

  const inline =
    isApiError(update.error) &&
    (update.error.code === "port_in_use" ||
      (update.error.code === "conflict" && sentPort) ||
      update.error.field === "image" ||
      update.error.field === "port");
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={`Edit ${server.name}`}
      description="Changes apply on the next start or restart."
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="edit-server" loading={update.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="edit-server" onSubmit={submit} noValidate className="flex flex-col gap-4">
        {images.isPending ? (
          <LoadingState label="Loading managed images" />
        ) : images.isError ? (
          <ErrorState error={images.error} title="Could not load the managed images" onRetry={() => void images.refetch()} />
        ) : options.length === 0 ? (
          <NoImagesNotice />
        ) : (
          <Field label="Image" error={errors.image} hint="Managed images only.">
            {(p) => (
              <Select {...p} value={image} onChange={(e) => setImage(e.target.value)}>
                {!options.some((o) => o.value === server.image) && (
                  <option value={server.image}>{imageLabel(server.image, images.data.items)} (current, no longer managed)</option>
                )}
                {options.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                    {o.value === server.image ? " (current)" : ""}
                  </option>
                ))}
              </Select>
            )}
          </Field>
        )}
        <Field
          label="Port"
          error={errors.port}
          hint={
            locked === "running"
              ? "Stop the server to change its port: a running container keeps the port it was started with."
              : locked === "unknown"
                ? "Docker cannot be reached, so the daemon cannot tell whether the server is running. The port can be changed once Docker is reachable and the server is stopped."
                : "Published as UDP and TCP."
          }
        >
          {(p) => (
            <Input {...p} inputMode="numeric" className="w-32" value={locked ? String(server.port) : port} disabled={locked !== null} onChange={(e) => setPort(e.target.value)} />
          )}
        </Field>
        {update.isError && !inline && <ErrorText error={update.error} />}
      </form>
    </Dialog>
  );
}
