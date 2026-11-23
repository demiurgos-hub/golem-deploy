import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { ArrowLeft, Download, GitBranch, Pencil, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError } from "@/api/errors";
import { isJobActive, isProjectBusy, useAccount, useBranches, useProject, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Job, Project, UpdateProjectRequest } from "@/api/types.gen";
import { InlineJob, JobsPanel } from "@/components/jobs";
import { ProjectAuthFields, emptyAuth, hasErrors, toProjectAuth, validateAuth, type AuthDraft, type AuthErrors } from "@/components/project-auth";
import { RepoLink } from "@/components/repo-link";
import { ProjectStatusBadge } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/input";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative, shortCommit } from "@/lib/format";
import { cn } from "@/lib/utils";
import { validateRepoUrl } from "@/lib/validators";
import { NotFoundPage } from "@/pages/not-found";

export function ProjectDetailPage() {
  const { id: rawId } = useParams({ strict: false });
  const id = Number(rawId);
  if (!Number.isSafeInteger(id) || id <= 0) return <NotFoundPage />;
  return <ProjectDetail key={id} id={id} />;
}

function BackLink() {
  return (
    <Link to="/projects" className="inline-flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
      <ArrowLeft className="size-4" />
      Projects
    </Link>
  );
}

function ProjectDetail({ id }: { id: number }) {
  const api = useApi();
  const toast = useToast();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const project = useProject(id);
  const account = useAccount();
  const jobs = useSubjectJobs("project", id);
  const isAdmin = account.data?.role === "admin";
  const [startedJob, setStartedJob] = useState<Job | null>(null);
  const [dialog, setDialog] = useState<"branch" | "edit" | "delete" | null>(null);

  const onJob = (job: Job) => {
    setStartedJob(job);
    queryClient.setQueryData(qk.jobs.detail(job.id), job);
    void queryClient.invalidateQueries({ queryKey: qk.projects.detail(id) });
    void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("project", id) });
  };

  const pull = useMutation({
    mutationFn: () => api.projects.pull(id),
    onSuccess: onJob,
    onError: (err) => toast.error(err, "Could not start the pull"),
  });

  if (project.isPending) return <LoadingState label="Loading project" />;
  if (project.isError) {
    if (isApiError(project.error) && project.error.code === "not_found") return <NotFoundPage />;
    return (
      <>
        <PageHeader title="Project" back={<BackLink />} />
        <ErrorState error={project.error} title="Could not load the project" onRetry={() => void project.refetch()} />
      </>
    );
  }

  const p = project.data;
  // Show the job this page started, or else whatever job is running for the project (e.g. the initial clone).
  const activeJob = jobs.data?.items.find(isJobActive) ?? null;
  const currentJob = startedJob ?? activeJob;
  const busy = isProjectBusy(p) || activeJob !== null;

  return (
    <>
      <PageHeader
        back={<BackLink />}
        title={
          <span className="inline-flex flex-wrap items-center gap-2">
            {p.name}
            <ProjectStatusBadge status={p.status} />
          </span>
        }
        actions={
          <>
            <Button variant="outline" loading={pull.isPending} disabled={busy} onClick={() => pull.mutate()}>
              {!pull.isPending && <Download />}
              Pull
            </Button>
            <Button variant="outline" disabled={busy} onClick={() => setDialog("branch")}>
              <GitBranch />
              Switch branch
            </Button>
            {isAdmin && (
              <>
                <Button variant="outline" onClick={() => setDialog("edit")}>
                  <Pencil />
                  Edit
                </Button>
                <Button variant="outline" onClick={() => setDialog("delete")}>
                  <Trash2 />
                  Delete
                </Button>
              </>
            )}
          </>
        }
      />
      <div className="flex flex-col gap-6">
        {currentJob && (
          <InlineJob
            key={currentJob.id}
            job={currentJob}
            onDismiss={startedJob ? () => setStartedJob(null) : undefined}
          />
        )}
        <Section title="Details">
          <DetailList
            items={[
              { term: "Repository", value: <RepoLink url={p.repo_url} /> },
              { term: "Branch", value: <code className="font-mono text-xs">{p.branch || "-"}</code> },
              {
                term: "Last commit",
                value: p.last_commit ? (
                  <span className="inline-flex items-center gap-2">
                    <code className="font-mono text-xs" title={p.last_commit}>
                      {shortCommit(p.last_commit)}
                    </code>
                    <CopyButton value={p.last_commit} size="icon" variant="ghost" label="Copy commit hash" />
                  </span>
                ) : (
                  <span className="text-muted-foreground">Not cloned yet</span>
                ),
              },
              { term: "Last pulled", value: <span title={formatDateTime(p.last_pulled_at)}>{formatRelative(p.last_pulled_at)}</span> },
              { term: "Authentication", value: authSummary(p) },
              { term: "Created", value: formatDateTime(p.created_at) },
              { term: "Updated", value: formatDateTime(p.updated_at) },
            ]}
          />
        </Section>
        <Section title="Jobs" description="Recent operations on this project. Select one to see its log.">
          <JobsPanel subjectType="project" subjectId={id} />
        </Section>
      </div>

      {dialog === "branch" && <SwitchBranchDialog project={p} onClose={() => setDialog(null)} onJob={onJob} />}
      {dialog === "edit" && <EditProjectDialog project={p} onClose={() => setDialog(null)} />}
      <ConfirmDialog
        open={dialog === "delete"}
        onOpenChange={(o) => !o && setDialog(null)}
        title="Delete project"
        description={
          <>
            Delete <span className="font-medium text-foreground">{p.name}</span>? The checkout and stored credentials are removed.
            Servers created from it keep running but lose the link to the project.
          </>
        }
        confirmLabel="Delete project"
        destructive
        onConfirm={async () => {
          await api.projects.remove(p.id);
          toast.success(`Project ${p.name} deleted.`);
          queryClient.removeQueries({ queryKey: qk.projects.detail(p.id) });
          void queryClient.invalidateQueries({ queryKey: qk.projects.list });
          void queryClient.invalidateQueries({ queryKey: qk.status });
          void navigate({ to: "/projects" });
        }}
      />
    </>
  );
}

