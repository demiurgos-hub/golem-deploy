import { useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArchiveRestore, Download, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { ApiError, describeError, isApiError } from "@/api/errors";
import { useBackups, useServers, useSuccessfulBuilds } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Backup, BackupKind, Job, Server } from "@/api/types.gen";
import { buildName } from "@/components/builds";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { DataTable, type Column } from "@/components/ui/data-table";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { EmptyState, ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { saveBlob } from "@/lib/download";
import { formatBytes, formatDateTime, formatRelative, shortCommit, shortFingerprint } from "@/lib/format";

const KIND_LABELS: Record<BackupKind, string> = {
  manual: "Manual",
  scheduled: "Scheduled",
  pre_restore: "Before a restore",
  pre_reset: "Before a reset",
};

const KIND_HINTS: Record<BackupKind, string> = {
  manual: "Taken with Back up now. Kept until an administrator deletes it.",
  scheduled: "Taken by the server's backup schedule. Only the newest scheduled backups are kept.",
  pre_restore: "The data the server had before a backup was restored into it. Kept until an administrator deletes it.",
  pre_reset: "The data the server had before its data was reset. Kept until an administrator deletes it.",
};

/** Why a backup was taken, in plain words; unknown kinds are shown verbatim. */
export function backupKindLabel(kind: string): string {
  return KIND_LABELS[kind as BackupKind] ?? kind;
}

export type SchemaMatch = "match" | "differs" | "unknown";

/** Whether a game with fingerprint `build` can load a world saved with `saved`: only when both are known can they differ. */
export function schemaMatch(saved: string, build: string): SchemaMatch {
  if (!saved || !build) return "unknown";
  return saved === build ? "match" : "differs";
}

/** The schema fingerprint of each successful build, by build id (what a server can run). */
export function useBuildFingerprints(): Map<number, string> {
  const builds = useSuccessfulBuilds();
  return useMemo(() => new Map((builds.data?.items ?? []).map((b) => [b.id, b.schema_fingerprint])), [builds.data]);
}

/** The schema fingerprint of a server's build; "" when it has none or it is not known yet. */
export function serverBuildFingerprint(server: Pick<Server, "build_id">, fingerprints: Map<number, string>): string {
  return server.build_id === null ? "" : (fingerprints.get(server.build_id) ?? "");
}

/** A fingerprint shortened, with the full value in the title and a copy button. */
export function Fingerprint({ value, unknown = "Unknown" }: { value: string; unknown?: string }) {
  if (!value) return <span className="text-muted-foreground">{unknown}</span>;
  return (
    <span className="inline-flex items-center gap-1">
      <code className="font-mono text-xs" title={value}>
        {shortFingerprint(value)}
      </code>
      <CopyButton value={value} size="icon" variant="ghost" label="Copy schema fingerprint" />
    </span>
  );
}

export function SchemaMatchBadge({ match }: { match: SchemaMatch }) {
  if (match === "match") return <Badge tone="success">Same schema as the build</Badge>;
  if (match === "differs")
    return (
      <Badge tone="danger" title="The server's build cannot load this backup: its schema differs.">
        Different schema
      </Badge>
    );
  return (
    <Badge tone="neutral" title="The backup's or the build's schema fingerprint is not known, so they cannot be compared.">
      Not comparable
    </Badge>
  );
}

/** The server a backup came from: a link while it exists, its old name marked as deleted afterwards. */
export function BackupServer({ backup }: { backup: Pick<Backup, "server_id" | "server_name"> }) {
  if (backup.server_id === null) {
    return (
      <span className="inline-flex flex-wrap items-center gap-1.5">
        <span className="text-muted-foreground">{backup.server_name || "-"}</span>
        <Badge tone="warning">Deleted server</Badge>
      </span>
    );
  }
  return (
    <Link to="/servers/$id" params={{ id: String(backup.server_id) }} className="hover:underline">
      {backup.server_name}
    </Link>
  );
}

function BackupBuild({ backup }: { backup: Backup }) {
  if (backup.build_id === null) {
    return backup.commit ? (
      <span className="text-muted-foreground" title="The build was deleted since.">
        <code className="font-mono text-xs">{shortCommit(backup.commit)}</code>
      </span>
    ) : (
      <span className="text-muted-foreground">-</span>
    );
  }
  return (
    <Link to="/builds/$id" params={{ id: String(backup.build_id) }} className="font-mono text-xs hover:underline">
      {buildName({ id: backup.build_id, commit: backup.commit })}
    </Link>
  );
}

function backupCreator(b: Backup): string {
  if (b.kind === "scheduled") return "the schedule";
  return b.created_by_username ?? (b.created_by === null ? "a deleted account" : `account ${b.created_by}`);
}

/** Downloads a backup's archive with the bearer header (an <a href> would not carry it). */
export function useDownloadBackup() {
  const api = useApi();
  const toast = useToast();
  return useMutation({
    mutationFn: async (b: Backup) => {
      const { blob, filename } = await api.backups.download(b.id);
      saveBlob(blob, filename ?? `${b.server_name || "server"}-backup-${b.id}.tar.gz`);
    },
    onError: (err) => toast.error(err, "Could not download the backup"),
  });
}

/**
 * Backups as a table. `compareWith` is the schema fingerprint of the server
 * the backups would be restored into (the server page), shown as a match
 * badge; `showServer` adds the server and project columns (installation-wide
 * lists). Restore and delete are offered only when their handlers are given
 * (administrators).
 */
export function BackupsTable({
  label,
  backups,
  showServer = false,
  compareWith,
  onRestore,
  onDelete,
}: {
  label: string;
  backups: readonly Backup[];
  showServer?: boolean;
  compareWith?: string;
  onRestore?: (b: Backup) => void;
  onDelete?: (b: Backup) => void;
}) {
  const download = useDownloadBackup();
  const columns: Column<Backup>[] = [
    {
      key: "taken",
      header: "Taken",
      sortValue: (b) => b.created_at,
      cell: (b) => (
        <span className="flex flex-col">
          <span title={formatDateTime(b.created_at)}>{formatRelative(b.created_at)}</span>
          <span className="text-xs text-muted-foreground">by {backupCreator(b)}</span>
        </span>
      ),
    },
    ...(showServer
      ? [
          { key: "server", header: "Server", sortValue: (b: Backup) => b.server_name, cell: (b: Backup) => <BackupServer backup={b} /> },
          {
            key: "project",
            header: "Project",
            hideOnSmall: true,
            sortValue: (b: Backup) => b.project_name,
            cell: (b: Backup) =>
              b.project_id === null ? (
                <span className="text-muted-foreground">{b.project_name || "-"}</span>
              ) : (
                <Link to="/projects/$id" params={{ id: String(b.project_id) }} className="hover:underline">
                  {b.project_name}
                </Link>
              ),
          },
        ]
      : []),
    {
      key: "kind",
      header: "Kind",
      sortValue: (b) => b.kind,
      cell: (b) => (
        <span className="flex flex-col" title={KIND_HINTS[b.kind]}>
          <span>{backupKindLabel(b.kind)}</span>
          {!b.live && <span className="text-xs text-muted-foreground">Files copied as they were</span>}
        </span>
      ),
    },
    {
      key: "size",
      header: "Size",
      sortValue: (b) => b.size_bytes,
      cell: (b) => (
        <span className="tabular-nums" title={`${b.file_count.toLocaleString()} files, ${formatBytes(b.content_bytes)} before compression`}>
          {formatBytes(b.size_bytes)}
        </span>
      ),
    },
    { key: "build", header: "Build", hideOnSmall: true, cell: (b) => <BackupBuild backup={b} /> },
    {
      key: "schema",
      header: "Schema",
      hideOnSmall: true,
      cell: (b) => (
        <span className="flex flex-col items-start gap-1">
          <Fingerprint value={b.schema_fingerprint} />
          {compareWith !== undefined && <SchemaMatchBadge match={schemaMatch(b.schema_fingerprint, compareWith)} />}
        </span>
      ),
    },
    {
      key: "note",
      header: "Note",
      className: "max-w-64",
      cell: (b) => (b.note ? <span className="break-words">{b.note}</span> : <span className="text-muted-foreground">-</span>),
    },
  ];
  return (
    <DataTable
      label={label}
      data={backups}
      rowKey={(b) => b.id}
      initialSort={{ key: "taken", asc: false }}
      columns={columns}
      actions={(b) => (
        <>
          <Button
            size="sm"
            variant="outline"
            aria-label={`Download backup ${b.id}`}
            loading={download.isPending && download.variables?.id === b.id}
            onClick={() => download.mutate(b)}
          >
            {!(download.isPending && download.variables?.id === b.id) && <Download />}
            Download
          </Button>
          {onRestore && (
            <Button size="sm" variant="outline" aria-label={`Restore backup ${b.id}`} onClick={() => onRestore(b)}>
              <ArchiveRestore />
              Restore
            </Button>
          )}
          {onDelete && (
            <Button size="sm" variant="outline" aria-label={`Delete backup ${b.id}`} onClick={() => onDelete(b)}>
              <Trash2 />
              Delete
            </Button>
          )}
        </>
      )}
    />
  );
}

export function DeleteBackupDialog({ backup, onClose }: { backup: Backup | null; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  return (
    <ConfirmDialog
      open={backup !== null}
      onOpenChange={(o) => !o && onClose()}
      title="Delete backup"
      description={
        backup && (
          <>
            Delete the backup of <span className="font-medium text-foreground">{backup.server_name || "a deleted server"}</span> taken{" "}
            {formatDateTime(backup.created_at)} ({backupKindLabel(backup.kind).toLowerCase()}, {formatBytes(backup.size_bytes)})? The
            archive is removed from the host and cannot be restored afterwards.
          </>
        )
      }
      confirmLabel="Delete backup"
      destructive
      onConfirm={async () => {
        if (!backup) return;
        await api.backups.remove(backup.id);
        toast.success("Backup deleted.");
        await queryClient.invalidateQueries({ queryKey: qk.backups.all });
        void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      }}
    />
  );
}

/** The secondary "skip the safety backup" option, kept out of the way under a disclosure. */
function SkipSafetyBackup({ checked, onChange, disabled }: { checked: boolean; onChange: (v: boolean) => void; disabled?: boolean }) {
  return (
    <details className="rounded-md border border-border px-3 py-2 text-sm" open={checked || undefined}>
      <summary className="cursor-pointer text-muted-foreground">More options</summary>
      <div className="mt-2 flex flex-col gap-1">
        <Checkbox
          label="Skip the safety backup"
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
        <p className="pl-6 text-xs text-muted-foreground">
          The current data is then discarded without a copy and cannot be brought back. Use this only when the disk has no room
          for another backup.
        </p>
      </div>
    </details>
  );
}

export function afterServerJob(queryClient: ReturnType<typeof useQueryClient>, job: Job, serverId: number) {
  queryClient.setQueryData(qk.jobs.detail(job.id), job);
  void queryClient.invalidateQueries({ queryKey: qk.servers.all });
  void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("server", serverId) });
}

