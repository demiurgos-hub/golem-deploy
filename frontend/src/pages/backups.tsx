import { useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { useAccount, useBackups } from "@/api/queries";
import type { Backup, Job, Server } from "@/api/types.gen";
import { BackupsTable, DeleteBackupDialog, RestoreDialog } from "@/components/backups";
import { InlineJob } from "@/components/jobs";
import { Field, Select } from "@/components/ui/input";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, LoadingState } from "@/components/ui/states";
import { formatBytes } from "@/lib/format";

/** "all", "server:<id>" for an existing server, or "deleted:<name>" for a deleted one (its id is gone). */
type Filter = string;

function filterOf(b: Backup): Filter {
  return b.server_id === null ? `deleted:${b.server_name}` : `server:${b.server_id}`;
}

/** One filter option per server that has backups: existing servers by name, then deleted ones. */
export function serverFilterOptions(items: readonly Backup[]): { value: Filter; label: string }[] {
  const existing = new Map<string, string>();
  const deleted = new Map<string, string>();
  // Newest first: the first name seen is the server's current one.
  for (const b of items) {
    const key = filterOf(b);
    if (b.server_id === null) {
      if (!deleted.has(key)) deleted.set(key, `${b.server_name || "unnamed"} (deleted)`);
    } else if (!existing.has(key)) existing.set(key, b.server_name);
  }
  const sorted = (m: Map<string, string>) => [...m].sort((a, b) => a[1].localeCompare(b[1])).map(([value, label]) => ({ value, label }));
  return [...sorted(existing), ...sorted(deleted)];
}

export function BackupsPage() {
  const backups = useBackups(null);
  const account = useAccount();
  const isAdmin = account.data?.role === "admin";
  const [filter, setFilter] = useState<Filter>("all");
  const [restoring, setRestoring] = useState<Backup | null>(null);
  const [deleting, setDeleting] = useState<Backup | null>(null);
  const [started, setStarted] = useState<{ job: Job; server: Server } | null>(null);
  const items = useMemo(() => backups.data?.items ?? [], [backups.data]);
  const options = useMemo(() => serverFilterOptions(items), [items]);
  const shown = filter === "all" ? items : items.filter((b) => filterOf(b) === filter);

  return (
    <>
      <PageHeader
        title="Backups"
        description="Every backup of this installation. Backups outlive their servers: a backup of a deleted server can still be downloaded and restored into another server."
      />
      <div className="flex flex-col gap-4">
        {started && (
          <div className="flex flex-col gap-1">
            <p className="text-sm">
              Restoring into{" "}
              <Link to="/servers/$id" params={{ id: String(started.server.id) }} className="font-medium hover:underline">
                {started.server.name}
              </Link>
              :
            </p>
            <InlineJob key={started.job.id} job={started.job} onDismiss={() => setStarted(null)} />
          </div>
        )}
        {backups.isPending ? (
          <LoadingState label="Loading backups" />
        ) : backups.isError ? (
          <ErrorState error={backups.error} title="Could not load the backups" onRetry={() => void backups.refetch()} />
        ) : items.length === 0 ? (
          <EmptyState title="No backups yet" description="Back up a server from its page, or turn on its backup schedule there." />
        ) : (
          <>
            <div className="flex flex-wrap items-end justify-between gap-3">
              <Field label="Server" className="w-64 max-w-full">
                {(p) => (
                  <Select {...p} value={filter} onChange={(e) => setFilter(e.target.value)}>
                    <option value="all">All servers</option>
                    {options.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
              <p className="text-sm text-muted-foreground">
                {items.length} backup{items.length === 1 ? "" : "s"}, {formatBytes(backups.data.total_bytes)} stored in total. A backup holds at
                most {formatBytes(backups.data.max_content_bytes)} of server data.
              </p>
            </div>
            {!isAdmin && <p className="text-xs text-muted-foreground">Restoring and deleting backups need an administrator.</p>}
            <BackupsTable
              label="Backups"
              backups={shown}
              showServer
              onRestore={isAdmin ? setRestoring : undefined}
              onDelete={isAdmin ? setDeleting : undefined}
            />
          </>
        )}
      </div>
      {restoring && (
        <RestoreDialog backup={restoring} onClose={() => setRestoring(null)} onJob={(job, server) => setStarted({ job, server })} />
      )}
      <DeleteBackupDialog backup={deleting} onClose={() => setDeleting(null)} />
    </>
  );
}
