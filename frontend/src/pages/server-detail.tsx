import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { ArrowLeft, Pencil, RotateCw, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError } from "@/api/errors";
import { isJobActive, useProjects, useServer, useServerStats, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Job, Server, UpdateServerRequest } from "@/api/types.gen";
import { EnvEditor } from "@/components/env-editor";
import { InlineJob, JobsPanel } from "@/components/jobs";
import { ServerActions } from "@/components/server-actions";
import { ServerStateBadge } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { LogViewer } from "@/components/ui/log-viewer";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { useLogStream } from "@/hooks/use-log-stream";
import { computeEnvDiff, emptyDraft, type EnvDraft } from "@/lib/env-diff";
import { formatBytes, formatDateTime, formatPercent, formatRelative } from "@/lib/format";
import { validatePort } from "@/lib/validators";
import { NotFoundPage } from "@/pages/not-found";

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

/**
 * Configuration changes apply on the next (re)start. Without an explicit flag
 * in the API, a running container that started before the last change is
 * assumed to be out of date.
 */
export function restartRequired(s: Server): boolean {
  if (s.state !== "running" || !s.started_at) return false;
  return new Date(s.updated_at).getTime() > new Date(s.started_at).getTime();
}

function ServerDetail({ id }: { id: number }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const server = useServer(id);
  const jobs = useSubjectJobs("server", id);
  const projects = useProjects();
  const [startedJob, setStartedJob] = useState<Job | null>(null);
  const [dialog, setDialog] = useState<"edit" | "delete" | null>(null);

  if (server.isPending) return <LoadingState label="Loading server" />;
  if (server.isError) {
    if (isApiError(server.error) && server.error.code === "not_found") return <NotFoundPage />;
    return (
      <>
        <PageHeader title="Server" back={<BackLink />} />
        <ErrorState error={server.error} title="Could not load the server" onRetry={() => void server.refetch()} />
      </>
    );
  }

  const s = server.data;
  const activeJob = jobs.data?.items.find(isJobActive) ?? null;
  const currentJob = startedJob ?? activeJob;
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
            <ServerActions server={s} size="md" busy={activeJob !== null} onJob={setStartedJob} />
            <Button variant="outline" onClick={() => setDialog("edit")}>
              <Pencil />
              Edit
            </Button>
            <Button variant="outline" onClick={() => setDialog("delete")}>
              <Trash2 />
              Delete
            </Button>
          </>
        }
      />
      <div className="flex flex-col gap-6">
        {restartRequired(s) && (
          <Notice tone="warning" title="Restart required">
            The configuration changed after this server started. Restart it to apply the changes.
          </Notice>
        )}
        {s.state === "unknown" && (
          <Notice tone="warning" title="State unknown">
            Docker could not be queried, so the actual state of this server is not known.
          </Notice>
        )}
        {currentJob && (
          <InlineJob key={currentJob.id} job={currentJob} onDismiss={startedJob ? () => setStartedJob(null) : undefined} />
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
                { term: "Image", value: <code className="break-all font-mono text-xs">{s.image}</code> },
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
        <Section title="Jobs" description="Recent start, stop and restart operations.">
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
            Delete <span className="font-medium text-foreground">{s.name}</span>? Its container is removed and port {s.port} is
            freed. Its data volume is kept and can be deleted under Volumes.
          </>
        }
        confirmLabel="Delete server"
        destructive
        onConfirm={async () => {
          await api.servers.remove(s.id);
          toast.success(`Server ${s.name} deleted.`);
          queryClient.removeQueries({ queryKey: qk.servers.detail(s.id) });
          void queryClient.invalidateQueries({ queryKey: qk.servers.list });
          void queryClient.invalidateQueries({ queryKey: qk.volumes });
          void queryClient.invalidateQueries({ queryKey: qk.status });
          void navigate({ to: "/servers" });
        }}
      />
    </>
  );
}

