import { describe, expect, it } from "vitest";
import { createLimiter } from "@/api/client";
import { ApiError, describeError, readyAgain, withWait } from "@/api/errors";
import { logSegments } from "@/components/ui/log-viewer";

describe("rate-limit wording", () => {
  it("says the wait once", () => {
    expect(withWait("too many attempts; try again later", 360)).toBe("Too many attempts; try again in 6 minutes.");
    expect(withWait("too many concurrent logins from your address; try again in a moment", 1)).toBe(
      "Too many concurrent logins from your address; try again in 1 second.",
    );
    expect(withWait("too many uploads in progress for this account; wait for one to finish", 1)).toBe(
      "Too many uploads in progress for this account; wait for one to finish. Try again in 1 second.",
    );
    expect(withWait("too many attempts; try again later", undefined)).toBe("Too many attempts; try again later.");
    expect(readyAgain("too many attempts; try again later")).toBe("Too many attempts; you can try again now.");
    const err = new ApiError({ status: 429, code: "rate_limited", message: "too many attempts; try again later", retryAfter: 360 });
    expect(describeError(err)).toBe("Too many attempts; try again in 6 minutes.");
  });
});

describe("createLimiter", () => {
  it("runs at most N at once, in call order, and frees slots on failure and abort", async () => {
    const limiter = createLimiter(2);
    let active = 0;
    let peak = 0;
    const order: number[] = [];
    const releases: (() => void)[] = [];
    const task = (i: number, fail = false) => () => {
      active++;
      peak = Math.max(peak, active);
      order.push(i);
      return new Promise<void>((resolve, reject) =>
        releases.push(() => {
          active--;
          if (fail) reject(new Error("x"));
          else resolve();
        }),
      );
    };
    const aborted = new AbortController();
    const runs = [
      limiter.run(task(1, true)).catch(() => "failed"),
      limiter.run(task(2)),
      limiter.run(task(3), aborted.signal).catch((e: unknown) => (e instanceof ApiError ? e.code : "?")),
      limiter.run(task(4)),
    ];
    await Promise.resolve();
    expect(order).toEqual([1, 2]);
    aborted.abort();
    releases.shift()?.(); // 1 fails: its slot goes to 4 (3 was aborted while waiting)
    await new Promise((r) => setTimeout(r, 0));
    expect(order).toEqual([1, 2, 4]);
    releases.shift()?.();
    releases.shift()?.();
    expect(await Promise.all(runs)).toEqual(["failed", undefined, "aborted", undefined]);
    expect(peak).toBe(2);
    expect(limiter.active).toBe(0);
  });
});

describe("logSegments", () => {
  it("marks escaped control sequences and the job log's container prefix", () => {
    expect(logSegments("container | \\x1b[1;31mfail\\x1b[0m \\u202e \\x07 done")).toEqual([
      { text: "container | ", kind: "prefix" },
      { text: "\\x1b[1;31m", kind: "escape" },
      { text: "fail", kind: "text" },
      { text: "\\x1b[0m", kind: "escape" },
      { text: " ", kind: "text" },
      { text: "\\u202e", kind: "escape" },
      { text: " ", kind: "text" },
      { text: "\\x07", kind: "escape" },
      { text: " done", kind: "text" },
    ]);
    expect(logSegments("plain C:\\path")).toEqual([{ text: "plain C:\\path", kind: "text" }]);
    // Raw escape sequences (not from the sanitiser) are still removed.
    expect(logSegments("\u001b[32mok\u001b[0m")).toEqual([{ text: "ok", kind: "text" }]);
  });
});
