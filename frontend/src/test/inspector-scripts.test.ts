// The daemon's Inspector scripts (internal/inspector/boot.js and enter.js)
// are plain ES5 files embedded in the Go binary; they run here in jsdom
// against a stand-in window, since jsdom does not navigate.
import { beforeEach, describe, expect, it, vi } from "vitest";
import bootSource from "../../../internal/inspector/boot.js?raw";
import enterSource from "../../../internal/inspector/enter.js?raw";

const STORE = "golem_deploy_inspector_key";
const ENDED = "/_golem-deploy/ended";
const MARKER = "golem-deploy.inspector";
const KEY = "Ab-_".repeat(10) + "xyz";
const PAGE = "https://insp.example:8444/?targetId=arena";

type FetchMock = ReturnType<typeof vi.fn<(input: unknown, init?: RequestInit) => Promise<Response>>>;

function run(source: string, w: object) {
  new Function("window", source)(w);
}

function fakeLocation(href: string) {
  const u = new URL(href);
  return { href, protocol: u.protocol, host: u.host, pathname: u.pathname, search: u.search, hash: u.hash, replace: vi.fn() };
}

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  readonly args: unknown[];
  constructor(...args: unknown[]) {
    this.args = args;
  }
}

function boot(
  key: string | null,
  respond: (input: unknown, init?: RequestInit) => Response = () => new Response("{}"),
  baseURI?: string,
) {
  if (key !== null) window.sessionStorage.setItem(STORE, key);
  const fetch: FetchMock = vi.fn(async (input: unknown, init?: RequestInit) => respond(input, init));
  const w = {
    location: fakeLocation(PAGE),
    document: baseURI === undefined ? undefined : { baseURI },
    sessionStorage: window.sessionStorage,
    fetch,
    WebSocket: FakeWebSocket,
    URL,
    Headers,
    Request,
    stop: vi.fn(),
  };
  run(bootSource, w);
  return { w, fetch, ws: w.WebSocket as unknown as new (...args: unknown[]) => FakeWebSocket };
}

function sentHeaders(fetch: FetchMock, call = 0): Headers {
  return new Headers(fetch.mock.calls[call][1]?.headers);
}

beforeEach(() => {
  window.sessionStorage.clear();
});