function authSummary(p: Project): string {
  switch (p.auth_type) {
    case "https":
      return `HTTPS as ${p.auth_username || "(no username)"}${p.has_secret ? ", secret stored" : ", no secret stored"}`;
    case "ssh":
      return p.has_secret ? "SSH key stored" : "SSH, no key stored";
    default:
      return "None";
  }
}

function SwitchBranchDialog({ project, onClose, onJob }: { project: Project; onClose: () => void; onJob: (job: Job) => void }) {
  const api = useApi();
  const branches = useBranches(project.id, true);
  const [branch, setBranch] = useState("");
  const selected = branch || branches.data?.current || project.branch;
  const switchBranch = useMutation({
    mutationFn: () => api.projects.switchBranch(project.id, { branch: selected }),
    onSuccess: (job) => {
      onJob(job);
      onClose();
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    if (selected && selected !== project.branch) switchBranch.mutate();
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="Switch branch"
      description={`Check out another branch of ${project.name}. The list comes from the last fetch; pull first to see new branches.`}
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            form="switch-branch"
            loading={switchBranch.isPending}
            disabled={!branches.data || !selected || selected === project.branch}
          >
            Switch
          </Button>
        </>
      }
    >
      <form id="switch-branch" onSubmit={submit} className="flex flex-col gap-4">
        {branches.isPending ? (
          <LoadingState label="Loading branches" />
        ) : branches.isError ? (
          <ErrorState error={branches.error} title="Could not load branches" onRetry={() => void branches.refetch()} />
        ) : branches.data.items.length === 0 ? (
          <p className="text-sm text-muted-foreground">No remote branches found. Pull the project and try again.</p>
        ) : (
          <Field label="Branch" hint={`Current branch: ${branches.data.current || project.branch}`}>
            {(p) => (
              <Select {...p} value={selected} onChange={(e) => setBranch(e.target.value)}>
                {branches.data.items.map((b) => (
                  <option key={b} value={b}>
                    {b}
                    {b === branches.data.current ? " (current)" : ""}
                  </option>
                ))}
              </Select>
            )}
          </Field>
        )}
        {switchBranch.isError && <ErrorText error={switchBranch.error} />}
      </form>
    </Dialog>
  );
}

type CredentialMode = "keep" | "replace" | "clear";

function EditProjectDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [repoUrl, setRepoUrl] = useState(project.repo_url);
  const [mode, setMode] = useState<CredentialMode>("keep");
  const [auth, setAuth] = useState<AuthDraft>(emptyAuth(project.auth_type === "none" ? "https" : project.auth_type));
  const [urlError, setUrlError] = useState<string | null>(null);
  const [authErrors, setAuthErrors] = useState<AuthErrors>({});

  const update = useMutation({
    mutationFn: (body: UpdateProjectRequest) => api.projects.update(project.id, body),
    onSuccess: (p) => {
      queryClient.setQueryData(qk.projects.detail(p.id), p);
      void queryClient.invalidateQueries({ queryKey: qk.projects.list });
      toast.success(`Project ${p.name} updated.`);
      onClose();
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "validation_failed" && err.field === "repo_url") setUrlError(err.message);
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const uErr = validateRepoUrl(repoUrl);
    setUrlError(uErr);
    const body: UpdateProjectRequest = {};
    if (repoUrl.trim() !== project.repo_url) body.repo_url = repoUrl.trim();
    if (mode === "replace") {
      const aErrs = validateAuth(auth, repoUrl);
      setAuthErrors(aErrs);
      if (hasErrors(aErrs)) return;
      body.auth = toProjectAuth(auth);
    } else if (mode === "clear") {
      body.auth = { type: "none" };
    }
    if (uErr) return;
    if (Object.keys(body).length === 0) {
      onClose();
      return;
    }
    update.mutate(body);
  }

  const modes: { value: CredentialMode; label: string; hint: string }[] = [
    { value: "keep", label: "Keep current", hint: authSummary(project) },
    { value: "replace", label: "Replace", hint: "Enter new credentials" },
    ...(project.auth_type !== "none" ? [{ value: "clear" as const, label: "Remove", hint: "Use no credentials" }] : []),
  ];

  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={`Edit ${project.name}`}
      description="Stored secrets are never shown. Replace them to change them."
      size="lg"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="edit-project" loading={update.isPending}>
            Save
          </Button>
        </>
      }
    >
      <form id="edit-project" onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Repository URL" error={urlError}>
          {(p) => <Input {...p} spellCheck={false} value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} />}
        </Field>
        <fieldset>
          <legend className="mb-1.5 text-sm font-medium">Credentials</legend>
          <div className="grid gap-2 sm:grid-cols-3" role="radiogroup">
            {modes.map((m) => (
              <label
                key={m.value}
                className={cn(
                  "flex cursor-pointer flex-col rounded-md border px-3 py-2 text-sm focus-within:ring-2 focus-within:ring-ring",
                  mode === m.value ? "border-primary bg-accent" : "border-input",
                )}
              >
                <input
                  type="radio"
                  name="credential-mode"
                  className="sr-only"
                  checked={mode === m.value}
                  onChange={() => setMode(m.value)}
                />
                <span className="font-medium">{m.label}</span>
                <span className="text-xs text-muted-foreground">{m.hint}</span>
              </label>
            ))}
          </div>
        </fieldset>
        {mode === "replace" && <ProjectAuthFields draft={auth} onChange={setAuth} errors={authErrors} />}
        {update.isError && !(isApiError(update.error) && update.error.field === "repo_url") && <ErrorText error={update.error} />}
      </form>
    </Dialog>
  );
}
