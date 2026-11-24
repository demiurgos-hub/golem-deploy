import { useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { ArrowLeft, Server as ServerIcon, Trash2 } from "lucide-react";
import { isApiError } from "@/api/errors";
import { isBuildActive, useAccount, useBuild, useJob, useProject, useServers, useSubjectJobs } from "@/api/queries";
import type { Build } from "@/api/types.gen";
import { BuildStatusBadge, DeleteBuildDialog, ServerLinks, buildCreator, buildName } from "@/components/builds";
import { InlineJob } from "@/components/jobs";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { ErrorState, LoadingState, Notice } from "@/components/ui/states";
import { formatDateTime, formatRelative } from "@/lib/format";
import { NotFoundPage } from "@/pages/not-found";

export function BuildDetailPage() {
  const { id: rawId } = useParams({ strict: false });
  const id = Number(rawId);
  if (!Number.isSafeInteger(id) || id <= 0) return <NotFoundPage />;
  return <BuildDetail key={id} id={id} />;
}

function BackLink({ projectId, projectName }: { projectId?: number; projectName?: string }) {
  if (projectId === undefined) {
    return (
      <Link to="/projects" className="inline-flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground">
        <ArrowLeft className="size-4" />
        Projects
      </Link>
    );
  }
  return (
    <Link
      to="/projects/$id"
      params={{ id: String(projectId) }}
      className="inline-flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
    >
      <ArrowLeft className="size-4" />
      {projectName ?? "Project"}
    </Link>
  );
}

function Mono({ value, copy }: { value: string; copy?: string }) {
  return (
    <span className="inline-flex max-w-full items-center gap-2">
      <code className="break-all font-mono text-xs">{value}</code>
      {copy && <CopyButton value={value} size="icon" variant="ghost" label={copy} />}
    </span>
  );
}

function BuildDetail({ id }: { id: number }) {
  const navigate = useNavigate();
  const build = useBuild(id);
  const account = useAccount();
  const [deleting, setDeleting] = useState<Build | null>(null);
  const isAdmin = account.data?.role === "admin";

  if (build.isPending) return <LoadingState label="Loading build" />;
  if (build.isError) {
    if (isApiError(build.error) && build.error.code === "not_found") return <NotFoundPage />;
    return (
      <>
        <PageHeader title={`Build #${id}`} back={<BackLink />} />
        <ErrorState error={build.error} title="Could not load the build" onRetry={() => void build.refetch()} />
      </>
    );
  }
  const b = build.data;
  return (
    <>
      <BuildHeader build={b} isAdmin={isAdmin} onDelete={() => setDeleting(b)} />
      <div className="flex flex-col gap-6">
        {b.status === "error" && b.detail && (
          <Notice tone="danger" title="The build failed">
            <span className="whitespace-pre-wrap break-words">{b.detail}</span>
          </Notice>
        )}
        {b.status === "success" && b.server_ids.length === 0 && (
          <Notice
            tone="info"
            title="Ready to run"
            action={
              <Link to="/servers" search={{ create: b.project_id }} className="text-sm font-medium underline underline-offset-4">
                Create a server
              </Link>
            }
          >
            No server runs this build yet. Create a server from it, or deploy it to an existing server of this project from that
            server&apos;s page.
          </Notice>
        )}
        <BuildLogSection build={b} />
        <BuildDetailsSection build={b} />
      </div>
      <DeleteBuildDialog
        build={deleting}
        onClose={() => setDeleting(null)}
        onDeleted={(d) => void navigate({ to: "/projects/$id", params: { id: String(d.project_id) } })}
      />
    </>
  );
}

function BuildHeader({ build: b, isAdmin, onDelete }: { build: Build; isAdmin: boolean; onDelete: () => void }) {
  const project = useProject(b.project_id);
  return (
    <PageHeader
      back={<BackLink projectId={b.project_id} projectName={project.data?.name} />}
      title={
        <span className="inline-flex flex-wrap items-center gap-2">
          Build {buildName(b)}
          <BuildStatusBadge status={b.status} />
        </span>
      }
      description={project.data ? `Project ${project.data.name}, branch ${b.branch || "-"}.` : undefined}
      actions={
        isAdmin && (
          <Button
            variant="outline"
            onClick={onDelete}
            disabled={isBuildActive(b)}
            title={isBuildActive(b) ? "Cancel the build's job first." : undefined}
          >
            <Trash2 />
            Delete
          </Button>
        )
      }
    />
  );
}

/** The build's output: the log of its project.build job, followed live while it runs. */
function BuildLogSection({ build }: { build: Build }) {
  return (
    <Section title="Build log" description="The sanitised output of the build job: the generated Dockerfile first, then docker build.">
      {build.job_id === null ? (
        <p className="text-sm text-muted-foreground">The job record of this build is gone, so its log is no longer available.</p>
      ) : (
        <BuildJob jobId={build.job_id} />
      )}
    </Section>
  );
}

function BuildJob({ jobId }: { jobId: number }) {
  const job = useJob(jobId);
  if (job.isPending) return <LoadingState label="Loading the build job" />;
  if (job.isError) {
    if (isApiError(job.error) && job.error.code === "not_found") {
      return <p className="text-sm text-muted-foreground">The job record of this build is gone, so its log is no longer available.</p>;
    }
    return <ErrorState error={job.error} title="Could not load the build job" onRetry={() => void job.refetch()} />;
  }
  return <InlineJob key={job.data.id} job={job.data} />;
}

function BuildDetailsSection({ build: b }: { build: Build }) {
  const account = useAccount();
  const project = useProject(b.project_id);
  const servers = useServers();
  const jobs = useSubjectJobs("project", b.project_id, 100);
  const ok = b.status === "success";
  return (
    <Section title="Details">
      <DetailList
        items={[
          {
            term: "Project",
            value: (
              <Link to="/projects/$id" params={{ id: String(b.project_id) }} className="hover:underline">
                {project.data?.name ?? `Project ${b.project_id}`}
              </Link>
            ),
          },
          { term: "Branch", value: <code className="font-mono text-xs">{b.branch || "-"}</code> },
          {
            term: "Commit",
            value: b.commit ? <Mono value={b.commit} copy="Copy commit hash" /> : <span className="text-muted-foreground">Not known until the build starts</span>,
          },
          { term: "Engine version", value: b.engine_version ? <code className="font-mono text-xs">{b.engine_version}</code> : "-" },
          {
            term: "Image tag",
            value: b.image_tag ? <Mono value={b.image_tag} copy="Copy image tag" /> : <span className="text-muted-foreground">Set when the build succeeds</span>,
          },
          { term: "Image id", value: b.image_id ? <Mono value={b.image_id} /> : <span className="text-muted-foreground">-</span> },
          {
            term: "Schema fingerprint",
            value: b.schema_fingerprint ? (
              <Mono value={b.schema_fingerprint} copy="Copy schema fingerprint" />
            ) : (
              <span className="text-muted-foreground">
                {ok ? "Not known yet: recorded when a server running this build first reports ready." : "-"}
              </span>
            ),
          },
          {
            term: "Transport paths",
            value: ok ? (
              <span className="flex flex-col gap-0.5 text-xs">
                <span>
                  WebTransport <code className="font-mono">{b.webtransport_path || "-"}</code>
                </span>
                <span>
                  WebSocket fallback <code className="font-mono">{b.websocket_path || "-"}</code>
                </span>
              </span>
            ) : (
              <span className="text-muted-foreground">Set when the build succeeds</span>
            ),
          },
          {
            term: "Servers",
            value: (
              <span className="inline-flex items-center gap-1.5">
                <ServerIcon aria-hidden className="size-3.5 text-muted-foreground" />
                <ServerLinks ids={b.server_ids} servers={servers.data?.items} />
              </span>
            ),
          },
          { term: "Started by", value: buildCreator(b, jobs.data?.items, account.data) },
          { term: "Queued", value: <span title={formatDateTime(b.created_at)}>{formatRelative(b.created_at)}</span> },
          { term: "Started", value: formatDateTime(b.started_at) },
          { term: "Finished", value: formatDateTime(b.finished_at) },
        ]}
      />
    </Section>
  );
}
