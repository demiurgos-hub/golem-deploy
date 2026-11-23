import { describe, expect, it } from "vitest";
import { LogBuffer } from "@/lib/log-buffer";

const lines = (from: number, n: number) => Array.from({ length: n }, (_, i) => `line ${from + i}`);

describe("LogBuffer", () => {
  it("keeps everything below the cap", () => {
    const b = new LogBuffer(10);
    b.push(lines(0, 4));
    b.push(lines(4, 4));
    expect(b.length).toBe(8);
    expect(b.dropped).toBe(0);
    expect(b.at(0)).toBe("line 0");
  });

  it("drops the oldest lines past the cap", () => {
    const b = new LogBuffer(10);
    b.push(lines(0, 8));
    b.push(lines(8, 5));
    expect(b.length).toBe(10);
    expect(b.dropped).toBe(3);
    expect(b.at(0)).toBe("line 3");
    expect(b.at(9)).toBe("line 12");
  });

  it("handles a single batch larger than the cap", () => {
    const b = new LogBuffer(5);
    b.push(lines(0, 2));
    b.push(lines(2, 12));
    expect(b.length).toBe(5);
    expect(b.dropped).toBe(9);
    expect(b.at(0)).toBe("line 9");
    expect(b.at(4)).toBe("line 13");
  });

  it("stays capped under a long stream", () => {
    const b = new LogBuffer(100_000);
    for (let i = 0; i < 30; i++) b.push(lines(i * 5000, 5000));
    expect(b.length).toBe(100_000);
    expect(b.dropped).toBe(50_000);
    expect(b.at(0)).toBe("line 50000");
    expect(b.at(99_999)).toBe("line 149999");
  });

  it("bumps the version on change and clears", () => {
    const b = new LogBuffer(3);
    const v0 = b.version;
    b.push(["a"]);
    expect(b.version).toBeGreaterThan(v0);
    b.push([]);
    const v1 = b.version;
    b.push([]);
    expect(b.version).toBe(v1);
    expect(b.toText()).toBe("a\n");
    b.clear();
    expect(b.length).toBe(0);
    expect(b.dropped).toBe(0);
    expect(b.toText()).toBe("");
  });
});
