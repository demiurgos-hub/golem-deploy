import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ArchiveRestore, DatabaseBackup, Eraser, Rocket } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError, sentence } from "@/api/errors";
import { useBackups } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Backup, BackupSchedule, Job, Server, UpdateServerRequest } from "@/api/types.gen";
import { BackupsTable, DeleteBackupDialog, Fingerprint, SchemaMatchBadge, afterServerJob, schemaMatch } from "@/components/backups";
import { buildName } from "@/components/builds";
import { isContainerActive } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { Field, Input } from "@/components/ui/input";
import { DetailList, Section } from "@/components/ui/page";
import { EmptyState, ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatBytes, formatDateTime, formatDurationWords, formatRelative, shortFingerprint } from "@/lib/format";
import {
  BACKUP_KEEP_DEFAULT,
  BACKUP_NOTE_MAX,
  SNAPSHOT_INTERVAL_DEFAULT,
  validateBackupInterval,
  validateBackupKeep,
  validateBackupNote,
  trimBackupNote,
  validateSnapshotInterval,
} from "@/lib/validators";

/** The autosave period and backup schedule as typed (strings, so that a half-typed value can be shown and validated). */
export interface DataSettingsDraft {
  snapshot: string;
  interval: string;
  keep: string;
}

export type DataSettingsErrors = Partial<Record<keyof DataSettingsDraft, string | null>>;

export const DEFAULT_DATA_SETTINGS: DataSettingsDraft = { snapshot: String(SNAPSHOT_INTERVAL_DEFAULT), interval: "0", keep: String(BACKUP_KEEP_DEFAULT) };

export function dataSettingsOf(s: Pick<Server, "snapshot_interval_seconds" | "backup_schedule">): DataSettingsDraft {
  return { snapshot: String(s.snapshot_interval_seconds), interval: String(s.backup_schedule.interval_hours), keep: String(s.backup_schedule.keep) };
}

export function validateDataSettings(d: DataSettingsDraft): DataSettingsErrors {
  return { snapshot: validateSnapshotInterval(d.snapshot), interval: validateBackupInterval(d.interval), keep: validateBackupKeep(d.keep) };
}

export function hasDataSettingsErrors(e: DataSettingsErrors): boolean {
  return Boolean(e.snapshot || e.interval || e.keep);
}

const FIELD_OF: Record<string, keyof DataSettingsDraft> = {
  snapshot_interval_seconds: "snapshot",
  "backup_schedule.interval_hours": "interval",
  "backup_schedule.keep": "keep",
  backup_schedule: "interval",
};

/** The daemon's validation error of one of the settings, routed to its field; null for anything else. */
export function dataSettingsErrorsFrom(err: unknown): DataSettingsErrors | null {
  if (!isApiError(err) || err.code !== "validation_failed" || !err.field) return null;
  const key = FIELD_OF[err.field];
  return key ? { [key]: sentence(err.message) } : null;
}

export function scheduleOf(d: DataSettingsDraft): BackupSchedule {
  return { interval_hours: Number(d.interval.trim()), keep: Number(d.keep.trim()) };
}

function autosaveHint(raw: string): string {
  const n = Number(raw.trim());
  if (!/^\d+$/.test(raw.trim())) return "";
  return n === 0 ? "Autosave off: the game saves only when it stops and for backups." : `Saves every ${formatDurationWords(n)}.`;
}

