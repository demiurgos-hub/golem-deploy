import { useState, useCallback } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { parse, stringify } from "yaml";
import { apiClient } from "@/api/client";
import type { MassRunConfig, AddMassRunRequest } from "@/api/types";
import { DataTable, type Column } from "@/components/shared/data-table";
import { ActionButton } from "@/components/shared/action-button";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { FormDialog } from "@/components/shared/form-dialog";
import { FileUpload } from "@/components/shared/file-upload";
import { RunFields } from "@/components/shared/run-fields";
import { useDashboard } from "@/components/layout/dashboard-layout";
import { usePersistedDefaults } from "@/hooks/use-persisted-defaults";
import { useRunFormState } from "@/hooks/use-run-form-state";
import { Plus, Pencil, Copy, Trash2, Upload, Download } from "lucide-react";

function normalizeMassRunConfig(
  value: unknown,
  index: number,
): AddMassRunRequest {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`Config #${index + 1} must be an object.`);
  }

  const record = value as Record<string, unknown>;
  const name = typeof record.name === "string" ? record.name.trim() : "";
  const containerName =
    typeof record.container_name === "string"
      ? record.container_name.trim()
      : "";

  if (!name) {
    throw new Error(`Config #${index + 1} is missing name.`);
  }
  if (!containerName) {
    throw new Error(`Config "${name}" is missing container_name.`);
  }

  const envVars: Record<string, string> = {};
  if (record.env_vars !== undefined && record.env_vars !== null) {
    if (
      typeof record.env_vars !== "object" ||
      Array.isArray(record.env_vars)
    ) {
      throw new Error(`Config "${name}" has invalid env_vars.`);
    }
    for (const [key, envValue] of Object.entries(
      record.env_vars as Record<string, unknown>,
    )) {
      if (typeof envValue !== "string") {
        throw new Error(
          `Config "${name}" env_vars.${key} must be a string.`,
        );
      }
      envVars[key] = envValue;
    }
  }

  return {
    name,
    container_name: containerName,
    port: typeof record.port === "string" ? record.port : "",
    label: typeof record.label === "string" ? record.label : "",
    env_vars: envVars,
  };
}

function validateUniqueNames(configs: AddMassRunRequest[]): void {
  const seen = new Set<string>();
  for (const config of configs) {
    if (seen.has(config.name)) {
      throw new Error(`Duplicate config name "${config.name}" in file.`);
    }
    seen.add(config.name);
  }
}

function parseMassRunYaml(text: string): AddMassRunRequest[] {
  let parsed: unknown;
  try {
    parsed = parse(text);
  } catch (err) {
    throw new Error(
      err instanceof Error ? err.message : "Invalid YAML file.",
      { cause: err },
    );
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("YAML must contain a top-level configs array.");
  }

  const configsValue = (parsed as Record<string, unknown>).configs;
  if (!Array.isArray(configsValue)) {
    throw new Error("YAML must contain a top-level configs array.");
  }

  const configs = configsValue.map((value, index) =>
    normalizeMassRunConfig(value, index),
  );
  validateUniqueNames(configs);
  return configs;
}

function toMassRunYamlConfigs(configs: MassRunConfig[]): AddMassRunRequest[] {
  return configs.map((config) => ({
    name: config.name,
    container_name: config.container_name,
    port: config.port ?? "",
    label: config.label ?? "",
    env_vars: config.env_vars ?? {},
  }));
}

function downloadTextFile(
  contents: string,
  filename: string,
  type: string,
): void {
  const blob = new Blob([contents], { type });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  document.body.appendChild(link);
  link.click();
  document.body.removeChild(link);
  URL.revokeObjectURL(url);
}

