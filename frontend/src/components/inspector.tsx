import { useEffect, useState, type ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ExternalLink, ScanSearch } from "lucide-react";
import { useApi } from "@/app/services";
import { ApiError } from "@/api/errors";
import { isJobActive, useAccount, useInspector, useServers, useSubjectJobs } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Inspector, Job, JobList, Server, UpdateInspectorRequest } from "@/api/types.gen";
import { InlineJob, useShownJob } from "@/components/jobs";
import { ServerActions } from "@/components/server-actions";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox } from "@/components/ui/input";
import { DetailList, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative, shortImageId } from "@/lib/format";
import { inspectorAddress, inspectorEntryUrl, serverInspectorState } from "@/lib/inspector";

/** The hint next to a server's Inspector switch (new-server dialog and server page). */
export const INSPECTOR_SWITCH_HINT =
  "Takes effect at the next start or restart. It needs the Inspector of this installation, which an administrator turns on in Settings.";

/** The longest a link is offered after the browser blocked the new tab: it works once, for 60 seconds. */
const LINK_LIFETIME_MS = 60_000;

/**
 * Opens the Inspector in a new tab with a one-time link from POST
 * /inspector/sessions. Browsers block tabs opened after an await, so the
 * tab is opened during the click and navigated when the link arrives; it
 * cannot reach this page through window.opener. A failed request closes it
 * again. When no tab could be opened, the link is offered in a dialog for
 * the minute it works. The link lives only in this component's state.
 */
export function OpenInspectorButton({
  serverId,
  label = "Open Inspector",
  size = "sm",
  variant = "outline",
}: {
  serverId?: number;
  label?: string;
  size?: "sm" | "md";
  variant?: "outline" | "primary";
}) {
  const api = useApi();
  const toast = useToast();
  const [pending, setPending] = useState(false);
  const [fallback, setFallback] = useState<{ url: string; ms: number } | null>(null);
  const [expired, setExpired] = useState(false);

  useEffect(() => {
    if (!fallback) return;
    const t = setTimeout(() => setExpired(true), fallback.ms);
    return () => clearTimeout(t);
  }, [fallback]);

  function open() {
    if (pending) return;
    let tab: Window | null = null;
    try {
      tab = window.open("about:blank", "_blank");
    } catch {
      tab = null;
    }
    if (tab) {
      try {
        tab.opener = null;
        tab.document.title = "Opening the Inspector";
        tab.document.body.textContent = "Opening the Inspector...";
      } catch {
        // The blank tab is ours; if the browser refuses these, it is navigated all the same.
      }
    }
    setPending(true);
    api.inspector
      .createSession(serverId === undefined ? {} : { server_id: serverId })
      .then((session) => {
        const url = inspectorEntryUrl(session);
        if (url === null) {
          throw new ApiError({ status: 200, code: "invalid_response", message: "The daemon sent an Inspector link this page cannot use." });
        }
        if (tab && !tab.closed) {
          tab.location.replace(url);
          return;
        }
        const left = Date.parse(session.expires_at) - Date.now();
        setExpired(false);
        setFallback({ url, ms: left > 0 && left <= LINK_LIFETIME_MS ? left : LINK_LIFETIME_MS });
      })
      .catch((err: unknown) => {
        tab?.close();
        toast.error(err, "Could not open the Inspector");
      })
      .finally(() => setPending(false));
  }

  return (
    <>
      <Button size={size} variant={variant} loading={pending} onClick={open}>
        {!pending && <ScanSearch />}
        {label}
        <span className="sr-only"> (opens in a new tab)</span>
      </Button>
      <Dialog
        open={fallback !== null}
        onOpenChange={(o) => !o && setFallback(null)}
        title="Open the Inspector"
        description="Your browser did not let this page open a new tab."
      >
        {expired ? (
          <p className="text-sm">The link has expired. Close this and open the Inspector again.</p>
        ) : (
          <div className="flex flex-col items-start gap-3 text-sm">
            <p>Use this link instead. It works once, within a minute.</p>
            <a
              href={fallback?.url}
              target="_blank"
              rel="noopener noreferrer"
              onClick={() => setFallback(null)}
              className="inline-flex h-9 items-center gap-2 rounded-md bg-primary px-3.5 text-sm font-medium text-primary-foreground hover:bg-primary/90"
            >
              Open the Inspector
              <ExternalLink aria-hidden className="size-4" />
              <span className="sr-only">(opens in a new tab)</span>
            </a>
          </div>
        )}
      </Dialog>
    </>
  );
}

