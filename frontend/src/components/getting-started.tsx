import type { ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { CheckCircle2, Circle } from "lucide-react";
import { useAccount, useProjects, useServers, useSuccessfulBuilds } from "@/api/queries";
import { cn } from "@/lib/utils";

function Step({ n, done, title, children }: { n: number; done: boolean; title: string; children: ReactNode }) {
  return (
    <li className="flex gap-3">
      {done ? (
        <CheckCircle2 aria-label="Done" className="mt-0.5 size-5 shrink-0 text-emerald-600 dark:text-emerald-400" />
      ) : (
        <Circle aria-label="To do" className="mt-0.5 size-5 shrink-0 text-muted-foreground" />
      )}
      <div className={cn("min-w-0 text-sm", done && "text-muted-foreground")}>
        <p className="font-medium text-foreground">
          {n}. {title}
        </p>
        <div className="mt-0.5">{children}</div>
      </div>
    </li>
  );
}

/**
 * The order things happen in, shown while no server exists: a project
 * (a Git repository), a successful build of it (an image), then a server
 * running that build. Each step says who can do it and links to where.
 */
export function GettingStarted() {
  const account = useAccount();
  const projects = useProjects();
  const servers = useServers();
  const builds = useSuccessfulBuilds();
  if (!projects.data || !servers.data || servers.data.items.length > 0) return null;
  const isAdmin = account.data?.role === "admin";
  const hasProject = projects.data.items.length > 0;
  const firstBuild = builds.data?.items[0];
  const firstProject = projects.data.items[0];
  return (
    <section aria-label="Getting started" className="mb-6 rounded-lg border border-sky-600/30 bg-sky-600/5 px-4 py-4">
      <h2 className="text-sm font-semibold">Getting started</h2>
      <p className="mt-0.5 text-xs text-muted-foreground">A game goes from a Git repository to a running server in three steps.</p>
      <ol className="mt-3 flex flex-col gap-3">
        <Step n={1} done={hasProject} title="Add a project">
          {hasProject ? (
            "Done."
          ) : isAdmin ? (
            <>A project is the game&apos;s Git repository. Use New project on the Projects page; the daemon clones it.</>
          ) : (
            <>A project is the game&apos;s Git repository. An administrator has to add it.</>
          )}
        </Step>
        <Step n={2} done={firstBuild !== undefined} title="Build it">
          {firstBuild ? (
            "Done: a successful build exists."
          ) : hasProject ? (
            <>
              Open the project (for example{" "}
              <Link to="/projects/$id" params={{ id: String(firstProject.id) }} className="font-medium underline underline-offset-4">
                {firstProject.name}
              </Link>
              ) and use Start build. The build log shows what happens; a build needs <code className="font-mono text-xs">golem.yaml</code> and a{" "}
              <code className="font-mono text-xs">go.mod</code> requiring Golem Engine v0.4.0-rc.1 or newer.
            </>
          ) : (
            "Builds turn a project's checkout into an image that servers run."
          )}
        </Step>
        <Step n={3} done={false} title="Create a server">
          {firstBuild ? (
            <>
              <Link to="/servers" search={{ create: firstBuild.project_id }} className="font-medium underline underline-offset-4">
                Create a server
              </Link>{" "}
              from a successful build, start it and open its game URL.
            </>
          ) : (
            "A server runs a successful build on a port of this host. Players connect to its game URL."
          )}
        </Step>
      </ol>
    </section>
  );
}
