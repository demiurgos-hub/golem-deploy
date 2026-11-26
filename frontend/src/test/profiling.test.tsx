import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import type { Server } from "@/api/types.gen";
import { checkServerImport } from "@/lib/server-import";
import { account, errorResponse, job, jsonResponse, streamResponse, type Handler } from "@/test/fake-api";
import { buildFixture, inspectorOff, projectFixture, runtimeFixture, serverFixture } from "@/test/fixtures";
import { renderApp } from "@/test/render-app";

// Profiling (docs/API.md, "Profiling"): the server's switch, its state and the capture download.

const developer = account({ id: 2, role: "developer", username: "dev" });
const FILE = "echo-cpu-20261126T101500Z.pb.gz";

function serverRoutes(s: Server, extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    "GET /servers/{id}": () => jsonResponse(200, s),
    "GET /inspector": () => jsonResponse(200, inspectorOff()),
    "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
    "GET /jobs": () => jsonResponse(200, { items: [] }),
    "GET /jobs/{id}/log": () => streamResponse().response,
    "GET /servers/{id}/stats": () => errorResponse(409, "conflict", "the server is not running"),
    "GET /servers/{id}/runtime": () => jsonResponse(200, runtimeFixture()),
    "GET /servers/{id}/logs": () => streamResponse().response,
    "GET /builds": () => jsonResponse(200, { items: [buildFixture()] }),
    ...extra,
  };
}

function profileFile(name = FILE): Response {
  return new Response(new Blob([new Uint8Array([0x1f, 0x8b, 0x08, 0x00])]), {
    status: 200,
    headers: { "Content-Type": "application/octet-stream", "Content-Disposition": `attachment; filename=${name}`, "X-Request-Id": "req-p" },
  });
}

async function section() {
  return screen.findByRole("region", { name: "Profiling" });
}

/** Captures the names saveBlob gives to downloads. */
function catchDownloads() {
  const names: string[] = [];
  vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
    names.push(this.download);
  });
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => "blob:profile");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  return names;
}

afterEach(() => vi.restoreAllMocks());

