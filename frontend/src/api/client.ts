import {
  ApiError,
  abortedError,
  errorFromResponse,
  networkError,
} from "@/api/errors";

export const API_BASE = "/api/v1";

export interface HeaderReader {
  get(name: string): string | null;
}

export interface UploadRequest {
  url: string;
  body: FormData;
  headers: Record<string, string>;
  onProgress?: (loaded: number, total: number) => void;
  signal?: AbortSignal;
}

export interface RawResponse {
  status: number;
  headers: HeaderReader;
  bodyText: string;
}

/**
 * The network layer the client sits on. The default uses window.fetch and
 * XMLHttpRequest (the only browser API that reports upload progress); the
 * dev-only mock layer and the tests substitute their own.
 */
export interface Transport {
  fetch(url: string, init: RequestInit): Promise<Response>;
  upload(req: UploadRequest): Promise<RawResponse>;
}

export type Query = Record<string, string | number | boolean | undefined>;

export interface RequestOptions {
  body?: unknown;
  query?: Query;
  signal?: AbortSignal;
  /** Send the bearer token (default true). Public routes pass false. */
  auth?: boolean;
}

export interface ApiClient {
  request<T>(method: string, path: string, opts?: RequestOptions): Promise<T>;
  /** Opens a streaming text response; the caller reads `body`. */
  stream(path: string, opts: { query?: Query; signal: AbortSignal }): Promise<Response>;
  download(path: string, opts?: { signal?: AbortSignal }): Promise<{ blob: Blob; filename: string | null }>;
  upload<T>(
    path: string,
    body: FormData,
    opts?: { onProgress?: (loaded: number, total: number) => void; signal?: AbortSignal },
  ): Promise<T>;
}

export interface ApiClientOptions {
  transport: Transport;
  getToken: () => string | null;
  /** Called with the token that a request was rejected for (401 unauthenticated). */
  onUnauthenticated: (tokenUsed: string) => void;
  baseUrl?: string;
}

export function buildUrl(base: string, path: string, query?: Query): string {
  let url = base + path;
  if (query) {
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) {
      if (v !== undefined) params.set(k, String(v));
    }
    const qs = params.toString();
    if (qs) url += "?" + qs;
  }
  return url;
}

function filenameFromDisposition(value: string | null): string | null {
  if (!value) return null;
  const star = /filename\*\s*=\s*(?:UTF-8'')?([^;]+)/i.exec(value);
  if (star) {
    try {
      return decodeURIComponent(star[1].trim().replace(/^"|"$/g, ""));
    } catch {
      // Fall through to the plain parameter.
    }
  }
  const plain = /filename\s*=\s*"?([^";]+)"?/i.exec(value);
  return plain ? plain[1].trim() : null;
}

