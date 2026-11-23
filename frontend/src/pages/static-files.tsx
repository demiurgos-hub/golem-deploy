import { useRef, useState, type DragEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Download, Trash2, Upload, X } from "lucide-react";
import { useApi } from "@/app/services";
import { describeError, isAbortError, isApiError } from "@/api/errors";
import { useStaticFiles } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { StaticFile } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { PageHeader, Section } from "@/components/ui/page";
import { EmptyState, ErrorState, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { saveBlob } from "@/lib/download";
import { formatBytes, formatDateTime, formatRelative } from "@/lib/format";
import { cn, localId } from "@/lib/utils";
import { STATIC_UPLOAD_LIMIT_BYTES, validateStaticName } from "@/lib/validators";

type UploadState = "invalid" | "ready" | "uploading" | "done" | "failed" | "cancelled" | "conflict";

interface UploadItem {
  id: string;
  file: File;
  state: UploadState;
  loaded: number;
  error: string | null;
  /** The name existed when the file was queued: uploading it replaces that file (after confirmation). */
  replaces: boolean;
}

/** Client-side checks that mirror the daemon's rules, so bad files fail before uploading. */
export function checkUploadFile(file: File): string | null {
  const nameError = validateStaticName(file.name);
  if (nameError) return nameError;
  if (file.size > STATIC_UPLOAD_LIMIT_BYTES) return `Larger than the 1 GiB upload limit (${formatBytes(file.size)}).`;
  return null;
}

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

  function addFiles(list: FileList | null) {
    if (!list || list.length === 0) return;
    // Copy now: the FileList is live and the input is reset right after this call,
    // while the state updater below may run later.
    const files = Array.from(list);
    setQueue((q) => {
      const names = new Set(q.filter((i) => i.state === "ready" || i.state === "uploading").map((i) => i.file.name));
      const added: UploadItem[] = [];
      for (const file of files) {
        let error = checkUploadFile(file);
        if (!error && names.has(file.name)) error = "A file with this name is already queued.";
        names.add(file.name);
        added.push({
          id: localId("upload"),
          file,
          state: error ? "invalid" : "ready",
          loaded: 0,
          error,
          replaces: existing.has(file.name),
        });
      }
      return [...q, ...added];
    });
  }

  const update = (id: string, patch: Partial<UploadItem>) =>
    setQueue((q) => q.map((i) => (i.id === id ? { ...i, ...patch } : i)));

  /**
   * Uploads one file per request. Only files confirmed as replacements are
   * sent with replace=1; any other name that exists by now (another user
   * uploaded it meanwhile) comes back as 409 conflict and is offered for
   * replacement on its row instead of being overwritten silently.
   */
  async function runUploads(items: UploadItem[]) {
    setRunning(true);
    let ok = 0;
    for (const item of items) {
      if (cancelled.current.has(item.id)) continue;
      const controller = new AbortController();
      controllers.current.set(item.id, controller);
      update(item.id, { state: "uploading", loaded: 0, error: null });
      try {
        await api.staticFiles.upload([item.file], {
          replace: item.replaces,
          signal: controller.signal,
          onProgress: (loaded) => update(item.id, { loaded }),
        });
        update(item.id, { state: "done", loaded: item.file.size });
        ok++;
        void queryClient.invalidateQueries({ queryKey: qk.staticFiles });
      } catch (err) {
        if (isAbortError(err)) update(item.id, { state: "cancelled", error: null });
        else if (isApiError(err) && err.code === "conflict") {
          update(item.id, { state: "conflict", error: "A file with this name was stored in the meantime. Replace it?" });
          void queryClient.invalidateQueries({ queryKey: qk.staticFiles });
        } else update(item.id, { state: "failed", error: describeError(err) });
      } finally {
        controllers.current.delete(item.id);
      }
    }
    setRunning(false);
    if (ok > 0) toast.success(`Uploaded ${ok} file${ok === 1 ? "" : "s"}.`);
  }

  function start() {
    if (replacing.length > 0) setConfirmReplace(true);
    else void runUploads(ready);
  }

  function cancel(item: UploadItem) {
    if (item.state === "uploading") {
      controllers.current.get(item.id)?.abort();
    } else {
      cancelled.current.add(item.id);
      update(item.id, { state: "cancelled" });
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

  return (
    <>
      <PageHeader title="Static files" description="Files made available to project builds, such as assets too large for the repository." />
      <div className="flex flex-col gap-6">
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
              <ul className="flex flex-col divide-y divide-border rounded-md border border-border" aria-label="Upload queue">
                {queue.map((item) => (
                  <UploadRow
                    key={item.id}
                    item={item}
                    onCancel={() => cancel(item)}
                    onReplace={running ? undefined : () => void runUploads([{ ...item, replaces: true }])}
                  />
                ))}
              </ul>
              <div className="flex flex-wrap gap-2">
                <Button onClick={start} disabled={ready.length === 0 || running} loading={running}>
                  {!running && <Upload />}
                  Upload {ready.length > 0 ? `${ready.length} file${ready.length === 1 ? "" : "s"}` : ""}
                </Button>
                <Button
                  variant="outline"
                  disabled={running}
                  onClick={() => setQueue((q) => q.filter((i) => i.state === "ready" || i.state === "uploading"))}
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

function UploadRow({ item, onCancel, onReplace }: { item: UploadItem; onCancel: () => void; onReplace?: () => void }) {
  const pct = item.file.size > 0 ? Math.min(100, Math.round((item.loaded / item.file.size) * 100)) : 100;
  return (
    <li className="flex flex-col gap-1.5 px-3 py-2 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="min-w-0 flex-1 truncate font-mono text-xs" title={item.file.name}>
          {item.file.name}
        </span>
        <span className="text-xs text-muted-foreground">{formatBytes(item.file.size)}</span>
        {item.replaces && item.state === "ready" && <Badge tone="warning">Replaces existing</Badge>}
        {item.state === "ready" && <Badge>Ready</Badge>}
        {item.state === "uploading" && <Badge tone="info">{pct}%</Badge>}
        {item.state === "done" && <Badge tone="success">Uploaded</Badge>}
        {item.state === "failed" && <Badge tone="danger">Failed</Badge>}
        {item.state === "invalid" && <Badge tone="danger">Not allowed</Badge>}
        {item.state === "cancelled" && <Badge tone="warning">Cancelled</Badge>}
        {item.state === "conflict" && <Badge tone="warning">Name exists</Badge>}
        {item.state === "conflict" && onReplace && (
          <Button size="sm" variant="outline" onClick={onReplace} aria-label={`Replace the stored ${item.file.name}`}>
            Replace
          </Button>
        )}
        {(item.state === "ready" || item.state === "uploading") && (
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
      {item.error && <p className="text-xs text-destructive">{item.error}</p>}
    </li>
  );
}
