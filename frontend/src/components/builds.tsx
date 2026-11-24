import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Hammer, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { isBuildActive, isProjectBusy, useAccount, useProjectBuilds, useServers, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Build, CurrentAccount, Job, Project, Server } from "@/api/types.gen";
import { JobStatus } from "@/components/jobs";
import { GameAddress } from "@/components/game-address";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox } from "@/components/ui/input";
import { Section } from "@/components/ui/page";
import { EmptyState, ErrorState, ErrorText, LoadingState } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative, shortCommit } from "@/lib/format";

/** Build statuses are the job statuses (queued, running, success, error, cancelled). */
export function BuildStatusBadge({ status }: { status: Build["status"] }) {
  return <JobStatus status={status} />;
}

/** "#12 (4f1c2a9e0b7d)": a build as people refer to it. */
export function buildName(b: Pick<Build, "id" | "commit">): string {
  return b.commit ? `#${b.id} (${shortCommit(b.commit)})` : `#${b.id}`;
}

export function BuildLink({ build, className }: { build: Pick<Build, "id" | "commit">; className?: string }) {
  return (
    <Link to="/builds/$id" params={{ id: String(build.id) }} className={className ?? "font-medium underline-offset-4 hover:underline"}>
      Build {buildName(build)}
    </Link>
  );
}

/**
 * Who started a build. Builds carry only the account id; the username comes
 * from the build's job when it is in the loaded job list.
 */
export function buildCreator(b: Pick<Build, "created_by" | "job_id">, jobs: readonly Job[] | undefined, me: Pick<CurrentAccount, "id"> | undefined): string {
  if (b.created_by === null) return "the daemon or a deleted account";
  if (me && b.created_by === me.id) return "you";
  const job = b.job_id !== null ? jobs?.find((j) => j.id === b.job_id) : undefined;
  return job?.created_by_username ?? `account ${b.created_by}`;
}

/** Links to the servers configured with a build. */
export function ServerLinks({ ids, servers }: { ids: readonly number[]; servers: readonly Server[] | undefined }) {
  if (ids.length === 0) return <span className="text-muted-foreground">None</span>;
  return (
    <span className="flex flex-wrap gap-x-2">
      {ids.map((id) => {
        const server = servers?.find((s) => s.id === id);
        return (
          <span key={id} className="flex flex-col items-start gap-1">
            <Link to="/servers/$id" params={{ id: String(id) }} className="hover:underline">
              {server?.name ?? `server ${id}`}
            </Link>
            {server && <GameAddress url={server.public_url} />}
          </span>
        );
      })}
    </span>
  );
}

