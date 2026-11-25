import { useMemo, useState, type ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArchiveRestore, Download, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { API_BASE } from "@/api/client";
import { ApiError, describeError, isApiError } from "@/api/errors";
import { useBackups, useServers, useSuccessfulBuilds } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Backup, BackupKind, Job, RestoreServerRequest, Server } from "@/api/types.gen";
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
  pre_delete: "Before a deletion",
};

const KIND_HINTS: Record<BackupKind, string> = {
  manual: "Taken with Back up now. Kept until an administrator deletes it.",
  scheduled: "Taken by the server's backup schedule. Only the newest scheduled backups are kept.",
  pre_restore: "The data the server had before a backup was restored into it. Kept until an administrator deletes it.",
  pre_reset: "The data the server had before its data was reset. Kept until an administrator deletes it.",
  pre_delete: "The last data of a server, stored before the server was deleted. Kept until an administrator deletes it.",
};

const NOT_LIVE_HINT =
  "The game did not write a fresh save for this backup: its files were copied as they were (the server was not running, or the game has no snapshot hook).";

/** Why a backup can be incomplete, shown with its omitted-entries count. */
export const OMITTED_HINT =
  "Links, pipes, devices and files whose names are not allowed cannot be backed up. This backup leaves them out, so restoring it does not bring them back.";

/** "3 entries left out": what a backup leaves out of the data directory. */
export function omittedEntriesLabel(n: number): string {
  return `${n.toLocaleString()} ${n === 1 ? "entry" : "entries"} left out`;
}

/** Archives above this size get a warning before a browser download (the browser holds the whole file in memory). */
export const LARGE_DOWNLOAD_BYTES = 512 * 1024 * 1024;

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
  const [large, setLarge] = useState<Backup | null>(null);
  const columns: Column<Backup>[] = [
    {
      key: "taken",
      header: "Taken",
      sortValue: (b) => b.created_at,
      cell: (b) => (
        <span className="flex flex-col whitespace-nowrap">
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
            className: "min-w-24",
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
        <span className="flex flex-col whitespace-nowrap" title={KIND_HINTS[b.kind]}>
          <span>{backupKindLabel(b.kind)}</span>
          {!b.live && (
            <span className="text-xs text-muted-foreground" title={NOT_LIVE_HINT}>
              No fresh save
            </span>
          )}
        </span>
      ),
    },
    {
      key: "size",
      header: "Size",
      sortValue: (b) => b.size_bytes,
      cell: (b) => (
        <span className="flex flex-col whitespace-nowrap">
          <span className="tabular-nums" title={`${b.file_count.toLocaleString()} files, ${formatBytes(b.content_bytes)} before compression`}>
            {formatBytes(b.size_bytes)}
          </span>
          {b.omitted_entries > 0 && (
            <span className="text-xs text-amber-700 dark:text-amber-400" title={OMITTED_HINT}>
              {omittedEntriesLabel(b.omitted_entries)}
            </span>
          )}
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
      className: "min-w-32 max-w-64",
      cell: (b) => (b.note ? <span className="break-words">{b.note}</span> : <span className="text-muted-foreground">-</span>),
    },
  ];
  // The installation-wide table has two more columns: its actions show
  // only their icons until the window is wide enough for the words, so they
  // are not pushed past the right edge.
  const actionText = showServer ? "hidden 2xl:inline" : undefined;
  return (
    <>
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
              title="Download"
              loading={download.isPending && download.variables?.id === b.id}
              onClick={() => (b.size_bytes > LARGE_DOWNLOAD_BYTES ? setLarge(b) : download.mutate(b))}
            >
              {!(download.isPending && download.variables?.id === b.id) && <Download />}
              <span className={actionText}>Download</span>
            </Button>
            {onRestore && (
              <Button size="sm" variant="outline" aria-label={`Restore backup ${b.id}`} title="Restore" onClick={() => onRestore(b)}>
                <ArchiveRestore />
                <span className={actionText}>Restore</span>
              </Button>
            )}
            {onDelete && (
              <Button size="sm" variant="outline" aria-label={`Delete backup ${b.id}`} title="Delete" onClick={() => onDelete(b)}>
                <Trash2 />
                <span className={actionText}>Delete</span>
              </Button>
            )}
          </>
        )}
      />
      {large && <LargeDownloadDialog backup={large} onClose={() => setLarge(null)} onDownload={() => download.mutate(large)} />}
    </>
  );
}