/** The autosave period and backup schedule fields, shared by the server page and the new-server dialog. */
export function DataSettingsFields({
  value,
  onChange,
  errors,
  disabled,
  running,
}: {
  value: DataSettingsDraft;
  onChange: (d: DataSettingsDraft) => void;
  errors: DataSettingsErrors;
  disabled?: boolean;
  /** The server's container runs: say that the autosave period waits for a restart. */
  running?: boolean;
}) {
  const set = (k: keyof DataSettingsDraft) => (e: { target: { value: string } }) => onChange({ ...value, [k]: e.target.value });
  const scheduleOff = value.interval.trim() === "0";
  return (
    <div className="grid gap-4 sm:grid-cols-3">
      <Field
        label="Autosave period (seconds)"
        error={errors.snapshot}
        hint={
          <>
            How often the running game saves its world: 0, or 10 to 86,400. {autosaveHint(value.snapshot)}{" "}
            {running ? "Applies when the server is restarted." : "Applies on the next start."}
          </>
        }
      >
        {(p) => <Input {...p} inputMode="numeric" className="w-32" value={value.snapshot} disabled={disabled} onChange={set("snapshot")} />}
      </Field>
      <Field
        label="Back up every (hours)"
        error={errors.interval}
        hint="0 turns scheduled backups off, otherwise 1 to 720. A backup is taken while the server runs, this long after its previous backup. Applies at once."
      >
        {(p) => <Input {...p} inputMode="numeric" className="w-32" value={value.interval} disabled={disabled} onChange={set("interval")} />}
      </Field>
      <Field
        label="Scheduled backups to keep"
        error={errors.keep}
        hint={`1 to 100. After each scheduled backup the older ones are deleted; manual and safety backups are never deleted this way.${scheduleOff ? " Used once the schedule is on." : ""}`}
      >
        {(p) => <Input {...p} inputMode="numeric" className="w-32" value={value.keep} disabled={disabled} onChange={set("keep")} />}
      </Field>
    </div>
  );
}

function DataSettings({ server }: { server: Server }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const saved = dataSettingsOf(server);
  const [draft, setDraftState] = useState<DataSettingsDraft | null>(null);
  const [errors, setErrors] = useState<DataSettingsErrors>({});
  const value = draft ?? saved;
  const snapshotChanged = value.snapshot.trim() !== saved.snapshot;
  const scheduleChanged = value.interval.trim() !== saved.interval || value.keep.trim() !== saved.keep;
  const changed = draft !== null && (snapshotChanged || scheduleChanged);
  const setDraft = (d: DataSettingsDraft | null) => {
    setDraftState(d);
    setErrors({});
  };
  const save = useMutation({
    mutationFn: (body: UpdateServerRequest) => api.servers.update(server.id, body),
    onSuccess: (s, body) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      setDraft(null);
      const parts: string[] = [];
      if (body.snapshot_interval_seconds !== undefined) {
        parts.push(isContainerActive(s.state) ? "Autosave period saved; it applies when the server is restarted." : "Autosave period saved; it applies on the next start.");
      }
      if (body.backup_schedule) parts.push("Backup schedule saved; it applies now.");
      toast.success(parts.join(" "));
    },
    onError: (err) => {
      const routed = dataSettingsErrorsFrom(err);
      if (routed) setErrors(routed);
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = validateDataSettings(value);
    setErrors(errs);
    if (hasDataSettingsErrors(errs) || !changed) return;
    save.mutate({
      ...(snapshotChanged ? { snapshot_interval_seconds: Number(value.snapshot.trim()) } : {}),
      ...(scheduleChanged ? { backup_schedule: scheduleOf(value) } : {}),
    });
  }

  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-3" aria-label="Autosave and backup schedule">
      <h3 className="text-sm font-medium">Autosave and backup schedule</h3>
      <DataSettingsFields value={value} onChange={setDraft} errors={errors} disabled={save.isPending} running={isContainerActive(server.state)} />
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="sm" disabled={!changed} loading={save.isPending}>
          Save settings
        </Button>
        {changed && (
          <Button size="sm" variant="ghost" onClick={() => setDraft(null)} disabled={save.isPending}>
            Discard
          </Button>
        )}
      </div>
      {save.isError && !dataSettingsErrorsFrom(save.error) && <ErrorText error={save.error} />}
    </form>
  );
}

