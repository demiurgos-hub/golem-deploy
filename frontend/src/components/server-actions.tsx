import type { ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Play, RotateCw, Square } from "lucide-react";
import { useApi } from "@/app/services";
import { qk } from "@/api/query-keys";
import type { Job, Server } from "@/api/types.gen";
import { isContainerActive, serverActionsBlocked } from "@/components/status-badges";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";

type Action = "start" | "stop" | "restart";

const LABELS: Record<Action, string> = { start: "Start", stop: "Stop", restart: "Restart" };

/**
 * Start/stop/restart for one server. Each instance owns its mutation, so a
 * pending action disables only this server's buttons. `onJob` receives the
 * returned job so the caller can show it inline.
 *
 * Per state (docs/API.md, Servers): a running, restarting or paused
 * container can be stopped or restarted; otherwise the server can be
 * started (and stopped while it should be running, to accept the stop);
 * restart needs a container, so it is not offered for "missing" (no
 * container). "unknown" (a container, Docker unreachable) disables
 * everything until Docker answers again.
 */
export function ServerActions({
  server,
  onJob,
  size = "sm",
  busy = false,
  only,
}: {
  server: Server;
  onJob: (job: Job) => void;
  size?: "sm" | "md";
  /** A job is already active for this server. */
  busy?: boolean;
  /** Only the restart button (where a section says that a change needs a restart). */
  only?: "restart";
}) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const run = useMutation({
    mutationFn: (action: Action) => api.servers[action](server.id),
    onSuccess: (job) => {
      queryClient.setQueryData(qk.jobs.detail(job.id), job);
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      void queryClient.invalidateQueries({ queryKey: qk.jobs.forSubject("server", server.id) });
      onJob(job);
    },
    onError: (err, action) => toast.error(err, `Could not ${action} ${server.name}`),
  });

  const blocked = serverActionsBlocked(server);
  const active = isContainerActive(server.state);
  const pendingAction = run.isPending ? run.variables : null;
  const disabled = run.isPending || busy || blocked !== null;
  const button = (action: Action, icon: ReactNode, variant: "outline" | "primary" = "outline") => (
    <Button
      size={size}
      variant={variant}
      loading={pendingAction === action}
      disabled={disabled}
      title={blocked ?? undefined}
      onClick={() => run.mutate(action)}
      aria-label={size === "sm" ? `${LABELS[action]} ${server.name}` : undefined}
    >
      {pendingAction !== action && icon}
      {LABELS[action]}
    </Button>
  );

  if (only === "restart") return server.state !== "missing" ? button("restart", <RotateCw />) : null;
  return (
    <>
      {active || server.desired_state === "running"
        ? button("stop", <Square />)
        : button("start", <Play />, size === "md" ? "primary" : "outline")}
      {server.state !== "missing" && button("restart", <RotateCw />)}
      {server.state === "missing" && server.desired_state === "running" && button("start", <Play />)}
    </>
  );
}
