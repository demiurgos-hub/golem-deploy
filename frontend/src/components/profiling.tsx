import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Activity } from "lucide-react";
import { useApi } from "@/app/services";
import { describeError, isApiError } from "@/api/errors";
import { qk } from "@/api/query-keys";
import type { Job, ProfileKind, Server } from "@/api/types.gen";
import { ServerActions } from "@/components/server-actions";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { Section } from "@/components/ui/page";
import { ErrorText } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { saveBlob } from "@/lib/download";
import { PROFILE_SECONDS, validateProfileSeconds } from "@/lib/validators";

/** The hint next to a server's Profiling switch (new-server dialog and server page). */
export const PROFILING_SWITCH_HINT = "Takes effect at the next start or restart.";

export const PROFILE_KIND_LABELS: Record<ProfileKind, string> = {
  cpu: "CPU",
  trace: "Execution trace",
  heap: "Heap",
  allocs: "Allocations",
  goroutine: "Goroutines",
};

/** What the server page says about a server's Profiling switch and its running container. */
export type ServerProfilingState =
  /** The switch is off and the running container (if any) was started without it. */
  | { kind: "off" }
  /** The switch is off, but the running container was started with it: the daemon refuses captures at once; a restart drops it from the game. */
  | { kind: "leaving" }
  /** Docker could not be queried for the server. */
  | { kind: "server-unknown" }
  | { kind: "not-running" }
  /** The running container was started without profiling. */
  | { kind: "restart-needed" }
  | { kind: "ready" };

export function serverProfilingState(server: Pick<Server, "profiling" | "profiling_active" | "state">): ServerProfilingState {
  if (!server.profiling) return { kind: server.profiling_active && server.state === "running" ? "leaving" : "off" };
  if (server.state === "unknown") return { kind: "server-unknown" };
  if (server.state !== "running") return { kind: "not-running" };
  if (!server.profiling_active) return { kind: "restart-needed" };
  return { kind: "ready" };
}

/** The server page's Profiling section: the switch, its state and the capture form. */
export function ServerProfilingSection({ server, busy, onJob }: { server: Server; busy: boolean; onJob: (job: Job) => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const save = useMutation({
    mutationFn: (profiling: boolean) => api.servers.update(server.id, { profiling }),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      const running = s.state === "running";
      toast.success(
        s.profiling
          ? running && s.profiling_active
            ? "Profiling switch turned on."
            : "Profiling switch turned on. It takes effect at the next start or restart."
          : running && s.profiling_active
            ? "Profiling switch turned off. Captures are refused now; a restart also turns it off in the game server."
            : "Profiling switch turned off. It takes effect at the next start or restart.",
      );
    },
  });
  const checked = save.isPending ? (save.variables ?? server.profiling) : server.profiling;
  const st = serverProfilingState(server);
  return (
    <Section
      title="Profiling"
      description="Go profiles of the running game server (CPU, execution trace, heap, allocations, goroutines), captured through its admin endpoint and downloaded as a file."
    >
      <div className="flex flex-col gap-4">
        <div className="space-y-1">
          <Checkbox
            role="switch"
            label="Allow profiling of this server"
            checked={checked}
            disabled={save.isPending}
            aria-describedby={`profiling-hint-${server.id}`}
            onChange={(e) => save.mutate(e.target.checked)}
          />
          <p id={`profiling-hint-${server.id}`} className="pl-6 text-xs text-muted-foreground">
            {PROFILING_SWITCH_HINT}
          </p>
        </div>
        {save.isError && <ErrorText error={save.error} />}
        <ProfilingStatus server={server} state={st} busy={busy} onJob={onJob} />
        <CaptureForm server={server} ready={st.kind === "ready"} />
      </div>
    </Section>
  );
}

function ProfilingStatus({ server, state, busy, onJob }: { server: Server; state: ServerProfilingState; busy: boolean; onJob: (job: Job) => void }) {
  const row = (badge: ReactNode, text: ReactNode, action?: ReactNode) => (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm" role="status">
      {badge}
      <span className="min-w-0 flex-1 basis-64">{text}</span>
      {action && <div className="flex flex-wrap items-center gap-2">{action}</div>}
    </div>
  );
  const restart = <ServerActions server={server} busy={busy} onJob={onJob} only="restart" />;
  switch (state.kind) {
    case "off":
      return row(<Badge>Off</Badge>, "Profiling is off for this server. Turn the switch on and start or restart the server to capture profiles.");
    case "leaving":
      return row(
        <Badge tone="warning">Restart required</Badge>,
        "The switch is off, so captures are refused now. The running game server was started with profiling on and keeps it until its next restart.",
        restart,
      );
    case "server-unknown":
      return row(<Badge tone="warning">Unknown</Badge>, "Docker cannot be queried, so the state of this server is not known.");
    case "not-running":
      return row(<Badge>Not running</Badge>, "The server is not running. Captures need a running server.");
    case "restart-needed":
      return row(
        <Badge tone="warning">Restart required</Badge>,
        "The running game server was started without profiling. Restart the server to apply the switch.",
        restart,
      );
    case "ready":
      return row(<Badge tone="success">On</Badge>, "The running game server serves profiles.");
  }
}

