import { screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/api/errors";
import type { ServerConnectivity } from "@/api/types.gen";
import { runConnectionTest, type Step, type StepResult } from "@/lib/connection-test";
import { account, jsonResponse } from "@/test/fake-api";
import { certFixture, runtimeFixture, serverFixture } from "@/test/fixtures";
import { renderApp } from "@/test/render-app";

const certificate = certFixture({ not_before: "2020-01-01T00:00:00Z", self_signed: false });
const page = "https://game.example.com:7810/";

function payload(overrides: Partial<ServerConnectivity> = {}): ServerConnectivity {
  return {
    running: true, pinned: false, certificate,
    page_url: page,
    webtransport_url: `${page}custom/wt`,
    websocket_url: "wss://game.example.com:7810/custom/ws",
    test_origins: [window.location.origin], test_origins_active: true,
    ...overrides,
  };
}

function browser({ ws = "open", wt = "open", tcp = true } = {}) {
  const closeWS = vi.fn();
  const closeWT = vi.fn();
  const fetch = vi.fn(() => tcp ? Promise.resolve(new Response()) : Promise.reject(new TypeError("network error")));
  const socket = vi.fn(function () {
    const s = { close: closeWS, onopen: null as (() => void) | null, onerror: null as (() => void) | null, onclose: null };
    void Promise.resolve().then(() => {
      if (ws === "open") s.onopen?.();
      if (ws === "error") s.onerror?.();
    });
    return s;
  });
  const transport = vi.fn(function () {
    const ready = wt === "open" ? Promise.resolve() : wt === "error" ? Promise.reject(new Error("UDP failed")) : new Promise<void>(() => {});
    return { ready, closed: wt === "error" ? Promise.reject(new Error("closed")) : Promise.resolve(), close: closeWT };
  });
  vi.stubGlobal("fetch", fetch);
  vi.stubGlobal("WebSocket", socket);
  vi.stubGlobal("WebTransport", transport);
  return { fetch, socket, transport, closeWS, closeWT };
}

function probe(data = payload(), runtime = runtimeFixture()) {
  const api = { connectivity: vi.fn().mockResolvedValue(data), runtime: vi.fn().mockResolvedValue(runtime) };
  const results = new Map<Step, StepResult>();
  const controller = new AbortController();
  const done = runConnectionTest(api, 3, controller.signal, (step, result) => results.set(step, result));
  return { api, results, controller, done };
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("browser connection probes", () => {
  it("fetches without credentials and opens and closes both transports in order", async () => {
    const b = browser();
    const { results, done } = probe();
    await done;
    expect([...results.values()].map((r) => r.status)).toEqual(["passed", "passed", "passed", "passed"]);
    expect(b.fetch).toHaveBeenCalledWith(page, expect.objectContaining({ mode: "no-cors", credentials: "omit", cache: "no-store", redirect: "manual" }));
    expect(b.socket).toHaveBeenCalledWith("wss://game.example.com:7810/custom/ws");
    expect(b.transport).toHaveBeenCalledWith(`${page}custom/wt`, { serverCertificateHashes: undefined });
    expect(b.fetch).toHaveBeenCalledBefore(b.socket);
    expect(b.closeWS).toHaveBeenCalledBefore(b.transport);
    expect(b.closeWT).toHaveBeenCalledTimes(1);
    expect(results.get("webtransport")).toMatchObject({ milliseconds: expect.any(Number) });
  });

  it("skips both TCP probes for pinned certificates and passes the 32-byte hash", async () => {
    const b = browser();
    const { results, done } = probe(payload({ pinned: true }));
    await done;
    expect([...results.values()].map((r) => r.status)).toEqual(["passed", "skipped", "skipped", "passed"]);
    expect(b.fetch).not.toHaveBeenCalled();
    expect(b.socket).not.toHaveBeenCalled();
    expect(b.transport).toHaveBeenCalledWith(`${page}custom/wt`, {
      serverCertificateHashes: [{ algorithm: "sha-256", value: new Uint8Array(32).fill(0x9f) }],
    });
  });

  it.each([
    [{ running: false }, /Start it/],
    [{ certificate: null }, /no certificate installed/],
    [{ certificate: { ...certificate, not_after: "2020-01-01T00:00:00Z" } }, /expired or not yet valid/],
    [{ certificate: { ...certificate, not_before: "2098-01-01T00:00:00Z" } }, /expired or not yet valid/],
    [{ certificate: { ...certificate, covers_game_host: false } }, /does not cover/],
    [{ test_origins: ["https://panel.example.com"] }, /Open the panel at .*panel.example.com/],
    [{ test_origins: [] }, /no panel origins configured/],
    [{ test_origins_active: false }, /Restart the server/],
    [{ pinned: true, certificate: { ...certificate, fingerprint_sha256: "bogus" } }, /invalid certificate fingerprint/],
    [{ page_url: "" }, /usable game endpoint/],
  ] satisfies [Partial<ServerConnectivity>, RegExp][])("skips the transports when preflight fails (%j)", async (overrides, message) => {
    const b = browser();
    const { results, done } = probe(payload(overrides));
    await done;
    expect(results.get("server")).toMatchObject({ status: "failed", detail: expect.stringMatching(message) });
    expect([...results.values()].map((r) => r.status)).toEqual(["failed", "skipped", "skipped", "skipped"]);
    expect(b.fetch).not.toHaveBeenCalled();
  });

  it("shows the game's reason when the running server is not ready", async () => {
    browser();
    const { results, done } = probe(payload(), runtimeFixture({ ready: false, not_ready_reason: "Loading map" }));
    await done;
    expect(results.get("server")).toMatchObject({ status: "failed", detail: expect.stringContaining("Loading map") });
  });

  it("shows a useful message when WebTransport is unavailable", async () => {
    browser();
    vi.stubGlobal("WebTransport", undefined);
    const { results, done } = probe();
    await done;
    expect(results.get("server")).toMatchObject({ status: "failed", detail: expect.stringContaining("WebTransport is unavailable in this browser") });
  });

  it("exposes API failures and request IDs", async () => {
    const report = vi.fn();
    const api = {
      connectivity: vi.fn().mockRejectedValue(new ApiError({
        status: 503, code: "service_unavailable", message: "Docker is unreachable", requestId: "req-42",
      })),
      runtime: vi.fn(),
    };
    await runConnectionTest(api, 3, new AbortController().signal, report);
    expect(report).toHaveBeenCalledWith("server", expect.objectContaining({ status: "failed", detail: "Docker is unreachable. Request ID: req-42." }));
    expect(api.runtime).not.toHaveBeenCalled();
  });

  it("isolates failed WebSocket and UDP steps when HTTPS passes", async () => {
    const b = browser({ ws: "error", wt: "error" });
    const { results, done } = probe();
    await done;
    expect(results.get("websocket")).toMatchObject({ status: "failed", detail: expect.stringContaining("WebSocket upgrade support") });
    expect(results.get("webtransport")).toMatchObject({ status: "failed", detail: expect.stringContaining("TCP works. UDP port 7810 is most likely blocked") });
    expect(b.closeWS).toHaveBeenCalledOnce();
    expect(b.closeWT).toHaveBeenCalledOnce();
  });

  it("explains the localhost IPv4-only case", async () => {
    browser({ wt: "error" });
    const { results, done } = probe(payload({
      pinned: true,
      page_url: "https://localhost:7810/",
      webtransport_url: "https://localhost:7810/wt",
      websocket_url: "wss://localhost:7810/api/ws",
    }));
    await done;
    expect(results.get("webtransport")).toMatchObject({
      status: "failed", detail: expect.stringContaining("If the server listens only on IPv4, set the game host to 127.0.0.1"),
    });
  });

  it("still tests both transports after an HTTPS failure", async () => {
    browser({ tcp: false });
    const { results, done } = probe();
    await done;
    expect(results.get("https")).toMatchObject({ status: "failed", detail: expect.stringContaining("TCP port 7810") });
    expect(results.get("websocket")?.status).toBe("passed");
    expect(results.get("webtransport")?.status).toBe("passed");
  });

  it("closes stalled transports when they time out", async () => {
    vi.useFakeTimers();
    const b = browser({ ws: "stall", wt: "stall" });
    const { results, done } = probe();
    await vi.advanceTimersByTimeAsync(12_000);
    await done;
    expect(results.get("websocket")?.status).toBe("failed");
    expect(results.get("webtransport")?.status).toBe("failed");
    expect(b.closeWS).toHaveBeenCalledOnce();
    expect(b.closeWT).toHaveBeenCalledOnce();
  });

  it("aborts a stalled HTTPS fetch and continues with the transports", async () => {
    vi.useFakeTimers();
    browser();
    let signal: AbortSignal | undefined;
    vi.stubGlobal("fetch", (_: string, options: RequestInit) => {
      signal = options.signal ?? undefined;
      return new Promise<Response>(() => {});
    });
    const { results, done } = probe();
    await vi.advanceTimersByTimeAsync(6000);
    await done;
    expect(signal?.aborted).toBe(true);
    expect(results.get("https")?.status).toBe("failed");
    expect(results.get("websocket")?.status).toBe("passed");
    expect(results.get("webtransport")?.status).toBe("passed");
  });
});

it("renders the test for a developer and closes a pending transport on navigation", async () => {
  const b = browser({ wt: "stall" });
  const view = renderApp({
    path: "/servers/3",
    signedInAs: account({ role: "developer" }),
    routes: {
      "GET /servers/{id}": () => jsonResponse(200, serverFixture()),
      "GET /servers/{id}/runtime": () => jsonResponse(200, runtimeFixture()),
      "GET /servers/{id}/connectivity": () => jsonResponse(200, payload()),
      "GET /projects": () => jsonResponse(200, { items: [] }),
      "GET /jobs": () => jsonResponse(200, { items: [] }),
    },
  });
  await view.user.click(await screen.findByRole("button", { name: "Run connection test" }, { timeout: 5000 }));
  await waitFor(() => expect(b.transport).toHaveBeenCalledOnce());
  const section = screen.getByRole("region", { name: "Connection test" });
  expect(within(section).getByText(/player joining and leaving/)).toBeInTheDocument();
  expect(within(section).getAllByText("Passed")).toHaveLength(3);
  expect(within(section).getAllByText(/^\d+ ms$/)).toHaveLength(3);
  view.unmount();
  await waitFor(() => expect(b.closeWT).toHaveBeenCalledOnce());
});
