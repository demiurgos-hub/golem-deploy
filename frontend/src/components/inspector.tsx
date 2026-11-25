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

/** How long a link works after the daemon issued it (once). */
const LINK_LIFETIME_MS = 60_000;

/**
 * Cuts the blank tab off from this page (window.opener). False when the
 * browser does not let it: such a tab is never given the link.
 */
function cutOff(tab: Window): boolean {
  try {
    tab.opener = null;
    if (tab.opener !== null) return false;
  } catch {
    return false;
  }
  try {
    tab.document.title = "Opening the Inspector";
    tab.document.body.textContent = "Opening the Inspector...";
  } catch {
    // Only the text of the waiting tab; it is navigated all the same.
  }
  return true;
}

/** Why the dialog is shown: no usable tab, or the link expired before it could be used in the tab. */
type OfferReason = "no-tab" | "late";

const OFFER_DESCRIPTIONS: Record<OfferReason, string> = {
  "no-tab": "The Inspector could not be opened in a new tab: the browser blocked the tab, or the tab was closed.",
  late: "The answer came after the link had expired, so the new tab was closed.",
};

/**
 * Opens the Inspector in a new tab with a one-time link from POST
 * /inspector/sessions. Browsers block tabs opened after an await, so the
 * tab is opened during the click and navigated when the link arrives, if
 * it has not expired by then; it cannot reach this page through
 * window.opener. A failed request or an expired link closes it again. When
 * there is no usable tab (blocked, closed by the user, or its opener could
 * not be cleared), a dialog offers a button that opens the link in a new
 * tab until it expires (checked again on every click). The link is never
 * put in the page; it lives only in this component's state and is dropped
 * when it expires.
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
  // The dialog: the link (null once it expired) and when it stops working (ms since the epoch, this browser's clock).
  const [offer, setOffer] = useState<{ url: string | null; deadline: number; reason: OfferReason } | null>(null);
  const expired = offer !== null && offer.url === null;

  useEffect(() => {
    if (!offer || offer.url === null) return;
    const t = setTimeout(() => setOffer((o) => o && { ...o, url: null }), Math.max(0, offer.deadline - Date.now()));
    return () => clearTimeout(t);
  }, [offer]);

  function openOffered() {
    // A timer can fire late in a background tab: check again when the button is used.
    if (!offer || offer.url === null || Date.now() >= offer.deadline) {
      setOffer((o) => o && { ...o, url: null });
      return;
    }
    window.open(offer.url, "_blank", "noopener,noreferrer");
    setOffer(null);
  }

  function open() {
    if (pending) return;
    const asked = Date.now();
    let tab: Window | null = null;
    try {
      tab = window.open("about:blank", "_blank");
    } catch {
      tab = null;
    }
    if (tab && !cutOff(tab)) {
      try {
        tab.close();
      } catch {
        // Nothing more to do: the link goes to the dialog, never to that tab.
      }
      tab = null;
    }
    setPending(true);
    api.inspector
      .createSession(serverId === undefined ? {} : { server_id: serverId })
      .then((session) => {
        const url = inspectorEntryUrl(session);
        if (url === null) {
          throw new ApiError({ status: 200, code: "invalid_response", message: "The daemon sent an Inspector link this page cannot use." });
        }
        // The daemon issued the link after this request was sent, so it works at least until a minute after that;
        // expires_at can only make it earlier (it comes from the daemon's clock).
        const expires = Date.parse(session.expires_at);
        const deadline = Math.min(asked + LINK_LIFETIME_MS, Number.isFinite(expires) ? expires : Infinity);
        const live = Date.now() < deadline;
        if (tab && !tab.closed) {
          if (live) {
            tab.location.replace(url);
            return;
          }
          // A slow answer: never send the tab to a link that no longer works.
          tab.close();
          setOffer({ url: null, deadline, reason: "late" });
          return;
        }
        setOffer({ url: live ? url : null, deadline, reason: "no-tab" });
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
        open={offer !== null}
        onOpenChange={(o) => !o && setOffer(null)}
        title="Open the Inspector"
        description={OFFER_DESCRIPTIONS[offer?.reason ?? "no-tab"]}
      >
        {expired ? (
          <div className="flex flex-col items-start gap-3 text-sm">
            <p>The link has expired. Ask for a new one.</p>
            <Button
              size="sm"
              onClick={() => {
                setOffer(null);
                open();
              }}
            >
              Get a new link
            </Button>
          </div>
        ) : (
          <div className="flex flex-col items-start gap-3 text-sm">
            <p>Open it from here instead. The link works once, within a minute.</p>
            <Button size="sm" onClick={openOffered}>
              Open the Inspector
              <ExternalLink aria-hidden />
              <span className="sr-only">(opens in a new tab)</span>
            </Button>
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
  // The daemon's 409 for a change it cannot make (the Inspector is not available): kept when the page learns that and drops the controls.
  const [refusal, setRefusal] = useState<ApiError | null>(null);
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
        <div className="flex flex-col gap-3">
          {refusal && <ErrorText error={refusal} />}
          <Notice tone="info" title="Not available on this host">
            The daemon was started with the Inspector listener turned off (<code className="font-mono text-xs">--inspector-listen off</code>), so
            the Inspector cannot be turned on. To use it, start the daemon with an address for the Inspector listener.
          </Notice>
        </div>
      ) : (
        <InspectorControls inspector={inspector.data} onRefused={setRefusal} />
      )}
    </Section>
  );
}

function InspectorControls({ inspector, onRefused }: { inspector: Inspector; onRefused: (err: ApiError) => void }) {
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
    onError: (err) => {
      if (err instanceof ApiError && err.code === "conflict") {
        // The Inspector is not available (any more): show what the daemon reports now.
        onRefused(err);
        void queryClient.invalidateQueries({ queryKey: qk.inspector });
      }
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
          {inspector.other_targets} {inspector.other_targets === 1 ? "entry in the Inspector belongs" : "entries in the Inspector belong"} to no
          server with the Inspector switch on, for example a server whose switch was turned off while it runs.
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
        {(server.inspector || server.inspector_active) && <ServerInspectorStatus server={server} busy={busy} onJob={onJob} />}
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
  const restart = <ServerActions server={server} busy={busy} onJob={onJob} only="restart" />;
  switch (st.kind) {
    case "off":
      return null;
    case "leaving":
      return row(
        <Badge tone="warning">Restart required</Badge>,
        "The switch is off, so the Inspector no longer shows this server. The running game server was started with it on and keeps trying to connect until its next restart.",
        restart,
      );
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
    case "unreachable":
      return row(
        <Badge tone="warning">Not connected</Badge>,
        "Game servers cannot reach the Inspector on this host: its listener is bound to a loopback address. An administrator has to change how the daemon is started.",
      );
    case "server-unknown":
      return row(<Badge tone="warning">Unknown</Badge>, "Docker cannot be queried, so the state of this server is not known.");
    case "not-running":
      return row(<Badge>Not connected</Badge>, "The server is not running. It connects to the Inspector when it starts.");
    case "restart-needed":
      return row(
        <Badge tone="warning">Restart required</Badge>,
        "The running game server was started without the Inspector. It connects at the next restart.",
        restart,
      );
    case "restart-pending":
      return row(
        <Badge tone="warning">Not connected</Badge>,
        "The game server is not connected to the Inspector. It has changes that wait for a restart.",
        restart,
      );
    case "targets-unknown":
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
