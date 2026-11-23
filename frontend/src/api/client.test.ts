import { describe, expect, it, vi } from "vitest";
import { buildUrl, createApiClient, type Transport } from "@/api/client";
import { ApiError, describeError, errorFromResponse } from "@/api/errors";
import { SessionStore } from "@/api/session";
import { errorResponse, fakeTransport, jsonResponse } from "@/test/fake-api";

function clientWith(transport: Transport, token: string | null = "tok-1") {
  const onUnauthenticated = vi.fn();
  const client = createApiClient({ transport, getToken: () => token, onUnauthenticated });
  return { client, onUnauthenticated };
}

describe("errorFromResponse", () => {
  it("parses the error envelope", () => {
    const err = errorFromResponse(
      400,
      new Headers({ "X-Request-Id": "hdr" }),
      JSON.stringify({ error: { code: "validation_failed", message: "password must be at least 12 bytes", request_id: "abc", field: "password" } }),
    );
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 400, code: "validation_failed", requestId: "abc", field: "password" });
    expect(err.message).toBe("password must be at least 12 bytes");
  });

  it("reads Retry-After", () => {
    const err = errorFromResponse(
      429,
      new Headers({ "Retry-After": "42" }),
      JSON.stringify({ error: { code: "rate_limited", message: "too many attempts; try again later", request_id: "r" } }),
    );
    expect(err.retryAfter).toBe(42);
    expect(describeError(err)).toBe("Too many attempts; try again later. Try again in 42 seconds.");
  });

  it("says which limit was hit, from the daemon's message", () => {
    const streams = errorFromResponse(
      429,
      new Headers({ "Retry-After": "5" }),
      JSON.stringify({ error: { code: "rate_limited", message: "too many open log streams; close one and retry", request_id: "r" } }),
    );
    expect(describeError(streams)).toBe("Too many open log streams; close one and retry. Try again in 5 seconds.");
    const busy = errorFromResponse(
      503,
      new Headers({ "Retry-After": "2" }),
      JSON.stringify({ error: { code: "service_unavailable", message: "the server is busy checking passwords", request_id: "r" } }),
    );
    expect(describeError(busy)).toBe("The server is busy checking passwords. Try again in 2 seconds.");
    const docker = errorFromResponse(503, new Headers(), JSON.stringify({ error: { code: "service_unavailable", message: "Docker is unreachable", request_id: "r" } }));
    expect(describeError(docker)).toBe("Docker is unreachable.");
  });

  it("explains a rejected host name", () => {
    const err = errorFromResponse(421, new Headers(), JSON.stringify({ error: { code: "bad_host", message: "x", request_id: "r" } }));
    expect(describeError(err)).toMatch(/by IP address, by localhost, or by a host name the daemon was started with \(--public-host\)/);
  });

  it("explains account administration conflicts in plain words", () => {
    const body = (code: string) => JSON.stringify({ error: { code, message: "raw", request_id: "r" } });
    expect(describeError(errorFromResponse(409, new Headers(), body("last_admin")))).toMatch(/no approved administrator/);
    expect(describeError(errorFromResponse(409, new Headers(), body("cannot_modify_self")))).toMatch(/your own account/);
  });

  it("never exposes a non-JSON body", () => {
    const err = errorFromResponse(502, new Headers({ "X-Request-Id": "rid" }), "<html>Bad Gateway from nginx</html>");
    expect(err.code).toBe("invalid_response");
    expect(err.message).not.toContain("nginx");
    expect(err.message).toContain("502");
    expect(err.requestId).toBe("rid");
  });

  it("treats JSON without the envelope as invalid", () => {
    const err = errorFromResponse(500, new Headers(), JSON.stringify({ message: "panic: secret path /var/lib" }));
    expect(err.code).toBe("invalid_response");
    expect(err.message).not.toContain("/var/lib");
  });
});

