import type { ErrorCode } from "@/api/types.gen";

/**
 * Codes the client produces itself when there is no error envelope: the
 * request never reached the daemon, or the daemon (or a proxy in front of it)
 * answered with something that is not the documented error body.
 */
export type ClientErrorCode = "network_error" | "invalid_response" | "aborted";

export type ApiErrorCode = ErrorCode | ClientErrorCode;

/** The single error type every API call rejects with. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: ApiErrorCode;
  readonly requestId: string;
  readonly field: string | undefined;
  /** Seconds from the Retry-After header (429), when present. */
  readonly retryAfter: number | undefined;

  constructor(init: {
    status: number;
    code: ApiErrorCode;
    message: string;
    requestId?: string;
    field?: string;
    retryAfter?: number;
  }) {
    super(init.message);
    this.name = "ApiError";
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId ?? "";
    this.field = init.field;
    this.retryAfter = init.retryAfter;
  }

  get isClientError(): boolean {
    return this.status >= 400 && this.status < 500;
  }
}

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

export function isAbortError(err: unknown): boolean {
  return (
    (err instanceof DOMException && err.name === "AbortError") ||
    (err instanceof ApiError && err.code === "aborted")
  );
}

function parseRetryAfter(value: string | null): number | undefined {
  if (!value) return undefined;
  const n = Number(value);
  if (Number.isFinite(n) && n >= 0) return Math.ceil(n);
  const date = Date.parse(value);
  if (!Number.isNaN(date)) return Math.max(0, Math.ceil((date - Date.now()) / 1000));
  return undefined;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * Builds an ApiError from a failed response's status, headers and body text.
 * The body is only trusted when it matches the documented envelope; anything
 * else (an HTML page from a proxy, an empty body) becomes a generic message so
 * raw response text never reaches the UI.
 */
export function errorFromResponse(
  status: number,
  headers: { get(name: string): string | null },
  bodyText: string,
): ApiError {
  const requestId = headers.get("X-Request-Id") ?? "";
  const retryAfter = parseRetryAfter(headers.get("Retry-After"));
  let parsed: unknown;
  try {
    parsed = bodyText ? JSON.parse(bodyText) : undefined;
  } catch {
    parsed = undefined;
  }
  if (isRecord(parsed) && isRecord(parsed.error)) {
    const e = parsed.error;
    if (typeof e.code === "string" && typeof e.message === "string") {
      return new ApiError({
        status,
        code: e.code as ErrorCode,
        message: e.message,
        requestId: typeof e.request_id === "string" ? e.request_id : requestId,
        field: typeof e.field === "string" && e.field ? e.field : undefined,
        retryAfter,
      });
    }
  }
  return new ApiError({
    status,
    code: "invalid_response",
    message: genericStatusMessage(status),
    requestId,
    retryAfter,
  });
}

function genericStatusMessage(status: number): string {
  if (status === 502 || status === 503 || status === 504) {
    return `The daemon is not available right now (HTTP ${status}).`;
  }
  if (status >= 500) return `The daemon failed to handle the request (HTTP ${status}).`;
  if (status >= 400) return `The request was rejected (HTTP ${status}).`;
  return `Unexpected response from the daemon (HTTP ${status}).`;
}

export function networkError(): ApiError {
  return new ApiError({
    status: 0,
    code: "network_error",
    message: "Could not reach the daemon. Check your connection and try again.",
  });
}

export function abortedError(): ApiError {
  return new ApiError({ status: 0, code: "aborted", message: "The request was cancelled." });
}

/** The daemon's message as a sentence: capitalised, ending with a full stop. */
export function sentence(message: string): string {
  const m = message.trim();
  if (!m) return m;
  const first = m.charAt(0).toUpperCase() + m.slice(1);
  return /[.!?]$/.test(first) ? first : `${first}.`;
}

/** "Try again in N seconds." from Retry-After, or "" when the daemon sent none. */
export function retryHint(err: ApiError, seconds = err.retryAfter): string {
  return seconds !== undefined && seconds > 0 ? ` Try again in ${formatWait(seconds)}.` : "";
}

/**
 * Plain-language text for an error, for toasts and error states. Codes that
 * need an explanation the daemon cannot give get their own wording;
 * everything else uses the daemon's message, which the contract guarantees
 * is safe to display (it is specific: e.g. why a job may not be cancelled).
 */
export function describeError(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "Something went wrong in the web client. Reload the page and try again.";
  }
  switch (err.code) {
    case "rate_limited":
      // Too many failed attempts, too many concurrent logins, or too many open log streams:
      // the daemon's message says which.
      return sentence(err.message || "Too many requests") + retryHint(err);
    case "invalid_credentials":
      return "Sign-in failed. Check the username and password. New accounts must be approved by an administrator first.";
    case "last_admin":
      return "This change would leave no approved administrator. Promote or approve another administrator first.";
    case "cannot_modify_self":
      return "You cannot delete, demote or unapprove your own account. Another administrator has to do it.";
    case "session_required":
      return "This action needs a signed-in session, not an API token.";
    case "job_in_progress":
      return "Another operation is already running for this item. Wait for it to finish.";
    case "bad_host":
      return (
        "The daemon does not accept the host name in this page's address. Open the panel by IP address, " +
        "by localhost, or by a host name the daemon was started with (--public-host)."
      );
    case "service_unavailable":
      // Docker or git unreachable, a Git remote failing, shutdown, or busy checking passwords.
      return sentence(err.message || "The daemon is not available right now") + retryHint(err);
    default:
      return err.message ? sentence(err.message) : `The request failed (HTTP ${err.status}).`;
  }
}

/** Errors worth an automatic "Retry" offer: the daemon said to come back later. */
export function isRetryable(err: unknown): boolean {
  return err instanceof ApiError && (err.code === "service_unavailable" || err.code === "rate_limited" || err.code === "network_error");
}

export function formatWait(seconds: number): string {
  if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"}`;
  const minutes = Math.ceil(seconds / 60);
  return `${minutes} minute${minutes === 1 ? "" : "s"}`;
}
