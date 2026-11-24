import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { ArrowLeft, Download, GitBranch, Pencil, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError, sentence } from "@/api/errors";
import { isJobActive, isProjectBusy, useAccount, useBranches, useProject, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Job, Project, UpdateProjectRequest } from "@/api/types.gen";
import { ProjectBuildsSection } from "@/components/builds";
import { InlineJob, JobsPanel, useShownJob } from "@/components/jobs";
import { BuildSettingsSection, StaticAddonsSection } from "@/components/static-addons";
import {
  ProjectAuthFields,
  authErrorsFrom,
  emptyAuth,
  hasErrors,
  toProjectAuth,
  validateAuth,
  type AuthDraft,
  type AuthErrors,
} from "@/components/project-auth";
import { RepoLink } from "@/components/repo-link";
import { ProjectStatusBadge } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Field, Input, Select } from "@/components/ui/input";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative, shortCommit } from "@/lib/format";
import { cn } from "@/lib/utils";
import { checkRepoUrl, sameRepoOrigin, validateRepoUrl } from "@/lib/validators";
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
  const [dialog, setDialog] = useState<"branch" | "edit" | "delete" | null>(null);
  const activeJob = jobs.data?.items.find(isJobActive) ?? null;
  const shown = useShownJob(activeJob);

  const onJob = (job: Job) => {
    shown.show(job);
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
  const currentJob = shown.job;
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
        {p.status === "error" && !activeJob && (
          <Notice
            tone="warning"
            title={p.last_commit ? "The checkout may be out of date" : "The repository is not cloned"}
            action={
              <Button size="sm" variant="outline" loading={pull.isPending} onClick={() => pull.mutate()}>
                {p.last_commit ? "Pull to repair" : "Pull to clone again"}
              </Button>
            }
          >
            {p.last_commit
              ? "The last operation failed after it changed the checkout, or the daemon restarted during it. A pull resets the checkout to the remote branch."
              : "The clone failed. Check the job log for the reason, fix the URL or credentials if needed, then pull: without a checkout, a pull clones again."}
          </Notice>
        )}
        {currentJob && (
          <InlineJob
            key={currentJob.id}
            job={currentJob}
            onDismiss={shown.dismiss}
          />
        )}
        <ProjectBuildsSection project={p} busy={activeJob !== null} onJob={onJob} />
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
        <div className="grid gap-6 lg:grid-cols-2">
          <StaticAddonsSection project={p} isAdmin={isAdmin} />
          <BuildSettingsSection project={p} isAdmin={isAdmin} />
        </div>
        <Section title="Jobs" description="Recent clones, pulls, branch switches and builds of this project. Select one to see its log.">
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
            Delete <span className="font-medium text-foreground">{p.name}</span>? The checkout, the stored credentials and every
            build of the project with its image are removed. This is refused while a job runs for the project, while servers are
            configured with one of its builds, or while a server&apos;s container still uses one of its images.
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
      description={`Check out another branch of ${project.name}. Branches are listed from the remote (cached for 30 seconds); local changes in the checkout are discarded.`}
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
          <ErrorState error={branches.error} title="Could not list the remote's branches" onRetry={() => void branches.refetch()} />
        ) : branches.data.items.length === 0 ? (
          <p className="text-sm text-muted-foreground">The remote has no branches.</p>
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

/** The credential type to offer for replacement with this URL ("none" is the separate "clear" choice). */
function replaceTypeFor(draft: AuthDraft, repoUrl: string): AuthDraft {
  const kind = checkRepoUrl(repoUrl.trim()).kind;
  if (kind === "ssh" && draft.type !== "ssh") return { ...draft, type: "ssh" };
  if (kind === "https" && draft.type !== "https") return { ...draft, type: "https" };
  return draft;
}

function EditProjectDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [repoUrl, setRepoUrl] = useState(project.repo_url);
  const [mode, setMode] = useState<CredentialMode>("keep");
  const [auth, setAuth] = useState<AuthDraft>(emptyAuth(project.auth_type === "none" ? "https" : project.auth_type));
  const [urlError, setUrlError] = useState<string | null>(null);
  const [modeError, setModeError] = useState<string | null>(null);
  const [authErrors, setAuthErrors] = useState<AuthErrors>({});

  const url = repoUrl.trim();
  const kind = checkRepoUrl(url).kind;
  // A new URL on another scheme, host or port: the daemon never sends stored credentials there.
  const originChanged = url !== project.repo_url && kind !== null && !sameRepoOrigin(project.repo_url, url);

  const update = useMutation({
    mutationFn: (body: UpdateProjectRequest) => api.projects.update(project.id, body),
    onSuccess: (p) => {
      queryClient.setQueryData(qk.projects.detail(p.id), p);
      void queryClient.invalidateQueries({ queryKey: qk.projects.list });
      toast.success(`Project ${p.name} updated.`);
      onClose();
    },
    onError: (err) => {
      const aErrs = authErrorsFrom(err);
      if (aErrs) setAuthErrors(aErrs);
      else if (isApiError(err) && err.code === "validation_failed" && err.field === "repo_url") setUrlError(sentence(err.message));
      else if (isApiError(err) && err.code === "validation_failed" && err.field === "auth") setModeError(sentence(err.message));
    },
  });

  function changeUrl(value: string) {
    setRepoUrl(value);
    setModeError(null);
    setAuth((a) => replaceTypeFor(a, value));
    // "No credentials" is not possible for SSH URLs.
    if (checkRepoUrl(value.trim()).kind === "ssh" && mode === "clear") setMode("replace");
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    let uErr = validateRepoUrl(repoUrl);
    let mErr: string | null = null;
    if (!uErr && originChanged && mode === "keep") {
      mErr =
        kind === "ssh"
          ? "The new URL is on another host, port or scheme. Stored credentials are never sent to a new host: paste a deploy key for it."
          : "The new URL is on another host, port or scheme. Stored credentials are never sent to a new host: enter credentials for it, or choose No credentials for a public repository.";
    } else if (!uErr && mode === "keep" && project.auth_type !== "none" && kind !== null && kind !== project.auth_type) {
      uErr =
        project.auth_type === "https"
          ? "HTTPS credentials require an https:// repository URL; replace or remove the credentials together with the URL."
          : "An SSH key requires an ssh:// or user@host:path repository URL; replace or remove the credentials together with the URL.";
    } else if (!uErr && mode === "clear" && kind === "ssh") {
      mErr = "SSH repository URLs need a private key: choose Replace and paste a deploy key.";
    }
    setUrlError(uErr);
    setModeError(mErr);
    const body: UpdateProjectRequest = {};
    if (url !== project.repo_url) body.repo_url = url;
    if (mode === "replace") {
      const aErrs = validateAuth(auth, repoUrl);
      setAuthErrors(aErrs);
      if (hasErrors(aErrs)) return;
      body.auth = toProjectAuth(auth);
    } else if (mode === "clear") {
      body.auth = { type: "none" };
    }
    if (uErr || mErr) return;
    if (Object.keys(body).length === 0) {
      onClose();
      return;
    }
    update.mutate(body);
  }

  const modes: { value: CredentialMode; label: string; hint: string; disabled?: boolean }[] = [
    {
      value: "keep",
      label: "Keep current",
      hint: originChanged ? "Not possible for a new host" : authSummary(project),
      disabled: originChanged,
    },
    { value: "replace", label: project.auth_type === "none" && !originChanged ? "Add" : "Replace", hint: "Enter new credentials" },
    ...(kind !== "ssh" && (project.auth_type !== "none" || originChanged)
      ? [{ value: "clear" as const, label: project.auth_type !== "none" ? "Remove" : "No credentials", hint: "Public https repository" }]
      : []),
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
          {(p) => <Input {...p} spellCheck={false} value={repoUrl} onChange={(e) => changeUrl(e.target.value)} />}
        </Field>
        {originChanged && (
          <Notice tone="info" title="Credentials needed for the new host">
            The new URL uses another scheme, host or port than <span className="break-all font-mono text-xs">{project.repo_url}</span>.
            Credentials stored for the old host are never sent to a new one, so they cannot be kept:{" "}
            {kind === "ssh"
              ? "paste a deploy key for the new host."
              : "enter credentials for the new host, or choose No credentials for a public repository."}
          </Notice>
        )}
        <fieldset>
          <legend className="mb-1.5 text-sm font-medium">Credentials</legend>
          <div className="grid gap-2 sm:grid-cols-3" role="radiogroup">
            {modes.map((m) => {
              const checked = mode === m.value && !m.disabled;
              return (
                <label
                  key={m.value}
                  className={cn(
                    "flex flex-col rounded-md border px-3 py-2 text-sm focus-within:ring-2 focus-within:ring-ring",
                    m.disabled ? "cursor-not-allowed opacity-60" : "cursor-pointer",
                    checked ? "border-primary bg-accent" : "border-input",
                  )}
                >
                  <input
                    type="radio"
                    name="credential-mode"
                    className="sr-only"
                    checked={checked}
                    disabled={m.disabled}
                    onChange={() => {
                      setMode(m.value);
                      setModeError(null);
                    }}
                  />
                  <span className="font-medium">{m.label}</span>
                  <span className="text-xs text-muted-foreground">{m.hint}</span>
                </label>
              );
            })}
          </div>
          {modeError && (
            <p className="mt-1.5 text-xs text-destructive" role="alert">
              {modeError}
            </p>
          )}
        </fieldset>
        {mode === "replace" && <ProjectAuthFields draft={auth} onChange={setAuth} errors={authErrors} repoUrl={repoUrl} allowNone={false} />}
        {update.isError &&
          !(isApiError(update.error) && (update.error.field === "repo_url" || update.error.field === "auth" || authErrorsFrom(update.error))) && (
            <ErrorText error={update.error} />
          )}
      </form>
    </Dialog>
  );
}
