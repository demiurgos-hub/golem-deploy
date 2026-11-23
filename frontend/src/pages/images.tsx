import { useState, useCallback } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "@/api/client";
import type { DockerImage, RunContainerRequest } from "@/api/types";
import { DataTable, type Column } from "@/components/shared/data-table";
import { ActionButton } from "@/components/shared/action-button";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { FormDialog } from "@/components/shared/form-dialog";
import { RunFields } from "@/components/shared/run-fields";
import { useDashboard } from "@/components/layout/dashboard-layout";
import { usePersistedDefaults } from "@/hooks/use-persisted-defaults";
import { useRunFormState } from "@/hooks/use-run-form-state";
import { Play, Layers, Trash2 } from "lucide-react";

function massRunErrorDetail(err: unknown): string {
  const raw = err instanceof Error ? err.message : "Unknown error";
  try {
    const parsed = JSON.parse(raw) as { detail?: string };
    if (parsed.detail) return parsed.detail;
  } catch {
    // keep raw API text when it is not a JSON envelope
  }
  return raw;
}

export function ImagesTab() {
  const { activeTab, refreshKey } = useDashboard();
  const queryClient = useQueryClient();
  const defaults = usePersistedDefaults();
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState(false);
  const [runImage, setRunImage] = useState<DockerImage | null>(null);
  const [massRunImage, setMassRunImage] = useState<string | null>(null);
  const [massRunResult, setMassRunResult] = useState<{ started: number; failures: string[] } | null>(null);

  const runForm = useRunFormState();

  const { data: images = [], isLoading } = useQuery({
    queryKey: ["images", refreshKey],
    queryFn: () => apiClient.listImages(),
    enabled: activeTab === "images",
    refetchInterval: activeTab === "images" ? 10000 : false,
  });

  const deleteMutation = useMutation({
    mutationFn: (ids: string[]) => apiClient.deleteImages(ids),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["images"] });
      setSelectedIds(new Set());
    },
  });

  const runMutation = useMutation({
    mutationFn: (req: RunContainerRequest) => apiClient.runContainer(req),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["containers"] });
      setRunImage(null);
    },
  });

  const massRunMutation = useMutation({
    mutationFn: (imageName: string) => apiClient.executeMassRun(imageName),
    onSuccess: (result) => {
      queryClient.invalidateQueries({ queryKey: ["containers"] });
      setMassRunImage(null);
      setMassRunResult({
        started: result.started,
        failures: (result.failed ?? []).map((f) => `${f.name}: ${f.detail}`),
      });
    },
    onError: (err) => {
      setMassRunImage(null);
      setMassRunResult({ started: 0, failures: [massRunErrorDetail(err)] });
    },
  });

  const openRunDialog = useCallback(
    (img: DockerImage) => {
      setRunImage(img);
      runForm.loadDefaults(defaults);
    },
    [defaults, runForm],
  );

  const handleRun = useCallback(() => {
    if (!runImage) return;
    const imageName =
      runImage.repository === "<none>"
        ? runImage.id
        : `${runImage.repository}:${runImage.tag}`;
    runForm.persistDefaults(defaults);
    runMutation.mutate({
      image_name: imageName,
      ...runForm.asRunPayloadFields(),
    });
  }, [runImage, defaults, runMutation, runForm]);

  const columns: Column<DockerImage>[] = [
    {
      key: "repository",
      label: "Repository",
      sortable: true,
      render: (i) => <span className="font-medium">{i.repository}</span>,
      getValue: (i) => i.repository,
    },
    {
      key: "tag",
      label: "Tag",
      sortable: true,
      render: (i) => i.tag,
      getValue: (i) => i.tag,
    },
    {
      key: "size",
      label: "Size",
      sortable: true,
      render: (i) => i.size,
      getValue: (i) => i.size,
    },
    {
      key: "created",
      label: "Created",
      sortable: true,
      render: (i) => i.created,
      getValue: (i) => i.created,
    },
  ];

  if (isLoading) {
    return <p className="text-muted-foreground">Loading images...</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">Images ({images.length})</h2>
        {selectedIds.size > 0 && (
          <ActionButton
            variant="destructive"
            onClick={() => setConfirmBulkDelete(true)}
          >
            <Trash2 className="mr-1 h-3 w-3" />
            Delete Selected ({selectedIds.size})
          </ActionButton>
        )}
      </div>

      <DataTable
        data={images}
        columns={columns}
        getRowId={(i) => i.id}
        selectable
        selectedIds={selectedIds}
        onSelectionChange={setSelectedIds}
        emptyMessage="No images found"
        actions={(img) => (
          <>
            <ActionButton onClick={() => openRunDialog(img)} title="Run">
              <Play className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              onClick={() => {
                const name = img.repository === "<none>" ? img.id : `${img.repository}:${img.tag}`;
                setMassRunImage(name);
              }}
              title="Mass Run"
            >
              <Layers className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmDelete(img.id)}
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
        title="Delete Image"
        description="Are you sure you want to delete this image?"
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
        title="Delete Selected Images"
        description={`Delete ${selectedIds.size} image(s)?`}
        confirmLabel="Delete All"
        destructive
        onConfirm={() => {
          deleteMutation.mutate(Array.from(selectedIds));
          setConfirmBulkDelete(false);
        }}
      />

      {/* Mass Run Confirmation */}
      <ConfirmDialog
        open={!!massRunImage}
        onOpenChange={() => setMassRunImage(null)}
        title="Mass Run Containers"
        description={`This will start a new container using image "${massRunImage ?? ""}" for each saved Mass Run configuration. Do you want to continue?`}
        confirmLabel={massRunMutation.isPending ? "Running..." : "Run All"}
        onConfirm={() => {
          if (massRunImage) massRunMutation.mutate(massRunImage);
        }}
      />

      {/* Mass Run Result */}
      <FormDialog
        open={!!massRunResult}
        onOpenChange={() => setMassRunResult(null)}
        title="Mass Run Result"
      >
        {massRunResult && (
          <div className="space-y-3">
            <p className="text-sm text-foreground">
              Started {massRunResult.started} container(s) successfully.
            </p>
            {massRunResult.failures.length > 0 && (
              <div>
                <p className="text-sm font-medium text-destructive mb-1">
                  Failed ({massRunResult.failures.length}):
                </p>
                <div className="rounded border border-border bg-muted/30 p-2 text-xs space-y-1 max-h-40 overflow-auto">
                  {massRunResult.failures.map((f, i) => (
                    <div key={i} className="text-destructive">{f}</div>
                  ))}
                </div>
              </div>
            )}
            <div className="flex justify-end">
              <button
                onClick={() => setMassRunResult(null)}
                className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90"
              >
                OK
              </button>
            </div>
          </div>
        )}
      </FormDialog>

      {/* Run Container Dialog */}
      <FormDialog
        open={!!runImage}
        onOpenChange={() => setRunImage(null)}
        title={`Run Container from ${runImage?.repository ?? ""}:${runImage?.tag ?? ""}`}
      >
        <div className="space-y-4">
          <RunFields
            form={runForm}
            placeholders={{
              containerName: "my-container",
              port: "8080",
              label: "default",
            }}
          />
          <div className="flex justify-end gap-2 pt-2">
            <button
              onClick={() => setRunImage(null)}
              className="rounded-md border border-input bg-background px-4 py-2 text-sm hover:bg-accent"
            >
              Cancel
            </button>
            <button
              onClick={handleRun}
              disabled={runMutation.isPending}
              className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
            >
              {runMutation.isPending ? "Running..." : "Run"}
            </button>
          </div>
        </div>
      </FormDialog>
    </div>
  );
}