/** Warns before a browser download of a large archive and shows how to fetch it with curl instead. */
function LargeDownloadDialog({ backup, onClose, onDownload }: { backup: Backup; onClose: () => void; onDownload: () => void }) {
  const url = new URL(`${API_BASE}/backups/${backup.id}/download`, window.location.origin).href;
  const command = `curl -fL -H "Authorization: Bearer $TOKEN" -o backup-${backup.id}.tar.gz ${url}`;
  return (
    <ConfirmDialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="Download a large backup"
      description={
        <span className="flex flex-col gap-2">
          <span>
            This archive is {formatBytes(backup.size_bytes)}. The browser keeps a download in memory until it is complete, so a
            download this large may fail in the browser.
          </span>
          <span>
            Large archives are best downloaded with an API token (create one on your account page) and curl, with TOKEN set to the
            token&apos;s secret:
          </span>
          <span className="flex items-start gap-1">
            <code className="min-w-0 flex-1 break-all rounded-md bg-muted px-2 py-1 font-mono text-xs text-foreground">{command}</code>
            <CopyButton value={command} size="icon" variant="ghost" label="Copy the curl command" />
          </span>
        </span>
      }
      confirmLabel="Download in the browser"
      onConfirm={onDownload}
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

const SKIP_SAFETY_HINT =
  "The current data is then discarded without a copy and cannot be brought back. Use this only when the disk has no room for another backup, or when the data holds entries that cannot be backed up and losing them is acceptable.";

/** The secondary "skip the safety backup" option, kept out of the way under a disclosure. */
function SkipSafetyBackup({
  checked,
  onChange,
  disabled,
  label = "Skip the safety backup",
  hint = SKIP_SAFETY_HINT,
  expanded = false,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  label?: string;
  hint?: ReactNode;
  /** Keeps the disclosure open, for example after the daemon refused the action without the option. */
  expanded?: boolean;
}) {
  return (
    <details className="rounded-md border border-border px-3 py-2 text-sm" open={checked || expanded || undefined}>
      <summary className="cursor-pointer text-muted-foreground">More options</summary>
      <div className="mt-2 flex flex-col gap-1">
        <Checkbox label={label} checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
        <p className="pl-6 text-xs text-muted-foreground">{hint}</p>
      </div>
    </details>
  );
}

/** The incomplete-backup warning of the restore dialog. */
function OmittedEntriesNotice({ backup }: { backup: Backup }) {
  if (backup.omitted_entries === 0) return null;
  return (
    <Notice tone="warning" title={`This backup is incomplete: ${omittedEntriesLabel(backup.omitted_entries)}`}>
      {OMITTED_HINT}
    </Notice>
  );
}

export function afterServerJob(queryClient: ReturnType<typeof useQueryClient>, job: Job, serverId: number) {
  queryClient.setQueryData(qk.jobs.detail(job.id), job);
  void queryClient.invalidateQueries({ queryKey: qk.servers.all });
  void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("server", serverId) });
}

/** What an admin agreed to in the restore dialog, for one set of facts (`key`), and the daemon's schema refusal for them. */
interface Consent {
  key: string;
  force: boolean;
  skipSafety: boolean;
  refusal: ApiError | null;
}