/** "Open Inspector" for every signed-in account, shown only while the Inspector runs. */
export function OpenInspectorIfRunning() {
  const inspector = useInspector();
  if (inspector.data?.state !== "running") return null;
  return <OpenInspectorButton size="md" />;
}

const STATE_BADGES: Record<Inspector["state"], ReactNode> = {
  off: <Badge>Off</Badge>,
  running: <Badge tone="success">Running</Badge>,
  stopped: <Badge tone="danger">Not running</Badge>,
  unknown: <Badge tone="warning">Unknown</Badge>,
};

const BUSY_HINT = "Wait for the running Inspector job to finish.";
const JOB_LIMIT = 5;

/** Admin Settings: turn the installation's Inspector on and off, and see what it does. */
export function InspectorSettingsSection() {
  const inspector = useInspector();
  return (
    <Section
      id="inspector"
      title="Inspector"
      description="The Inspector is a web page that shows the live entities of running game servers. When it is on, the daemon runs it on this host on its own TCP port. Each server has an Inspector switch: servers with the switch on connect to it and appear there under their name."
    >
      {inspector.isPending ? (
        <LoadingState label="Loading the Inspector" />
      ) : inspector.isError && !inspector.data ? (
        <ErrorState error={inspector.error} title="Could not load the Inspector" onRetry={() => void inspector.refetch()} />
      ) : !inspector.data.available ? (
        <Notice tone="info" title="Not available on this host">
          The daemon was started with the Inspector listener turned off (<code className="font-mono text-xs">--inspector-listen off</code>), so the
          Inspector cannot be turned on. To use it, start the daemon with an address for the Inspector listener.
        </Notice>
      ) : (
        <InspectorControls inspector={inspector.data} />
      )}
    </Section>
  );
}