describe("boot.js", () => {
  it("leaves for the ended page without a key, before the page starts", () => {
    for (const key of [null, "", "short", KEY + "!"]) {
      window.sessionStorage.clear();
      const { w, fetch } = boot(key);
      expect(w.stop).toHaveBeenCalled();
      expect(w.location.replace).toHaveBeenCalledWith(ENDED);
      expect(w.fetch).toBe(fetch);
      expect(w.WebSocket).toBe(FakeWebSocket);
      expect(window.sessionStorage.getItem(STORE)).toBeNull();
    }
  });

  it("adds the bearer header to same-origin fetch calls of every input form", async () => {
    const { w, fetch } = boot(KEY);
    expect(w.location.replace).not.toHaveBeenCalled();
    const f = w.fetch as unknown as typeof globalThis.fetch;

    await f("/api/targets", { credentials: "include", headers: { "content-type": "application/json" }, method: "POST", body: "{}" });
    let h = sentHeaders(fetch, 0);
    expect(h.get("authorization")).toBe(`Bearer ${KEY}`);
    expect(h.get("content-type")).toBe("application/json");
    // The URL that was checked, absolute, is what the native fetch gets.
    expect(fetch.mock.calls[0][0]).toBe("https://insp.example:8444/api/targets");
    expect(fetch.mock.calls[0][1]).toMatchObject({ credentials: "include", method: "POST", body: "{}" });

    await f(new URL("https://insp.example:8444/auth/session"));
    expect(sentHeaders(fetch, 1).get("authorization")).toBe(`Bearer ${KEY}`);

    const req = new Request("https://insp.example:8444/api/targets", { headers: { "x-a": "1" } });
    await f(req);
    // A native copy of the Request, whose URL was the one checked.
    const sent = fetch.mock.calls[2][0] as Request;
    expect(sent).toBeInstanceOf(Request);
    expect(sent).not.toBe(req);
    expect(sent.url).toBe(req.url);
    h = sentHeaders(fetch, 2);
    expect(h.get("x-a")).toBe("1");
    expect(h.get("authorization")).toBe(`Bearer ${KEY}`);

    // Headers in init replace those of the Request, as fetch does.
    await f(req, { headers: [["x-b", "2"]] });
    h = sentHeaders(fetch, 3);
    expect(h.get("x-a")).toBeNull();
    expect(h.get("x-b")).toBe("2");
    expect(h.get("authorization")).toBe(`Bearer ${KEY}`);

    // An Authorization of the page is replaced.
    await f("/api/x", { headers: new Headers({ Authorization: "Basic eDp5" }) });
    expect(sentHeaders(fetch, 4).get("authorization")).toBe(`Bearer ${KEY}`);
  });

  it("never sends the key to another origin", async () => {
    const { w, fetch } = boot(KEY);
    const f = w.fetch as unknown as typeof globalThis.fetch;
    const init = { headers: { "x-a": "1" } };
    for (const url of ["https://other.example/api", "https://insp.example:8445/api", "http://insp.example:8444/api", "//other.example/x"]) {
      fetch.mockClear();
      await f(url, init);
      expect(fetch.mock.calls[0]).toEqual([url, init]);
    }
    fetch.mockClear();
    const req = new Request("https://other.example/api");
    await f(req);
    expect(fetch.mock.calls[0]).toHaveLength(1);
    const sent = fetch.mock.calls[0][0] as Request;
    expect(sent.url).toBe(req.url);
    expect(sent.headers.get("authorization")).toBeNull();
  });

  it("checks the URL once, against the document's base URL, and sends exactly that", async () => {
    // A <base> element that points elsewhere: relative URLs leave the
    // Inspector's origin, so they get no key.
    const based = boot(KEY, undefined, "https://other.example/app/");
    await (based.w.fetch as unknown as typeof globalThis.fetch)("/api/targets", { headers: { "x-a": "1" } });
    expect(based.fetch.mock.calls[0]).toEqual(["/api/targets", { headers: { "x-a": "1" } }]);
    expect(new based.ws("/ws/client").args).toEqual(["/ws/client"]);

    // A value whose string form changes: the first form is checked and
    // sent, the second is never seen by the native calls.
    const { w, fetch, ws: WS } = boot(KEY);
    let n = 0;
    const shifty = { toString: () => (n++ === 0 ? "/api/targets" : "https://evil.example/steal") };
    await (w.fetch as unknown as typeof globalThis.fetch)(shifty as unknown as string);
    expect(fetch.mock.calls[0][0]).toBe("https://insp.example:8444/api/targets");
    expect(sentHeaders(fetch, 0).get("authorization")).toBe(`Bearer ${KEY}`);
    n = 0;
    expect(new WS(shifty).args[0]).toBe("wss://insp.example:8444/api/targets");

    // A Request whose url getter lies: the native copy's URL counts.
    class Lying extends Request {
      get url() {
        return "https://insp.example:8444/api/targets";
      }
    }
    fetch.mockClear();
    await (w.fetch as unknown as typeof globalThis.fetch)(new Lying("https://evil.example/steal"));
    const sent = fetch.mock.calls[0][0] as Request;
    expect(sent.url).toBe("https://evil.example/steal");
    expect(sent.headers.get("authorization")).toBeNull();
  });

  it("drops the key and leaves when the daemon says the session ended", async () => {
    const { w } = boot(KEY, () => new Response('{"error":"auth_required"}', { status: 401, headers: { "X-Golem-Deploy-Session": "ended" } }));
    const resp = await (w.fetch as unknown as typeof globalThis.fetch)("/api/targets");
    expect(resp.status).toBe(401);
    expect(window.sessionStorage.getItem(STORE)).toBeNull();
    expect(w.location.replace).toHaveBeenCalledWith(ENDED);
  });

  it("keeps the key on the hub's own 401 and on other origins", async () => {
    const { w } = boot(KEY, (input) =>
      String(input).startsWith("https://other")
        ? new Response("", { status: 401, headers: { "X-Golem-Deploy-Session": "ended" } })
        : new Response('{"error":"auth_required"}', { status: 401 }),
    );
    const f = w.fetch as unknown as typeof globalThis.fetch;
    await f("/api/targets");
    await f("https://other.example/x");
    expect(window.sessionStorage.getItem(STORE)).toBe(KEY);
    expect(w.location.replace).not.toHaveBeenCalled();
  });

  it("drops the key and leaves after a successful sign-out only", async () => {
    let status = 500;
    const { w } = boot(KEY, () => new Response('{"authenticated":false}', { status }));
    const f = w.fetch as unknown as typeof globalThis.fetch;
    await f("/auth/logout", { method: "POST" });
    await f("/auth/logout");
    expect(w.location.replace).not.toHaveBeenCalled();
    status = 200;
    await f("https://insp.example:8444/auth/logout", { method: "post" });
    expect(window.sessionStorage.getItem(STORE)).toBeNull();
    expect(w.location.replace).toHaveBeenCalledWith(ENDED);
  });

  it("offers the marker and key subprotocols on same-origin WebSockets", () => {
    const { w, ws: WS } = boot(KEY);
    const keyProtocol = `golem-deploy.key.${KEY}`;
    expect(new WS("wss://insp.example:8444/ws/client").args).toEqual(["wss://insp.example:8444/ws/client", [MARKER, keyProtocol]]);
    expect(new WS("/ws/client").args).toEqual(["wss://insp.example:8444/ws/client", [MARKER, keyProtocol]]);
    expect(new WS("https://insp.example:8444/ws/client").args[0]).toBe("wss://insp.example:8444/ws/client");
    expect(new WS("wss://insp.example:8444/ws/client", "x").args[1]).toEqual(["x", MARKER, keyProtocol]);
    expect(new WS("wss://insp.example:8444/ws/client", ["x", "y"]).args[1]).toEqual(["x", "y", MARKER, keyProtocol]);
    // Other origins get exactly what the page asked for.
    expect(new WS("wss://other.example/ws").args).toEqual(["wss://other.example/ws"]);
    expect(new WS("ws://insp.example:8444/ws/client").args).toEqual(["ws://insp.example:8444/ws/client"]);
    expect(new WS("wss://insp.example:9/ws", ["p"]).args).toEqual(["wss://insp.example:9/ws", ["p"]]);
    // The wrapper still looks like WebSocket.
    const sock = new WS("/ws/client");
    expect(sock).toBeInstanceOf(w.WebSocket);
    expect(sock).toBeInstanceOf(FakeWebSocket);
    expect(w.WebSocket.prototype).toBe(FakeWebSocket.prototype);
    expect([w.WebSocket.CONNECTING, w.WebSocket.OPEN, w.WebSocket.CLOSING, w.WebSocket.CLOSED]).toEqual([0, 1, 2, 3]);
  });
});

