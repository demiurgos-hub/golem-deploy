import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Download, Plus, Upload } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError, sentence } from "@/api/errors";
import { useAccount, useImages, useProjects, useServers } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Image, Job, Project, ServerExport } from "@/api/types.gen";
import { imageLabel, imageName } from "@/lib/images";
import { EnvEditor } from "@/components/env-editor";
import { InlineJob } from "@/components/jobs";
import { ServerActions } from "@/components/server-actions";
import { ServerStateBadge } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { Badge } from "@/components/ui/badge";
import { useToast } from "@/components/ui/toast";
import { saveBlob } from "@/lib/download";
import { computeEnvDiff, emptyDraft, envErrorsFrom, mergeEnvErrors, noEnvErrors, type EnvDraft, type EnvErrors } from "@/lib/env-diff";
import { IMPORT_LIMIT_BYTES, checkServerImport, describeImportField } from "@/lib/server-import";
import { formatBytes } from "@/lib/format";
import { validatePort, validateResourceName } from "@/lib/validators";

export function ServersPage() {
  const servers = useServers();
  const projects = useProjects();
  const images = useImages();
  const account = useAccount();
  const isAdmin = account.data?.role === "admin";
  const [creating, setCreating] = useState(false);
  const [exporting, setExporting] = useState(false);
  const [importing, setImporting] = useState(false);
  const [jobs, setJobs] = useState<Record<number, Job>>({});
  const projectNames = useMemo(() => new Map((projects.data?.items ?? []).map((p) => [p.id, p.name])), [projects.data]);

  return (
    <>
      <PageHeader
        title="Servers"
        description="Game server containers. Each publishes its port on the host over both UDP and TCP."
        actions={
          <>
            {isAdmin && (
              <>
                <Button variant="outline" onClick={() => setExporting(true)}>
                  <Download />
                  Export
                </Button>
                <Button variant="outline" onClick={() => setImporting(true)}>
                  <Upload />
                  Import
                </Button>
              </>
            )}
            <Button onClick={() => setCreating(true)}>
              <Plus />
              New server
            </Button>
          </>
        }
      />
      {servers.isPending ? (
        <LoadingState label="Loading servers" />
      ) : servers.isError ? (
        <ErrorState error={servers.error} title="Could not load servers" onRetry={() => void servers.refetch()} />
      ) : servers.data.items.length === 0 ? (
        <EmptyState
          title="No servers yet"
          description="Create a server from an image to run a game."
          action={
            <Button onClick={() => setCreating(true)}>
              <Plus />
              New server
            </Button>
          }
        />
      ) : (
        <DataTable
          label="Servers"
          data={servers.data.items}
          rowKey={(s) => s.id}
          initialSort={{ key: "name", asc: true }}
          columns={[
            {
              key: "name",
              header: "Name",
              sortValue: (s) => s.name,
              cell: (s) => (
                <Link to="/servers/$id" params={{ id: String(s.id) }} className="font-medium underline-offset-4 hover:underline">
                  {s.name}
                </Link>
              ),
            },
            {
              key: "project",
              header: "Project",
              hideOnSmall: true,
              cell: (s) =>
                s.project_id === null ? (
                  <span className="text-muted-foreground">-</span>
                ) : (
                  <Link to="/projects/$id" params={{ id: String(s.project_id) }} className="hover:underline">
                    {projectNames.get(s.project_id) ?? `project ${s.project_id}`}
                  </Link>
                ),
            },
            {
              key: "image",
              header: "Image",
              hideOnSmall: true,
              cell: (s) => (
                <code className="block max-w-56 truncate font-mono text-xs" title={s.image}>
                  {imageLabel(s.image, images.data?.items)}
                </code>
              ),
            },
            { key: "port", header: "Port", sortValue: (s) => s.port, cell: (s) => <span className="tabular-nums">{s.port}</span> },
            {
              key: "state",
              header: "State",
              sortValue: (s) => s.state,
              cell: (s) => (
                <span className="inline-flex flex-wrap items-center gap-1.5">
                  <ServerStateBadge server={s} />
                  {s.restart_required && <Badge tone="warning">Restart required</Badge>}
                </span>
              ),
            },
          ]}
          actions={(s) => <ServerActions server={s} onJob={(job) => setJobs((m) => ({ ...m, [s.id]: job }))} />}
          rowDetail={(s) =>
            jobs[s.id] ? (
              <InlineJob
                key={jobs[s.id].id}
                job={jobs[s.id]}
                defaultShowLog={false}
                onDismiss={() =>
                  setJobs((m) => {
                    const next = { ...m };
                    delete next[s.id];
                    return next;
                  })
                }
              />
            ) : null
          }
        />
      )}
      {creating && <CreateServerDialog onClose={() => setCreating(false)} />}
      <ExportDialog open={exporting} onOpenChange={setExporting} />
      {importing && <ImportDialog onClose={() => setImporting(false)} />}
    </>
  );
}