function noConsent(key: string): Consent {
  return { key, force: false, skipSafety: false, refusal: null };
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
  // The chosen server: undefined until the list first loads (then the default target), null when none is chosen.
  const [choice, setChoice] = useState<number | null | undefined>(target ? target.id : undefined);
  if (!target && choice === undefined && servers.data) {
    const initial = defaultTarget(backup, all);
    if (initial) setChoice(initial.id);
  }
  const server = target ?? (choice == null ? null : (all.find((s) => s.id === choice) ?? null));
  // The chosen server left the list while the dialog was open: never fall back to another one silently.
  const lost = !target && choice != null && servers.data !== undefined && server === null;
  const buildFp = server ? serverBuildFingerprint(server, fingerprints) : "";
  // The facts a confirmation is given for. When any of them changes, every tick and the daemon's refusal are cleared.
  const factsKey = server ? `${server.id}:${server.build_id ?? ""}:${backup.schema_fingerprint}:${buildFp}` : "";
  const [consent, setConsent] = useState<Consent>(() => noConsent(factsKey));
  if (consent.key !== factsKey) setConsent(noConsent(factsKey));
  const { force, skipSafety, refusal } = consent.key === factsKey ? consent : noConsent(factsKey);
  const known = schemaMatch(backup.schema_fingerprint, buildFp);
  const mismatch = known === "differs" || refusal !== null;
  const noBuild = server !== null && server.build_id === null;
  const setForce = (v: boolean) => setConsent((c) => ({ ...c, force: v }));
  const setSkipSafety = (v: boolean) => setConsent((c) => ({ ...c, skipSafety: v }));

  const restore = useMutation({
    mutationFn: (v: { server: Server; body: RestoreServerRequest; key: string }) => api.servers.restore(v.server.id, v.body),
    onSuccess: (job, { server: s }) => {
      afterServerJob(queryClient, job, s.id);
      toast.success(`Restoring the backup into ${s.name}.`);
      onJob(job, s);
      onClose();
    },
    onError: (err, { key }) => {
      if (isApiError(err) && err.code === "schema_mismatch") {
        setConsent((c) => (c.key === key ? { ...c, force: false, refusal: err } : c));
      }
    },
  });
  const otherError = restore.isError && !(isApiError(restore.error) && restore.error.code === "schema_mismatch") ? restore.error : null;

  function pick(id: number) {
    setChoice(id);
    restore.reset();
  }

  function submit() {
    if (!server) return;
    restore.mutate({
      server,
      key: factsKey,
      body: {
        backup_id: backup.id,
        ...(mismatch && force ? { force: true } : {}),
        ...(skipSafety ? { skip_safety_backup: true } : {}),
      },
    });
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
            onClick={submit}
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
                  {server === null && (
                    <option value="" disabled>
                      Choose a server
                    </option>
                  )}
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
        {lost && (
          <Notice tone="warning" title="The chosen server is no longer listed">
            It was deleted while this dialog was open. Choose the server to restore into again.
          </Notice>
        )}
        <OmittedEntriesNotice backup={backup} />
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
                      : "Its current data is backed up first (a backup of kind “before a restore”), so this can be undone. If that backup would leave entries out, the restore stops before anything is replaced."}
                  </li>
                  <li>The backup&apos;s data replaces it.</li>
                  <li>
                    {mismatch
                      ? "The server stays stopped until a build with the backup's schema is assigned to it."
                      : server.desired_state === "running"
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
              : "Its saved data is backed up first (a backup of kind “before a reset”), then discarded. If that backup would leave entries out, the reset stops before anything is discarded."}
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

/** The daemon refused a deletion because the last backup could not be taken or would be incomplete (nothing was deleted). */
export function isLastBackupRefusal(err: unknown): boolean {
  return isApiError(err) && err.code === "conflict";
}

/**
 * Confirms deleting a server. A server with saved data gets a last backup
 * first (kind "pre_delete"); admins may skip it under "More options", which
 * sends `skip_safety_backup=1`. `onDelete` rejects with the daemon's error to
 * keep the dialog open (a refused last backup), and resolves otherwise.
 */
export function DeleteServerDialog({
  server,
  isAdmin,
  onClose,
  onDelete,
}: {
  server: Server;
  isAdmin: boolean;
  onClose: () => void;
  onDelete: (skipLastBackup: boolean) => Promise<void>;
}) {
  const [skip, setSkip] = useState(false);
  const [refused, setRefused] = useState(false);
  const hasData = server.data.volume !== "";
  return (
    <ConfirmDialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="Delete server"
      description={
        <>
          Delete <span className="font-medium text-foreground">{server.name}</span>?{" "}
          {!hasData
            ? "It has no saved world, so there is nothing to back up."
            : skip
              ? "Its saved world is not backed up: it is deleted with the server and cannot be brought back."
              : "A last backup of its saved world is stored first (a backup of kind “before a deletion”) and kept on the Backups page; then the world is deleted with the server."}{" "}
          The container is stopped gracefully (this can take up to 30 seconds) and removed, its stored environment is deleted and
          port {server.port} is freed. Its backups are kept: they stay on the Backups page and can be restored into another server.
        </>
      }
      confirmLabel="Delete server"
      destructive
      onConfirm={async () => {
        try {
          await onDelete(hasData && skip);
        } catch (err) {
          if (isLastBackupRefusal(err)) setRefused(true);
          throw err;
        }
      }}
      errorHint={(err) =>
        isLastBackupRefusal(err) && hasData ? (
          <p className="text-xs text-muted-foreground">
            Nothing was deleted.{" "}
            {isAdmin
              ? "To delete the server anyway, tick “Skip the last backup” under More options: its saved world is then lost."
              : "An administrator can delete the server without the last backup."}
          </p>
        ) : null
      }
    >
      {isAdmin && hasData && (
        <SkipSafetyBackup
          label="Skip the last backup"
          hint="The saved world is then deleted without a copy and cannot be brought back. Use this only when the last backup cannot be taken, for example when the disk has no room or the world holds entries that cannot be backed up."
          checked={skip}
          onChange={setSkip}
          expanded={refused}
        />
      )}
    </ConfirmDialog>
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