function StatsSection({ server }: { server: Server }) {
  const stats = useServerStats(server);
  const running = server.state === "running";
  return (
    <Section title="Resource usage" description={running ? "Sampled every few seconds while this page is visible." : undefined}>
      {!running ? (
        <p className="text-sm text-muted-foreground">Statistics are available while the server is running.</p>
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
  const [draft, setDraft] = useState<EnvDraft>(emptyDraft());
  const result = computeEnvDiff(server.env_keys, draft);
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
  });

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
      <EnvEditor existingKeys={server.env_keys} draft={draft} onChange={setDraft} rowErrors={result.rowErrors} disabled={save.isPending} />
      {save.isError && (
        <div className="mt-3">
          <ErrorText error={save.error} />
        </div>
      )}
    </Section>
  );
}

const TAIL_OPTIONS = [100, 1000, 10000] as const;

function LogsSection({ server }: { server: Server }) {
  const api = useApi();
  const [tail, setTail] = useState<number>(1000);
  const [follow, setFollow] = useState(true);
  const stream = useLogStream(api.servers.logsPath(server.id), { tail, follow: follow ? 1 : undefined });
  const status =
    stream.state === "connecting"
      ? "Connecting"
      : stream.state === "streaming"
        ? follow
          ? "Live"
          : "Loading"
        : stream.state === "error"
          ? "Disconnected"
          : follow
            ? "Stream ended"
            : "Loaded";

  return (
    <Section title="Logs" description="Container output. Long histories are trimmed to the newest 100,000 lines in the browser.">
      <div className="flex flex-col gap-2">
        <LogViewer
          buffer={stream.buffer}
          version={stream.version}
          filename={`${server.name}.log`}
          heightClass="h-[28rem]"
          status={<span>{status}</span>}
          emptyText={server.state === "missing" ? "The server has no container yet. Start it to see output." : "No output yet."}
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
              {(stream.state === "error" || stream.state === "ended") && (
                <Button size="sm" variant="outline" onClick={stream.restart}>
                  <RotateCw />
                  Reconnect
                </Button>
              )}
            </>
          }
        />
        {stream.state === "error" && <ErrorText error={stream.error} />}
      </div>
    </Section>
  );
}

function EditServerDialog({ server, onClose }: { server: Server; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [image, setImage] = useState(server.image);
  const [port, setPort] = useState(String(server.port));
  const [errors, setErrors] = useState<Partial<Record<"image" | "port", string | null>>>({});
  const update = useMutation({
    mutationFn: (body: UpdateServerRequest) => api.servers.update(server.id, body),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      toast.success(s.state === "running" ? "Server updated. Restart it to apply the changes." : "Server updated.");
      onClose();
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "port_in_use") setErrors({ port: "This port is already assigned to another server." });
      else if (isApiError(err) && err.code === "validation_failed" && (err.field === "image" || err.field === "port")) {
        setErrors({ [err.field]: err.message });
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = { image: image.trim() ? null : "Enter an image reference.", port: validatePort(port, { optional: false }) };
    setErrors(errs);
    if (errs.image || errs.port) return;
    const body: UpdateServerRequest = {};
    if (image.trim() !== server.image) body.image = image.trim();
    if (Number(port) !== server.port) body.port = Number(port);
    if (Object.keys(body).length === 0) {
      onClose();
      return;
    }
    update.mutate(body);
  }

  const inline = isApiError(update.error) && (update.error.code === "port_in_use" || update.error.field === "image" || update.error.field === "port");
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
        <Field label="Image" error={errors.image}>
          {(p) => <Input {...p} spellCheck={false} value={image} onChange={(e) => setImage(e.target.value)} />}
        </Field>
        <Field label="Port" error={errors.port} hint="Published as UDP and TCP.">
          {(p) => <Input {...p} inputMode="numeric" className="w-32" value={port} onChange={(e) => setPort(e.target.value)} />}
        </Field>
        {update.isError && !inline && <ErrorText error={update.error} />}
      </form>
    </Dialog>
  );
}
