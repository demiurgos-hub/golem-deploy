import { useEffect, useRef, useState, type ReactNode } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ArrowDownToLine, Download, Pause, Play, WrapText } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { LogBuffer } from "@/lib/log-buffer";
import { saveBlob } from "@/lib/download";
import { cn } from "@/lib/utils";

const LINE_HEIGHT = 18;

// Strip ANSI escape sequences (colours, cursor moves) from container output.
// eslint-disable-next-line no-control-regex -- matching ESC is the point
const ANSI = /\u001b\[[0-9;?]*[ -/]*[@-~]|\u001b\][^\u0007]*\u0007/g;

export function stripAnsi(line: string): string {
  return line.includes("\u001b") ? line.replace(ANSI, "") : line;
}

interface LogViewerProps {
  buffer: LogBuffer;
  /** Pass the stream's version so new lines re-render the view. */
  version: number;
  /** Name for the downloaded file. */
  filename: string;
  /** Status text or controls shown on the left of the toolbar. */
  status?: ReactNode;
  /** Extra controls (tail size, follow toggle) placed before the built-in ones. */
  controls?: ReactNode;
  emptyText?: string;
  /** Shown inside the viewer above the lines, e.g. why the stream could not be opened and a retry. */
  notice?: ReactNode;
  className?: string;
  heightClass?: string;
}

/**
 * Windowed log display: only visible lines are in the DOM, so tens of
 * thousands of lines stay responsive. Auto-scroll follows the end until the
 * user scrolls up; "Pause" freezes the view while the stream keeps
 * buffering. All text is rendered as text.
 */
export function LogViewer({
  buffer,
  version,
  filename,
  status,
  controls,
  emptyText = "No output yet.",
  notice,
  className,
  heightClass = "h-80",
}: LogViewerProps) {
  const [wrap, setWrap] = useState(false);
  // A snapshot of the lines when paused, and the buffer version it was taken at.
  const [frozen, setFrozen] = useState<{ lines: string[]; version: number } | null>(null);
  const [follow, setFollow] = useState(true);

  const paused = frozen !== null;
  const liveCount = buffer.length;
  const newWhilePaused = paused && version !== frozen.version;

  function togglePause() {
    if (paused) {
      setFrozen(null);
    } else {
      const lines: string[] = [];
      for (let i = 0; i < buffer.length; i++) lines.push(buffer.at(i));
      setFrozen({ lines, version });
    }
  }

  return (
    <div className={cn("flex min-w-0 flex-col overflow-hidden rounded-md border border-border", className)}>
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border bg-muted/40 px-2 py-1.5">
        <div className="flex min-w-0 flex-wrap items-center gap-2 text-xs text-muted-foreground">
          {status}
          <span>
            {liveCount.toLocaleString()} line{liveCount === 1 ? "" : "s"}
            {buffer.dropped > 0 && `, ${buffer.dropped.toLocaleString()} older dropped`}
          </span>
          {newWhilePaused && <span className="text-amber-600 dark:text-amber-400">Paused, new output waiting</span>}
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          {controls}
          <Button size="sm" variant="outline" onClick={togglePause} aria-pressed={paused}>
            {paused ? <Play /> : <Pause />}
            {paused ? "Resume" : "Pause"}
          </Button>
          <Button size="sm" variant="outline" onClick={() => setWrap((w) => !w)} aria-pressed={wrap}>
            <WrapText />
            Wrap
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={() => setFollow(true)}
            disabled={follow}
            title="Scroll to the newest line and keep following"
          >
            <ArrowDownToLine />
            End
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={liveCount === 0}
            onClick={() => saveBlob(new Blob([buffer.toText()], { type: "text/plain;charset=utf-8" }), filename)}
          >
            <Download />
            Download
          </Button>
        </div>
      </div>
      {notice && <div className="border-b border-border bg-muted/20 px-3 py-2 text-sm">{notice}</div>}
      <LogLines
        key={wrap ? "wrap" : "nowrap"}
        count={paused ? frozen.lines.length : liveCount}
        line={(i) => (paused ? frozen.lines[i] : buffer.at(i))}
        version={paused ? -1 : version}
        wrap={wrap}
        follow={follow && !paused}
        onFollowChange={setFollow}
        emptyText={emptyText}
        heightClass={heightClass}
      />
    </div>
  );
}

function LogLines({
  count,
  line,
  version,
  wrap,
  follow,
  onFollowChange,
  emptyText,
  heightClass,
}: {
  count: number;
  line: (i: number) => string;
  version: number;
  wrap: boolean;
  follow: boolean;
  onFollowChange: (follow: boolean) => void;
  emptyText: string;
  heightClass: string;
}) {
  const parentRef = useRef<HTMLDivElement>(null);
  const programmatic = useRef(false);
  // eslint-disable-next-line react-hooks/incompatible-library -- the React Compiler is not enabled in this build
  const virtualizer = useVirtualizer({
    count,
    getScrollElement: () => parentRef.current,
    estimateSize: () => LINE_HEIGHT,
    overscan: 30,
  });

  useEffect(() => {
    if (!follow || count === 0) return;
    programmatic.current = true;
    virtualizer.scrollToIndex(count - 1, { align: "end" });
  }, [follow, count, version, virtualizer]);

  function onScroll() {
    const el = parentRef.current;
    if (!el) return;
    if (programmatic.current) {
      programmatic.current = false;
      return;
    }
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < LINE_HEIGHT * 2;
    if (atBottom !== follow) onFollowChange(atBottom);
  }

  return (
    <div
      ref={parentRef}
      onScroll={onScroll}
      tabIndex={0}
      role="log"
      aria-label="Log output"
      className={cn(
        "relative overflow-auto bg-background font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
        heightClass,
      )}
    >
      {count === 0 ? (
        <p className="p-3 text-muted-foreground">{emptyText}</p>
      ) : (
        <div style={{ height: virtualizer.getTotalSize(), position: "relative", minWidth: "100%" }}>
          {virtualizer.getVirtualItems().map((item) => (
            <div
              key={item.key}
              data-index={item.index}
              ref={wrap ? virtualizer.measureElement : undefined}
              className={cn(
                "absolute left-0 px-3",
                wrap ? "w-full whitespace-pre-wrap break-all" : "min-w-full whitespace-pre",
              )}
              style={{ transform: `translateY(${item.start}px)`, lineHeight: `${LINE_HEIGHT}px`, minHeight: LINE_HEIGHT }}
            >
              {stripAnsi(line(item.index)) || " "}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
