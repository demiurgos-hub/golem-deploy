import { useRef, useState, type DragEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Download, Trash2, Upload, X } from "lucide-react";
import { useApi } from "@/app/services";
import { MAX_CONCURRENT_UPLOADS } from "@/api/client";
import { describeError, isAbortError, isApiError } from "@/api/errors";
import { useStaticFiles } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { StaticFile, StaticFileList } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { PageHeader, Section } from "@/components/ui/page";
import { EmptyState, ErrorState, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { saveBlob } from "@/lib/download";
import { formatBytes, formatDateTime, formatRelative } from "@/lib/format";
import { cn, localId } from "@/lib/utils";
import { STATIC_UPLOAD_LIMIT_BYTES, validateStaticName } from "@/lib/validators";

type UploadState = "invalid" | "ready" | "waiting" | "uploading" | "done" | "failed" | "unknown" | "cancelled" | "conflict";

interface UploadItem {
  id: string;
  file: File;
  state: UploadState;
  loaded: number;
  error: string | null;
  /** The name existed when the file was queued: uploading it replaces that file (after confirmation). */
  replaces: boolean;
}

/** How often a 429 (another upload of this account, e.g. from another tab) is waited out before giving up. */
const RATE_LIMIT_RETRIES = 5;

/** Client-side checks that mirror the daemon's rules, so bad files fail before uploading. */
export function checkUploadFile(file: File): string | null {
  const nameError = validateStaticName(file.name);
  if (nameError) return nameError;
  if (file.size > STATIC_UPLOAD_LIMIT_BYTES) return `Larger than the 1 GiB upload limit (${formatBytes(file.size)}).`;
  return null;
}

/** What a set of queued files needs from the store: bytes added (net of replaced files) and new names. */
export function uploadNeeds(
  items: readonly { file: File }[],
  stored: readonly StaticFile[],
): { bytes: number; newFiles: number } {
  const sizes = new Map(stored.map((f) => [f.name, f.size_bytes]));
  let bytes = 0;
  let newFiles = 0;
  for (const { file } of items) {
    const old = sizes.get(file.name);
    bytes += file.size - (old ?? 0);
    if (old === undefined) newFiles++;
  }
  return { bytes, newFiles };
}

/**
 * Why queued files obviously cannot fit, from the store's usage (the daemon
 * checks again, against the bytes it receives, and also needs 1 GiB free on
 * the host's disk, which the client cannot see). Null when nothing is known
 * to be wrong.
 */
export function fitWarning(list: StaticFileList, items: readonly { file: File }[]): string | null {
  if (items.length === 0) return null;
  const { bytes, newFiles } = uploadNeeds(items, list.items);
  const problems: string[] = [];
  if (list.quota_bytes > 0 && list.used_bytes + bytes > list.quota_bytes) {
    const left = Math.max(0, list.quota_bytes - list.used_bytes);
    problems.push(`they need ${formatBytes(Math.max(0, bytes))} but only ${formatBytes(left)} of the ${formatBytes(list.quota_bytes)} quota is left`);
  }
  if (list.max_files > 0 && list.file_count + newFiles > list.max_files) {
    problems.push(`they add ${newFiles} files but the store holds at most ${list.max_files.toLocaleString()} (${list.file_count.toLocaleString()} now)`);
  }
  if (problems.length === 0) return null;
  return `These uploads cannot all fit: ${problems.join("; ")}. Files that do not fit are rejected with "insufficient storage"; delete files or upload fewer.`;
}

/** Runs fn over items with at most `limit` in flight, in order. */
async function runPool<T>(items: readonly T[], limit: number, fn: (item: T) => Promise<void>) {
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const item = items[next++];
      await fn(item);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
}

const sleep = (ms: number, signal: AbortSignal) =>
  new Promise<void>((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(t);
      resolve();
    });
  });