function InspectorControls({ inspector }: { inspector: Inspector }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const jobs = useSubjectJobs("inspector", "hub", JOB_LIMIT);
  const activeJob = jobs.data?.items.find(isJobActive) ?? null;
  const shown = useShownJob(activeJob);
  const [confirm, setConfirm] = useState<"off" | "rotate" | null>(null);
  const apply = useMutation({
    mutationFn: (body: UpdateInspectorRequest) => api.inspector.update(body),
    onSuccess: (job) => {
      queryClient.setQueryData(qk.jobs.detail(job.id), job);
      // Known as active at once: the buttons stay disabled and the Inspector is polled fast until the list says otherwise.
      queryClient.setQueryData<JobList>([...qk.jobs.forSubject("inspector", "hub"), JOB_LIMIT], (old) => ({
        items: [job, ...(old?.items ?? []).filter((j) => j.id !== job.id)].slice(0, JOB_LIMIT),
      }));
      void queryClient.invalidateQueries({ queryKey: qk.inspector });
      shown.show(job);
    },
  });
  const busy = activeJob !== null;
  const disabled = busy || apply.isPending;
  const title = busy ? BUSY_HINT : undefined;
  const on = inspector.enabled;
  const pending = (body: UpdateInspectorRequest) => apply.isPending && JSON.stringify(apply.variables) === JSON.stringify(body);

  return (
    <div className="flex flex-col gap-4">
      <InspectorStateLine inspector={inspector} />
      {on && <InspectorDetails inspector={inspector} />}
      <div className="flex flex-wrap items-center gap-2">
        {inspector.state === "running" && <OpenInspectorButton />}
        {on ? (
          <>
            {inspector.state === "stopped" && (
              <Button size="sm" variant="outline" disabled={disabled} title={title} loading={pending({})} onClick={() => apply.mutate({})}>
                Try again
              </Button>
            )}
            <Button size="sm" variant="outline" disabled={disabled} title={title} onClick={() => setConfirm("off")}>
              Turn off
            </Button>
          </>
        ) : (
          <Button size="sm" disabled={disabled} title={title} loading={apply.isPending} onClick={() => apply.mutate({ enabled: true })}>
            Turn on
          </Button>
        )}
      </div>
      {!on && (
        <p className="text-xs text-muted-foreground">
          Turning it on downloads the Inspector&apos;s image if this host does not have it yet, starts it and opens TCP port {inspector.port}.
          Servers with the Inspector switch on connect after their next start or restart.
        </p>
      )}
      {on && (
        <details className="rounded-md border border-border px-3 py-2 text-sm">
          <summary className="cursor-pointer text-muted-foreground">More options</summary>
          <div className="mt-3 flex flex-col gap-3">
            <div className="flex flex-col items-start gap-1">
              <Button
                size="sm"
                variant="outline"
                disabled={disabled}
                title={title}
                loading={pending({ pull_image: true })}
                onClick={() => apply.mutate({ pull_image: true })}
              >
                Download the image again
              </Button>
              <p className="text-xs text-muted-foreground">
                Downloads the Inspector&apos;s image even though this host has it, for when its tag points to a newer version, and starts the
                Inspector again with it.
              </p>
            </div>
            <div className="flex flex-col items-start gap-1">
              <Button size="sm" variant="outline" disabled={disabled} title={title} onClick={() => setConfirm("rotate")}>
                Replace secrets
              </Button>
              <p className="text-xs text-muted-foreground">
                Creates new secrets for the Inspector and for every server, for example after a secret may have leaked.
              </p>
            </div>
          </div>
        </details>
      )}
      {apply.isError && <ErrorText error={apply.error} />}
      {shown.job && <InlineJob key={shown.job.id} job={shown.job} onDismiss={shown.dismiss} />}
      <ConfirmDialog
        open={confirm === "off"}
        onOpenChange={(o) => !o && setConfirm(null)}
        title="Turn off the Inspector"
        description={
          <>
            The daemon stops the Inspector and closes TCP port {inspector.port}. Inspector tabs that are open stop working. Game servers keep running;
            the ones that were started with the Inspector show &quot;Restart required&quot;.
          </>
        }
        confirmLabel="Turn off"
        destructive
        onConfirm={() => apply.mutateAsync({ enabled: false })}
      />
      <ConfirmDialog
        open={confirm === "rotate"}
        onOpenChange={(o) => !o && setConfirm(null)}
        title="Replace the Inspector's secrets"
        description={
          <>
            Open Inspector sessions end: everyone has to open the Inspector again from this panel. Every server with the Inspector switch on
            needs a restart to connect again, and restarting a server disconnects its players.
          </>
        }
        confirmLabel="Replace secrets"
        destructive
        onConfirm={() => apply.mutateAsync({ rotate_secrets: true })}
      />
    </div>
  );
}

function InspectorStateLine({ inspector }: { inspector: Inspector }) {
  let text: ReactNode;
  switch (inspector.state) {
    case "off":
      text = inspector.enabled ? "The Inspector is turned on but not started yet." : "The Inspector is off. No game server is connected to it.";
      break;
    case "running":
      text = inspector.enabled ? "The Inspector is running." : "The Inspector is still running; it is being turned off.";
      break;
    case "stopped":
      text = (
        <>
          The Inspector is turned on but is not running
          {inspector.detail ? (
            <>
              : <span className="break-words">{inspector.detail}</span>
            </>
          ) : (
            ". The log of its last job may say why."
          )}
        </>
      );
      break;
    default:
      text = `Docker cannot be queried, so the state of the Inspector is not known. It is turned ${inspector.enabled ? "on" : "off"}. This page checks again every few seconds.`;
  }
  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      {STATE_BADGES[inspector.state] ?? <Badge>{String(inspector.state)}</Badge>}
      <span className="min-w-0">{text}</span>
    </div>
  );
}

