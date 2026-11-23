import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/api/errors";
import { STORAGE_KEY, SessionRefresher, SessionStore, decodeJwtExp, type ChannelLike } from "@/api/session";
import type { SessionResponse } from "@/api/types.gen";
import { account, fakeJwt } from "@/test/fake-api";

class MemoryStorage {
  data = new Map<string, string>();
  getItem(k: string) {
    return this.data.get(k) ?? null;
  }
  setItem(k: string, v: string) {
    this.data.set(k, v);
  }
  removeItem(k: string) {
    this.data.delete(k);
  }
}

/** Channels connected like BroadcastChannel: a message reaches every other member, asynchronously. */
function channelHub() {
  const members = new Set<FakeChannel>();
  class FakeChannel implements ChannelLike {
    listeners = new Set<(ev: MessageEvent) => void>();
    constructor() {
      members.add(this);
    }
    postMessage(message: unknown) {
      for (const m of members) {
        if (m === this) continue;
        queueMicrotask(() => m.listeners.forEach((l) => l({ data: structuredClone(message) } as MessageEvent)));
      }
    }
    addEventListener(_t: "message", l: (ev: MessageEvent) => void) {
      this.listeners.add(l);
    }
    removeEventListener(_t: "message", l: (ev: MessageEvent) => void) {
      this.listeners.delete(l);
    }
    close() {
      members.delete(this);
    }
  }
  return () => new FakeChannel();
}

const flush = () => new Promise((r) => setTimeout(r, 0));

describe("SessionStore", () => {
  it("keeps the token in sessionStorage by default and in localStorage when asked", () => {
    const local = new MemoryStorage();
    const session = new MemoryStorage();
    const store = new SessionStore({ local, session });
    store.start("a", false);
    expect(session.getItem(STORAGE_KEY)).toBe("a");
    expect(local.getItem(STORAGE_KEY)).toBeNull();
    store.start("b", true);
    expect(local.getItem(STORAGE_KEY)).toBe("b");
    expect(session.getItem(STORAGE_KEY)).toBeNull();
    // A new store (page reload) restores it.
    expect(new SessionStore({ local, session }).getToken()).toBe("b");
    store.end("logout");
    expect(local.getItem(STORAGE_KEY)).toBeNull();
    expect(new SessionStore({ local, session }).getToken()).toBeNull();
  });

  it("ends only once", () => {
    const store = new SessionStore();
    store.start("a", false);
    const fn = vi.fn();
    store.subscribe(fn);
    store.end("expired", "x");
    store.end("expired", "x");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("syncs logins, refreshes and logouts across tabs", async () => {
    const newChannel = channelHub();
    const tabA = new SessionStore({ session: new MemoryStorage(), channel: newChannel() });
    const tabBStorage = new MemoryStorage();
    const tabB = new SessionStore({ session: tabBStorage, channel: newChannel() });

    tabA.start("t1", false);
    await flush();
    expect(tabB.getToken()).toBe("t1");
    expect(tabBStorage.getItem(STORAGE_KEY)).toBe("t1");

    tabB.update("t2");
    await flush();
    expect(tabA.getToken()).toBe("t2");

    const endedInA = vi.fn();
    tabA.subscribe((e) => e.type === "ended" && endedInA(e));
    tabB.end("logout");
    await flush();
    expect(tabA.getToken()).toBeNull();
    expect(endedInA).toHaveBeenCalledWith({ type: "ended", reason: "remote" });
    expect(tabA.getSnapshot().notice).toMatch(/another tab/);
  });

  it("lets a new tab borrow the session of an open tab", async () => {
    const newChannel = channelHub();
    const open = new SessionStore({ channel: newChannel() });
    open.start("t1", false);
    const fresh = new SessionStore({ channel: newChannel() });
    await fresh.requestFromPeers(1000);
    expect(fresh.getToken()).toBe("t1");
  });
});

describe("decodeJwtExp", () => {
  it("reads exp and rejects junk", () => {
    expect(decodeJwtExp(fakeJwt(1_900_000_000))).toBe(1_900_000_000);
    expect(decodeJwtExp("gdp_0123")).toBeNull();
    expect(decodeJwtExp("a.%%%.c")).toBeNull();
  });
});

describe("SessionRefresher", () => {
  const NOW = Date.UTC(2026, 10, 23, 12, 0, 0);
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  function response(token: string): SessionResponse {
    return { token, expires_at: "", session_expires_at: "", account: account() };
  }

  it("refreshes 30 minutes before exp and reschedules for the new token", async () => {
    const store = new SessionStore();
    const exp = NOW / 1000 + 12 * 3600;
    store.start(fakeJwt(exp), false);
    const second = fakeJwt(exp + 12 * 3600);
    const refresh = vi.fn().mockResolvedValueOnce(response(second));
    const r = new SessionRefresher({ store, refresh });
    r.start();
    expect(r.nextRefreshAt).toBe(exp * 1000 - 30 * 60_000);

    await vi.advanceTimersByTimeAsync(11.5 * 3600_000 - 1000);
    expect(refresh).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1000);
    expect(refresh).toHaveBeenCalledTimes(1);
    expect(store.getToken()).toBe(second);
    expect(r.nextRefreshAt).toBe((exp + 12 * 3600) * 1000 - 30 * 60_000);
    r.stop();
  });

  it("retries after a transient failure but not after 401", async () => {
    const store = new SessionStore();
    store.start(fakeJwt(NOW / 1000 + 60), false);
    const refresh = vi
      .fn()
      .mockRejectedValueOnce(new ApiError({ status: 0, code: "network_error", message: "offline" }))
      .mockRejectedValueOnce(new ApiError({ status: 401, code: "unauthenticated", message: "expired" }));
    const r = new SessionRefresher({ store, refresh, retryMs: 5000 });
    r.start();
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(5000);
    expect(refresh).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(refresh).toHaveBeenCalledTimes(2);
    r.stop();
  });

  it("refreshes when a tab returns after a long absence", async () => {
    const store = new SessionStore();
    store.start(fakeJwt(NOW / 1000 + 12 * 3600), false);
    const refresh = vi.fn().mockResolvedValue(response(fakeJwt(NOW / 1000 + 24 * 3600)));
    const listeners: (() => void)[] = [];
    const doc = {
      visibilityState: "visible" as DocumentVisibilityState,
      addEventListener: (_t: string, l: () => void) => listeners.push(l),
      removeEventListener: () => {},
    };
    const r = new SessionRefresher({ store, refresh, inactivityMs: 30 * 60_000, document: doc as unknown as Document });
    r.start();
    doc.visibilityState = "hidden";
    listeners.forEach((l) => l());
    vi.setSystemTime(NOW + 10 * 60_000);
    doc.visibilityState = "visible";
    listeners.forEach((l) => l());
    expect(refresh).not.toHaveBeenCalled();

    doc.visibilityState = "hidden";
    listeners.forEach((l) => l());
    vi.setSystemTime(NOW + 2 * 3600_000);
    doc.visibilityState = "visible";
    listeners.forEach((l) => l());
    await vi.advanceTimersByTimeAsync(0);
    expect(refresh).toHaveBeenCalledTimes(1);
    r.stop();
  });
});
