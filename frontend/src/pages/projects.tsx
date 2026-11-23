import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Download, Plus } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError } from "@/api/errors";
import { isProjectBusy, useAccount, useProjects } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Job, Project } from "@/api/types.gen";
import { InlineJob } from "@/components/jobs";
import { ProjectAuthFields, emptyAuth, hasErrors, toProjectAuth, validateAuth, type AuthDraft, type AuthErrors } from "@/components/project-auth";
import { ProjectStatusBadge } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { Dialog } from "@/components/ui/dialog";
import { Field, Input } from "@/components/ui/input";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, ErrorText, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative } from "@/lib/format";
import { RepoLink } from "@/components/repo-link";
import { validateRepoUrl, validateResourceName } from "@/lib/validators";

export function ProjectsPage() {
  const projects = useProjects();
  const account = useAccount();
  const isAdmin = account.data?.role === "admin";
  const [creating, setCreating] = useState(false);
  // Latest job started from this list, per project.
  const [jobs, setJobs] = useState<Record<number, Job>>({});

  return (
    <>
      <PageHeader
        title="Projects"
        description="Git repositories the daemon clones and builds into game server images."
        actions={
          isAdmin && (
            <Button onClick={() => setCreating(true)}>
              <Plus />
              New project
            </Button>
          )
        }
      />
      {projects.isPending ? (
        <LoadingState label="Loading projects" />
      ) : projects.isError ? (
        <ErrorState error={projects.error} title="Could not load projects" onRetry={() => void projects.refetch()} />
      ) : projects.data.items.length === 0 ? (
        <EmptyState
          title="No projects yet"
          description={isAdmin ? "Add a Git repository to get started." : "An administrator has to add a project first."}
          action={
            isAdmin && (
              <Button onClick={() => setCreating(true)}>
                <Plus />
                New project
              </Button>
            )
          }
        />
      ) : (
        <DataTable
          label="Projects"
          data={projects.data.items}
          rowKey={(p) => p.id}
          initialSort={{ key: "name", asc: true }}
          columns={[
            {
              key: "name",
              header: "Name",
              sortValue: (p) => p.name,
              cell: (p) => (
                <Link to="/projects/$id" params={{ id: String(p.id) }} className="font-medium underline-offset-4 hover:underline">
                  {p.name}
                </Link>
              ),
            },
            {
              key: "repo",
              header: "Repository",
              hideOnSmall: true,
              cell: (p) => <RepoLink url={p.repo_url} className="max-w-72 truncate" />,
            },
            { key: "branch", header: "Branch", sortValue: (p) => p.branch, cell: (p) => <code className="font-mono text-xs">{p.branch || "-"}</code> },
            { key: "status", header: "Status", sortValue: (p) => p.status, cell: (p) => <ProjectStatusBadge status={p.status} /> },
            {
              key: "pulled",
              header: "Last pulled",
              hideOnSmall: true,
              sortValue: (p) => p.last_pulled_at ?? "",
              cell: (p) => <span title={formatDateTime(p.last_pulled_at)}>{formatRelative(p.last_pulled_at)}</span>,
            },
          ]}
          actions={(p) => <PullButton project={p} onJob={(job) => setJobs((m) => ({ ...m, [p.id]: job }))} />}
          rowDetail={(p) =>
            jobs[p.id] ? (
              <InlineJob
                key={jobs[p.id].id}
                job={jobs[p.id]}
                defaultShowLog={false}
                onDismiss={() =>
                  setJobs((m) => {
                    const next = { ...m };
                    delete next[p.id];
                    return next;
                  })
                }
              />
            ) : null
          }
        />
      )}
      {creating && <CreateProjectDialog onClose={() => setCreating(false)} />}
    </>
  );
}

/** Per-row action with its own pending state. */
function PullButton({ project, onJob }: { project: Project; onJob: (job: Job) => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const pull = useMutation({
    mutationFn: () => api.projects.pull(project.id),
    onSuccess: (job) => {
      onJob(job);
      void queryClient.invalidateQueries({ queryKey: qk.projects.all });
    },
    onError: (err) => toast.error(err, `Could not pull ${project.name}`),
  });
  return (
    <Button size="sm" variant="outline" loading={pull.isPending} disabled={isProjectBusy(project)} onClick={() => pull.mutate()}>
      {!pull.isPending && <Download />}
      Pull
    </Button>
  );
}

function CreateProjectDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [repoUrl, setRepoUrl] = useState("");
  const [branch, setBranch] = useState("");
  const [auth, setAuth] = useState<AuthDraft>(emptyAuth());
  const [errors, setErrors] = useState<Partial<Record<"name" | "repoUrl", string | null>>>({});
  const [authErrors, setAuthErrors] = useState<AuthErrors>({});

  const create = useMutation({
    mutationFn: () =>
      api.projects.create({
        name,
        repo_url: repoUrl.trim(),
        ...(branch.trim() ? { branch: branch.trim() } : {}),
        auth: toProjectAuth(auth),
      }),
    onSuccess: (res) => {
      toast.success(`Project ${res.project.name} created. Cloning started.`);
      queryClient.setQueryData(qk.jobs.detail(res.job.id), res.job);
      void queryClient.invalidateQueries({ queryKey: qk.projects.all });
      void queryClient.invalidateQueries({ queryKey: qk.status });
      onClose();
      void navigate({ to: "/projects/$id", params: { id: String(res.project.id) } });
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "name_taken") setErrors({ name: "A project with this name exists." });
      else if (isApiError(err) && err.code === "validation_failed") {
        if (err.field === "name") setErrors({ name: err.message });
        else if (err.field === "repo_url") setErrors({ repoUrl: err.message });
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = { name: validateResourceName(name), repoUrl: validateRepoUrl(repoUrl) };
    const aErrs = validateAuth(auth, repoUrl);
    setErrors(errs);
    setAuthErrors(aErrs);
    if (errs.name || errs.repoUrl || hasErrors(aErrs)) return;
    create.mutate();
  }

  const inline = isApiError(create.error) && (create.error.code === "name_taken" || create.error.field === "name" || create.error.field === "repo_url");
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="New project"
      description="The daemon clones the repository right away. You can follow the clone on the project page."
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="create-project" loading={create.isPending}>
            Create and clone
          </Button>
        </>
      }
    >
      <form id="create-project" onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Name" error={errors.name} hint="Lower-case letters, digits and '-'. Cannot be changed later.">
          {(p) => <Input {...p} spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} autoFocus />}
        </Field>
        <Field label="Repository URL" error={errors.repoUrl} hint="https://host/owner/repo.git, ssh://git@host/owner/repo.git or git@host:owner/repo.git">
          {(p) => <Input {...p} spellCheck={false} autoComplete="off" value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} />}
        </Field>
        <Field label="Branch" hint="Leave blank to use the repository's default branch.">
          {(p) => <Input {...p} spellCheck={false} value={branch} onChange={(e) => setBranch(e.target.value)} />}
        </Field>
        <ProjectAuthFields draft={auth} onChange={setAuth} errors={authErrors} />
        {create.isError && !inline && <ErrorText error={create.error} />}
      </form>
    </Dialog>
  );
}
