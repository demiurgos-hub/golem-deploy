import type { ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Play, RotateCw, Square } from "lucide-react";
import { useApi } from "@/app/services";
import { qk } from "@/api/query-keys";
import type { Job, Server } from "@/api/types.gen";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";

type Action = "start" | "stop" | "restart";

const LABELS: Record<Action, string> = { start: "Start", stop: "Stop", restart: "Restart" };

/**
 * Start/stop/restart for one server. Each instance owns its mutation, so a
 * pending action disables only this server's buttons. `onJob` receives the
 * returned job so the caller can show it inline.
 */
export function ServerActions({
  server,
  onJob,
  size = "sm",
  busy = false,
}: {
  server: Server;
  onJob: (job: Job) => void;
  size?: "sm" | "md";
  /** A job is already active for this server. */
  busy?: boolean;
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

  const running = server.state === "running" || server.state === "restarting";
  const pendingAction = run.isPending ? run.variables : null;
  const disabled = run.isPending || busy;
  const button = (action: Action, icon: ReactNode, variant: "outline" | "primary" = "outline") => (
    <Button
      size={size}
      variant={variant}
      loading={pendingAction === action}
      disabled={disabled}
      onClick={() => run.mutate(action)}
      aria-label={size === "sm" ? `${LABELS[action]} ${server.name}` : undefined}
    >
      {pendingAction !== action && icon}
      {LABELS[action]}
    </Button>
  );

  return (
    <>
      {running || server.desired_state === "running"
        ? button("stop", <Square />)
        : button("start", <Play />, size === "md" ? "primary" : "outline")}
      {button("restart", <RotateCw />)}
    </>
  );
}