/** Admin-only deletion of a finished build; the daemon's 409 reason (in use, still running) is shown in the dialog. */
export function DeleteBuildDialog({ build, onClose, onDeleted }: { build: Build | null; onClose: () => void; onDeleted?: (b: Build) => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  return (
    <ConfirmDialog
      open={build !== null}
      onOpenChange={(o) => !o && onClose()}
      title="Delete build"
      description={
        build && (
          <>
            Delete build <span className="font-medium text-foreground">{buildName(build)}</span> and its image? Builds that servers
            are configured with, or whose image a server&apos;s container still uses (also a stopped one), cannot be deleted.
          </>
        )
      }
      confirmLabel="Delete build"
      destructive
      onConfirm={async () => {
        if (!build) return;
        await api.builds.remove(build.id);
        toast.success(`Build ${buildName(build)} deleted.`);
        queryClient.removeQueries({ queryKey: qk.builds.detail(build.id) });
        void queryClient.invalidateQueries({ queryKey: qk.builds.all });
        void queryClient.invalidateQueries({ queryKey: qk.images });
        onDeleted?.(build);
      }}
    />
  );
}

/**
 * The project's builds, newest first, with starting a build (optionally
 * pulling first) and, for admins, deleting one. A started build is a
 * project.build job: `onJob` shows it inline on the page.
 */
export function ProjectBuildsSection({ project, busy, onJob }: { project: Project; busy: boolean; onJob: (job: Job) => void }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const account = useAccount();
  const builds = useProjectBuilds(project.id);
  const servers = useServers();
  // Usernames of who started each build (builds only carry the account id).
  const jobs = useSubjectJobs("project", project.id, 100);
  const isAdmin = account.data?.role === "admin";
  const noCheckout = project.last_commit === "";
  // Without a checkout a build must pull; otherwise the user decides (off by default).
  const [pullChoice, setPull] = useState(false);
  const pull = noCheckout || pullChoice;
  const [deleting, setDeleting] = useState<Build | null>(null);
  const activeBuild = builds.data?.items.find(isBuildActive);

  const start = useMutation({
    mutationFn: () => api.projects.build(project.id, pull ? { pull: true } : {}),
    onSuccess: (res) => {
      queryClient.setQueryData(qk.jobs.detail(res.job.id), res.job);
      queryClient.setQueryData(qk.builds.detail(res.build.id), res.build);
      void queryClient.invalidateQueries({ queryKey: qk.builds.forProject(project.id) });
      void queryClient.invalidateQueries({ queryKey: qk.projects.detail(project.id) });
      void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("project", project.id) });
      onJob(res.job);
    },
  });

  const blockedReason = isProjectBusy(project)
    ? "Wait for the project's current operation to finish."
    : busy
      ? "Another job is running for this project; builds wait for it."
      : null;

  return (
    <Section
      title="Builds"
      description="Each build turns the checkout into a verified image that servers run. The full output is the build's log."
      actions={
        <>
          <Checkbox
            label="Pull first"
            checked={pull}
            disabled={noCheckout || start.isPending}
            onChange={(e) => setPull(e.target.checked)}
            className="text-xs"
            title={noCheckout ? "The project has no checkout yet: a build without a pull is refused." : undefined}
          />
          <Button size="sm" loading={start.isPending} disabled={blockedReason !== null} title={blockedReason ?? undefined} onClick={() => start.mutate()}>
            {!start.isPending && <Hammer />}
            Start build
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          {pull
            ? "The build first pulls the branch (local changes are discarded), then builds the new commit."
            : `The build uses the current checkout${project.last_commit ? ` (commit ${shortCommit(project.last_commit)})` : ""}. Tick "Pull first" to build the branch's latest commit.`}
          {activeBuild ? " One build runs at a time on this host; others wait in the queue." : ""}
        </p>
        {start.isError && <ErrorText error={start.error} />}
        {builds.isPending ? (
          <LoadingState label="Loading builds" />
        ) : builds.isError ? (
          <ErrorState error={builds.error} title="Could not load builds" onRetry={() => void builds.refetch()} />
        ) : builds.data.items.length === 0 ? (
          <EmptyState
            title="No builds yet"
            description="Start a build to turn this project into an image. Once a build succeeds, you can create a server from it."
          />
        ) : (
          <DataTable
            label="Builds"
            data={builds.data.items}
            rowKey={(b) => b.id}
            initialSort={{ key: "id", asc: false }}
            columns={[
              {
                key: "id",
                header: "Build",
                sortValue: (b) => b.id,
                cell: (b) => (
                  <Link to="/builds/$id" params={{ id: String(b.id) }} className="font-medium underline-offset-4 hover:underline">
                    #{b.id}
                  </Link>
                ),
              },
              {
                key: "status",
                header: "Status",
                sortValue: (b) => b.status,
                cell: (b) => (
                  <span className="flex flex-col items-start gap-1">
                    <BuildStatusBadge status={b.status} />
                    {b.detail && (
                      <span className={(b.status === "error" ? "text-destructive" : "text-muted-foreground") + " max-w-80 whitespace-pre-wrap break-words text-xs"}>
                        {b.detail}
                      </span>
                    )}
                  </span>
                ),
              },
              { key: "branch", header: "Branch", hideOnSmall: true, cell: (b) => <code className="font-mono text-xs">{b.branch || "-"}</code> },
              {
                key: "commit",
                header: "Commit",
                cell: (b) =>
                  b.commit ? (
                    <code className="font-mono text-xs" title={b.commit}>
                      {shortCommit(b.commit)}
                    </code>
                  ) : (
                    <span className="text-muted-foreground">{isBuildActive(b) ? "pending" : "-"}</span>
                  ),
              },
              { key: "engine", header: "Engine", hideOnSmall: true, cell: (b) => <code className="font-mono text-xs">{b.engine_version || "-"}</code> },
              {
                key: "started",
                header: "Started",
                hideOnSmall: true,
                sortValue: (b) => b.created_at,
                cell: (b) => (
                  <span className="text-xs" title={formatDateTime(b.created_at)}>
                    {formatRelative(b.created_at)} by {buildCreator(b, jobs.data?.items, account.data)}
                  </span>
                ),
              },
              { key: "servers", header: "Servers", cell: (b) => <ServerLinks ids={b.server_ids} servers={servers.data?.items} /> },
            ]}
            actions={
              isAdmin
                ? (b) => (
                    <Button size="sm" variant="outline" aria-label={`Delete build #${b.id}`} disabled={isBuildActive(b)} title={isBuildActive(b) ? "Cancel the build's job first." : undefined} onClick={() => setDeleting(b)}>
                      <Trash2 />
                    </Button>
                  )
                : undefined
            }
          />
        )}
      </div>
      <DeleteBuildDialog build={deleting} onClose={() => setDeleting(null)} />
    </Section>
  );
}
