import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "@/api/client";
import type { Repository } from "@/api/types";
import { DataTable, type Column } from "@/components/shared/data-table";
import { ActionButton } from "@/components/shared/action-button";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { FormDialog } from "@/components/shared/form-dialog";
import { useDashboard } from "@/components/layout/dashboard-layout";
import {
  GitBranch,
  Download,
  Trash2,
  Plus,
} from "lucide-react";

export function RepositoriesTab() {
  const { activeTab, refreshKey } = useDashboard();
  const queryClient = useQueryClient();
  const [confirmDelete, setConfirmDelete] = useState<Repository | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [branchDialog, setBranchDialog] = useState<Repository | null>(null);
  const [branchInput, setBranchInput] = useState("");

  // Add repo form state
  const [addName, setAddName] = useState("");
  const [addUrl, setAddUrl] = useState("");
  const [addBranch, setAddBranch] = useState("main");
  const [addAuthType, setAddAuthType] = useState("none");
  const [addUsername, setAddUsername] = useState("");
  const [addPassword, setAddPassword] = useState("");
  const [addSshKey, setAddSshKey] = useState("");

  const { data: repos = [], isLoading } = useQuery({
    queryKey: ["repositories", refreshKey],
    queryFn: () => apiClient.listRepositories(),
    enabled: activeTab === "repositories",
    refetchInterval: activeTab === "repositories" ? 10000 : false,
  });

  const addMutation = useMutation({
    mutationFn: () =>
      apiClient.addRepository(
        addName, addUrl, addBranch, addAuthType, addUsername, addPassword, addSshKey,
      ),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["repositories"] });
      setShowAdd(false);
      resetAddForm();
    },
  });

  const pullMutation = useMutation({
    mutationFn: (id: number) => apiClient.pullRepository(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["repositories"] }),
  });

  const branchMutation = useMutation({
    mutationFn: ({ id, branch }: { id: number; branch: string }) =>
      apiClient.switchRepositoryBranch(id, branch),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["repositories"] });
      setBranchDialog(null);
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (id: number) => apiClient.deleteRepository(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["repositories"] }),
  });

  const resetAddForm = () => {
    setAddName("");
    setAddUrl("");
    setAddBranch("main");
    setAddAuthType("none");
    setAddUsername("");
    setAddPassword("");
    setAddSshKey("");
  };

  const columns: Column<Repository>[] = [
    {
      key: "name",
      label: "Name",
      sortable: true,
      render: (r) => <span className="font-medium">{r.name}</span>,
      getValue: (r) => r.name,
    },
    {
      key: "branch",
      label: "Branch",
      render: (r) => r.branch,
    },
    {
      key: "status",
      label: "Status",
      sortable: true,
      render: (r) => (
        <span
          className={
            r.status === "error"
              ? "text-destructive"
              : r.status === "idle"
                ? "text-green-600 dark:text-green-400"
                : "text-yellow-600 dark:text-yellow-400"
          }
        >
          {r.status}
        </span>
      ),
      getValue: (r) => r.status,
    },
    {
      key: "last_commit",
      label: "Last Commit",
      render: (r) => (
        <span className="text-xs max-w-[120px] truncate block">
          {r.last_commit || "-"}
        </span>
      ),
    },
    {
      key: "repo_size",
      label: "Size",
      sortable: true,
      render: (r) => r.repo_size,
      getValue: (r) => r.repo_bytes_size,
    },
  ];

  if (isLoading) {
    return <p className="text-muted-foreground">Loading repositories...</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">
          Repositories ({repos.length})
        </h2>
        <ActionButton onClick={() => setShowAdd(true)}>
          <Plus className="mr-1 h-3 w-3" /> Add Repository
        </ActionButton>
      </div>

      <DataTable
        data={repos}
        columns={columns}
        getRowId={(r) => String(r.id)}
        emptyMessage="No repositories found"
        actions={(r) => (
          <>
            <ActionButton
              onClick={() => pullMutation.mutate(r.id)}
              disabled={pullMutation.isPending}
              title="Pull"
            >
              <Download className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              onClick={() => {
                setBranchDialog(r);
                setBranchInput(r.branch);
              }}
              title="Switch Branch"
            >
              <GitBranch className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmDelete(r)}
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
        title="Delete Repository"
        description={`Delete repository "${confirmDelete?.name}"? This will remove the cloned files.`}
        confirmLabel="Delete"
        destructive
        onConfirm={() => {
          if (confirmDelete) deleteMutation.mutate(confirmDelete.id);
          setConfirmDelete(null);
        }}
      />

      {/* Add Repository Dialog */}
      <FormDialog
        open={showAdd}
        onOpenChange={(open) => {
          setShowAdd(open);
          if (!open) resetAddForm();
        }}
        title="Add Repository"
      >
        <div className="space-y-4">
          <div className="space-y-2">
            <label className="text-sm font-medium">Name</label>
            <input
              type="text" value={addName}
              onChange={(e) => setAddName(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              placeholder="my-repo"
            />
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium">URL</label>
            <input
              type="text" value={addUrl}
              onChange={(e) => setAddUrl(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              placeholder="https://github.com/user/repo.git"
            />
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium">Branch</label>
            <input
              type="text" value={addBranch}
              onChange={(e) => setAddBranch(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
            />
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium">Auth Type</label>
            <select
              value={addAuthType}
              onChange={(e) => setAddAuthType(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
            >
              <option value="none">None</option>
              <option value="basic">Basic (Username/Password)</option>
              <option value="ssh">SSH Key</option>
            </select>
          </div>
          {addAuthType === "basic" && (
            <>
              <div className="space-y-2">
                <label className="text-sm font-medium">Username</label>
                <input
                  type="text" value={addUsername}
                  onChange={(e) => setAddUsername(e.target.value)}
                  className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
                />
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium">Password / Token</label>
                <input
                  type="password" value={addPassword}
                  onChange={(e) => setAddPassword(e.target.value)}
                  className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
                />
              </div>
            </>
          )}
          {addAuthType === "ssh" && (
            <div className="space-y-2">
              <label className="text-sm font-medium">SSH Key Path</label>
              <input
                type="text" value={addSshKey}
                onChange={(e) => setAddSshKey(e.target.value)}
                className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
                placeholder="/path/to/id_rsa"
              />
            </div>
          )}
          <div className="flex justify-end gap-2 pt-2">
            <button
              onClick={() => { setShowAdd(false); resetAddForm(); }}
              className="rounded-md border border-input bg-background px-4 py-2 text-sm hover:bg-accent"
            >
              Cancel
            </button>
            <button
              onClick={() => addMutation.mutate()}
              disabled={!addName || !addUrl || addMutation.isPending}
              className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
            >
              {addMutation.isPending ? "Adding..." : "Add"}
            </button>
          </div>
        </div>
      </FormDialog>

      {/* Switch Branch Dialog */}
      <FormDialog
        open={!!branchDialog}
        onOpenChange={() => setBranchDialog(null)}
        title={`Switch Branch: ${branchDialog?.name ?? ""}`}
      >
        <div className="space-y-4">
          <div className="space-y-2">
            <label className="text-sm font-medium">Branch Name</label>
            <input
              type="text" value={branchInput}
              onChange={(e) => setBranchInput(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
            />
          </div>
          <div className="flex justify-end gap-2">
            <button
              onClick={() => setBranchDialog(null)}
              className="rounded-md border border-input bg-background px-4 py-2 text-sm hover:bg-accent"
            >
              Cancel
            </button>
            <button
              onClick={() => {
                if (branchDialog) {
                  branchMutation.mutate({
                    id: branchDialog.id,
                    branch: branchInput,
                  });
                }
              }}
              disabled={!branchInput || branchMutation.isPending}
              className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
            >
              {branchMutation.isPending ? "Switching..." : "Switch"}
            </button>
          </div>
        </div>
      </FormDialog>

    </div>
  );
}
