import { useState, useEffect, useCallback } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "@/api/client";
import type { VolumeInfo } from "@/api/types";
import { DataTable, type Column } from "@/components/shared/data-table";
import { ActionButton } from "@/components/shared/action-button";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { FormDialog } from "@/components/shared/form-dialog";
import { useDashboard } from "@/components/layout/dashboard-layout";
import { Info, Trash2 } from "lucide-react";

interface SavesTabProps {
  pendingContainerName: string | null;
  onConsumed: () => void;
}

export function SavesTab({ pendingContainerName, onConsumed }: SavesTabProps) {
  const { activeTab, refreshKey } = useDashboard();
  const queryClient = useQueryClient();
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState(false);
  const [volumeDetail, setVolumeDetail] = useState<VolumeInfo | null>(null);

  const { data: volumes = [], isLoading } = useQuery({
    queryKey: ["volumes", refreshKey],
    queryFn: () => apiClient.listVolumes(),
    enabled: activeTab === "saves",
    refetchInterval: activeTab === "saves" ? 10000 : false,
  });

  const deleteMutation = useMutation({
    mutationFn: (names: string[]) => apiClient.deleteVolumes(names),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["volumes"] });
      setSelectedIds(new Set());
    },
  });

  const openVolumeDetail = useCallback(
    async (containerName: string, fallback?: VolumeInfo) => {
      if (!containerName) {
        if (fallback) setVolumeDetail(fallback);
        return;
      }

      try {
        const vol = await apiClient.getVolumeInfo(containerName);
        setVolumeDetail(vol);
      } catch {
        if (fallback) setVolumeDetail(fallback);
      }
    },
    [],
  );

  useEffect(() => {
    if (pendingContainerName && activeTab === "saves") {
      // One-shot navigation request from the Containers tab; state is only set
      // after the async volume fetch resolves, which the rule cannot see.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      openVolumeDetail(pendingContainerName).finally(onConsumed);
    }
  }, [pendingContainerName, activeTab, onConsumed, openVolumeDetail]);

  const columns: Column<VolumeInfo>[] = [
    {
      key: "name",
      label: "Name",
      sortable: true,
      render: (v) => (
        <span className="font-medium max-w-xs truncate block">{v.name}</span>
      ),
      getValue: (v) => v.name,
    },
    {
      key: "driver",
      label: "Driver",
      render: (v) => v.driver,
    },
    {
      key: "container",
      label: "Container",
      sortable: true,
      render: (v) => v.container_name || "-",
      getValue: (v) => v.container_name,
    },
    {
      key: "created",
      label: "Created",
      sortable: true,
      render: (v) => new Date(v.created_at).toLocaleDateString(),
      getValue: (v) => v.created_at,
    },
  ];

  if (isLoading) {
    return <p className="text-muted-foreground">Loading volumes...</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">Saves / Volumes ({volumes.length})</h2>
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
        data={volumes}
        columns={columns}
        getRowId={(v) => v.name}
        selectable
        selectedIds={selectedIds}
        onSelectionChange={setSelectedIds}
        emptyMessage="No volumes found"
        actions={(v) => (
          <>
            <ActionButton
              onClick={() => openVolumeDetail(v.container_name, v)}
              title="Info"
            >
              <Info className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmDelete(v.name)}
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
        title="Delete Volume"
        description={`Delete volume "${confirmDelete}"? This action cannot be undone.`}
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
        title="Delete Selected Volumes"
        description={`Delete ${selectedIds.size} volume(s)? This action cannot be undone.`}
        confirmLabel="Delete All"
        destructive
        onConfirm={() => {
          deleteMutation.mutate(Array.from(selectedIds));
          setConfirmBulkDelete(false);
        }}
      />

      <FormDialog
        open={!!volumeDetail}
        onOpenChange={() => setVolumeDetail(null)}
        title={`Volume: ${volumeDetail?.name ?? ""}`}
        wide
      >
        {volumeDetail && (
          <div className="space-y-3 text-sm">
            <div className="grid grid-cols-2 gap-2">
              <div>
                <span className="text-muted-foreground">Driver:</span>{" "}
                {volumeDetail.driver}
              </div>
              <div>
                <span className="text-muted-foreground">Container:</span>{" "}
                {volumeDetail.container_name || "None"}
              </div>
              <div className="col-span-2">
                <span className="text-muted-foreground">Mountpoint:</span>{" "}
                <span className="break-all">{volumeDetail.mountpoint}</span>
              </div>
              <div>
                <span className="text-muted-foreground">Created:</span>{" "}
                {new Date(volumeDetail.created_at).toLocaleString()}
              </div>
            </div>
            {volumeDetail.files && volumeDetail.files.length > 0 && (
              <div>
                <p className="font-medium text-muted-foreground mb-1">
                  Files ({volumeDetail.files.length}):
                </p>
                <div className="max-h-48 overflow-auto rounded border border-border bg-muted/30 p-2">
                  {volumeDetail.files.map((f, i) => (
                    <div key={i} className="text-xs text-foreground">
                      {f}
                    </div>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}
      </FormDialog>
    </div>
  );
}
