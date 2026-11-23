/**
 * Reads a streamed text body as lines. Bytes are decoded with a streaming
 * TextDecoder so multi-byte characters split across chunks stay intact, and
 * lines split across chunks are joined. Yields batches (one per chunk) so a
 * fast stream does not cost one render per line. A trailing line without a
 * newline is yielded when the stream ends. Aborting the signal stops reading
 * and cancels the body.
 */
export async function* readLines(
  body: ReadableStream<Uint8Array>,
  signal?: AbortSignal,
): AsyncGenerator<string[], void, undefined> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8");
  let partial = "";
  const onAbort = () => {
    reader.cancel().catch(() => {});
  };
  signal?.addEventListener("abort", onAbort, { once: true });
  try {
    while (true) {
      if (signal?.aborted) return;
      const { value, done } = await reader.read();
      if (signal?.aborted) return;
      if (done) break;
      const text = partial + decoder.decode(value, { stream: true });
      const parts = text.split("\n");
      partial = parts.pop() ?? "";
      if (parts.length > 0) yield parts.map(stripCR);
    }
    const rest = partial + decoder.decode();
    if (rest) yield [stripCR(rest)];
  } finally {
    signal?.removeEventListener("abort", onAbort);
    try {
      reader.releaseLock();
    } catch {
      // Already released or errored.
    }
  }
}

function stripCR(line: string): string {
  return line.endsWith("\r") ? line.slice(0, -1) : line;
}
