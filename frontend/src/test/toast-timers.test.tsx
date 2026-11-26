import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { ToastProvider, useToast } from "@/components/ui/toast";

function Pusher() {
  const toast = useToast();
  return (
    <button type="button" onClick={() => toast.success("Saved")}>
      push
    </button>
  );
}

describe("toast timers", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("removes a toast after its delay", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Pusher />
      </ToastProvider>,
    );
    act(() => screen.getByText("push").click());
    expect(screen.getByText("Saved")).toBeTruthy();
    act(() => {
      vi.advanceTimersByTime(4000);
    });
    expect(screen.queryByText("Saved")).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });

  // A timer left running after unmount fired during a later test, or after
  // the test environment was gone, and failed the whole run.
  it("cancels pending timers when the provider unmounts", () => {
    vi.useFakeTimers();
    const view = render(
      <ToastProvider>
        <Pusher />
      </ToastProvider>,
    );
    act(() => screen.getByText("push").click());
    act(() => screen.getByText("push").click());
    expect(vi.getTimerCount()).toBe(2);
    view.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("cancels the timer of a toast dismissed by hand", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Pusher />
      </ToastProvider>,
    );
    act(() => screen.getByText("push").click());
    act(() => screen.getByLabelText("Dismiss notification").click());
    expect(screen.queryByText("Saved")).toBeNull();
    expect(vi.getTimerCount()).toBe(0);
  });
});
