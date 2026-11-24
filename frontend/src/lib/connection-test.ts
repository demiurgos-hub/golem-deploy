import { describeError, isApiError } from "@/api/errors";
import type { Endpoints } from "@/api/endpoints";
import type { ServerConnectivity } from "@/api/types.gen";

export const connectionSteps = {
  server: "Server and browser checks",
  https: "HTTPS",
  websocket: "WebSocket fallback",
  webtransport: "WebTransport over UDP",
} as const;

export type Step = keyof typeof connectionSteps;
export type StepResult =
  | { status: "running" }
  | { status: "passed" | "failed" | "skipped"; detail: string; milliseconds: number };

const skippedPinned = "This server uses a pinned self-signed certificate. The browser can use its fingerprint for WebTransport only.";
const timeoutMs = 6000;
class ConnectionCheckError extends Error {}

// Fail promptly even if the transport never settles. Aborting the step also
// cancels a fetch whose headers arrived before its body.
async function timed<T>(parent: AbortSignal, action: (signal: AbortSignal) => Promise<T>): Promise<T> {
  parent.throwIfAborted();
  const controller = new AbortController();
  const signal = controller.signal;
  const abort = () => controller.abort(parent.reason);
  parent.addEventListener("abort", abort, { once: true });
  const timeout = setTimeout(() => controller.abort(new DOMException("Connection timed out", "TimeoutError")), timeoutMs);
  let onAbort: () => void = () => {};
  try {
    const aborted = new Promise<never>((_, reject) => {
      onAbort = () => reject(signal.reason);
      signal.addEventListener("abort", onAbort, { once: true });
    });
    return await Promise.race([aborted, action(signal)]);
  } finally {
    clearTimeout(timeout);
    parent.removeEventListener("abort", abort);
    signal.removeEventListener("abort", onAbort);
    controller.abort();
  }
}

function fingerprintBytes(hex: string): Uint8Array<ArrayBuffer> {
  if (!/^[0-9a-f]{64}$/i.test(hex)) throw new ConnectionCheckError("The server reported an invalid certificate fingerprint. Check its certificate and retry.");
  const bytes = new Uint8Array(32);
  for (let i = 0; i < bytes.length; i++) bytes[i] = Number.parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  return bytes;
}

function probeURL(value: string, protocol: string): URL {
  let url: URL | undefined;
  try {
    url = new URL(value);
  } catch {
    // Missing or malformed endpoints get the same operator-facing message.
  }
  if (!url || url.protocol !== protocol || url.username || url.password || url.hash) {
    throw new ConnectionCheckError("The server did not report a usable game endpoint. Check the game host and transport paths.");
  }
  return url;
}

async function probeWebSocket(url: URL, parent: AbortSignal) {
  let socket: WebSocket | undefined;
  try {
    await timed(parent, () => new Promise<void>((resolve, reject) => {
      socket = new WebSocket(url.href);
      socket.onopen = () => resolve();
      socket.onerror = () => reject(new Error("WebSocket failed"));
      socket.onclose = () => reject(new Error("WebSocket closed before opening"));
    }));
  } finally {
    if (socket) {
      socket.onopen = socket.onerror = socket.onclose = null;
      socket.close();
    }
  }
}

async function probeWebTransport(url: URL, hashes: WebTransportHash[] | undefined, parent: AbortSignal) {
  let transport: WebTransport | undefined;
  try {
    await timed(parent, () => {
      transport = new WebTransport(url.href, { serverCertificateHashes: hashes });
      // A failed opening can reject both ready and closed.
      void transport.closed.catch(() => {});
      return transport.ready;
    });
  } finally {
    transport?.close();
  }
}

function checkServer(data: ServerConnectivity) {
  const cert = data.certificate;
  if (!cert) throw new ConnectionCheckError("The server has no certificate installed. Check the game certificate settings and the server log.");
  const now = Date.now();
  if (!(Date.parse(cert.not_before) <= now && now < Date.parse(cert.not_after))) {
    throw new ConnectionCheckError("The game certificate is expired or not yet valid. Check the clock and renew or replace the certificate.");
  }
  if (!cert.covers_game_host) throw new ConnectionCheckError("The certificate does not cover the game host. Install a certificate issued for that host.");
  if (!data.test_origins.includes(window.location.origin)) {
    throw new ConnectionCheckError(data.test_origins.length
      ? `Open the panel at one of these addresses to test this server: ${data.test_origins.join(", ")}.`
      : "There are no panel origins configured for testing. Configure a public host and use its panel address.");
  }
  if (!data.test_origins_active) {
    throw new ConnectionCheckError("The running container may not allow the panel origin yet. Restart the server and try again.");
  }
}

