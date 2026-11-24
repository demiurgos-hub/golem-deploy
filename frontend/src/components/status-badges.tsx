import { AlertTriangle } from "lucide-react";
import type { Project, Server } from "@/api/types.gen";
import { Badge, type BadgeTone } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";

const PROJECT_STATUS: Record<string, { label: string; tone: BadgeTone; busy?: boolean }> = {
  idle: { label: "Ready", tone: "success" },
  cloning: { label: "Cloning", tone: "info", busy: true },
  pulling: { label: "Pulling", tone: "info", busy: true },
  switching_branch: { label: "Switching branch", tone: "info", busy: true },
  error: { label: "Error", tone: "danger" },
};

export function ProjectStatusBadge({ status }: { status: Project["status"] }) {
  const s = PROJECT_STATUS[status] ?? { label: status, tone: "neutral" as const };
  return (
    <Badge tone={s.tone}>
      {s.busy && <Spinner className="size-3" />}
      {s.label}
    </Badge>
  );
}

const SERVER_STATE: Record<string, { label: string; tone: BadgeTone }> = {
  running: { label: "Running", tone: "success" },
  restarting: { label: "Restarting", tone: "info" },
  created: { label: "Created", tone: "neutral" },
  exited: { label: "Exited", tone: "neutral" },
  paused: { label: "Paused", tone: "warning" },
  dead: { label: "Dead", tone: "danger" },
  missing: { label: "No container", tone: "neutral" },
  unknown: { label: "Unknown", tone: "warning" },
};

/** States in which the server's container runs (the daemon refuses port changes then). */
export function isContainerActive(state: Server["state"]): boolean {
  return state === "running" || state === "restarting" || state === "paused";
}

/**
 * Why the port cannot be changed now, or null: a running (or restarting or
 * paused) container keeps its port (409 conflict), and with Docker
 * unreachable the daemon cannot tell (it answers 503).
 */
export function portLocked(s: Pick<Server, "state">): "running" | "unknown" | null {
  if (isContainerActive(s.state)) return "running";
  if (s.state === "unknown") return "unknown";
  return null;
}

/**
 * Why start/stop/restart/delete are unavailable, or null. "unknown" only
 * occurs for a server with a container while Docker is unreachable: every
 * such action would fail. ("missing" means no container: start works,
 * also while Docker is down it fails as a job with a clear detail.)
 */
export function serverActionsBlocked(s: Pick<Server, "state">): string | null {
  return s.state === "unknown" ? "Docker is unreachable, so the server cannot be started, stopped, restarted or deleted right now." : null;
}

/** True when the observed state disagrees with what the operator asked for. */
export function stateMismatch(s: Pick<Server, "desired_state" | "state">): boolean {
  if (s.state === "unknown") return false;
  if (s.desired_state === "running") return s.state !== "running" && s.state !== "restarting";
  return s.state === "running" || s.state === "restarting";
}

/** Actual state, with the desired state called out when they differ. */
export function ServerStateBadge({ server }: { server: Pick<Server, "desired_state" | "state" | "exit_code"> }) {
  const s = SERVER_STATE[server.state] ?? { label: server.state, tone: "neutral" as const };
  const mismatch = stateMismatch(server);
  const tone: BadgeTone = mismatch && server.desired_state === "running" ? "danger" : s.tone;
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      <Badge
        tone={tone}
        title={
          server.state === "unknown"
            ? "The server has a container, but Docker could not be queried, so its actual state is not known."
            : server.state === "missing"
              ? "The server has no container: it was never started, or its container was removed. Start it to create one."
              : undefined
        }
      >
        {server.state === "unknown" && <AlertTriangle aria-hidden />}
        {s.label}
        {server.state === "exited" && server.exit_code !== null && ` (${server.exit_code})`}
      </Badge>
      {mismatch && (
        <span className="inline-flex items-center gap-1 text-xs text-amber-700 dark:text-amber-400">
          <AlertTriangle aria-hidden className="size-3" />
          should be {server.desired_state}
        </span>
      )}
    </span>
  );
}
