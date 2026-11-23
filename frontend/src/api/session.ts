import { isApiError } from "@/api/errors";
import type { SessionResponse } from "@/api/types.gen";

/**
 * Session state for the web client.
 *
 * The token lives in memory and is mirrored to sessionStorage (the default:
 * it ends with the browser tab) or, with "Keep me signed in", localStorage.
 * Tabs keep each other in sync over a BroadcastChannel: a new tab asks open
 * tabs for the current session, and logins, refreshes and sign-outs are
 * announced to every tab.
 */

export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export interface ChannelLike {
  postMessage(message: unknown): void;
  addEventListener(type: "message", listener: (ev: MessageEvent) => void): void;
  removeEventListener(type: "message", listener: (ev: MessageEvent) => void): void;
  close(): void;
}

export type EndReason = "logout" | "expired" | "remote";

export interface SessionSnapshot {
  token: string | null;
  persist: boolean;
  /** One-off message for the login page, e.g. after the session expired. */
  notice: string | null;
}

export type SessionEvent =
  | { type: "started"; source: "local" | "remote" }
  | { type: "updated"; source: "local" | "remote" }
  | { type: "ended"; reason: EndReason };

type Listener = (event: SessionEvent) => void;

type ChannelMessage =
  | { type: "hello" }
  | { type: "session"; token: string; persist: boolean }
  | { type: "ended"; reason: "logout" | "expired" };

export const STORAGE_KEY = "golem_deploy_session";
export const CHANNEL_NAME = "golem_deploy_session";

export const NOTICE_EXPIRED = "Your session ended. Sign in again to continue.";
export const NOTICE_REMOTE_LOGOUT = "You were signed out in another tab.";

function safeGet(s: StorageLike | null, key: string): string | null {
  try {
    return s?.getItem(key) ?? null;
  } catch {
    return null;
  }
}
function safeSet(s: StorageLike | null, key: string, value: string) {
  try {
    s?.setItem(key, value);
  } catch {
    // Storage can be unavailable (private mode, quota); memory still works.
  }
}
function safeRemove(s: StorageLike | null, key: string) {
  try {
    s?.removeItem(key);
  } catch {
    // See safeSet.
  }
}

function isChannelMessage(v: unknown): v is ChannelMessage {
  if (typeof v !== "object" || v === null) return false;
  const m = v as Record<string, unknown>;
  if (m.type === "hello") return true;
  if (m.type === "session") return typeof m.token === "string" && typeof m.persist === "boolean";
  if (m.type === "ended") return m.reason === "logout" || m.reason === "expired";
  return false;
}

export interface SessionStoreOptions {
  local?: StorageLike | null;
  session?: StorageLike | null;
  channel?: ChannelLike | null;
}

export class SessionStore {
  private snap: SessionSnapshot;
  private listeners = new Set<Listener>();
  private snapshotListeners = new Set<() => void>();
  private readonly local: StorageLike | null;
  private readonly sessionStorage: StorageLike | null;
  private readonly channel: ChannelLike | null;
  private readonly onMessage = (ev: MessageEvent) => this.handleMessage(ev.data);

  constructor(opts: SessionStoreOptions = {}) {
    this.local = opts.local ?? null;
    this.sessionStorage = opts.session ?? null;
    this.channel = opts.channel ?? null;
    const fromSession = safeGet(this.sessionStorage, STORAGE_KEY);
    const fromLocal = safeGet(this.local, STORAGE_KEY);
    if (fromSession) this.snap = { token: fromSession, persist: false, notice: null };
    else if (fromLocal) this.snap = { token: fromLocal, persist: true, notice: null };
    else this.snap = { token: null, persist: false, notice: null };
    this.channel?.addEventListener("message", this.onMessage);
  }

  dispose() {
    this.channel?.removeEventListener("message", this.onMessage);
    this.channel?.close();
    this.listeners.clear();
    this.snapshotListeners.clear();
  }

  getToken(): string | null {
    return this.snap.token;
  }

  /** Stable snapshot for useSyncExternalStore. */
  getSnapshot = (): SessionSnapshot => this.snap;