export function StaticFilesPage() {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const files = useStaticFiles();
  const [queue, setQueue] = useState<UploadItem[]>([]);
  const [confirmReplace, setConfirmReplace] = useState(false);
  const [deleting, setDeleting] = useState<StaticFile | null>(null);
  const [dragging, setDragging] = useState(false);
  const [running, setRunning] = useState(false);
  const controllers = useRef(new Map<string, AbortController>());
  // Queued items cancelled before their turn; the upload loop skips them.
  const cancelled = useRef(new Set<string>());
  const inputRef = useRef<HTMLInputElement>(null);

  const existing = new Set((files.data?.items ?? []).map((f) => f.name));
  const ready = queue.filter((q) => q.state === "ready");
  const replacing = ready.filter((q) => q.replaces);
  const warning = files.data ? fitWarning(files.data, ready) : null;

  function addFiles(list: FileList | null) {
    if (!list || list.length === 0) return;
    // Copy now: the FileList is live and the input is reset right after this call,
    // while the state updater below may run later.
    const added = Array.from(list);
    setQueue((q) => {
      const names = new Set(q.filter((i) => i.state === "ready" || i.state === "uploading" || i.state === "waiting").map((i) => i.file.name));
      const next: UploadItem[] = [];
      for (const file of added) {
        let error = checkUploadFile(file);
        if (!error && names.has(file.name)) error = "A file with this name is already queued.";
        names.add(file.name);
        next.push({
          id: localId("upload"),
          file,
          state: error ? "invalid" : "ready",
          loaded: 0,
          error,
          replaces: existing.has(file.name),
        });
      }
      return [...q, ...next];
    });
  }

  const update = (id: string, patch: Partial<UploadItem>) =>
    setQueue((q) => q.map((i) => (i.id === id ? { ...i, ...patch } : i)));

  /**
   * Uploads one file per request, so each row's status is exactly the
   * outcome of its own request: the daemon commits a request all or
   * nothing (docs/API.md, Atomicity), so a failed row stored nothing and
   * replaced nothing. Up to MAX_CONCURRENT_UPLOADS requests run at once
   * (the API client enforces the cap for the whole tab); a 429 caused by
   * uploads elsewhere is waited out. Only files confirmed as replacements
   * are sent with replace=1; any other name that exists by now comes back
   * as 409 conflict and is offered for replacement on its row.
   */
  async function upload(item: UploadItem): Promise<boolean> {
    if (cancelled.current.has(item.id)) return false;
    const controller = new AbortController();
    controllers.current.set(item.id, controller);
    try {
      for (let attempt = 0; ; attempt++) {
        update(item.id, { state: "uploading", loaded: 0, error: null });
        try {
          await api.staticFiles.upload([item.file], {
            replace: item.replaces,
            signal: controller.signal,
            onProgress: (loaded) => update(item.id, { loaded }),
          });
          update(item.id, { state: "done", loaded: item.file.size });
          void queryClient.invalidateQueries({ queryKey: qk.staticFiles });
          return true;
        } catch (err) {
          if (isAbortError(err)) {
            update(item.id, { state: "cancelled", error: null });
          } else if (isApiError(err) && err.code === "rate_limited" && attempt < RATE_LIMIT_RETRIES) {
            const wait = Math.max(1, err.retryAfter ?? 1);
            update(item.id, { state: "waiting", error: "Another upload of your account is running (another tab or tool); waiting for it." });
            await sleep(wait * 1000, controller.signal);
            if (controller.signal.aborted) {
              update(item.id, { state: "cancelled", error: null });
              return false;
            }
            continue;
          } else if (isApiError(err) && err.code === "conflict") {
            update(item.id, { state: "conflict", error: "A file with this name was stored in the meantime. Replace it?" });
            void queryClient.invalidateQueries({ queryKey: qk.staticFiles });
          } else if (isApiError(err) && err.code === "insufficient_storage") {
            update(item.id, { state: "failed", error: `${describeError(err)} The file was not stored.` });
            void queryClient.invalidateQueries({ queryKey: qk.staticFiles });
          } else if (isApiError(err) && (err.status >= 500 || err.code === "network_error")) {
            // The request may have been committed before the failure was reported.
            update(item.id, {
              state: "unknown",
              error: `${describeError(err)} Whether the file was stored is not known; check the list of stored files.`,
            });
            void queryClient.invalidateQueries({ queryKey: qk.staticFiles });
          } else {
            update(item.id, { state: "failed", error: `${describeError(err)} The file was not stored.` });
          }
          return false;
        }
      }
    } finally {
      controllers.current.delete(item.id);
    }
  }

  async function runUploads(items: UploadItem[]) {
    setRunning(true);
    let ok = 0;
    for (const item of items) if (!cancelled.current.has(item.id)) update(item.id, { state: "waiting", error: null });
    await runPool(items, MAX_CONCURRENT_UPLOADS, async (item) => {
      if (await upload(item)) ok++;
    });
    setRunning(false);
    if (ok > 0) toast.success(`Uploaded ${ok} file${ok === 1 ? "" : "s"}.`);
  }

  function start() {
    if (replacing.length > 0) setConfirmReplace(true);
    else void runUploads(ready);
  }

  function cancel(item: UploadItem) {
    const controller = controllers.current.get(item.id);
    if (controller) {
      controller.abort();
    } else {
      cancelled.current.add(item.id);
      update(item.id, { state: "cancelled", error: null });
    }
  }

  const download = useMutation({
    mutationFn: async (name: string) => {
      const { blob } = await api.staticFiles.download(name);
      saveBlob(blob, name);
    },
    onError: (err, name) => toast.error(err, `Could not download ${name}`),
  });

  function onDrop(e: DragEvent) {
    e.preventDefault();
    setDragging(false);
    addFiles(e.dataTransfer.files);
  }

  const remaining = files.data && files.data.quota_bytes > 0 ? Math.max(0, files.data.quota_bytes - files.data.used_bytes) : null;

  return (
    <>
      <PageHeader title="Static files" description="Files made available to project builds, such as assets too large for the repository." />
      <div className="flex flex-col gap-6">
        {files.data && <StorageUsage list={files.data} />}
        <Section
          title="Upload"
          description="Names may use letters, digits, '.', '_' and '-' (up to 128 characters, starting with a letter or digit). Up to 1 GiB per file. Existing files are only replaced after you confirm."
        >
          <div
            onDragOver={(e) => {
              e.preventDefault();
              setDragging(true);
            }}
            onDragLeave={() => setDragging(false)}
            onDrop={onDrop}
            className={cn(
              "flex flex-col items-center gap-2 rounded-md border border-dashed px-4 py-6 text-center text-sm",
              dragging ? "border-primary bg-accent" : "border-border",
            )}
          >
            <Upload aria-hidden className="size-5 text-muted-foreground" />
            <p className="text-muted-foreground">Drop files here, or</p>
            <Button size="sm" variant="outline" onClick={() => inputRef.current?.click()}>
              Choose files
            </Button>
            <input
              ref={inputRef}
              type="file"
              multiple
              className="sr-only"
              tabIndex={-1}
              aria-hidden
              onChange={(e) => {
                addFiles(e.target.files);
                e.target.value = "";
              }}
            />
          </div>
          {queue.length > 0 && (
            <div className="mt-4 flex flex-col gap-3">
              {warning && (
                <Notice tone="warning" title="Not enough room">
                  {warning}
                </Notice>
              )}
              <ul className="flex flex-col divide-y divide-border rounded-md border border-border" aria-label="Upload queue">
                {queue.map((item) => (
                  <UploadRow
                    key={item.id}
                    item={item}
                    tooLarge={
                      item.state === "ready" && remaining !== null && uploadNeeds([item], files.data?.items ?? []).bytes > remaining
                    }
                    onCancel={() => cancel(item)}
                    onReplace={running ? undefined : () => void runUploads([{ ...item, replaces: true }])}
                  />
                ))}
              </ul>
              <p className="text-xs text-muted-foreground">
                Each file is sent as its own request, {MAX_CONCURRENT_UPLOADS} at a time; a file that fails is not stored and
                replaces nothing.
              </p>
              <div className="flex flex-wrap gap-2">
                <Button onClick={start} disabled={ready.length === 0 || running} loading={running}>
                  {!running && <Upload />}
                  Upload {ready.length > 0 ? `${ready.length} file${ready.length === 1 ? "" : "s"}` : ""}
                </Button>
                <Button
                  variant="outline"
                  disabled={running}
                  onClick={() => setQueue((q) => q.filter((i) => i.state === "ready" || i.state === "uploading" || i.state === "waiting"))}
                >
                  Clear finished
                </Button>
              </div>
            </div>
          )}
        </Section>

        <Section title="Stored files">
          {files.isPending ? (
            <LoadingState label="Loading files" />
          ) : files.isError ? (
            <ErrorState error={files.error} title="Could not load static files" onRetry={() => void files.refetch()} />
          ) : files.data.items.length === 0 ? (
            <EmptyState title="No static files" description="Uploaded files appear here." />
          ) : (
            <DataTable
              label="Static files"
              data={files.data.items}
              rowKey={(f) => f.name}
              initialSort={{ key: "name", asc: true }}
              columns={[
                { key: "name", header: "Name", sortValue: (f) => f.name, cell: (f) => <span className="font-mono text-xs">{f.name}</span> },
                { key: "size", header: "Size", sortValue: (f) => f.size_bytes, cell: (f) => formatBytes(f.size_bytes) },
                {
                  key: "modified",
                  header: "Modified",
                  sortValue: (f) => f.modified_at,
                  cell: (f) => <span title={formatDateTime(f.modified_at)}>{formatRelative(f.modified_at)}</span>,
                },
                {
                  key: "kind",
                  header: "Type",
                  hideOnSmall: true,
                  sortValue: (f) => (f.is_zip ? 0 : 1),
                  cell: (f) =>
                    f.is_zip ? (
                      <Badge tone="info" title="A ZIP archive: builds can extract it as a static add-on.">
                        ZIP archive
                      </Badge>
                    ) : (
                      <span className="text-muted-foreground">File</span>
                    ),
                },
              ]}
              actions={(f) => (
                <>
                  <Button
                    size="sm"
                    variant="outline"
                    aria-label={`Download ${f.name}`}
                    loading={download.isPending && download.variables === f.name}
                    onClick={() => download.mutate(f.name)}
                  >
                    {!(download.isPending && download.variables === f.name) && <Download />}
                    Download
                  </Button>
                  <Button size="sm" variant="outline" aria-label={`Delete ${f.name}`} onClick={() => setDeleting(f)}>
                    <Trash2 />
                  </Button>
                </>
              )}
            />
          )}
        </Section>
      </div>

      <ConfirmDialog
        open={confirmReplace}
        onOpenChange={setConfirmReplace}
        title="Replace existing files"
        description={
          <>
            <p>These files already exist and will be replaced:</p>
            <ul className="mt-2 list-disc pl-5 font-mono text-xs text-foreground">
              {replacing.map((i) => (
                <li key={i.id}>{i.file.name}</li>
              ))}
            </ul>
          </>
        }
        confirmLabel="Replace and upload"
        onConfirm={() => {
          void runUploads(ready);
        }}
      />
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(o) => !o && setDeleting(null)}
        title="Delete static file"
        description={
          <>
            Delete <span className="font-mono text-foreground">{deleting?.name}</span> ({formatBytes(deleting?.size_bytes ?? 0)})?
            Builds that use it will fail until it is uploaded again.
          </>
        }
        confirmLabel="Delete file"
        destructive
        onConfirm={async () => {
          if (!deleting) return;
          await api.staticFiles.remove(deleting.name);
          toast.success(`${deleting.name} deleted.`);
          await queryClient.invalidateQueries({ queryKey: qk.staticFiles });
        }}
      />
    </>
  );
}