function enter(hash: string, respond: () => Promise<Response>, storage: Pick<Storage, "setItem"> = window.sessionStorage) {
  document.title = "Opening the Inspector";
  document.body.innerHTML = '<h1 id="title">Opening the Inspector</h1><p id="text">Opening the Inspector.</p>';
  const fetch: FetchMock = vi.fn(respond);
  const w = {
    document,
    location: fakeLocation("https://insp.example:8444/_golem-deploy/enter" + hash),
    history: { replaceState: vi.fn() },
    sessionStorage: storage,
    fetch,
  };
  run(enterSource, w);
  return { w, fetch };
}

const TICKET = "T-_t".repeat(10) + "abc";
const ok = (body: object) => async () => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
const text = () => [document.title, document.getElementById("title")?.textContent, document.getElementById("text")?.textContent];

describe("enter.js", () => {
  it("removes the fragment at once, exchanges the ticket, keeps the key and opens the target", async () => {
    const { w, fetch } = enter("#t=" + TICKET, ok({ key: KEY, target_id: "arena one" }));
    expect(w.history.replaceState).toHaveBeenCalledWith(null, "", "/_golem-deploy/enter");
    expect(fetch).toHaveBeenCalledTimes(1);
    const [url, init] = fetch.mock.calls[0];
    expect(url).toBe("/_golem-deploy/session");
    expect(init).toMatchObject({ method: "POST", credentials: "omit" });
    expect(new Headers(init?.headers).get("content-type")).toBe("application/json");
    expect(JSON.parse(String(init?.body))).toEqual({ ticket: TICKET });
    await vi.waitFor(() => expect(w.location.replace).toHaveBeenCalledWith("/?targetId=arena%20one"));
    expect(window.sessionStorage.getItem(STORE)).toBe(KEY);

    // The Inspector page then boots with that key.
    const page = boot(null);
    expect(page.w.location.replace).not.toHaveBeenCalled();
    await (page.w.fetch as unknown as typeof globalThis.fetch)("/api/targets");
    expect(sentHeaders(page.fetch).get("authorization")).toBe(`Bearer ${KEY}`);
  });

  it("opens the Inspector itself without a target", async () => {
    const { w } = enter("#t=" + TICKET, ok({ key: KEY }));
    await vi.waitFor(() => expect(w.location.replace).toHaveBeenCalledWith("/"));
  });

  it("says why a link cannot be used", async () => {
    const cases: [() => Promise<Response>, string][] = [
      [async () => new Response('{"error":"link_invalid"}', { status: 403 }), "This Inspector link was already used or has expired. Open the Inspector again from Golem Deploy."],
      [async () => new Response('{"error":"account_invalid"}', { status: 403 }), "The account of this Inspector link can no longer open it. Sign in to Golem Deploy and open the Inspector again."],
      [async () => new Response('{"error":"not_running"}', { status: 503 }), "The Inspector is not running. Check its state in Golem Deploy."],
      [async () => new Response('{"error":"too_many_attempts"}', { status: 429 }), "Too many Inspector links failed from this address. Wait a few minutes, then open the Inspector again from Golem Deploy."],
      [async () => new Response('{"error":"toString"}', { status: 400 }), "The Inspector could not be opened right now. Try again in a moment from Golem Deploy."],
      [async () => new Response("<html>", { status: 502 }), "The Inspector could not be opened right now. Try again in a moment from Golem Deploy."],
      [async () => new Response('{"target_id":"x"}', { status: 200 }), "The Inspector could not be opened right now. Try again in a moment from Golem Deploy."],
      [() => Promise.reject(new TypeError("offline")), "The Inspector could not be opened right now. Try again in a moment from Golem Deploy."],
    ];
    for (const [respond, message] of cases) {
      window.sessionStorage.clear();
      const { w } = enter("#t=" + TICKET, respond);
      await vi.waitFor(() => expect(text()[2]).toBe(message));
      expect(text()[0]).toBe(text()[1]);
      expect(w.location.replace).not.toHaveBeenCalled();
      expect(window.sessionStorage.getItem(STORE)).toBeNull();
    }
  });

  it("refuses a link without a valid ticket without asking the daemon", () => {
    for (const hash of ["", "#t=", "#t=short", `#t=${TICKET}&x=1`, `#x=${TICKET}`]) {
      const { w, fetch } = enter(hash, ok({ key: KEY }));
      expect(fetch).not.toHaveBeenCalled();
      expect(w.history.replaceState).toHaveBeenCalled();
      expect(text()).toEqual([
        "Inspector link not valid",
        "Inspector link not valid",
        "This Inspector link was already used or has expired. Open the Inspector again from Golem Deploy.",
      ]);
    }
  });

  it("says so when the tab cannot keep the key", async () => {
    const storage = {
      setItem: () => {
        throw new Error("denied");
      },
    };
    const { w } = enter("#t=" + TICKET, ok({ key: KEY }), storage);
    await vi.waitFor(() => expect(text()[2]).toContain("Allow site data for this address"));
    expect(w.location.replace).not.toHaveBeenCalled();
  });
});
