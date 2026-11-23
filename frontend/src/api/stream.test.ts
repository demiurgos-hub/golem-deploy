import { describe, expect, it } from "vitest";
import { readLines } from "@/api/stream";

function streamOf(chunks: Uint8Array[]): ReadableStream<Uint8Array> {
  return new ReadableStream({
    start(c) {
      for (const ch of chunks) c.enqueue(ch);
      c.close();
    },
  });
}

async function collect(body: ReadableStream<Uint8Array>, signal?: AbortSignal): Promise<string[]> {
  const out: string[] = [];
  for await (const batch of readLines(body, signal)) out.push(...batch);
  return out;
}

const enc = new TextEncoder();

describe("readLines", () => {
  it("joins lines split across chunks and keeps a trailing partial line", async () => {
    const chunks = ["first li", "ne\nsecond\nthi", "rd line\n", "no newline at end"].map((s) => enc.encode(s));
    expect(await collect(streamOf(chunks))).toEqual(["first line", "second", "third line", "no newline at end"]);
  });

  it("decodes multi-byte characters split across chunks", async () => {
    const bytes = enc.encode("café ☃ \u{1F680}\nnext\n");
    // Split inside every multi-byte sequence by sending one byte per chunk.
    const chunks = Array.from(bytes, (b) => new Uint8Array([b]));
    expect(await collect(streamOf(chunks))).toEqual(["café ☃ \u{1F680}", "next"]);
  });

  it("strips CR from CRLF line endings and keeps empty lines", async () => {
    expect(await collect(streamOf([enc.encode("a\r\n\r\nb\r"), enc.encode("\n")]))).toEqual(["a", "", "b"]);
  });

  it("yields one batch per chunk", async () => {
    const batches: string[][] = [];
    for await (const b of readLines(streamOf([enc.encode("1\n2\n3\n"), enc.encode("4\n")]))) batches.push(b);
    expect(batches).toEqual([["1", "2", "3"], ["4"]]);
  });

  it("stops and cancels the body when aborted", async () => {
    let cancelled = false;
    let ctrl!: ReadableStreamDefaultController<Uint8Array>;
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        ctrl = c;
      },
      cancel() {
        cancelled = true;
      },
    });
    const abort = new AbortController();
    const seen: string[] = [];
    const done = (async () => {
      for await (const batch of readLines(body, abort.signal)) {
        seen.push(...batch);
        if (seen.length === 1) abort.abort();
      }
    })();
    ctrl.enqueue(enc.encode("one\n"));
    await done;
    expect(seen).toEqual(["one"]);
    expect(cancelled).toBe(true);
  });
});
