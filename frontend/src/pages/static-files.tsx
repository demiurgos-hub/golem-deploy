import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "@/api/client";
import type { StaticFile } from "@/api/types";
import { DataTable, type Column } from "@/components/shared/data-table";
import { ActionButton } from "@/components/shared/action-button";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { FormDialog } from "@/components/shared/form-dialog";
import { FileUpload } from "@/components/shared/file-upload";
import { useDashboard } from "@/components/layout/dashboard-layout";
import { Download, Trash2, Upload } from "lucide-react";

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function StaticFilesTab() {
  const { activeTab, refreshKey } = useDashboard();
  const queryClient = useQueryClient();
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState(false);
  const [showUpload, setShowUpload] = useState(false);
  const [uploadFiles, setUploadFiles] = useState<File[]>([]);

  const { data: files = [], isLoading } = useQuery({
    queryKey: ["static-files", refreshKey],
    queryFn: () => apiClient.listStaticFiles(),
    enabled: activeTab === "static",
    refetchInterval: activeTab === "static" ? 10000 : false,
  });

  const deleteMutation = useMutation({
    mutationFn: (names: string[]) => apiClient.deleteStaticFiles(names),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["static-files"] });
      setSelectedIds(new Set());
    },
  });

  const uploadMutation = useMutation({
    mutationFn: () => apiClient.uploadStaticFiles(uploadFiles),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["static-files"] });
      setShowUpload(false);
      setUploadFiles([]);
    },
  });

  const columns: Column<StaticFile>[] = [
    {
      key: "name",
      label: "Name",
      sortable: true,
      render: (f) => <span className="font-medium">{f.name}</span>,
      getValue: (f) => f.name,
    },
    {
      key: "size",
      label: "Size",
      sortable: true,
      render: (f) => formatSize(f.size),
      getValue: (f) => f.size,
    },
    {
      key: "extension",
      label: "Type",
      render: (f) => f.extension,
    },
  ];

  if (isLoading) {
    return <p className="text-muted-foreground">Loading static files...</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">
          Static Files ({files.length})
        </h2>
        <div className="flex gap-2">
          {selectedIds.size > 0 && (
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmBulkDelete(true)}
            >
              <Trash2 className="mr-1 h-3 w-3" />
              Delete Selected ({selectedIds.size})
            </ActionButton>
          )}
          <ActionButton onClick={() => setShowUpload(true)}>
            <Upload className="mr-1 h-3 w-3" />
            Upload Files
          </ActionButton>
        </div>
      </div>

      <DataTable
        data={files}
        columns={columns}
        getRowId={(f) => f.name}
        selectable
        selectedIds={selectedIds}
        onSelectionChange={setSelectedIds}
        emptyMessage="No static files found"
        actions={(f) => (
          <>
            <ActionButton
              onClick={() => apiClient.downloadStaticFile(f.name)}
              title="Download"
            >
              <Download className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmDelete(f.name)}
              title="Delete"
            >
              <Trash2 className="h-3 w-3" />
            </ActionButton>
          </>
        )}
      />

      <ConfirmDialog
        open={!!confirmDelete}
        onOpenChange={() => setConfirmDelete(null)}
        title="Delete File"
        description={`Delete "${confirmDelete}"?`}
        confirmLabel="Delete"
        destructive
        onConfirm={() => {
          if (confirmDelete) deleteMutation.mutate([confirmDelete]);
          setConfirmDelete(null);
        }}
      />

      <ConfirmDialog
        open={confirmBulkDelete}
        onOpenChange={setConfirmBulkDelete}
        title="Delete Selected Files"
        description={`Delete ${selectedIds.size} file(s)?`}
        confirmLabel="Delete All"
        destructive
        onConfirm={() => {
          deleteMutation.mutate(Array.from(selectedIds));
          setConfirmBulkDelete(false);
        }}
      />

      <FormDialog
        open={showUpload}
        onOpenChange={setShowUpload}
        title="Upload Static Files"
      >
        <div className="space-y-4">
          <FileUpload
            multiple
            onFiles={(files) => setUploadFiles((prev) => [...prev, ...files])}
            label="Drop files here or click to browse"
          />
          {uploadFiles.length > 0 && (
            <div className="space-y-1">
              <p className="text-sm font-medium">
                {uploadFiles.length} file(s) selected:
              </p>
              {uploadFiles.map((f, i) => (
                <div
                  key={i}
                  className="flex items-center justify-between text-sm text-muted-foreground"
                >
                  <span>{f.name}</span>
                  <button
                    onClick={() =>
                      setUploadFiles((prev) => prev.filter((_, j) => j !== i))
                    }
                    className="text-destructive hover:text-destructive/80"
                  >
                    Remove
                  </button>
                </div>
              ))}
            </div>
          )}
          <div className="flex justify-end gap-2">
            <button
              onClick={() => {
                setShowUpload(false);
                setUploadFiles([]);
              }}
              className="rounded-md border border-input bg-background px-4 py-2 text-sm hover:bg-accent"
            >
              Cancel
            </button>
            <button
              onClick={() => uploadMutation.mutate()}
              disabled={uploadFiles.length === 0 || uploadMutation.isPending}
              className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
            >
              {uploadMutation.isPending ? "Uploading..." : "Upload"}
            </button>
          </div>
        </div>
      </FormDialog>
    </div>
  );
}
