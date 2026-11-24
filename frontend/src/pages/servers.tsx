import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { Download, Plus, Upload } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError, sentence } from "@/api/errors";
import { useAccount, useProjects, useServers, useSuccessfulBuilds } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Build, Job, Project, ServerExport } from "@/api/types.gen";
import { buildName } from "@/components/builds";
import { EnvEditor } from "@/components/env-editor";
import { GettingStarted } from "@/components/getting-started";
import { InlineJob } from "@/components/jobs";
import { OriginsEditor, hasOriginErrors, noOriginErrors, originErrorsFrom, validateOrigins, type OriginErrors } from "@/components/origins-editor";
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
import { formatBytes, formatRelative } from "@/lib/format";
import { validatePort, validateResourceName } from "@/lib/validators";

export function ServersPage() {
  const servers = useServers();
  const projects = useProjects();
  const builds = useSuccessfulBuilds();
  const account = useAccount();
  const search: { create?: number } = useSearch({ strict: false });
  const navigate = useNavigate();
  const isAdmin = account.data?.role === "admin";
  const [creating, setCreating] = useState<{ projectId?: number } | null>(search.create ? { projectId: search.create } : null);
  const [exporting, setExporting] = useState(false);
  const [importing, setImporting] = useState(false);
  const [jobs, setJobs] = useState<Record<number, Job>>({});
  const projectNames = useMemo(() => new Map((projects.data?.items ?? []).map((p) => [p.id, p.name])), [projects.data]);
  const buildById = useMemo(() => new Map((builds.data?.items ?? []).map((b) => [b.id, b])), [builds.data]);

  function closeCreate() {
    setCreating(null);
    if (search.create) void navigate({ to: "/servers", search: {}, replace: true });
  }

  return (
    <>
      <PageHeader
        title="Servers"
        description="Each server runs a successful build of a project in a container, on a port published over both UDP and TCP."
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
            <Button onClick={() => setCreating({})}>
              <Plus />
              New server
            </Button>
          </>
        }
      />
      <GettingStarted />
      {servers.isPending ? (
        <LoadingState label="Loading servers" />
      ) : servers.isError ? (
        <ErrorState error={servers.error} title="Could not load servers" onRetry={() => void servers.refetch()} />
      ) : servers.data.items.length === 0 ? (
        <EmptyState
          title="No servers yet"
          description="Create a server from a successful build of a project to run a game."
          action={
            <Button onClick={() => setCreating({})}>
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
              key: "build",
              header: "Build",
              hideOnSmall: true,
              cell: (s) =>
                s.build_id === null ? (
                  <Badge tone="warning" title="Created before servers ran builds: assign a build on the server's page before it can start.">
                    No build
                  </Badge>
                ) : (
                  <Link to="/builds/$id" params={{ id: String(s.build_id) }} className="font-mono text-xs hover:underline">
                    {buildName(buildById.get(s.build_id) ?? { id: s.build_id, commit: s.commit })}
                  </Link>
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
      {creating && <CreateServerDialog initialProjectId={creating.projectId} onClose={closeCreate} />}
      <ExportDialog open={exporting} onOpenChange={setExporting} />
      {importing && <ImportDialog onClose={() => setImporting(false)} />}
    </>
  );
}

/** Successful builds grouped by project (newest first within each), only projects that have one. */
export function buildsByProject(builds: readonly Build[]): Map<number, Build[]> {
  const out = new Map<number, Build[]>();
  for (const b of [...builds].sort((a, z) => z.id - a.id)) {
    if (b.status !== "success") continue;
    out.set(b.project_id, [...(out.get(b.project_id) ?? []), b]);
  }
  return out;
}

/** One option per successful build: "#12 (4f1c2a9e0b7d), main, 2 hours ago". */
export function buildOptionLabel(b: Build, current?: number | null): string {
  return `${buildName(b)}, ${b.branch || "-"}, ${formatRelative(b.finished_at ?? b.created_at)}${b.id === current ? " (current)" : ""}`;
}

/** Shown instead of a server form while no successful build exists. */
export function NoBuildsNotice() {
  return (
    <Notice tone="info" title="No successful builds yet">
      A server runs a successful build of a project. Open a{" "}
      <Link to="/projects" className="underline underline-offset-2">
        project
      </Link>{" "}
      and start a build; once it succeeds you can create a server from it here.
    </Notice>
  );
}

type CreateErrors = Partial<Record<"name" | "build_id" | "port", string | null>>;

function CreateServerDialog({ initialProjectId, onClose }: { initialProjectId?: number; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const builds = useSuccessfulBuilds();
  const projects = useProjects();
  const grouped = useMemo(() => buildsByProject(builds.data?.items ?? []), [builds.data]);
  const choices: Project[] = (projects.data?.items ?? []).filter((p) => grouped.has(p.id));

  const [name, setName] = useState("");
  const [projectChoice, setProjectChoice] = useState<number | null>(initialProjectId ?? null);
  const [buildChoice, setBuildChoice] = useState<number | null>(null);
  const [port, setPort] = useState("");
  const [start, setStart] = useState(false);
  const [origins, setOriginsState] = useState<string[]>([]);
  const [originErrors, setOriginErrors] = useState<OriginErrors>(noOriginErrors());
  const [env, setEnvDraft] = useState<EnvDraft>(emptyDraft());
  // The daemon's env errors (value errors name the key: env.<KEY>), until the draft changes.
  const [envServerErrors, setEnvServerErrors] = useState<EnvErrors>(noEnvErrors());
  const [errors, setErrors] = useState<CreateErrors>({});
  const envResult = computeEnvDiff([], env);
  const setEnv = (d: EnvDraft) => {
    setEnvDraft(d);
    setEnvServerErrors(noEnvErrors());
  };
  const setOrigins = (o: string[]) => {
    setOriginsState(o);
    setOriginErrors(noOriginErrors());
  };
  const projectId = projectChoice !== null && grouped.has(projectChoice) ? projectChoice : (choices[0]?.id ?? null);
  const projectBuilds = projectId !== null ? (grouped.get(projectId) ?? []) : [];
  // The newest successful build of the project unless the user picked another.
  const buildId = buildChoice !== null && projectBuilds.some((b) => b.id === buildChoice) ? buildChoice : (projectBuilds[0]?.id ?? null);
  const loaded = builds.isSuccess && projects.isSuccess;
  const noBuilds = loaded && choices.length === 0;
  const askedProjectMissing = loaded && initialProjectId !== undefined && !grouped.has(initialProjectId);

  const create = useMutation({
    mutationFn: () =>
      api.servers.create({
        name,
        build_id: buildId ?? 0,
        ...(port.trim() ? { port: Number(port) } : {}),
        ...(origins.length ? { allowed_origins: origins.map((o) => o.trim()) } : {}),
        ...(Object.keys(envResult.diff.env_set).length ? { env: envResult.diff.env_set } : {}),
        desired_state: start ? "running" : "stopped",
      }),
    onSuccess: (server) => {
      toast.success(`Server ${server.name} created on port ${server.port}.${start ? " Starting it." : ""}`);
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      void queryClient.invalidateQueries({ queryKey: qk.builds.all });
      void queryClient.invalidateQueries({ queryKey: qk.status });
      onClose();
      void navigate({ to: "/servers/$id", params: { id: String(server.id) } });
    },
    onError: (err) => {
      if (!isApiError(err)) return;
      const envErrs = envErrorsFrom(err, env, []);
      const oErrs = originErrorsFrom(err);
      if (envErrs) setEnvServerErrors(envErrs);
      else if (oErrs) setOriginErrors(oErrs);
      else if (err.code === "name_taken") setErrors({ name: "A server with this name exists." });
      else if (err.code === "port_in_use") setErrors({ port: sentence(err.message) });
      else if (err.code === "validation_failed" && (err.field === "name" || err.field === "build_id" || err.field === "port")) {
        setErrors({ [err.field]: sentence(err.message) });
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs: CreateErrors = {
      name: validateResourceName(name),
      build_id: buildId === null ? "Choose a build." : null,
      port: validatePort(port, { optional: true }),
    };
    const oErrs = validateOrigins(origins);
    setErrors(errs);
    setOriginErrors(oErrs);
    if (errs.name || errs.build_id || errs.port || hasOriginErrors(oErrs) || !envResult.valid) return;
    create.mutate();
  }

  const inline =
    isApiError(create.error) &&
    (create.error.code === "name_taken" ||
      create.error.code === "port_in_use" ||
      envErrorsFrom(create.error, env, []) !== null ||
      originErrorsFrom(create.error) !== null ||
      ["name", "build_id", "port"].includes(create.error.field ?? ""));

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="New server"
      description="A server runs one successful build of a project. Its project cannot change later; its build can be replaced by another build of the same project."
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="create-server" loading={create.isPending} disabled={!loaded || noBuilds}>
            Create server
          </Button>
        </>
      }
    >
      {builds.isError ? (
        <ErrorState error={builds.error} title="Could not load the builds" onRetry={() => void builds.refetch()} />
      ) : projects.isError ? (
        <ErrorState error={projects.error} title="Could not load the projects" onRetry={() => void projects.refetch()} />
      ) : !loaded ? (
        <LoadingState label="Loading builds" />
      ) : noBuilds ? (
        <NoBuildsNotice />
      ) : (
        <form id="create-server" onSubmit={submit} noValidate className="flex flex-col gap-4">
          {askedProjectMissing && (
            <Notice tone="warning" title="That project has no successful build">
              Choose another project, or build it first.
            </Notice>
          )}
          <Field label="Name" error={errors.name} hint="Lower-case letters, digits and '-'. Cannot be changed later.">
            {(p) => <Input {...p} spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} autoFocus />}
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Project" hint="Only projects with a successful build. Cannot be changed later.">
              {(p) => (
                <Select
                  {...p}
                  value={projectId ?? ""}
                  onChange={(e) => {
                    setProjectChoice(Number(e.target.value));
                    setBuildChoice(null);
                    setErrors((x) => ({ ...x, build_id: null }));
                  }}
                >
                  {choices.map((proj) => (
                    <option key={proj.id} value={proj.id}>
                      {proj.name}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
            <Field label="Build" error={errors.build_id} hint="Successful builds, newest first.">
              {(p) => (
                <Select
                  {...p}
                  value={buildId ?? ""}
                  onChange={(e) => {
                    setBuildChoice(Number(e.target.value));
                    setErrors((x) => ({ ...x, build_id: null }));
                  }}
                >
                  {projectBuilds.map((b) => (
                    <option key={b.id} value={b.id}>
                      {buildOptionLabel(b)}
                    </option>
                  ))}
                </Select>
              )}
            </Field>
          </div>
          <Field label="Port" error={errors.port} hint="Leave blank to take the lowest free port of the daemon's port range. Published as UDP and TCP.">
            {(p) => <Input {...p} inputMode="numeric" className="w-32" value={port} onChange={(e) => setPort(e.target.value)} />}
          </Field>
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1 text-sm font-medium">Allowed origins</legend>
            <p className="text-xs text-muted-foreground">
              Optional. Extra browser origins (pages served elsewhere) that may open WebTransport and WebSocket connections to the
              game. The server&apos;s own address is always allowed.
            </p>
            <OriginsEditor origins={origins} onChange={setOrigins} errors={originErrors} />
          </fieldset>
          <fieldset className="flex flex-col gap-2">
            <legend className="mb-1 text-sm font-medium">Environment</legend>
            <p className="text-xs text-muted-foreground">
              Values are stored encrypted and never shown again. Names starting with GOLEM_ are set by Golem Deploy.
            </p>
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
            {check.value.servers.map((s) => s.name).join(", ")}. Projects are matched by name; each server gets the newest
            successful build of its commit (or of its project, without a commit) on this host.
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
