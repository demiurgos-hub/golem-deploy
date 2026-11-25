import type { Inspector, InspectorSession, InspectorTarget, Server } from "@/api/types.gen";

type PageLocation = Pick<Location, "protocol" | "hostname">;

/**
 * The Inspector's address: `public_url` when the daemon has one (a reverse
 * proxy in front of the Inspector listener), otherwise this page's scheme
 * and host name with the listener's port. `hostname` already carries the
 * brackets of an IPv6 address. Null when the result is not an http(s) URL.
 */
export function inspectorAddress(publicUrl: string, port: number, loc: PageLocation = window.location): string | null {
  const raw = publicUrl !== "" ? publicUrl.replace(/\/+$/, "") : `${loc.protocol}//${loc.hostname}:${port}`;
  try {
    const u = new URL(raw);
    if (u.protocol !== "http:" && u.protocol !== "https:") return null;
    return raw;
  } catch {
    return null;
  }
}

/**
 * The one-time entry link: the Inspector's address followed by the session's
 * absolute path, whose fragment (`#t=<ticket>`) is kept as it is.
 */
export function inspectorEntryUrl(session: InspectorSession, loc: PageLocation = window.location): string | null {
  const base = inspectorAddress(session.public_url, session.port, loc);
  // Only an absolute path on that address: never another host ("//x") or scheme.
  if (base === null || !session.path.startsWith("/") || session.path.startsWith("//") || session.path.includes("\\")) return null;
  try {
    const u = new URL(base + session.path);
    return u.origin === new URL(base).origin ? u.href : null;
  } catch {
    return null;
  }
}

/** What the server page says about one server and the Inspector. */
export type ServerInspectorState =
  /** The switch is off and the server is not connected. */
  | { kind: "off" }
  /** The switch is off, but the running container was started with it on: it stays connected until its next restart. */
  | { kind: "leaving" }
  | { kind: "installation-off" }
  | { kind: "installation-down"; state: Inspector["state"]; detail: string }
  /** The Inspector listener is bound where containers cannot reach it: no restart helps. */
  | { kind: "unreachable" }
  /** Docker could not be queried for the server. */
  | { kind: "server-unknown" }
  | { kind: "not-running" }
  /** The running container was started without the Inspector. */
  | { kind: "restart-needed" }
  /** Not connected, and the server has changes that wait for a restart (their cause cannot be told). */
  | { kind: "restart-pending" }
  /** The Inspector did not answer. */
  | { kind: "targets-unknown" }
  | { kind: "connected"; target: InspectorTarget }
  | { kind: "not-connected" };

/**
 * The server's Inspector state, from its switch, its current container and
 * what the installation's Inspector reports.
 */
export function serverInspectorState(
  server: Pick<Server, "id" | "inspector" | "inspector_active" | "restart_required" | "state">,
  inspector: Inspector,
): ServerInspectorState {
  if (!server.inspector) {
    const leaving = server.inspector_active && server.state === "running" && inspector.enabled && inspector.state === "running";
    return { kind: leaving ? "leaving" : "off" };
  }
  if (!inspector.available || !inspector.enabled) return { kind: "installation-off" };
  if (inspector.state !== "running") return { kind: "installation-down", state: inspector.state, detail: inspector.detail };
  if (!inspector.agents_reachable) return { kind: "unreachable" };
  if (server.state === "unknown") return { kind: "server-unknown" };
  if (server.state !== "running") return { kind: "not-running" };
  const target = inspector.targets?.find((t) => t.server_id === server.id);
  // A connected game server is connected, whatever else waits for a restart (an environment change, say).
  if (server.inspector_active && target?.connected) return { kind: "connected", target };
  if (!server.inspector_active) return { kind: "restart-needed" };
  if (inspector.targets === null) return { kind: "targets-unknown" };
  // restart_required does not say what changed: the Inspector's secrets, or only the environment.
  if (server.restart_required) return { kind: "restart-pending" };
  return { kind: "not-connected" };
}