function InspectorDetails({ inspector }: { inspector: Inspector }) {
  const servers = useServers();
  const address = inspectorAddress(inspector.public_url, inspector.port);
  const switchedOn = servers.data?.items.filter((s) => s.inspector) ?? null;
  const connected = inspector.targets?.filter((t) => t.connected) ?? null;
  const connectedOn = connected && switchedOn ? connected.filter((t) => switchedOn.some((s) => s.id === t.server_id)) : null;

  let serversText: ReactNode;
  if (inspector.state !== "running") serversText = <span className="text-muted-foreground">None while the Inspector is not running.</span>;
  else if (connected === null) serversText = <span className="text-muted-foreground">Unknown: the Inspector did not answer.</span>;
  else if (switchedOn && connectedOn) {
    serversText =
      switchedOn.length === 0
        ? "No server has the Inspector switch on."
        : `${connectedOn.length} of ${switchedOn.length} ${switchedOn.length === 1 ? "server" : "servers"} with the Inspector switch on ${connectedOn.length === 1 ? "is" : "are"} connected.`;
  } else serversText = `${connected.length} ${connected.length === 1 ? "server is" : "servers are"} connected.`;

  return (
    <div className="flex flex-col gap-3">
      <DetailList
        items={[
          {
            term: "Image",
            value: (
              <span className="flex flex-col gap-0.5">
                <code className="break-all font-mono text-xs">{inspector.image || "-"}</code>
                {inspector.image_id && (
                  <span className="text-xs text-muted-foreground">
                    Image id{" "}
                    <code className="font-mono" title={inspector.image_id}>
                      {shortImageId(inspector.image_id)}
                    </code>
                  </span>
                )}
              </span>
            ),
          },
          {
            term: "Started",
            value: inspector.started_at ? <span title={formatDateTime(inspector.started_at)}>{formatRelative(inspector.started_at)}</span> : "-",
          },
          {
            term: "Port",
            value: (
              <span className="flex flex-col gap-0.5">
                <span className="tabular-nums">TCP {inspector.port}</span>
                <span className="text-xs text-muted-foreground">
                  A host firewall has to allow this TCP port. Browsers open the Inspector on it, and game servers connect to it too.
                </span>
              </span>
            ),
          },
          ...(address ? [{ term: "Address", value: <code className="break-all font-mono text-xs">{address}</code> }] : []),
          {
            term: "Game servers connect to",
            value: inspector.agent_address ? (
              <code className="break-all font-mono text-xs">{inspector.agent_address}</code>
            ) : (
              <span className="text-muted-foreground">{inspector.agents_reachable ? "Not known yet." : "Nothing: see the warning below."}</span>
            ),
          },
          { term: "Connected servers", value: serversText },
        ]}
      />
      {inspector.other_targets > 0 && (
        <p className="text-xs text-muted-foreground">
          The Inspector also knows {inspector.other_targets} game {inspector.other_targets === 1 ? "server that is" : "servers that are"} not a
          server of this installation.
        </p>
      )}
      {!inspector.agents_reachable && (
        <Notice tone="warning" title="Game servers cannot reach the Inspector">
          The Inspector listens on a loopback address, which game servers cannot connect to from their containers. The Inspector opens in the
          browser, but no server appears in it. Start the daemon with an Inspector listener address that Docker&apos;s networks can reach.
        </Notice>
      )}
    </div>
  );
}