function BackUpNow({ server, busy, onJob }: { server: Server; busy: boolean; onJob: (job: Job) => void }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const [note, setNote] = useState("");
  const [noteError, setNoteError] = useState<string | null>(null);
  const backup = useMutation({
    mutationFn: (n: string) => api.servers.backup(server.id, n ? { note: n } : {}),
    onSuccess: (job) => {
      afterServerJob(queryClient, job, server.id);
      setNote("");
      onJob(job);
    },
    onError: (err) => {
      if (isApiError(err) && err.field === "note") setNoteError(sentence(err.message));
    },
  });
  const noData = server.data.volume === "";

  function submit(e: FormEvent) {
    e.preventDefault();
    const n = trimBackupNote(note);
    const err = validateBackupNote(n);
    setNoteError(err);
    if (!err) backup.mutate(n);
  }

  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-2" aria-label="Back up now">
      <h3 className="text-sm font-medium">Back up now</h3>
      <Field
          label="Note (optional)"
          error={noteError}
          hint={`A label for the backup, at most ${BACKUP_NOTE_MAX} characters. ${server.state === "running" ? "The running game writes a fresh save for it; players stay connected." : "The server is not running: its files are copied as they are."}`}
        >
          {(p) => (
            <Input
              {...p}
              value={note}
              maxLength={BACKUP_NOTE_MAX * 2}
              placeholder="before the map update"
              onChange={(e) => {
                setNote(e.target.value);
                setNoteError(null);
              }}
            />
          )}
        </Field>
      <div>
        <Button
          type="submit"
          size="sm"
          loading={backup.isPending}
          disabled={busy || noData}
          title={noData ? "The server has no saved data yet." : busy ? "Wait for the running job to finish." : undefined}
        >
          <DatabaseBackup />
          Back up now
        </Button>
      </div>
      {noData && <p className="text-xs text-muted-foreground">Nothing to back up yet: the game creates its world when the server first starts.</p>}
      {busy && !noData && <p className="text-xs text-muted-foreground">A job is running for this server; a backup can start once it ends.</p>}
      {backup.isError && !(isApiError(backup.error) && backup.error.field === "note") && <ErrorText error={backup.error} />}
    </form>
  );
}

/**
 * The server's saved data: where it lives, its schema compared with the
 * build's, the autosave and backup settings, backing up now and the
 * server's backups. Restores and resets are admin actions; the dialogs are
 * owned by the page so that the schema warning can open them too.
 */
export function ServerDataSection({
  server,
  buildFingerprint,
  busy,
  isAdmin,
  onJob,
  onRestore,
  onPickBackup,
  onReset,
}: {
  server: Server;
  buildFingerprint: string;
  busy: boolean;
  isAdmin: boolean;
  onJob: (job: Job) => void;
  onRestore: (b: Backup) => void;
  onPickBackup: () => void;
  onReset: () => void;
}) {
  const backups = useBackups(server.id);
  const [deleting, setDeleting] = useState<Backup | null>(null);
  const d = server.data;
  const match = schemaMatch(d.schema_fingerprint, buildFingerprint);
  return (
    <Section
      title="Saved data and backups"
      description="The game keeps its world on a Docker volume of this server, so it survives restarts and new builds. Deleting the server deletes the volume; backups are kept."
      actions={
        isAdmin ? (
          <>
            <Button size="sm" variant="outline" onClick={onPickBackup} disabled={busy} title={busy ? "Wait for the running job to finish." : undefined}>
              <ArchiveRestore />
              Restore from any backup
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={onReset}
              disabled={busy || d.volume === ""}
              title={d.volume === "" ? "The server has no saved data." : busy ? "Wait for the running job to finish." : undefined}
            >
              <Eraser />
              Reset data
            </Button>
          </>
        ) : undefined
      }
    >
      <div className="flex flex-col gap-6">
        <DetailList
          items={[
            {
              term: "Saved world",
              value: d.volume ? (
                <span>
                  On volume <code className="break-all font-mono text-xs">{d.volume}</code>
                </span>
              ) : (
                <span className="text-muted-foreground">None yet. The game creates its world when the server starts.</span>
              ),
            },
            {
              term: "World schema",
              value: (
                <span className="flex flex-wrap items-center gap-2">
                  <Fingerprint value={d.schema_fingerprint} unknown={d.volume ? "Not known yet; the game reports it once it is ready" : "-"} />
                  {match !== "unknown" && <SchemaMatchBadge match={match} />}
                </span>
              ),
            },
            {
              term: "Build schema",
              value:
                server.build_id === null ? (
                  <span className="text-muted-foreground">No build</span>
                ) : (
                  <span className="flex flex-wrap items-center gap-2">
                    <Fingerprint value={buildFingerprint} unknown="Not known yet" />
                    <span className="text-xs text-muted-foreground">build {buildName({ id: server.build_id, commit: server.commit })}</span>
                  </span>
                ),
            },
            {
              term: "Last backup",
              value: d.last_backup_at ? <span title={formatDateTime(d.last_backup_at)}>{formatRelative(d.last_backup_at)}</span> : "Never",
            },
            {
              term: "Schedule",
              value:
                server.backup_schedule.interval_hours === 0
                  ? "Off"
                  : `Every ${server.backup_schedule.interval_hours} hour${server.backup_schedule.interval_hours === 1 ? "" : "s"} while running, keeping ${server.backup_schedule.keep}`,
            },
          ]}
        />
        <BackUpNow server={server} busy={busy} onJob={onJob} />
        <DataSettings server={server} />
        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">Backups of this server</h3>
          {!isAdmin && <p className="text-xs text-muted-foreground">Restoring a backup, resetting the data and deleting backups need an administrator.</p>}
          {backups.isPending ? (
            <LoadingState label="Loading backups" />
          ) : backups.isError ? (
            <ErrorState error={backups.error} title="Could not load the backups" onRetry={() => void backups.refetch()} />
          ) : backups.data.items.length === 0 ? (
            <EmptyState title="No backups yet" description="Back up now, or turn on the schedule above." />
          ) : (
            <>
              <BackupsTable
                label="Backups of this server"
                backups={backups.data.items}
                compareWith={buildFingerprint}
                onRestore={isAdmin ? onRestore : undefined}
                onDelete={isAdmin ? setDeleting : undefined}
              />
              <p className="text-xs text-muted-foreground">
                {backups.data.items.length} backup{backups.data.items.length === 1 ? "" : "s"},{" "}
                {formatBytes(backups.data.items.reduce((n, b) => n + b.size_bytes, 0))}. A backup holds at most {formatBytes(backups.data.max_content_bytes)} of data.
              </p>
            </>
          )}
        </div>
      </div>
      <DeleteBackupDialog backup={deleting} onClose={() => setDeleting(null)} />
    </Section>
  );
}