describe("server page: Profiling", () => {
  it("turns the switch off with a PATCH and says that captures are refused until a restart drops it from the game", async () => {
    let current = serverFixture();
    const restartJob = job({ id: 97, kind: "server.restart", subject_type: "server", subject_id: "3", status: "running" });
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(current, {
        "GET /servers/{id}": () => jsonResponse(200, current),
        "PATCH /servers/{id}": (req) => {
          current = serverFixture({ profiling: (req.body as { profiling: boolean }).profiling, restart_required: true });
          return jsonResponse(200, current);
        },
        "POST /servers/{id}/restart": () => jsonResponse(202, restartJob),
        "GET /jobs/{id}": () => jsonResponse(200, restartJob),
      }),
    });
    const sec = await section();
    const toggle = within(sec).getByRole("switch", { name: "Allow profiling of this server" });
    expect(toggle).toBeChecked();
    expect(toggle).toHaveAccessibleDescription("Takes effect at the next start or restart.");
    expect(within(sec).getByText("The running game server serves profiles.")).toBeInTheDocument();
    expect(within(sec).getByRole("button", { name: "Capture" })).toBeEnabled();
    expect(within(sec).getByText("go tool pprof -http=: <file>")).toBeInTheDocument();
    expect(within(sec).getByText("go tool trace <file>")).toBeInTheDocument();

    await user.click(toggle);
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH").map((c) => c.body)).toEqual([{ profiling: false }]));
    expect(await screen.findByText("Profiling switch turned off. Captures are refused now; a restart also turns it off in the game server.")).toBeInTheDocument();
    expect(await within(sec).findByText(/The switch is off, so captures are refused now\. The running game server was started with profiling on/)).toBeInTheDocument();
    expect(within(sec).getByRole("button", { name: "Capture" })).toBeDisabled();
    await user.click(within(sec).getByRole("button", { name: "Restart echo" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/servers/3/restart")).toBe(true));
  });

  it.each([
    ["the switch is off", serverFixture({ profiling: false, profiling_active: false }), "Profiling is off for this server.", false],
    ["the server is not running", serverFixture({ state: "exited", desired_state: "stopped", started_at: null }), "The server is not running. Captures need a running server.", false],
    ["the state is unknown", serverFixture({ state: "unknown", profiling_active: false }), "Docker cannot be queried, so the state of this server is not known.", false],
    [
      "the container was started without profiling",
      serverFixture({ profiling_active: false, restart_required: true }),
      "The running game server was started without profiling. Restart the server to apply the switch.",
      true,
    ],
  ])("disables the capture form when %s and says why", async (_name, s, text, restart) => {
    const { calls } = renderApp({ path: "/servers/3", signedInAs: developer, routes: serverRoutes(s) });
    const sec = await section();
    expect(within(sec).getByText(text, { exact: false })).toBeInTheDocument();
    expect(within(sec).getByRole("button", { name: "Capture" })).toBeDisabled();
    expect(within(sec).getByLabelText("Profile")).toBeDisabled();
    expect(within(sec).queryByRole("button", { name: "Restart echo" }) !== null).toBe(restart);
    expect(calls.some((c) => c.path.endsWith("/profile"))).toBe(false);
  });

  it("asks for seconds only for CPU and trace, with their ranges and defaults, and checks them before sending", async () => {
    const { user, calls } = renderApp({ path: "/servers/3", signedInAs: developer, routes: serverRoutes(serverFixture()) });
    const sec = await section();
    const kind = within(sec).getByLabelText("Profile");
    expect([...(kind as HTMLSelectElement).options].map((o) => o.text)).toEqual(["CPU", "Execution trace", "Heap", "Allocations", "Goroutines"]);
    expect(within(sec).getByLabelText("Seconds (1 to 60)")).toHaveValue("30");
    for (const k of ["heap", "allocs", "goroutine"]) {
      await user.selectOptions(kind, k);
      expect(within(sec).queryByLabelText(/^Seconds/)).toBeNull();
    }
    await user.selectOptions(kind, "trace");
    const seconds = within(sec).getByLabelText("Seconds (1 to 15)");
    expect(seconds).toHaveValue("5");
    await user.clear(seconds);
    await user.type(seconds, "16");
    await user.click(within(sec).getByRole("button", { name: "Capture" }));
    expect(await within(sec).findByText("Enter a whole number of seconds from 1 to 15.")).toBeInTheDocument();
    expect(seconds).toHaveAttribute("aria-invalid", "true");
    expect(calls.some((c) => c.path.endsWith("/profile"))).toBe(false);
  });

  it("captures a timed profile with a countdown and saves it under the daemon's file name", async () => {
    const names = catchDownloads();
    let answer!: (r: Response) => void;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture(), { "GET /servers/{id}/profile": () => new Promise<Response>((r) => (answer = r)) }),
    });
    const sec = await section();
    const seconds = within(sec).getByLabelText("Seconds (1 to 60)");
    await user.clear(seconds);
    await user.type(seconds, "20");
    await user.click(within(sec).getByRole("button", { name: "Capture" }));
    expect(await within(sec).findByText(/^Capturing… (20|19) s left$/)).toBeInTheDocument();
    expect(within(sec).getByRole("button", { name: "Capture" })).toBeDisabled();
    const req = calls.find((c) => c.path === "/servers/3/profile");
    expect(req?.query.toString()).toBe("kind=cpu&seconds=20");
    expect(req?.headers.Authorization).toMatch(/^Bearer /);
    answer(profileFile());
    await waitFor(() => expect(names).toEqual([FILE]));
    expect(await screen.findByText(`Profile saved as ${FILE}.`)).toBeInTheDocument();
    await waitFor(() => expect(within(sec).getByRole("button", { name: "Capture" })).toBeEnabled());
    expect(within(sec).queryByText(/^Capturing/)).toBeNull();
  });

  it("sends no seconds for a heap profile", async () => {
    const names = catchDownloads();
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture(), { "GET /servers/{id}/profile": () => profileFile("echo-heap-20261126T101500Z.pb.gz") }),
    });
    const sec = await section();
    await user.selectOptions(within(sec).getByLabelText("Profile"), "heap");
    await user.click(within(sec).getByRole("button", { name: "Capture" }));
    await waitFor(() => expect(names).toEqual(["echo-heap-20261126T101500Z.pb.gz"]));
    expect(calls.find((c) => c.path === "/servers/3/profile")?.query.toString()).toBe("kind=heap");
  });

  it.each([
    ["profiling_disabled", "profiling is off for this server, its running container was started without it (restart the server), or its game's engine does not serve profiles"],
    ["profile_in_progress", "a profile of this server is already being captured; wait for it to finish"],
  ] as const)("shows a %s refusal as the daemon worded it, with the request id", async (code, message) => {
    const names = catchDownloads();
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture(), {
        "GET /servers/{id}/profile": () => errorResponse(409, code, message, { requestId: "req-409" }),
      }),
    });
    const sec = await section();
    await user.selectOptions(within(sec).getByLabelText("Profile"), "goroutine");
    await user.click(within(sec).getByRole("button", { name: "Capture" }));
    const alert = await within(sec).findByRole("alert");
    expect(alert).toHaveTextContent(message.charAt(0).toUpperCase() + message.slice(1));
    expect(alert).toHaveTextContent("Request id: req-409");
    expect(names).toEqual([]);
    expect(within(sec).getByRole("button", { name: "Capture" })).toBeEnabled();
  });

  it("puts the daemon's seconds error on the field", async () => {
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture(), {
        "GET /servers/{id}/profile": () => errorResponse(400, "validation_failed", "seconds must be from 1 to 60 for cpu", { field: "seconds" }),
      }),
    });
    const sec = await section();
    await user.click(within(sec).getByRole("button", { name: "Capture" }));
    expect(await within(sec).findByText("Seconds must be from 1 to 60 for cpu.")).toBeInTheDocument();
    expect(within(sec).getByLabelText("Seconds (1 to 60)")).toHaveAttribute("aria-invalid", "true");
  });
});

