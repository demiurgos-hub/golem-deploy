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
        title={server.state === "unknown" ? "Docker could not be queried, so the actual state is not known." : undefined}
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