/** The store's usage against its quota and file cap, with exact numbers. */
export function StorageUsage({ list }: { list: StaticFileList }) {
  const limited = list.quota_bytes > 0;
  const pct = limited ? Math.min(100, (list.used_bytes / list.quota_bytes) * 100) : 0;
  const tone = pct >= 95 ? "bg-destructive" : pct >= 80 ? "bg-amber-500" : "bg-primary";
  const exact = (n: number) => `${n.toLocaleString()} bytes`;
  return (
    <section aria-label="Storage usage" className="flex flex-col gap-2 rounded-md border border-border px-4 py-3">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-sm">
        <span>
          <span className="font-medium" title={exact(list.used_bytes)}>
            {formatBytes(list.used_bytes)}
          </span>{" "}
          {limited ? (
            <>
              of{" "}
              <span className="font-medium" title={exact(list.quota_bytes)}>
                {formatBytes(list.quota_bytes)}
              </span>{" "}
              used ({formatBytes(Math.max(0, list.quota_bytes - list.used_bytes))} left)
            </>
          ) : (
            "used, no size limit"
          )}
        </span>
        <span className="text-muted-foreground">
          {list.file_count.toLocaleString()} of {list.max_files.toLocaleString()} files
        </span>
      </div>
      {limited && (
        <div
          className="h-2 overflow-hidden rounded-full bg-muted"
          role="meter"
          aria-label="Static store usage"
          aria-valuemin={0}
          aria-valuemax={list.quota_bytes}
          aria-valuenow={Math.min(list.used_bytes, list.quota_bytes)}
          aria-valuetext={`${formatBytes(list.used_bytes)} of ${formatBytes(list.quota_bytes)}`}
        >
          <div className={cn("h-full", tone)} style={{ width: `${pct}%` }} />
        </div>
      )}
      <p className="text-xs text-muted-foreground">
        {limited ? `Used ${exact(list.used_bytes)} of ${exact(list.quota_bytes)}. ` : ""}
        Uploads also need 1 GiB of free disk space on the host.
      </p>
    </section>
  );
}