  subscribeSnapshot = (fn: () => void): (() => void) => {
    this.snapshotListeners.add(fn);
    return () => this.snapshotListeners.delete(fn);
  };

  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  private set(next: SessionSnapshot, event: SessionEvent | null) {
    this.snap = next;
    for (const fn of this.snapshotListeners) fn();
    if (event) for (const fn of this.listeners) fn(event);
  }

  private persistToken(token: string, persist: boolean) {
    if (persist) {
      safeSet(this.local, STORAGE_KEY, token);
      safeRemove(this.sessionStorage, STORAGE_KEY);
    } else {
      safeSet(this.sessionStorage, STORAGE_KEY, token);
      safeRemove(this.local, STORAGE_KEY);
    }
  }

  private post(message: ChannelMessage) {
    try {
      this.channel?.postMessage(message);
    } catch {
      // A closed channel only loses cross-tab sync.
    }
  }

  /** After a login. */
  start(token: string, persist: boolean) {
    this.persistToken(token, persist);
    this.set({ token, persist, notice: null }, { type: "started", source: "local" });
    this.post({ type: "session", token, persist });
  }

  /** After a refresh or password change: same login, new token. */
  update(token: string) {
    if (!this.snap.token) return;
    this.persistToken(token, this.snap.persist);
    this.set({ ...this.snap, token }, { type: "updated", source: "local" });
    this.post({ type: "session", token, persist: this.snap.persist });
  }

  /** Clears local state. Idempotent: ending an ended session does nothing. */
  end(reason: EndReason, notice: string | null = null) {
    if (!this.snap.token) {
      if (notice && this.snap.notice !== notice) this.set({ ...this.snap, notice }, null);
      return;
    }
    safeRemove(this.local, STORAGE_KEY);
    safeRemove(this.sessionStorage, STORAGE_KEY);
    this.set({ token: null, persist: false, notice }, { type: "ended", reason });
    if (reason !== "remote") this.post({ type: "ended", reason });
  }

  /**
   * A request was rejected with 401 unauthenticated. Only a rejection of the
   * current token ends the session, so a burst of failing queries ends it
   * once and a late failure of an old token cannot sign out a new login.
   */
  handleUnauthenticated(tokenUsed: string) {
    if (this.snap.token && this.snap.token === tokenUsed) this.end("expired", NOTICE_EXPIRED);
  }

  clearNotice() {
    if (this.snap.notice) this.set({ ...this.snap, notice: null }, null);
  }

  /**
   * With no token of its own (a new tab in sessionStorage mode), ask other
   * tabs for theirs. Resolves when a session arrives or after the timeout.
   */
  requestFromPeers(timeoutMs = 150): Promise<void> {
    if (this.snap.token || !this.channel) return Promise.resolve();
    return new Promise((resolve) => {
      const timer = setTimeout(done, timeoutMs);
      const unsubscribe = this.subscribe((e) => {
        if (e.type === "started") done();
      });
      function done() {
        clearTimeout(timer);
        unsubscribe();
        resolve();
      }
      this.post({ type: "hello" });
    });
  }

  private handleMessage(data: unknown) {
    if (!isChannelMessage(data)) return;
    switch (data.type) {
      case "hello":
        if (this.snap.token) {
          this.post({ type: "session", token: this.snap.token, persist: this.snap.persist });
        }
        return;
      case "session": {
        if (data.token === this.snap.token) return;
        const hadToken = this.snap.token !== null;
        // Mirror into this tab's own storage so a reload keeps the session.
        if (!data.persist) safeSet(this.sessionStorage, STORAGE_KEY, data.token);
        this.set(
          { token: data.token, persist: data.persist, notice: null },
          { type: hadToken ? "updated" : "started", source: "remote" },
        );
        return;
      }
      case "ended":
        if (!this.snap.token) return;
        safeRemove(this.sessionStorage, STORAGE_KEY);
        safeRemove(this.local, STORAGE_KEY);
        this.set(
          {
            token: null,
            persist: false,
            notice: data.reason === "logout" ? NOTICE_REMOTE_LOGOUT : NOTICE_EXPIRED,
          },
          { type: "ended", reason: "remote" },
        );
        return;
    }
  }
}