/**
 * Shown while the saved world has another schema than the server's build:
 * the server will not start, and the three ways out. Restore and reset are
 * admin actions.
 */
export function SchemaMismatchNotice({
  server,
  buildFingerprint,
  isAdmin,
  busy,
  onDeploy,
  onPickBackup,
  onReset,
}: {
  server: Server;
  buildFingerprint: string;
  isAdmin: boolean;
  busy: boolean;
  onDeploy: () => void;
  onPickBackup: () => void;
  onReset: () => void;
}) {
  const saved = server.data.schema_fingerprint;
  return (
    <Notice tone="danger" title="This server will not start with its saved world">
      <span className="flex flex-col gap-2">
        <span>
          The world was saved by a game with schema <code className="font-mono text-xs" title={saved}>{shortFingerprint(saved)}</code>, but build{" "}
          {server.build_id !== null ? buildName({ id: server.build_id, commit: server.commit }) : ""} has schema{" "}
          <code className="font-mono text-xs" title={buildFingerprint}>{shortFingerprint(buildFingerprint)}</code>. The game cannot load a world
          saved with other schemas, so starting or restarting the server fails before the running container is touched. Choose one:
        </span>
        <ul className="flex flex-col gap-2">
          <li className="flex flex-wrap items-center justify-between gap-2">
            <span>Deploy a build of the project that has the saved schema.</span>
            <Button size="sm" variant="outline" onClick={onDeploy}>
              <Rocket />
              Deploy another build
            </Button>
          </li>
          <li className="flex flex-wrap items-center justify-between gap-2">
            <span>Restore a backup that has this build&apos;s schema.</span>
            {isAdmin && (
              <Button size="sm" variant="outline" onClick={onPickBackup} disabled={busy}>
                <ArchiveRestore />
                Restore a backup
              </Button>
            )}
          </li>
          <li className="flex flex-wrap items-center justify-between gap-2">
            <span>Reset the data: the saved world is backed up, then discarded, and the game starts with an empty one.</span>
            {isAdmin && (
              <Button size="sm" variant="outline" onClick={onReset} disabled={busy}>
                <Eraser />
                Reset data
              </Button>
            )}
          </li>
        </ul>
        {!isAdmin && <span className="text-xs">Restoring a backup and resetting the data need an administrator.</span>}
      </span>
    </Notice>
  );
}