/** The server page's Inspector section: the server's switch and whether it is connected. */
export function ServerInspectorSection({ server, busy, onJob }: { server: Server; busy: boolean; onJob: (job: Job) => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const save = useMutation({
    mutationFn: (inspector: boolean) => api.servers.update(server.id, { inspector }),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.servers.detail(s.id), s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.list });
      toast.success(
        s.inspector
          ? "Inspector switch turned on. It takes effect at the next start or restart."
          : "Inspector switch turned off. It takes effect at the next start or restart.",
      );
    },
  });
  const checked = save.isPending ? (save.variables ?? server.inspector) : server.inspector;
  return (
    <Section
      title="Inspector"
      description="The Inspector is a web page that shows the live entities of a running game. With the switch on, this server connects to it."
    >
      <div className="flex flex-col gap-3">
        <div className="space-y-1">
          <Checkbox
            role="switch"
            label="Connect this server to the Inspector"
            checked={checked}
            disabled={save.isPending}
            aria-describedby={`inspector-hint-${server.id}`}
            onChange={(e) => save.mutate(e.target.checked)}
          />
          <p id={`inspector-hint-${server.id}`} className="pl-6 text-xs text-muted-foreground">
            {INSPECTOR_SWITCH_HINT}
          </p>
        </div>
        {save.isError && <ErrorText error={save.error} />}
        {server.inspector && <ServerInspectorStatus server={server} busy={busy} onJob={onJob} />}
      </div>
    </Section>
  );
}

function SettingsLink() {
  const account = useAccount();
  if (account.data?.role !== "admin") return <>An administrator turns it on in Settings.</>;
  return (
    <>
      Turn it on in{" "}
      <Link to="/admin/settings" hash="inspector" className="underline underline-offset-2">
        Settings
      </Link>
      .
    </>
  );
}

function ServerInspectorStatus({ server, busy, onJob }: { server: Server; busy: boolean; onJob: (job: Job) => void }) {
  const inspector = useInspector();
  if (inspector.isPending) return <LoadingState label="Checking the Inspector" />;
  if (!inspector.data) return <ErrorText error={inspector.error} />;
  const st = serverInspectorState(server, inspector.data);
  const row = (badge: ReactNode, text: ReactNode, action?: ReactNode) => (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm" role="status">
      {badge}
      <span className="min-w-0 flex-1 basis-64">{text}</span>
      {action && <div className="flex flex-wrap items-center gap-2">{action}</div>}
    </div>
  );
  switch (st.kind) {
    case "off":
      return null;
    case "installation-off":
      return row(
        <Badge tone="warning">Not connected</Badge>,
        inspector.data.available ? (
          <>
            The Inspector of this installation is off, so this server cannot connect to it. <SettingsLink />
          </>
        ) : (
          "The daemon was started without the Inspector, so this server cannot connect to it."
        ),
      );
    case "installation-down":
      return row(
        <Badge tone="warning">Not connected</Badge>,
        st.state === "unknown" ? (
          "Docker cannot be queried, so it is not known whether the Inspector of this installation runs."
        ) : (
          <>
            The Inspector of this installation is turned on but is not running
            {st.detail ? <>: <span className="break-words">{st.detail}</span></> : "."}
          </>
        ),
      );
    case "not-running":
      return row(<Badge>Not connected</Badge>, "The server is not running. It connects to the Inspector when it starts.");
    case "restart-needed":
      return row(
        <Badge tone="warning">Restart required</Badge>,
        st.reason === "without"
          ? "The running game server was started without the Inspector. It connects at the next restart."
          : "The Inspector settings changed since the game server started. It connects at the next restart.",
        <ServerActions server={server} busy={busy} onJob={onJob} only="restart" />,
      );
    case "unknown":
      return row(<Badge tone="warning">Unknown</Badge>, "The Inspector did not answer, so it is not known whether this server is connected.");
    case "connected":
      return row(
        <Badge tone="success">Connected</Badge>,
        st.target.connected_at ? (
          <span title={formatDateTime(st.target.connected_at)}>Since {formatRelative(st.target.connected_at)}.</span>
        ) : null,
        <OpenInspectorButton serverId={server.id} label="Open in Inspector" />,
      );
    case "not-connected":
      return (
        <div className="flex flex-col gap-1">
          {row(<Badge tone="warning">Not connected yet</Badge>, "The game server has not connected to the Inspector yet. Give it a few seconds after a start.")}
          <p className="text-xs text-muted-foreground">
            If it does not connect: a host firewall has to allow TCP port {inspector.data.port} from Docker&apos;s networks, and the game needs a
            Golem Engine version with Inspector support.
          </p>
        </div>
      );
  }
}