/** Reads the `exp` claim (seconds since the epoch) without verifying the token. */
export function decodeJwtExp(token: string): number | null {
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  try {
    const b64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
    const payload: unknown = JSON.parse(atob(padded));
    if (typeof payload === "object" && payload !== null) {
      const exp = (payload as Record<string, unknown>).exp;
      if (typeof exp === "number" && Number.isFinite(exp)) return exp;
    }
  } catch {
    return null;
  }
  return null;
}

export interface RefresherOptions {
  store: SessionStore;
  refresh: () => Promise<SessionResponse>;
  /** How long before `exp` to refresh. */
  leadMs?: number;
  /** Retry delay after a failed refresh that did not end the session. */
  retryMs?: number;
  /** A tab hidden at least this long refreshes as soon as it is visible again. */
  inactivityMs?: number;
  document?: Pick<Document, "addEventListener" | "removeEventListener" | "visibilityState"> | null;
}

/**
 * Keeps the session token fresh: refreshes `leadMs` before the token's `exp`,
 * and when a tab becomes visible after the refresh time passed or after a
 * long absence (background timers are throttled and laptops sleep). A
 * refresh rejected with 401 ends the session through the API client; the
 * absolute 30-day lifetime therefore ends in a normal login.
 */
export class SessionRefresher {
  private timer: ReturnType<typeof setTimeout> | null = null;
  private dueAt: number | null = null;
  private hiddenAt: number | null = null;
  private inflight: Promise<void> | null = null;
  private unsubscribe: (() => void) | null = null;
  private readonly leadMs: number;
  private readonly retryMs: number;
  private readonly inactivityMs: number;
  private readonly doc: RefresherOptions["document"];
  private readonly onVisibility = () => this.handleVisibility();

  constructor(private readonly opts: RefresherOptions) {
    this.leadMs = opts.leadMs ?? 30 * 60_000;
    this.retryMs = opts.retryMs ?? 60_000;
    this.inactivityMs = opts.inactivityMs ?? 30 * 60_000;
    this.doc = opts.document ?? null;
  }

  start() {
    this.unsubscribe = this.opts.store.subscribe(() => this.schedule());
    this.doc?.addEventListener("visibilitychange", this.onVisibility);
    this.schedule();
  }

  stop() {
    this.unsubscribe?.();
    this.unsubscribe = null;
    this.doc?.removeEventListener("visibilitychange", this.onVisibility);
    this.clearTimer();
  }

  /** When the next refresh is due (ms since the epoch), for tests and display. */
  get nextRefreshAt(): number | null {
    return this.dueAt;
  }

  private clearTimer() {
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.dueAt = null;
  }

  private schedule(delayOverride?: number) {
    this.clearTimer();
    const token = this.opts.store.getToken();
    if (!token) return;
    let delay = delayOverride;
    if (delay === undefined) {
      const exp = decodeJwtExp(token);
      if (exp === null) return;
      delay = Math.max(0, exp * 1000 - this.leadMs - Date.now());
    }
    this.dueAt = Date.now() + delay;
    this.timer = setTimeout(() => {
      void this.refreshNow();
    }, delay);
  }

  refreshNow(): Promise<void> {
    if (this.inflight) return this.inflight;
    const token = this.opts.store.getToken();
    if (!token) return Promise.resolve();
    this.clearTimer();
    this.inflight = this.opts
      .refresh()
      .then((resp) => {
        // Ignore a response for a session that ended or changed meanwhile.
        if (this.opts.store.getToken() === token) this.opts.store.update(resp.token);
        else this.schedule();
      })
      .catch((err: unknown) => {
        // 401 already ended the session through the client.
        if (isApiError(err) && err.code === "unauthenticated") return;
        if (this.opts.store.getToken()) this.schedule(this.retryMs);
      })
      .finally(() => {
        this.inflight = null;
      });
    return this.inflight;
  }

  private handleVisibility() {
    if (!this.doc) return;
    const now = Date.now();
    if (this.doc.visibilityState === "hidden") {
      this.hiddenAt = now;
      return;
    }
    const wasHiddenLong = this.hiddenAt !== null && now - this.hiddenAt >= this.inactivityMs;
    this.hiddenAt = null;
    if (!this.opts.store.getToken()) return;
    if ((this.dueAt !== null && now >= this.dueAt) || wasHiddenLong) void this.refreshNow();
  }
}
