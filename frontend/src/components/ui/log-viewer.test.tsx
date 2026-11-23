import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { LogViewer, stripAnsi } from "@/components/ui/log-viewer";
import { LogBuffer } from "@/lib/log-buffer";

describe("LogViewer", () => {
  it("renders only a window of a large buffer", () => {
    const buffer = new LogBuffer(100_000);
    buffer.push(Array.from({ length: 50_000 }, (_, i) => `line ${i}`));
    render(<LogViewer buffer={buffer} version={buffer.version} filename="x.log" />);
    const rows = screen.getByRole("log").querySelectorAll("[data-index]");
    expect(rows.length).toBeGreaterThan(0);
    expect(rows.length).toBeLessThan(200);
    // Locale-dependent thousands separator.
    expect(screen.getByText(/^50.000 lines$/)).toBeInTheDocument();
  });

  it("renders markup in log lines as text", () => {
    const buffer = new LogBuffer(10);
    buffer.push(['<img src=x onerror="alert(1)"> \u001b[31mred\u001b[0m']);
    render(<LogViewer buffer={buffer} version={buffer.version} filename="x.log" />);
    expect(screen.getByText('<img src=x onerror="alert(1)"> red')).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
  });

  it("strips ANSI escape sequences", () => {
    expect(stripAnsi("\u001b[1;32mok\u001b[0m done")).toBe("ok done");
    expect(stripAnsi("plain")).toBe("plain");
  });
});