describe("createApiClient", () => {
  it("sends the bearer token and JSON body, and decodes the response", async () => {
    const { transport, calls } = fakeTransport({
      "POST /accounts": (req) => jsonResponse(201, { echoed: req.body }),
    });
    const { client } = clientWith(transport);
    const res = await client.request<{ echoed: unknown }>("POST", "/accounts", { body: { a: 1 } });
    expect(res.echoed).toEqual({ a: 1 });
    expect(calls[0].headers.Authorization).toBe("Bearer tok-1");
    expect(calls[0].headers["Content-Type"]).toBe("application/json");
  });

  it("omits the token on public routes", async () => {
    const { transport, calls } = fakeTransport({ "POST /auth/login": () => jsonResponse(200, {}) });
    const { client } = clientWith(transport);
    await client.request("POST", "/auth/login", { body: {}, auth: false });
    expect(calls[0].headers.Authorization).toBeUndefined();
  });

  it("returns undefined for 204", async () => {
    const { transport } = fakeTransport({ "DELETE /x": () => jsonResponse(204, null) });
    const { client } = clientWith(transport);
    await expect(client.request("DELETE", "/x")).resolves.toBeUndefined();
  });

  it("maps fetch failures to network_error", async () => {
    const transport: Transport = {
      fetch: () => Promise.reject(new TypeError("Failed to fetch")),
      upload: () => Promise.reject(new Error("unused")),
    };
    const { client } = clientWith(transport);
    const err = await client.request("GET", "/status").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).code).toBe("network_error");
    expect((err as ApiError).status).toBe(0);
  });

  it("maps an aborted request to aborted, not network_error", async () => {
    const transport: Transport = {
      fetch: (_u, init) =>
        new Promise((_res, rej) => init.signal?.addEventListener("abort", () => rej(new DOMException("aborted", "AbortError")))),
      upload: () => Promise.reject(new Error("unused")),
    };
    const { client } = clientWith(transport);
    const ctrl = new AbortController();
    const p = client.request("GET", "/status", { signal: ctrl.signal });
    ctrl.abort();
    await expect(p).rejects.toMatchObject({ code: "aborted" });
  });

  it("reports 401 unauthenticated with the token that was used, but not invalid_credentials", async () => {
    const { transport } = fakeTransport({
      "GET /account": () => errorResponse(401, "unauthenticated", "session expired"),
      "POST /auth/login": () => errorResponse(401, "invalid_credentials", "invalid username or password"),
    });
    const { client, onUnauthenticated } = clientWith(transport);
    await expect(client.request("GET", "/account")).rejects.toMatchObject({ code: "unauthenticated" });
    expect(onUnauthenticated).toHaveBeenCalledWith("tok-1");
    await expect(client.request("POST", "/auth/login", { auth: false, body: {} })).rejects.toMatchObject({ code: "invalid_credentials" });
    expect(onUnauthenticated).toHaveBeenCalledTimes(1);
  });

  it("ends the session once when many requests fail with 401 together", async () => {
    const session = new SessionStore();
    session.start("tok-1", false);
    const ended = vi.fn();
    session.subscribe((e) => e.type === "ended" && ended(e));
    const { transport } = fakeTransport({ "GET /{any}": () => errorResponse(401, "unauthenticated", "expired") });
    const client = createApiClient({
      transport,
      getToken: () => session.getToken(),
      onUnauthenticated: (t) => session.handleUnauthenticated(t),
    });
    await Promise.allSettled(["/a", "/b", "/c", "/d"].map((p) => client.request("GET", p)));
    expect(ended).toHaveBeenCalledTimes(1);
    expect(session.getToken()).toBeNull();
    expect(session.getSnapshot().notice).toMatch(/session ended/i);
  });

  it("ignores a late 401 for a token that was already replaced", () => {
    const session = new SessionStore();
    session.start("old", false);
    session.update("new");
    session.handleUnauthenticated("old");
    expect(session.getToken()).toBe("new");
  });

  it("builds query strings and skips undefined values", () => {
    expect(buildUrl("/api/v1", "/jobs", { subject_type: "project", subject_id: "3", limit: undefined })).toBe(
      "/api/v1/jobs?subject_type=project&subject_id=3",
    );
  });

  it("parses errors from uploads", async () => {
    const transport: Transport = {
      fetch: () => Promise.reject(new Error("unused")),
      upload: async () => ({
        status: 413,
        headers: new Headers(),
        bodyText: JSON.stringify({ error: { code: "request_too_large", message: "request body too large", request_id: "u1" } }),
      }),
    };
    const { client } = clientWith(transport);
    await expect(client.upload("/static", new FormData())).rejects.toMatchObject({ code: "request_too_large", requestId: "u1" });
  });
});
