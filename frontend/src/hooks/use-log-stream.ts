import { useEffect, useState } from "react";
import { useServices } from "@/app/services";
import type { Query } from "@/api/client";
import { isAbortError, isApiError, networkError } from "@/api/errors";
import { readLines } from "@/api/stream";
import { LogBuffer } from "@/lib/log-buffer";

export type LogStreamState = "connecting" | "streaming" | "ended" | "error";

export const DEFAULT_LOG_CAP = 100_000;
const FLUSH_MS = 60;

export interface LogStream {
  buffer: LogBuffer;
  /** Changes whenever lines were added; use it to re-render. */
  version: number;
  state: LogStreamState;
  error: unknown;
  /** Opens the stream again from the start (the contract has no resume offset). */
  restart(): void;
}

/**
 * Streams a text endpoint (job log, server log) into a capped buffer. Lines
 * are flushed to React at most every FLUSH_MS so fast streams stay cheap.
 * The request is aborted on unmount or when `path`/`query` change. The
 * contract offers no way to resume a stream, so a dropped connection is not
 * retried automatically: a reconnect would replay or skip lines. The viewer
 * offers an explicit reconnect instead.
 */
export function useLogStream(
  path: string | null,
  query: Query | undefined,
  opts: { cap?: number; /** Reopens the stream from the start when it changes (e.g. a new container id). */ resetKey?: string } = {},
): LogStream {
  const { cap = DEFAULT_LOG_CAP, resetKey } = opts;
  const { client } = useServices();
  const [buffer] = useState(() => new LogBuffer(cap));
  const [version, setVersion] = useState(0);
  const [state, setState] = useState<LogStreamState>("connecting");
  const [error, setError] = useState<unknown>(null);
  const [attempt, setAttempt] = useState(0);
  const queryKey = JSON.stringify(query ?? {});

  useEffect(() => {
    if (!path) return;
    const controller = new AbortController();
    const params = JSON.parse(queryKey) as Query;
    let pending: string[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;

    const flush = () => {
      timer = null;
      if (pending.length === 0) return;
      buffer.push(pending);
      pending = [];
      setVersion(buffer.version);
    };

    const run = async () => {
      buffer.clear();
      setVersion(buffer.version);
      setState("connecting");
      setError(null);
      try {
        const res = await client.stream(path, { query: params, signal: controller.signal });
        if (!res.body) {
          setState("ended");
          return;
        }
        setState("streaming");
        for await (const batch of readLines(res.body, controller.signal)) {
          for (const line of batch) pending.push(line);
          if (timer === null) timer = setTimeout(flush, FLUSH_MS);
        }
        if (controller.signal.aborted) return;
        if (timer !== null) clearTimeout(timer);
        flush();
        setState("ended");
      } catch (err) {
        if (controller.signal.aborted || isAbortError(err)) return;
        if (timer !== null) clearTimeout(timer);
        flush();
        // A body that fails mid-stream rejects with a plain TypeError.
        setError(isApiError(err) ? err : networkError());
        setState("error");
      }
    };
    void run();

    return () => {
      controller.abort();
      if (timer !== null) clearTimeout(timer);
    };
  }, [client, buffer, path, queryKey, attempt, resetKey]);

  return { buffer, version, state, error, restart: () => setAttempt((n) => n + 1) };
}