export async function runConnectionTest(
  api: Pick<Endpoints["servers"], "connectivity" | "runtime">,
  id: number,
  signal: AbortSignal,
  report: (step: Step, result: StepResult) => void,
) {
  const skip = (step: Step, detail: string) => report(step, { status: "skipped", detail, milliseconds: 0 });
  async function run<T>(step: Step, action: () => Promise<T>, pass: string, fail: (error: unknown) => string) {
    signal.throwIfAborted();
    const start = performance.now();
    report(step, { status: "running" });
    try {
      const value = await action();
      signal.throwIfAborted();
      report(step, { status: "passed", detail: pass, milliseconds: performance.now() - start });
      return { passed: true, value } as const;
    } catch (error) {
      signal.throwIfAborted();
      const timedOut = error instanceof Error && error.name === "TimeoutError";
      const detail = `${timedOut && step !== "server" ? "The connection timed out. " : ""}${fail(error)}`;
      report(step, { status: "failed", detail, milliseconds: performance.now() - start });
      return { passed: false } as const;
    }
  }

  const preflight = await run("server", async () => {
    const data = await timed(signal, (stepSignal) => api.connectivity(id, stepSignal));
    if (!data.running) throw new ConnectionCheckError("The server is not running. Start it and wait for it to report ready, then try again.");
    const runtime = await timed(signal, (stepSignal) => api.runtime(id, stepSignal));
    if (!runtime.ready) {
      const detail = runtime.not_ready_reason ? ` (${runtime.not_ready_reason})` : "";
      throw new ConnectionCheckError(`The game is not ready${detail}. Check the server log and retry once it is ready.`);
    }
    checkServer(data);
    const page = probeURL(data.page_url, "https:");
    const wt = probeURL(data.webtransport_url, "https:");
    const ws = probeURL(data.websocket_url, "wss:");
    if (page.host !== wt.host || page.host !== ws.host) throw new ConnectionCheckError("The reported game endpoints disagree. Check the game host and port.");
    // A colon in a parsed hostname identifies an IPv6 literal, with or without brackets.
    if (page.hostname.includes(":")) {
      throw new ConnectionCheckError("Browser content security policy cannot allow a literal IPv6 game host. Configure a DNS name for that address and reload the panel.");
    }
    const hashes = data.pinned && data.certificate
      ? [{ algorithm: "sha-256", value: fingerprintBytes(data.certificate.fingerprint_sha256) }]
      : undefined;
    return { page, wt, ws, pinned: data.pinned, hashes };
  }, "The game is ready and this panel origin is allowed.", (error) => {
    if (error instanceof Error && error.name === "TimeoutError") return "The panel API took too long to answer. Check the daemon and retry.";
    if (error instanceof ConnectionCheckError) return error.message;
    const message = describeError(error);
    return isApiError(error) && error.requestId ? `${message} Request ID: ${error.requestId}.` : message;
  });

  if (!preflight.passed) {
    skip("https", "The server and browser checks did not pass.");
    skip("websocket", "The server and browser checks did not pass.");
    skip("webtransport", "The server and browser checks did not pass.");
    return;
  }

  const { page, wt, ws, pinned, hashes } = preflight.value;
  const port = page.port || "443";
  const loopbackHint = page.hostname === "localhost"
    ? " If the server listens only on IPv4, set the game host to 127.0.0.1, restart the server, and reload the panel."
    : "";
  let websocketPassed = false;
  if (pinned) {
    skip("https", skippedPinned);
    skip("websocket", skippedPinned);
  } else {
    const https = await run("https", () => timed(signal, (stepSignal) => fetch(page.href, {
      mode: "no-cors",
      credentials: "omit",
      // A no-cors request must follow redirects: browsers reject any other
      // redirect mode with a TypeError before sending anything. The page's
      // connect-src policy still limits where a redirect can lead.
      redirect: "follow",
      referrerPolicy: "no-referrer",
      cache: "no-store",
      signal: stepSignal,
    })), "The browser received an HTTPS response from the game address.", () =>
      `Check the game host DNS, TCP port ${port} in the firewall, and whether this browser trusts the game certificate. Reload the panel if the game host changed.`);
    const websocket = await run("websocket", () => probeWebSocket(ws, signal), "The browser opened the WebSocket fallback.", () =>
      https.passed
        ? "HTTPS worked. Check that the game enables WebSocket fallback at this path and accepts the panel origin. Check any proxy for WebSocket upgrade support."
        : `Check the game certificate and TCP port ${port}, then check the WebSocket path and any proxy's upgrade support.`);
    websocketPassed = websocket.passed;
  }

  if (typeof WebTransport !== "function") {
    skip("webtransport", "WebTransport is unavailable in this browser. Use a browser that supports it and open the panel over trusted HTTPS or on localhost.");
    return;
  }
  await run("webtransport", () => probeWebTransport(wt, hashes, signal), "The browser opened a WebTransport session.", () => {
    if (websocketPassed) {
      return `WebSocket passed, but WebTransport failed. The most common cause is UDP port ${port} being blocked between this browser and the host. The game may also not serve WebTransport on this path.${loopbackHint}`;
    }
    return pinned
      ? `Allow UDP port ${port} through the firewall or router. If the certificate just rotated, wait a minute for the server to reload it and retry.${loopbackHint}`
      : `Allow UDP port ${port} through the firewall or router. Check the game host and certificate if the HTTPS check also failed.${loopbackHint}`;
  });
}