/** The server a restore of `backup` goes into by default: its own, else one of the same project, else any. */
function defaultTarget(backup: Backup, servers: readonly Server[]): Server | null {
  return (
    servers.find((s) => s.id === backup.server_id) ??
    servers.find((s) => backup.project_id !== null && s.project_id === backup.project_id) ??
    servers[0] ??
    null
  );
}

/**
 * Restores a backup into a server (admin). With `target` the server is fixed
 * (the server page); without it the dialog asks which server to restore into.
 * A known fingerprint difference, and the daemon's 409 schema_mismatch, ask
 * for an explicit confirmation before the restore is sent with `force`.
 */
export function RestoreDialog({
  backup,
  target,
  onClose,
  onJob,
}: {
  backup: Backup;
  target?: Server;
  onClose: () => void;
  onJob: (job: Job, server: Server) => void;
}) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const servers = useServers();
  const fingerprints = useBuildFingerprints();
  const all = servers.data?.items ?? [];
  const [choice, setChoice] = useState<number | null>(null);
  const server = target ?? all.find((s) => s.id === choice) ?? defaultTarget(backup, all);
  const [refused, setRefused] = useState<{ serverId: number; error: ApiError } | null>(null);
  const [force, setForce] = useState(false);
  const [skipSafety, setSkipSafety] = useState(false);
  const buildFp = server ? serverBuildFingerprint(server, fingerprints) : "";
  const known = schemaMatch(backup.schema_fingerprint, buildFp);
  const refusal = refused && server && refused.serverId === server.id ? refused.error : null;
  const mismatch = known === "differs" || refusal !== null;
  const noBuild = server !== null && server.build_id === null;

  const restore = useMutation({
    mutationFn: (s: Server) =>
      api.servers.restore(s.id, {
        backup_id: backup.id,
        ...(mismatch && force ? { force: true } : {}),
        ...(skipSafety ? { skip_safety_backup: true } : {}),
      }),
    onSuccess: (job, s) => {
      afterServerJob(queryClient, job, s.id);
      toast.success(`Restoring the backup into ${s.name}.`);
      onJob(job, s);
      onClose();
    },
    onError: (err, s) => {
      if (isApiError(err) && err.code === "schema_mismatch") {
        setRefused({ serverId: s.id, error: err });
        setForce(false);
      }
    },
  });
  const otherError = restore.isError && !(isApiError(restore.error) && restore.error.code === "schema_mismatch") ? restore.error : null;

  function pick(id: number) {
    setChoice(id);
    setForce(false);
    restore.reset();
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && !restore.isPending && onClose()}
      title={target ? `Restore a backup into ${target.name}` : "Restore a backup"}
      description={
        <>
          Backup of {backup.server_name || "a deleted server"} taken {formatDateTime(backup.created_at)} ({backupKindLabel(backup.kind).toLowerCase()},{" "}
          {formatBytes(backup.size_bytes)}){backup.note ? `: ${backup.note}` : ""}.
        </>
      }
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose} disabled={restore.isPending}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={server === null || noBuild || (mismatch && !force)}
            loading={restore.isPending}
            onClick={() => server && restore.mutate(server)}
          >
            {mismatch ? "Restore anyway" : "Restore backup"}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        {!target &&
          (servers.isPending ? (
            <LoadingState label="Loading servers" />
          ) : servers.isError ? (
            <ErrorState error={servers.error} title="Could not load the servers" onRetry={() => void servers.refetch()} />
          ) : all.length === 0 ? (
            <Notice tone="info" title="No server to restore into">
              Create a server first; a backup can be restored into any server, also one of another project.
            </Notice>
          ) : (
            <Field label="Restore into" hint="The backup replaces this server's saved data. Servers of the backup's project are listed first.">
              {(p) => (
                <Select {...p} value={server?.id ?? ""} onChange={(e) => pick(Number(e.target.value))}>
                  {[...all]
                    .sort((a, b) => Number(b.project_id === backup.project_id) - Number(a.project_id === backup.project_id) || a.name.localeCompare(b.name))
                    .map((s) => (
                      <option key={s.id} value={s.id}>
                        {s.name}
                        {s.id === backup.server_id ? " (where it was taken)" : ""}
                      </option>
                    ))}
                </Select>
              )}
            </Field>
          ))}
        {server && (
          <>
            {noBuild ? (
              <Notice tone="warning" title={`${server.name} has no build`}>
                A server needs a successful build before a backup can be restored into it. Deploy a build on its page first.
              </Notice>
            ) : (
              <div className="flex flex-col gap-1.5 text-sm">
                <p>What happens:</p>
                <ol className="list-decimal space-y-1 pl-5 text-muted-foreground">
                  <li>
                    {server.name} stops{server.state === "running" ? "; every connected player is disconnected" : ""}.
                  </li>
                  <li>
                    {skipSafety
                      ? "Its current data is not backed up: it is discarded for good."
                      : "Its current data is backed up first (a backup of kind “before a restore”), so this can be undone."}
                  </li>
                  <li>The backup&apos;s data replaces it.</li>
                  <li>
                    {server.desired_state === "running"
                      ? "The server is meant to be running, so it starts again with the restored world."
                      : "The server is stopped and stays stopped; the restored world loads on its next start."}
                  </li>
                </ol>
              </div>
            )}
            {mismatch && !noBuild && (
              <Notice tone="danger" title={`The build of ${server.name} cannot load this backup`}>
                <span className="flex flex-col gap-2">
                  {known === "differs" ? (
                    <span>
                      The backup was saved with schema <code className="font-mono text-xs">{shortFingerprint(backup.schema_fingerprint)}</code>,
                      but build {buildName({ id: server.build_id ?? 0, commit: server.commit })} has schema{" "}
                      <code className="font-mono text-xs">{shortFingerprint(buildFp)}</code>.
                    </span>
                  ) : (
                    refusal && <span>{describeError(refusal)}</span>
                  )}
                  <span>
                    After the restore the server will not start until it runs a build with the backup&apos;s schema: deploy such a build
                    of its project afterwards, or choose another backup.
                  </span>
                  <Checkbox
                    label="Restore anyway: I will deploy a build that can load it"
                    checked={force}
                    onChange={(e) => setForce(e.target.checked)}
                  />
                </span>
              </Notice>
            )}
            {!noBuild && <SkipSafetyBackup checked={skipSafety} onChange={setSkipSafety} disabled={restore.isPending} />}
          </>
        )}
        {otherError !== null && <ErrorText error={otherError} />}
      </div>
    </Dialog>
  );
}