/** One option per managed image, keyed by image id (the server stores the id). */
export function imageOptions(images: readonly Image[], projects: Map<number, Project>) {
  return images.map((img) => {
    const project = img.project_id !== null ? projects.get(img.project_id) : undefined;
    const extra = [img.tags.length > 1 ? `+${img.tags.length - 1} tags` : "", project ? project.name : ""].filter(Boolean).join(", ");
    return { value: img.id, label: extra ? `${imageName(img)} (${extra})` : imageName(img), projectId: img.project_id };
  });
}

/** Shown instead of a server form while no managed image exists. */
export function NoImagesNotice() {
  return (
    <Notice tone="info" title="No managed images yet">
      Servers run images built by Golem Deploy from a project. The build pipeline is not available yet, so until then an
      operator has to label an image on the host by hand (see docs/SERVERS.md). Images that are not managed are never offered.
    </Notice>
  );
}

function CreateServerDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const images = useImages();
  const projects = useProjects();
  const projectMap = useMemo(() => new Map((projects.data?.items ?? []).map((p) => [p.id, p])), [projects.data]);
  const options = useMemo(() => imageOptions(images.data?.items ?? [], projectMap), [images.data, projectMap]);

  const [name, setName] = useState("");
  const [imageChoice, setImageChoice] = useState("");
  const [projectId, setProjectId] = useState("");
  const [port, setPort] = useState("");
  const [start, setStart] = useState(false);
  const [env, setEnvDraft] = useState<EnvDraft>(emptyDraft());
  // The daemon's env errors (value errors name the key: env.<KEY>), until the draft changes.
  const [envServerErrors, setEnvServerErrors] = useState<EnvErrors>(noEnvErrors());
  const [errors, setErrors] = useState<Partial<Record<"name" | "image" | "port", string | null>>>({});
  const envResult = computeEnvDiff([], env);
  const setEnv = (d: EnvDraft) => {
    setEnvDraft(d);
    setEnvServerErrors(noEnvErrors());
  };
  const image = imageChoice || options[0]?.value || "";
  const noImages = images.isSuccess && options.length === 0;

  const create = useMutation({
    mutationFn: () =>
      api.servers.create({
        name,
        image,
        ...(projectId ? { project_id: Number(projectId) } : {}),
        ...(port.trim() ? { port: Number(port) } : {}),
        ...(Object.keys(envResult.diff.env_set).length ? { env: envResult.diff.env_set } : {}),
        desired_state: start ? "running" : "stopped",
      }),
    onSuccess: (server) => {
      toast.success(`Server ${server.name} created on port ${server.port}.`);
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      void queryClient.invalidateQueries({ queryKey: qk.status });
      onClose();
      void navigate({ to: "/servers/$id", params: { id: String(server.id) } });
    },
    onError: (err) => {
      if (!isApiError(err)) return;
      const envErrs = envErrorsFrom(err, env, []);
      if (envErrs) setEnvServerErrors(envErrs);
      else if (err.code === "name_taken") setErrors({ name: "A server with this name exists." });
      else if (err.code === "port_in_use") setErrors({ port: sentence(err.message) });
      else if (err.code === "validation_failed" && (err.field === "name" || err.field === "image" || err.field === "port")) {
        setErrors({ [err.field]: sentence(err.message) });
      }
    },
  });

  function pickImage(value: string) {
    setImageChoice(value);
    const opt = options.find((o) => o.value === value);
    if (opt?.projectId != null) setProjectId(String(opt.projectId));
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = {
      name: validateResourceName(name),
      image: image ? null : "Choose an image.",
      port: validatePort(port, { optional: true }),
    };
    setErrors(errs);
    if (errs.name || errs.image || errs.port || !envResult.valid) return;
    create.mutate();
  }

  const inline =
    isApiError(create.error) &&
    (create.error.code === "name_taken" ||
      create.error.code === "port_in_use" ||
      envErrorsFrom(create.error, env, []) !== null ||
      ["name", "image", "port"].includes(create.error.field ?? ""));

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="New server"
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="create-server" loading={create.isPending} disabled={!images.isSuccess || noImages}>
            Create server
          </Button>
        </>
      }
    >
      {images.isPending ? (
        <LoadingState label="Loading managed images" />
      ) : images.isError ? (
        <ErrorState error={images.error} title="Could not load the managed images" onRetry={() => void images.refetch()} />
      ) : noImages ? (
        <NoImagesNotice />
      ) : (
      <form id="create-server" onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Name" error={errors.name} hint="Lower-case letters, digits and '-'. Cannot be changed later.">
          {(p) => <Input {...p} spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} autoFocus />}
        </Field>
        <Field label="Image" error={errors.image} hint="Managed images only (built by Golem Deploy or labelled on the host), declaring at most 8 volumes.">
          {(p) => (
            <Select {...p} value={image} onChange={(e) => pickImage(e.target.value)}>
              {options.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field label="Project" hint="Optional. Links the server to the project its image was built from. Cannot be changed later.">
          {(p) => (
            <Select {...p} value={projectId} onChange={(e) => setProjectId(e.target.value)}>
              <option value="">None</option>
              {(projects.data?.items ?? []).map((proj) => (
                <option key={proj.id} value={proj.id}>
                  {proj.name}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field
          label="Port"
          error={errors.port}
          hint="Leave blank to take the lowest free port of the daemon's port range. Published as UDP and TCP."
        >
          {(p) => <Input {...p} inputMode="numeric" className="w-32" value={port} onChange={(e) => setPort(e.target.value)} />}
        </Field>
        <fieldset className="flex flex-col gap-2">
          <legend className="mb-1 text-sm font-medium">Environment</legend>
          <p className="text-xs text-muted-foreground">Values are stored encrypted and never shown again.</p>
          <EnvEditor existingKeys={[]} draft={env} onChange={setEnv} errors={mergeEnvErrors(envResult, envServerErrors)} />
        </fieldset>
        <Checkbox label="Start the server after creating it" checked={start} onChange={(e) => setStart(e.target.checked)} />
        {create.isError && !inline && <ErrorText error={create.error} />}
      </form>
      )}
    </Dialog>
  );
}

function ExportDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const api = useApi();
  const toast = useToast();
  return (
    <ConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title="Export server definitions"
      description={
        <>
          The file contains every server's environment variables <span className="font-medium text-foreground">in plain text</span>,
          including secrets. Store it like a password and delete it when you no longer need it.
        </>
      }
      confirmLabel="Download export"
      onConfirm={async () => {
        const data = await api.servers.exportAll();
        const stamp = new Date().toISOString().slice(0, 10);
        saveBlob(new Blob([JSON.stringify(data, null, 2) + "\n"], { type: "application/json" }), `golem-deploy-servers-${stamp}.json`);
        toast.success(`Exported ${data.servers.length} server definition${data.servers.length === 1 ? "" : "s"}.`);
      }}
    />
  );
}

function ImportDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [fileName, setFileName] = useState<string | null>(null);
  const [check, setCheck] = useState<{ value: ServerExport | null; errors: string[] } | null>(null);
  const [sent, setSent] = useState<ServerExport | null>(null);
  const importAll = useMutation({
    mutationFn: (body: ServerExport) => {
      setSent(body);
      return api.servers.importAll(body);
    },
    onSuccess: (res) => {
      toast.success(`Imported ${res.created.length} server${res.created.length === 1 ? "" : "s"}.`);
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      void queryClient.invalidateQueries({ queryKey: qk.status });
      onClose();
    },
  });

  async function pick(file: File | undefined) {
    importAll.reset();
    if (!file) {
      setFileName(null);
      setCheck(null);
      return;
    }
    setFileName(file.name);
    if (file.size > IMPORT_LIMIT_BYTES) {
      setCheck({ value: null, errors: [`The file is larger than the ${formatBytes(IMPORT_LIMIT_BYTES)} the daemon accepts in one import; split it.`] });
      return;
    }
    setCheck(checkServerImport(await file.text()));
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="Import server definitions"
      description="Creates every server in a JSON export file, or none if anything is wrong. Existing servers are not changed, and imported servers are created stopped."
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={!check?.value}
            loading={importAll.isPending}
            onClick={() => check?.value && importAll.mutate(check.value)}
          >
            Import {check?.value ? `${check.value.servers.length} server${check.value.servers.length === 1 ? "" : "s"}` : ""}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-4">
        <Field label="Export file" hint="A JSON file from Export on this or another Golem Deploy host.">
          {(p) => (
            <Input
              {...p}
              type="file"
              accept="application/json,.json"
              className="h-auto py-1.5"
              onChange={(e) => void pick(e.target.files?.[0])}
            />
          )}
        </Field>
        {check && check.errors.length > 0 && (
          <Notice tone="danger" title={`${fileName ?? "The file"} cannot be imported`}>
            <ul className="mt-1 list-disc space-y-0.5 pl-5">
              {check.errors.slice(0, 50).map((e, i) => (
                <li key={i}>{e}</li>
              ))}
              {check.errors.length > 50 && <li>and {check.errors.length - 50} more</li>}
            </ul>
          </Notice>
        )}
        {check?.value && (
          <Notice tone="info" title="Ready to import">
            {check.value.servers.map((s) => s.name).join(", ")}. Projects are matched by name, and every image must be a
            managed image on this host.
          </Notice>
        )}
        {importAll.isError && (
          <ErrorText error={importAll.error}>
            {isApiError(importAll.error) && importAll.error.field ? (
              <>
                <span className="font-medium">{describeImportField(importAll.error.field, sent)}:</span>{" "}
                {sentence(importAll.error.message)} Nothing was imported.
              </>
            ) : undefined}
          </ErrorText>
        )}
      </div>
    </Dialog>
  );
}
