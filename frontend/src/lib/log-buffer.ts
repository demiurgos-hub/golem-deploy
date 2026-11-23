/**
 * A capped line buffer for log viewers. When it grows past `cap`, the oldest
 * lines are dropped and counted in `dropped`. Appends come in batches (one
 * per network chunk), so trimming with one splice per batch stays cheap.
 */
export class LogBuffer {
  private lines: string[] = [];
  private droppedCount = 0;
  private revision = 0;
  readonly cap: number;

  constructor(cap: number) {
    if (!Number.isInteger(cap) || cap < 1) throw new Error("LogBuffer cap must be a positive integer");
    this.cap = cap;
  }

  get length(): number {
    return this.lines.length;
  }

  /** Lines dropped from the front since the last clear. */
  get dropped(): number {
    return this.droppedCount;
  }

  /** Increments on every change; cheap to compare in renders. */
  get version(): number {
    return this.revision;
  }

  at(i: number): string {
    return this.lines[i] ?? "";
  }

  push(batch: readonly string[]) {
    if (batch.length === 0) return;
    if (batch.length >= this.cap) {
      this.droppedCount += this.lines.length + batch.length - this.cap;
      this.lines = batch.slice(batch.length - this.cap);
    } else {
      for (const l of batch) this.lines.push(l);
      const excess = this.lines.length - this.cap;
      if (excess > 0) {
        this.lines.splice(0, excess);
        this.droppedCount += excess;
      }
    }
    this.revision++;
  }

  clear() {
    this.lines = [];
    this.droppedCount = 0;
    this.revision++;
  }

  toText(): string {
    return this.lines.join("\n") + (this.lines.length ? "\n" : "");
  }
}