/** Discards a server's saved data after a safety backup (admin); the server name must be typed to confirm. */
export function ResetDataDialog({ server, onClose, onJob }: { server: Server; onClose: () => void; onJob: (job: Job) => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [typed, setTyped] = useState("");
  const [skipSafety, setSkipSafety] = useState(false);
  const reset = useMutation({
    mutationFn: () => api.servers.resetData(server.id, skipSafety ? { skip_safety_backup: true } : {}),
    onSuccess: (job) => {
      afterServerJob(queryClient, job, server.id);
      toast.success(`Resetting the data of ${server.name}.`);
      onJob(job);
      onClose();
    },
  });
  const confirmed = typed === server.name;
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && !reset.isPending && onClose()}
      title={`Reset the data of ${server.name}`}
      description="The saved world is discarded and the game starts over with an empty one."
      footer={
        <>
          <Button variant="outline" onClick={onClose} disabled={reset.isPending}>
            Cancel
          </Button>
          <Button variant="destructive" disabled={!confirmed} loading={reset.isPending} onClick={() => reset.mutate()}>
            Reset data
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <ol className="list-decimal space-y-1 pl-5 text-sm text-muted-foreground">
          <li>
            {server.name} stops{server.state === "running" ? "; every connected player is disconnected" : ""}.
          </li>
          <li>
            {skipSafety
              ? "Its saved data is not backed up: it is discarded for good."
              : "Its saved data is backed up first (a backup of kind “before a reset”), then discarded."}
          </li>
          <li>
            {server.desired_state === "running"
              ? "The server is meant to be running, so it starts again with an empty world."
              : "The server stays stopped; its next start creates an empty world."}
          </li>
        </ol>
        <Field label={`Type ${server.name} to confirm`}>
          {(p) => <Input {...p} spellCheck={false} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />}
        </Field>
        <SkipSafetyBackup checked={skipSafety} onChange={setSkipSafety} disabled={reset.isPending} />
        {reset.isError && <ErrorText error={reset.error} />}
      </div>
    </Dialog>
  );
}

