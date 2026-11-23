import type { RawResponse, Transport, UploadRequest } from "@/api/client";
import type { Account, ErrorCode } from "@/api/types.gen";

export interface FakeRequest {
  method: string;
  path: string;
  params: Record<string, string>;
  query: URLSearchParams;
  headers: Record<string, string>;
  body: unknown;
  signal?: AbortSignal | null;
}

export type Handler = (req: FakeRequest) => Response | Promise<Response>;

let seq = 0;

export function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  seq++;
  const h = { "X-Request-Id": `req-${seq}`, ...headers };
  if (status === 204) return new Response(null, { status, headers: h });
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...h } });
}

export function errorResponse(
  status: number,
  code: ErrorCode,
  message: string,
  opts: { field?: string; headers?: Record<string, string>; requestId?: string } = {},
): Response {
  const requestId = opts.requestId ?? `req-err-${++seq}`;
  return new Response(JSON.stringify({ error: { code, message, request_id: requestId, ...(opts.field ? { field: opts.field } : {}) } }), {
    status,
    headers: { "Content-Type": "application/json", "X-Request-Id": requestId, ...(opts.headers ?? {}) },
  });
}

/** A text response whose body is written by the test through the returned controller. */
export function streamResponse(): { response: Response; push: (text: string) => void; close: () => void } {
  let ctrl!: ReadableStreamDefaultController<Uint8Array>;
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      ctrl = c;
    },
  });
  return {
    response: new Response(body, { status: 200, headers: { "Content-Type": "text/plain; charset=utf-8" } }),
    push: (text) => ctrl.enqueue(enc.encode(text)),
    close: () => ctrl.close(),
  };
}

function compile(pattern: string): { method: string; re: RegExp; names: string[] } {
  const [method, path] = pattern.split(" ");
  const names: string[] = [];
  const re = new RegExp(
    "^" +
      path.replace(/\{(\w+)\}/g, (_m, name: string) => {
        names.push(name);
        return "([^/]+)";
      }) +
      "$",
  );
  return { method, re, names };
}

/**
 * A Transport answering "METHOD /path/{param}" patterns (paths relative to
 * /api/v1). Unmatched requests fail the test loudly with 599.
 */
export function fakeTransport(routes: Record<string, Handler>) {
  const compiled = Object.entries(routes).map(([pattern, handler]) => ({ ...compile(pattern), handler }));
  const calls: FakeRequest[] = [];
  const transport: Transport = {
    async fetch(url, init) {
      const u = new URL(url, "http://localhost");
      const path = u.pathname.replace(/^\/api\/v1/, "");
      const method = (init.method ?? "GET").toUpperCase();
      const headers = (init.headers ?? {}) as Record<string, string>;
      const req: FakeRequest = {
        method,
        path,
        params: {},
        query: u.searchParams,
        headers,
        body: typeof init.body === "string" ? JSON.parse(init.body) : undefined,
        signal: init.signal,
      };
      calls.push(req);
      for (const r of compiled) {
        if (r.method !== method) continue;
        const m = r.re.exec(path);
        if (!m) continue;
        r.names.forEach((n, i) => (req.params[n] = decodeURIComponent(m[i + 1])));
        return r.handler(req);
      }
      return new Response(`no fake route for ${method} ${path}`, { status: 599 });
    },
    async upload(req: UploadRequest): Promise<RawResponse> {
      throw new Error(`unexpected upload to ${req.url}`);
    },
  };
  return { transport, calls };
}

/** An unsigned JWT-shaped token whose payload carries `exp` (seconds). */
export function fakeJwt(expSeconds: number, extra: Record<string, unknown> = {}): string {
  const b64 = (o: unknown) => btoa(JSON.stringify(o)).replace(/=+$/, "").replace(/\+/g, "-").replace(/\//g, "_");
  return `${b64({ alg: "HS256", typ: "JWT" })}.${b64({ exp: expSeconds, ...extra })}.c2ln`;
}

export function account(overrides: Partial<Account> = {}): Account {
  return { id: 1, username: "alice", role: "admin", approved: true, created_at: "2026-01-01T00:00:00Z", ...overrides };
}