export function createApiClient(options: ApiClientOptions): ApiClient {
  const base = options.baseUrl ?? API_BASE;
  const { transport } = options;

  function authHeaders(auth: boolean): { headers: Record<string, string>; token: string | null } {
    const headers: Record<string, string> = {};
    const token = auth ? options.getToken() : null;
    if (token) headers.Authorization = `Bearer ${token}`;
    return { headers, token };
  }

  function fail(err: ApiError, token: string | null): never {
    if (err.status === 401 && err.code === "unauthenticated" && token) {
      options.onUnauthenticated(token);
    }
    throw err;
  }

  async function send(
    method: string,
    url: string,
    init: { headers: Record<string, string>; body?: BodyInit; signal?: AbortSignal },
  ): Promise<Response> {
    try {
      return await transport.fetch(url, {
        method,
        headers: init.headers,
        body: init.body,
        signal: init.signal,
        credentials: "omit",
        cache: "no-store",
      });
    } catch (err) {
      if (init.signal?.aborted || (err instanceof DOMException && err.name === "AbortError")) {
        throw abortedError();
      }
      throw networkError();
    }
  }

  async function readText(res: Response, signal?: AbortSignal): Promise<string> {
    try {
      return await res.text();
    } catch {
      if (signal?.aborted) throw abortedError();
      throw networkError();
    }
  }

  async function failed(res: Response, token: string | null, signal?: AbortSignal): Promise<never> {
    const text = await readText(res, signal).catch(() => "");
    return fail(errorFromResponse(res.status, res.headers, text), token);
  }

  return {
    async request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
      const { headers, token } = authHeaders(opts.auth ?? true);
      let body: BodyInit | undefined;
      if (opts.body !== undefined) {
        headers["Content-Type"] = "application/json";
        body = JSON.stringify(opts.body);
      }
      headers.Accept = "application/json";
      const res = await send(method, buildUrl(base, path, opts.query), { headers, body, signal: opts.signal });
      if (!res.ok) return failed(res, token, opts.signal);
      if (res.status === 204) return undefined as T;
      const text = await readText(res, opts.signal);
      if (!text) return undefined as T;
      try {
        return JSON.parse(text) as T;
      } catch {
        throw new ApiError({
          status: res.status,
          code: "invalid_response",
          message: "The daemon sent a response the web client could not read.",
          requestId: res.headers.get("X-Request-Id") ?? "",
        });
      }
    },

    async stream(path, opts) {
      const { headers, token } = authHeaders(true);
      const res = await send("GET", buildUrl(base, path, opts.query), { headers, signal: opts.signal });
      if (!res.ok) return failed(res, token, opts.signal);
      return res;
    },

    async download(path, opts = {}) {
      const { headers, token } = authHeaders(true);
      const res = await send("GET", buildUrl(base, path), { headers, signal: opts.signal });
      if (!res.ok) return failed(res, token, opts.signal);
      let blob: Blob;
      try {
        blob = await res.blob();
      } catch {
        if (opts.signal?.aborted) throw abortedError();
        throw networkError();
      }
      return { blob, filename: filenameFromDisposition(res.headers.get("Content-Disposition")) };
    },

    async upload<T>(
      path: string,
      body: FormData,
      opts: { onProgress?: (loaded: number, total: number) => void; signal?: AbortSignal } = {},
    ): Promise<T> {
      const { headers, token } = authHeaders(true);
      headers.Accept = "application/json";
      let raw: RawResponse;
      try {
        raw = await transport.upload({
          url: buildUrl(base, path),
          body,
          headers,
          onProgress: opts.onProgress,
          signal: opts.signal,
        });
      } catch (err) {
        if (err instanceof ApiError) throw err;
        if (opts.signal?.aborted) throw abortedError();
        throw networkError();
      }
      if (raw.status < 200 || raw.status >= 300) {
        return fail(errorFromResponse(raw.status, raw.headers, raw.bodyText), token);
      }
      try {
        return JSON.parse(raw.bodyText) as T;
      } catch {
        throw new ApiError({
          status: raw.status,
          code: "invalid_response",
          message: "The daemon sent a response the web client could not read.",
          requestId: raw.headers.get("X-Request-Id") ?? "",
        });
      }
    },
  };
}

/** Upload with progress through XMLHttpRequest. */
export function xhrUpload(req: UploadRequest): Promise<RawResponse> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", req.url);
    for (const [k, v] of Object.entries(req.headers)) xhr.setRequestHeader(k, v);
    xhr.withCredentials = false;
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) req.onProgress?.(e.loaded, e.total);
    };
    xhr.onload = () => {
      resolve({
        status: xhr.status,
        headers: { get: (name: string) => xhr.getResponseHeader(name) },
        bodyText: typeof xhr.responseText === "string" ? xhr.responseText : "",
      });
    };
    xhr.onerror = () => reject(networkError());
    xhr.onabort = () => reject(abortedError());
    if (req.signal) {
      if (req.signal.aborted) {
        reject(abortedError());
        return;
      }
      req.signal.addEventListener("abort", () => xhr.abort(), { once: true });
    }
    xhr.send(req.body);
  });
}

export function browserTransport(): Transport {
  return {
    fetch: (url, init) => window.fetch(url, init),
    upload: xhrUpload,
  };
}