function UploadRow({
  item,
  tooLarge,
  onCancel,
  onReplace,
}: {
  item: UploadItem;
  tooLarge: boolean;
  onCancel: () => void;
  onReplace?: () => void;
}) {
  const pct = item.file.size > 0 ? Math.min(100, Math.round((item.loaded / item.file.size) * 100)) : 100;
  return (
    <li className="flex flex-col gap-1.5 px-3 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="min-w-0 flex-1 truncate font-mono text-xs" title={item.file.name}>
          {item.file.name}
        </span>
        <span className="text-xs text-muted-foreground">{formatBytes(item.file.size)}</span>
        {item.replaces && item.state === "ready" && <Badge tone="warning">Replaces existing</Badge>}
        {tooLarge && <Badge tone="warning">Larger than the space left</Badge>}
        {item.state === "ready" && <Badge>Ready</Badge>}
        {item.state === "waiting" && <Badge>Waiting</Badge>}
        {item.state === "uploading" && <Badge tone="info">{pct}%</Badge>}
        {item.state === "done" && <Badge tone="success">Uploaded</Badge>}
        {item.state === "failed" && <Badge tone="danger">Not stored</Badge>}
        {item.state === "unknown" && <Badge tone="warning">Outcome unknown</Badge>}
        {item.state === "invalid" && <Badge tone="danger">Not allowed</Badge>}
        {item.state === "cancelled" && <Badge tone="warning">Cancelled</Badge>}
        {item.state === "conflict" && <Badge tone="warning">Name exists</Badge>}
        {item.state === "conflict" && onReplace && (
          <Button size="sm" variant="outline" onClick={onReplace} aria-label={`Replace the stored ${item.file.name}`}>
            Replace
          </Button>
        )}
        {(item.state === "ready" || item.state === "uploading" || item.state === "waiting") && (
          <Button size="sm" variant="ghost" onClick={onCancel} aria-label={`Cancel upload of ${item.file.name}`}>
            <X />
            Cancel
          </Button>
        )}
      </div>
      {item.state === "uploading" && (
        <div
          className="h-1.5 overflow-hidden rounded-full bg-muted"
          role="progressbar"
          aria-label={`Uploading ${item.file.name}`}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
        >
          <div className="h-full bg-primary transition-[width]" style={{ width: `${pct}%` }} />
        </div>
      )}
      {item.error && <p className={cn("break-words text-xs", item.state === "waiting" ? "text-muted-foreground" : "text-destructive")}>{item.error}</p>}
    </li>
  );
}