describe("the Profiling switch on create and import", () => {
  it("is on by default and sends profiling: false only when it is turned off", async () => {
    const { user, calls } = renderApp({
      path: "/servers",
      signedInAs: developer,
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
        "GET /builds": () => jsonResponse(200, { items: [buildFixture()] }),
        "POST /servers": () => jsonResponse(201, serverFixture({ profiling: false, profiling_active: false, state: "missing" })),
        "GET /servers/{id}": () => jsonResponse(200, serverFixture({ profiling: false, profiling_active: false, state: "missing" })),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New server" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    await user.type(await within(dialog).findByLabelText("Name"), "echo");
    const box = within(dialog).getByRole("checkbox", { name: "Allow profiling" });
    expect(box).toBeChecked();
    expect(box).toHaveAccessibleDescription(/Takes effect at the next start or restart\./);
    await user.click(box);
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.path === "/servers")?.body).toEqual({ name: "echo", build_id: 12, desired_state: "stopped", profiling: false }),
    );
  });

  it("accepts the optional boolean profiling in an import, null meaning absent like the daemon's", () => {
    const doc = (spec: Record<string, unknown>) => JSON.stringify({ format_version: 2, servers: [{ name: "a", project: "arena", ...spec }] });
    for (const v of [true, false, null, undefined]) expect(checkServerImport(doc({ profiling: v })).errors).toEqual([]);
    expect(checkServerImport(doc({ profiling: "no" })).errors).toEqual(["Server 1 (a): profiling must be true or false."]);
  });
});