const KINDS = Object.keys(PROFILE_KIND_LABELS) as ProfileKind[];

function CaptureForm({ server, ready }: { server: Server; ready: boolean }) {
  const api = useApi();
  const toast = useToast();
  const [kind, setKind] = useState<ProfileKind>("cpu");
  const [seconds, setSeconds] = useState(String(PROFILE_SECONDS.cpu?.default ?? ""));
  const [secondsError, setSecondsError] = useState<string | null>(null);
  // The end of the running timed capture (ms) and the time the countdown last ticked.
  const [clock, setClock] = useState<{ deadline: number; now: number } | null>(null);
  const capture = useMutation({
    mutationFn: async (req: { kind: ProfileKind; seconds?: number }) => {
      const { blob, filename } = await api.servers.profile(server.id, req.kind, req.seconds);
      const name = filename ?? `${server.name}-${req.kind}${req.kind === "trace" ? ".trace" : ".pb.gz"}`;
      saveBlob(blob, name);
      return name;
    },
    onSuccess: (name) => toast.success(`Profile saved as ${name}.`),
    onSettled: () => setClock(null),
  });
  const deadline = clock?.deadline ?? null;
  useEffect(() => {
    if (deadline === null) return;
    const t = setInterval(() => setClock((c) => c && { ...c, now: Date.now() }), 500);
    return () => clearInterval(t);
  }, [deadline]);

  const limits = PROFILE_SECONDS[kind];
  const err = capture.error;
  const fieldError = (field: string) => (isApiError(err) && err.field === field ? describeError(err) : null);
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (!ready || capture.isPending) return;
    const problem = validateProfileSeconds(kind, seconds);
    setSecondsError(problem);
    if (problem) return;
    const n = limits ? Number(seconds.trim()) : undefined;
    const now = Date.now();
    setClock(n !== undefined ? { deadline: now + n * 1000, now } : null);
    capture.mutate({ kind, seconds: n });
  };
  const left = clock ? Math.max(0, Math.ceil((clock.deadline - clock.now) / 1000)) : 0;
  const progress = !capture.isPending
    ? null
    : clock === null
      ? "Capturing…"
      : left > 0
        ? `Capturing… ${left} s left`
        : "Capturing… waiting for the file";

  return (
    <form className="flex flex-col gap-3" onSubmit={onSubmit} aria-label="Capture a profile" noValidate>
      <fieldset disabled={!ready} className="flex flex-wrap items-end gap-3">
        <Field label="Profile" className="w-48" error={fieldError("kind")}>
          {(p) => (
            <Select
              {...p}
              value={kind}
              disabled={capture.isPending}
              onChange={(e) => {
                const k = e.target.value as ProfileKind;
                setKind(k);
                setSeconds(String(PROFILE_SECONDS[k]?.default ?? ""));
                setSecondsError(null);
                capture.reset();
              }}
            >
              {KINDS.map((k) => (
                <option key={k} value={k}>
                  {PROFILE_KIND_LABELS[k]}
                </option>
              ))}
            </Select>
          )}
        </Field>
        {limits && (
          <Field label={`Seconds (1 to ${limits.max})`} className="w-36" error={secondsError ?? fieldError("seconds")}>
            {(p) => (
              <Input
                {...p}
                inputMode="numeric"
                value={seconds}
                disabled={capture.isPending}
                onChange={(e) => {
                  setSeconds(e.target.value);
                  setSecondsError(null);
                  capture.reset();
                }}
              />
            )}
          </Field>
        )}
        <Button type="submit" loading={capture.isPending}>
          {!capture.isPending && <Activity />}
          Capture
        </Button>
      </fieldset>
      {progress && (
        <p className="text-sm text-muted-foreground" role="status">
          {progress}
        </p>
      )}
      {capture.isError && !fieldError("kind") && !fieldError("seconds") && <ErrorText error={capture.error} />}
      <p className="text-xs text-muted-foreground">
        Open a CPU, heap, allocation or goroutine profile with <code className="font-mono">go tool pprof -http=: &lt;file&gt;</code> and an
        execution trace with <code className="font-mono">go tool trace &lt;file&gt;</code>.
      </p>
    </form>
  );
}