/**
 * Picks a backup from the whole installation to restore into `server`
 * (admin): by default backups of the server's project, including those of
 * deleted servers; "All projects" lists every backup.
 */
export function BackupPickerDialog({
  server,
  onPick,
  onClose,
}: {
  server: Server;
  onPick: (b: Backup) => void;
  onClose: () => void;
}) {
  const backups = useBackups(null);
  const fingerprints = useBuildFingerprints();
  const [scope, setScope] = useState<"project" | "all">(server.project_id === null ? "all" : "project");
  const buildFp = serverBuildFingerprint(server, fingerprints);
  const items = (backups.data?.items ?? []).filter((b) => scope === "all" || b.project_id === server.project_id);
  const projectName = backups.data?.items.find((b) => b.project_id === server.project_id)?.project_name;
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={`Choose a backup to restore into ${server.name}`}
      description="Backups of every server, including servers that were deleted since. A backup with the same schema as this server's build can be loaded by it."
      size="xl"
      footer={
        <Button variant="outline" onClick={onClose}>
          Cancel
        </Button>
      }
    >
      <div className="flex flex-col gap-4">
        <Field label="Show">
          {(p) => (
            <Select {...p} className="w-auto" value={scope} onChange={(e) => setScope(e.target.value as "project" | "all")}>
              {server.project_id !== null && <option value="project">Backups of this server&apos;s project{projectName ? ` (${projectName})` : ""}</option>}
              <option value="all">Backups of all projects</option>
            </Select>
          )}
        </Field>
        {backups.isPending ? (
          <LoadingState label="Loading backups" />
        ) : backups.isError ? (
          <ErrorState error={backups.error} title="Could not load the backups" onRetry={() => void backups.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState
            title="No backups"
            description={scope === "project" ? "No server of this project has a backup yet. Show the backups of all projects instead." : "No backup has been taken yet."}
          />
        ) : (
          <BackupsTable label="Backups to restore" backups={items} showServer compareWith={buildFp} onRestore={onPick} />
        )}
      </div>
    </Dialog>
  );
}
