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

/** The one-time entry link: the Inspector's address followed by the session's absolute path. */
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
  | { kind: "off" }
  | { kind: "installation-off" }
  | { kind: "installation-down"; state: Inspector["state"]; detail: string }
  | { kind: "not-running" }
  | { kind: "restart-needed"; reason: "without" | "changed" }
  | { kind: "unknown" }
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
  if (!server.inspector) return { kind: "off" };
  if (!inspector.available || !inspector.enabled) return { kind: "installation-off" };
  if (inspector.state !== "running") return { kind: "installation-down", state: inspector.state, detail: inspector.detail };
  if (server.state !== "running") return { kind: "not-running" };
  const target = inspector.targets?.find((t) => t.server_id === server.id);
  // A connected game server is connected, whatever else waits for a restart (an environment change, say).
  if (server.inspector_active && target?.connected) return { kind: "connected", target };
  if (!server.inspector_active) return { kind: "restart-needed", reason: "without" };
  if (server.restart_required) return { kind: "restart-needed", reason: "changed" };
  if (inspector.targets === null) return { kind: "unknown" };
  return { kind: "not-connected" };
}
