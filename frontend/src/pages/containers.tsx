import { useState, useCallback } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "@/api/client";
import type { DockerContainer, ContainerProcess } from "@/api/types";
import { DataTable, type Column } from "@/components/shared/data-table";
import { ActionButton } from "@/components/shared/action-button";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { FormDialog } from "@/components/shared/form-dialog";
import { useDashboard } from "@/components/layout/dashboard-layout";
import { Play, Square, FileText, BarChart3, Cpu, HardDrive, Trash2 } from "lucide-react";

export function ContainersTab() {
  const { activeTab, refreshKey, navigateToSaves } = useDashboard();
  const queryClient = useQueryClient();
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState(false);
  const [statsContainer, setStatsContainer] = useState<DockerContainer | null>(null);
  const [processesContainer, setProcessesContainer] = useState<string | null>(null);
  const [processes, setProcesses] = useState<ContainerProcess[]>([]);

  const { data: containers = [], isLoading } = useQuery({
    queryKey: ["containers", refreshKey],
    queryFn: () => apiClient.listContainers(),
    enabled: activeTab === "containers",
    refetchInterval: activeTab === "containers" ? 5000 : false,
  });

  const startMutation = useMutation({
    mutationFn: (name: string) => apiClient.startContainer(name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["containers"] }),
  });

  const stopMutation = useMutation({
    mutationFn: (name: string) => apiClient.stopContainer(name),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["containers"] }),
  });

  const deleteMutation = useMutation({
    mutationFn: (names: string[]) => apiClient.deleteContainers(names),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["containers"] });
      setSelectedIds(new Set());
    },
  });

  const handleDeleteConfirm = useCallback(() => {
    if (confirmDelete) {
      deleteMutation.mutate([confirmDelete]);
      setConfirmDelete(null);
    }
  }, [confirmDelete, deleteMutation]);

  const handleBulkDeleteConfirm = useCallback(() => {
    deleteMutation.mutate(Array.from(selectedIds));
    setConfirmBulkDelete(false);
  }, [selectedIds, deleteMutation]);

  const handleShowProcesses = useCallback(async (containerName: string) => {
    try {
      const procs = await apiClient.getContainerProcesses(containerName);
      setProcesses(procs);
      setProcessesContainer(containerName);
    } catch {
      setProcesses([]);
      setProcessesContainer(containerName);
    }
  }, []);

  const parseCpuPercent = (c: DockerContainer): number => {
    if (!c.stats?.cpu_perc) return 0;
    return parseFloat(c.stats.cpu_perc.replace("%", "")) || 0;
  };

  const parseMemPercent = (c: DockerContainer): number => {
    if (!c.stats?.mem_perc) return 0;
    return parseFloat(c.stats.mem_perc.replace("%", "")) || 0;
  };

  const columns: Column<DockerContainer>[] = [
    {
      key: "name",
      label: "Name",
      sortable: true,
      render: (c) => <span className="font-medium">{c.names}</span>,
      getValue: (c) => c.names,
    },
    {
      key: "label",
      label: "Label",
      sortable: true,
      render: (c) => c.label || "-",
      getValue: (c) => c.label,
    },
    {
      key: "status",
      label: "Status",
      sortable: true,
      render: (c) => (
        <span
          className={
            c.status.startsWith("Up")
              ? "text-green-600 dark:text-green-400"
              : "text-muted-foreground"
          }
        >
          {c.status}
        </span>
      ),
      getValue: (c) => c.status,
    },
    {
      key: "cpu",
      label: "CPU",
      sortable: true,
      render: (c) => c.stats?.cpu_perc || "-",
      getValue: parseCpuPercent,
    },
    {
      key: "ram",
      label: "RAM",
      sortable: true,
      render: (c) => c.stats?.mem_usage || "-",
      getValue: parseMemPercent,
    },
    {
      key: "ports",
      label: "Ports",
      render: (c) => c.ports || "-",
    },
  ];

  if (isLoading) {
    return <p className="text-muted-foreground">Loading containers...</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">
          Containers ({containers.length})
        </h2>
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
        data={containers}
        columns={columns}
        getRowId={(c) => c.names}
        selectable
        selectedIds={selectedIds}
        onSelectionChange={setSelectedIds}
        emptyMessage="No containers found"
        actions={(c) => (
          <>
            {c.status.startsWith("Up") ? (
              <ActionButton
                onClick={() => stopMutation.mutate(c.names)}
                title="Stop"
              >
                <Square className="h-3 w-3" />
              </ActionButton>
            ) : (
              <ActionButton
                onClick={() => startMutation.mutate(c.names)}
                title="Start"
              >
                <Play className="h-3 w-3" />
              </ActionButton>
            )}
            <ActionButton
              onClick={() => apiClient.downloadContainerLogs(c.names, c.label)}
              title="Logs"
            >
              <FileText className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              onClick={() => setStatsContainer(c)}
              title="Stats"
            >
              <BarChart3 className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              onClick={() => handleShowProcesses(c.names)}
              title="Processes"
            >
              <Cpu className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              onClick={() => navigateToSaves(c.names)}
              title="Saves"
            >
              <HardDrive className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmDelete(c.names)}
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
        title="Delete Container"
        description={`Are you sure you want to delete container "${confirmDelete}"?`}
        confirmLabel="Delete"
        destructive
        onConfirm={handleDeleteConfirm}
      />

      <ConfirmDialog
        open={confirmBulkDelete}
        onOpenChange={setConfirmBulkDelete}
        title="Delete Selected Containers"
        description={`Are you sure you want to delete ${selectedIds.size} container(s)?`}
        confirmLabel="Delete All"
        destructive
        onConfirm={handleBulkDeleteConfirm}
      />

      {/* Stats Dialog */}
      <FormDialog
        open={!!statsContainer}
        onOpenChange={() => setStatsContainer(null)}
        title={`Stats: ${statsContainer?.names ?? ""}`}
      >
        {statsContainer?.stats ? (
          <div className="grid grid-cols-2 gap-3 text-sm">
            <div>
              <span className="text-muted-foreground">CPU:</span>{" "}
              {statsContainer.stats.cpu_perc}
            </div>
            <div>
              <span className="text-muted-foreground">Memory:</span>{" "}
              {statsContainer.stats.mem_usage}
            </div>
            <div>
              <span className="text-muted-foreground">Mem %:</span>{" "}
              {statsContainer.stats.mem_perc}
            </div>
            <div>
              <span className="text-muted-foreground">Net I/O:</span>{" "}
              {statsContainer.stats.net_io}
            </div>
            <div>
              <span className="text-muted-foreground">Block I/O:</span>{" "}
              {statsContainer.stats.block_io}
            </div>
            <div>
              <span className="text-muted-foreground">PIDs:</span>{" "}
              {statsContainer.stats.pids}
            </div>
          </div>
        ) : (
          <p className="text-muted-foreground">No stats available (container may not be running)</p>
        )}
      </FormDialog>

      {/* Processes Dialog */}
      <FormDialog
        open={!!processesContainer}
        onOpenChange={() => setProcessesContainer(null)}
        title={`Processes: ${processesContainer ?? ""}`}
        wide
      >
        {processes.length > 0 ? (
          <div className="overflow-auto">
            <table className="w-full text-xs">
              <thead>
                <tr className="border-b border-border text-left text-muted-foreground">
                  <th className="px-2 py-1">PID</th>
                  <th className="px-2 py-1">User</th>
                  <th className="px-2 py-1">CPU</th>
                  <th className="px-2 py-1">Memory</th>
                  <th className="px-2 py-1">Command</th>
                </tr>
              </thead>
              <tbody>
                {processes.map((p, i) => (
                  <tr key={i} className="border-b border-border last:border-0">
                    <td className="px-2 py-1">{p.pid}</td>
                    <td className="px-2 py-1">{p.user}</td>
                    <td className="px-2 py-1">{p.cpu}</td>
                    <td className="px-2 py-1">{p.memory}</td>
                    <td className="px-2 py-1 max-w-xs truncate">{p.command}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <p className="text-muted-foreground">No processes found</p>
        )}
      </FormDialog>
    </div>
  );
}