export function MassRunTab() {
  const { activeTab, refreshKey } = useDashboard();
  const queryClient = useQueryClient();
  const defaults = usePersistedDefaults();
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [confirmBulkDelete, setConfirmBulkDelete] = useState(false);
  const [editConfig, setEditConfig] = useState<MassRunConfig | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [showImport, setShowImport] = useState(false);
  const [importFileName, setImportFileName] = useState<string | null>(null);
  const [parsedImportConfigs, setParsedImportConfigs] = useState<
    AddMassRunRequest[] | null
  >(null);
  const [importError, setImportError] = useState<string | null>(null);
  const [confirmReplace, setConfirmReplace] = useState(false);

  // Form state (shared between add/edit/duplicate)
  const [formName, setFormName] = useState("");
  const [formMode, setFormMode] = useState<"add" | "edit" | "duplicate">("add");
  const runForm = useRunFormState();

  const { data: configs = [], isLoading } = useQuery({
    queryKey: ["massrun", refreshKey],
    queryFn: () => apiClient.listMassRunConfigs(),
    enabled: activeTab === "massrun",
    refetchInterval: activeTab === "massrun" ? 10000 : false,
  });

  const addMutation = useMutation({
    mutationFn: (req: AddMassRunRequest) => apiClient.addMassRunConfig(req),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["massrun"] });
      setShowAdd(false);
    },
  });

  const editMutation = useMutation({
    mutationFn: () =>
      apiClient.editMassRunConfig({
        name: editConfig!.name,
        ...runForm.asMassRunPayloadFields(),
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["massrun"] });
      setEditConfig(null);
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (names: string[]) => apiClient.deleteMassRunConfigs(names),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["massrun"] });
      setSelectedIds(new Set());
    },
  });

  const replaceMutation = useMutation({
    mutationFn: (configs: AddMassRunRequest[]) =>
      apiClient.replaceMassRunConfigs(configs),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["massrun"] });
      setSelectedIds(new Set());
      resetImportState();
    },
  });

  const resetImportState = useCallback(() => {
    setShowImport(false);
    setImportFileName(null);
    setParsedImportConfigs(null);
    setImportError(null);
    setConfirmReplace(false);
  }, []);

  const handleImportFiles = useCallback(async (files: File[]) => {
    const file = files[0];
    if (!file) return;

    setImportError(null);
    setParsedImportConfigs(null);
    setImportFileName(file.name);

    try {
      const text = await file.text();
      const configs = parseMassRunYaml(text);
      setParsedImportConfigs(configs);
    } catch (err) {
      setImportError(
        err instanceof Error ? err.message : "Failed to parse YAML file.",
      );
    }
  }, []);

  const handleDownloadYaml = useCallback(() => {
    const exportConfigs = toMassRunYamlConfigs(configs);
    const yaml = stringify({ configs: exportConfigs }, { lineWidth: 0 });
    downloadTextFile(
      yaml,
      "mass-run-configs.yaml",
      "application/x-yaml;charset=utf-8",
    );
  }, [configs]);

  const openAddForm = useCallback(() => {
    setFormMode("add");
    setFormName("");
    runForm.loadDefaults(defaults);
    setShowAdd(true);
  }, [defaults, runForm]);

  const openEditForm = useCallback((config: MassRunConfig) => {
    setFormMode("edit");
    setEditConfig(config);
    setFormName(config.name);
    runForm.loadConfig(config);
  }, [runForm]);

  const openDuplicateForm = useCallback((config: MassRunConfig) => {
    setFormMode("duplicate");
    setFormName(config.name + " (copy)");
    runForm.loadConfig(config);
    setShowAdd(true);
  }, [runForm]);

  const handleSubmitAdd = () => {
    runForm.persistDefaults(defaults);
    addMutation.mutate({
      name: formName,
      ...runForm.asMassRunPayloadFields(),
    });
  };

  const columns: Column<MassRunConfig>[] = [
    {
      key: "name",
      label: "Name",
      sortable: true,
      render: (c) => <span className="font-medium">{c.name}</span>,
      getValue: (c) => c.name,
    },
    {
      key: "container_name",
      label: "Container",
      render: (c) => c.container_name,
    },
    {
      key: "port",
      label: "Port",
      render: (c) => c.port,
    },
    {
      key: "label",
      label: "Label",
      render: (c) => c.label || "-",
    },
  ];

  if (isLoading) {
    return <p className="text-muted-foreground">Loading configurations...</p>;
  }

  const isFormOpen = showAdd || !!editConfig;

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">
          Mass Run Configs ({configs.length})
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
          <ActionButton onClick={handleDownloadYaml}>
            <Download className="mr-1 h-3 w-3" /> Download YAML
          </ActionButton>
          <ActionButton onClick={() => setShowImport(true)}>
            <Upload className="mr-1 h-3 w-3" /> Add From File
          </ActionButton>
          <ActionButton onClick={openAddForm}>
            <Plus className="mr-1 h-3 w-3" /> Add Config
          </ActionButton>
        </div>
      </div>

      <DataTable
        data={configs}
        columns={columns}
        getRowId={(c) => c.name}
        selectable
        selectedIds={selectedIds}
        onSelectionChange={setSelectedIds}
        emptyMessage="No mass run configurations found"
        actions={(c) => (
          <>
            <ActionButton onClick={() => openEditForm(c)} title="Edit">
              <Pencil className="h-3 w-3" />
            </ActionButton>
            <ActionButton onClick={() => openDuplicateForm(c)} title="Duplicate">
              <Copy className="h-3 w-3" />
            </ActionButton>
            <ActionButton
              variant="destructive"
              onClick={() => setConfirmDelete(c.name)}
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
        title="Delete Config"
        description={`Delete mass run config "${confirmDelete}"?`}
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
        title="Delete Selected Configs"
        description={`Delete ${selectedIds.size} mass run config(s)?`}
        confirmLabel="Delete All"
        destructive
        onConfirm={() => {
          deleteMutation.mutate(Array.from(selectedIds));
          setConfirmBulkDelete(false);
        }}
      />

      <ConfirmDialog
        open={confirmReplace}
        onOpenChange={setConfirmReplace}
        title="Replace All Mass Run Configs"
        description={`This will delete all ${configs.length} existing config(s) and replace them with ${parsedImportConfigs?.length ?? 0} from "${importFileName ?? "file"}".\n\nThis cannot be undone.`}
        confirmLabel="Replace All"
        destructive
        onConfirm={() => {
          if (parsedImportConfigs) {
            replaceMutation.mutate(parsedImportConfigs);
          }
        }}
      />

      <FormDialog
        open={showImport}
        onOpenChange={(open) => {
          if (!open) resetImportState();
          else setShowImport(true);
        }}
        title="Import Mass Run Configs"
      >
        <div className="space-y-4">
          <p className="text-sm text-muted-foreground">
            Upload a YAML file with a top-level <code>configs</code> array.
            Importing will replace all existing mass run configs.
          </p>
          <FileUpload
            accept=".yaml,.yml"
            onFiles={handleImportFiles}
            label="Drop a .yaml or .yml file here"
          />
          {importFileName && (
            <p className="text-sm text-foreground">
              Selected: {importFileName}
            </p>
          )}
          {parsedImportConfigs && (
            <p className="text-sm text-foreground">
              Ready to import {parsedImportConfigs.length} config(s) from{" "}
              {importFileName}.
            </p>
          )}
          {importError && (
            <p className="text-sm text-destructive">{importError}</p>
          )}
          {replaceMutation.isError && (
            <p className="text-sm text-destructive">
              {replaceMutation.error.message}
            </p>
          )}
          <div className="flex justify-end gap-2">
            <button
              onClick={resetImportState}
              disabled={replaceMutation.isPending}
              className="rounded-md border border-input bg-background px-4 py-2 text-sm hover:bg-accent disabled:opacity-50"
            >
              Cancel
            </button>
            <button
              onClick={() => setConfirmReplace(true)}
              disabled={
                !parsedImportConfigs ||
                replaceMutation.isPending ||
                !!importError
              }
              className="rounded-md bg-destructive px-4 py-2 text-sm font-medium text-destructive-foreground hover:bg-destructive/90 disabled:opacity-50"
            >
              {replaceMutation.isPending ? "Replacing..." : "Replace All"}
            </button>
          </div>
        </div>
      </FormDialog>

      <FormDialog
        open={isFormOpen}
        onOpenChange={(open) => {
          if (!open) {
            setShowAdd(false);
            setEditConfig(null);
          }
        }}
        title={
          formMode === "edit"
            ? `Edit: ${editConfig?.name}`
            : formMode === "duplicate"
              ? "Duplicate Config"
              : "Add Mass Run Config"
        }
      >
        <div className="space-y-4">
          {formMode !== "edit" && (
            <div className="space-y-2">
              <label className="text-sm font-medium">Config Name</label>
              <input
                type="text"
                value={formName}
                onChange={(e) => setFormName(e.target.value)}
                className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              />
            </div>
          )}
          <RunFields
            form={runForm}
          />
          <div className="flex justify-end gap-2 pt-2">
            <button
              onClick={() => {
                setShowAdd(false);
                setEditConfig(null);
              }}
              className="rounded-md border border-input bg-background px-4 py-2 text-sm hover:bg-accent"
            >
              Cancel
            </button>
            <button
              onClick={() => {
                if (formMode === "edit") {
                  editMutation.mutate();
                } else {
                  handleSubmitAdd();
                }
              }}
              disabled={
                formMode === "edit"
                  ? editMutation.isPending
                  : addMutation.isPending
              }
              className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
            >
              {formMode === "edit"
                ? editMutation.isPending
                  ? "Saving..."
                  : "Save"
                : addMutation.isPending
                  ? "Adding..."
                  : "Add"}
            </button>
          </div>
        </div>
      </FormDialog>
    </div>
  );
}
