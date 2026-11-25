import { afterEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { invalidateJobSubject } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { Inspector, InspectorSession, Server } from "@/api/types.gen";
import { jobKindLabel, jobPermissions } from "@/components/jobs";
import { inspectorAddress, inspectorEntryUrl } from "@/lib/inspector";
import { checkServerImport } from "@/lib/server-import";
import { account, errorResponse, job, jsonResponse, streamResponse, type Handler } from "@/test/fake-api";
import { INSPECTOR_IMAGE_ID, buildFixture, inspectorFixture, inspectorOff, projectFixture, runtimeFixture, serverFixture, targetFixture } from "@/test/fixtures";
import { renderApp } from "@/test/render-app";

// The Inspector (docs/API.md, "Inspector"): the Settings section, the server's switch and state, and opening it.

const admin = account();
const developer = account({ id: 2, role: "developer", username: "dev" });
const PAGE = new URL(window.location.href);
const ADDRESS = `${PAGE.protocol}//${PAGE.hostname}:7700`;

function session(overrides: Partial<InspectorSession> = {}): InspectorSession {
  return { path: "/_golem-deploy/enter#t=abc", port: 7700, public_url: "", expires_at: new Date(Date.now() + 60_000).toISOString(), ...overrides };
}

/** A stand-in for the blank tab window.open returns. */
function fakeTab() {
  return {
    opener: {} as unknown,
    closed: false,
    document: { title: "", body: { textContent: "" } },
    location: { replace: vi.fn() },
    close: vi.fn(),
  };
}

function settingsRoutes(inspector: Inspector, extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    "GET /inspector": () => jsonResponse(200, inspector),
    "GET /settings": () => jsonResponse(200, { game_host: "", effective_game_host: "localhost", registration_enabled: false }),
    "GET /certificates/game": () => jsonResponse(200, { mode: "self_signed", game_host: "localhost", certificate: null }),
    "GET /certificates/panel": () =>
      jsonResponse(200, { mode: "none", configurable: false, hosts: [], certificate: null, covers_hosts: false }),
    "GET /certificates/acme": () =>
      jsonResponse(200, {
        email: "", staging: false, terms_agreed: false, directory_url: "https://acme-v02.api.letsencrypt.org/directory",
        http01_available: false, tls_alpn01_available: false, certificates: [],
      }),
    "GET /jobs": () => jsonResponse(200, { items: [] }),
    "GET /jobs/{id}/log": () => streamResponse().response,
    "GET /servers": () =>
      jsonResponse(200, {
        items: [
          serverFixture({ id: 3, name: "echo", inspector: true, inspector_active: true }),
          serverFixture({ id: 4, name: "kart", port: 7811, inspector: true, inspector_active: true }),
          serverFixture({ id: 5, name: "plain", port: 7812 }),
        ],
      }),
    ...extra,
  };
}

async function settingsSection() {
  return screen.findByRole("region", { name: "Inspector" });
}

function patches(calls: { method: string; path: string; body: unknown }[]) {
  return calls.filter((c) => c.method === "PATCH" && c.path === "/inspector").map((c) => c.body);
}

describe("Settings: the Inspector section", () => {
  it("explains the Inspector while it is off and turns it on with a job shown inline", async () => {
    const applyJob = job({ id: 90, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status: "running", restricted: true });
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorOff(), {
        "PATCH /inspector": () => jsonResponse(202, applyJob),
        "GET /jobs/{id}": () => jsonResponse(200, applyJob),
      }),
    });
    const section = await settingsSection();
    expect(within(section).getByText(/shows the live entities of running game servers/)).toBeInTheDocument();
    expect(await within(section).findByText("Off")).toBeInTheDocument();
    expect(within(section).getByText("The Inspector is off. No game server is connected to it.")).toBeInTheDocument();
    expect(within(section).getByText(/opens TCP port 7700/)).toBeInTheDocument();
    expect(within(section).queryByRole("button", { name: /Open Inspector/ })).toBeNull();
    expect(within(section).queryByText("More options")).toBeNull();
    await user.click(within(section).getByRole("button", { name: "Turn on" }));
    await waitFor(() => expect(patches(calls)).toEqual([{ enabled: true }]));
    const inline = await within(section).findByLabelText("Job 90");
    expect(within(inline).getByText("Update the Inspector")).toBeInTheDocument();
    // The job is known as active at once: no second change can be started.
    expect(within(section).getByRole("button", { name: "Turn on" })).toBeDisabled();
    expect(calls.some((c) => c.path === "/jobs" && c.query.get("subject_type") === "inspector" && c.query.get("subject_id") === "hub")).toBe(true);
  });

  it("refreshes the Inspector and the servers when the job ends", async () => {
    let status: "running" | "success" = "running";
    const applyJob = () => job({ id: 91, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status, restricted: true });
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorOff(), {
        "PATCH /inspector": () => jsonResponse(202, applyJob()),
        "GET /jobs/{id}": () => jsonResponse(200, applyJob()),
      }),
    });
    const section = await settingsSection();
    await user.click(await within(section).findByRole("button", { name: "Turn on" }));
    await within(section).findByLabelText("Job 91");
    const before = calls.filter((c) => c.path === "/inspector" && c.method === "GET").length;
    status = "success";
    await waitFor(() => expect(calls.filter((c) => c.path === "/inspector" && c.method === "GET").length).toBeGreaterThan(before), { timeout: 8000 });
  }, 15_000);

  it("marks the Inspector and the servers stale when an inspector.apply job ends", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(qk.inspector, inspectorOff());
    queryClient.setQueryData(qk.servers.list, { items: [] });
    queryClient.setQueryData(qk.certificates.game, {});
    invalidateJobSubject(queryClient, job({ kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status: "success" }));
    expect(queryClient.getQueryState(qk.inspector)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(qk.servers.list)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(qk.certificates.game)?.isInvalidated).toBe(false);
  });

  it("shows what a running Inspector does, with the firewall note and the connected servers", async () => {
    renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(
        inspectorFixture({ targets: [targetFixture({ server_id: 3 }), targetFixture({ server_id: 4, connected: false, connected_at: null })], other_targets: 2 }),
      ),
    });
    const section = await settingsSection();
    expect(await within(section).findByText("Running")).toBeInTheDocument();
    expect(within(section).getByText("ghcr.io/demiurgos-hub/golem-inspector:latest")).toBeInTheDocument();
    expect(within(section).getByText(INSPECTOR_IMAGE_ID.slice(7, 19))).toHaveAttribute("title", INSPECTOR_IMAGE_ID);
    expect(within(section).getByText("TCP 7700")).toBeInTheDocument();
    expect(within(section).getByText(/A host firewall has to allow this TCP port\. .*game servers connect to it too\./)).toBeInTheDocument();
    expect(within(section).getByText(ADDRESS)).toBeInTheDocument();
    expect(within(section).getByText("172.18.0.1:7700")).toBeInTheDocument();
    expect(await within(section).findByText("1 of 2 servers with the Inspector switch on is connected.")).toBeInTheDocument();
    expect(within(section).getByText(/also knows 2 game servers that are not a server of this installation/)).toBeInTheDocument();
    expect(within(section).queryByText("Game servers cannot reach the Inspector")).toBeNull();
    expect(within(section).getByRole("button", { name: /Open Inspector/ })).toBeInTheDocument();
    expect(within(section).getByRole("button", { name: "Turn off" })).toBeEnabled();
  });

  it("says why servers do not appear when the listener is on a loopback address, and when the Inspector did not answer", async () => {
    renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorFixture({ agents_reachable: false, agent_address: "", targets: null })),
    });
    const section = await settingsSection();
    expect(await within(section).findByText("Game servers cannot reach the Inspector")).toBeInTheDocument();
    expect(within(section).getByText(/listens on a loopback address/)).toBeInTheDocument();
    expect(within(section).getByText("Nothing: see the warning below.")).toBeInTheDocument();
    expect(within(section).getByText("Unknown: the Inspector did not answer.")).toBeInTheDocument();
  });

  it("shows the daemon's reason while the Inspector is on but not running, and tries again", async () => {
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorFixture({ state: "stopped", detail: "the Inspector image could not be downloaded", started_at: null, targets: null }), {
        "PATCH /inspector": () => jsonResponse(202, job({ id: 92, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", restricted: true })),
        "GET /jobs/{id}": () => jsonResponse(200, job({ id: 92, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", restricted: true })),
      }),
    });
    const section = await settingsSection();
    expect(await within(section).findByText("Not running")).toBeInTheDocument();
    expect(within(section).getByText("the Inspector image could not be downloaded")).toBeInTheDocument();
    expect(within(section).queryByRole("button", { name: /Open Inspector/ })).toBeNull();
    expect(within(section).getByText("None while the Inspector is not running.")).toBeInTheDocument();
    await user.click(within(section).getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(patches(calls)).toEqual([{}]));
  });

  it("says the state is unknown while Docker cannot be queried", async () => {
    renderApp({ path: "/admin/settings", signedInAs: admin, routes: settingsRoutes(inspectorFixture({ state: "unknown", started_at: null, targets: null })) });
    const section = await settingsSection();
    expect(await within(section).findByText("Unknown")).toBeInTheDocument();
    expect(within(section).getByText(/Docker cannot be queried, so the state of the Inspector is not known\. It is turned on\./)).toBeInTheDocument();
  });

  it("offers no controls when the daemon runs without the Inspector listener", async () => {
    renderApp({ path: "/admin/settings", signedInAs: admin, routes: settingsRoutes(inspectorOff({ available: false, port: 0 })) });
    const section = await settingsSection();
    expect(await within(section).findByText("Not available on this host")).toBeInTheDocument();
    expect(within(section).getByText(/started with the Inspector listener turned off/)).toBeInTheDocument();
    expect(within(section).queryAllByRole("button")).toHaveLength(0);
  });

  it("asks before turning off and says that open Inspector tabs stop working", async () => {
    const applyJob = job({ id: 93, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status: "running", restricted: true });
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorFixture(), { "PATCH /inspector": () => jsonResponse(202, applyJob), "GET /jobs/{id}": () => jsonResponse(200, applyJob) }),
    });
    const section = await settingsSection();
    await user.click(await within(section).findByRole("button", { name: "Turn off" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Turn off the Inspector" });
    expect(within(dialog).getByText(/Inspector tabs that are open stop working/)).toBeInTheDocument();
    expect(patches(calls)).toEqual([]);
    await user.click(within(dialog).getByRole("button", { name: "Turn off" }));
    await waitFor(() => expect(patches(calls)).toEqual([{ enabled: false }]));
    expect(await within(section).findByLabelText("Job 93")).toBeInTheDocument();
  });

  it("downloads the image again and replaces the secrets after a confirmation, under More options", async () => {
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorFixture(), {
        "PATCH /inspector": (req) =>
          (req.body as { pull_image?: boolean }).pull_image
            ? errorResponse(409, "job_in_progress", "another Inspector job is already running")
            : jsonResponse(202, job({ id: 94, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status: "running", restricted: true })),
        "GET /jobs/{id}": () => jsonResponse(200, job({ id: 94, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status: "running", restricted: true })),
      }),
    });
    const section = await settingsSection();
    await user.click(await within(section).findByText("More options"));
    await user.click(within(section).getByRole("button", { name: "Download the image again" }));
    await waitFor(() => expect(patches(calls)).toEqual([{ pull_image: true }]));
    // job_in_progress as the daemon worded it.
    expect(await within(section).findByText("Another Inspector job is already running.")).toBeInTheDocument();

    await user.click(within(section).getByRole("button", { name: "Replace secrets" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Replace the Inspector's secrets" });
    expect(within(dialog).getByText(/Open Inspector sessions end/)).toBeInTheDocument();
    expect(within(dialog).getByText(/Every server with the Inspector switch on\s+needs a restart to connect again/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Replace secrets" }));
    await waitFor(() => expect(patches(calls)).toEqual([{ pull_image: true }, { rotate_secrets: true }]));
  });

  it("shows a job that is already running and disables the actions until it ends", async () => {
    const running = job({ id: 95, kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", status: "running", restricted: true, created_by: 9, created_by_username: "bob" });
    renderApp({
      path: "/admin/settings",
      signedInAs: admin,
      routes: settingsRoutes(inspectorFixture(), {
        "GET /jobs": (req) => jsonResponse(200, { items: req.query.get("subject_type") === "inspector" ? [running] : [] }),
        "GET /jobs/{id}": () => jsonResponse(200, running),
      }),
    });
    const section = await settingsSection();
    const inline = await within(section).findByLabelText("Job 95");
    expect(within(inline).getByText(/started by bob/)).toBeInTheDocument();
    expect(within(section).getByRole("button", { name: "Turn off" })).toBeDisabled();
    expect(within(section).getByRole("button", { name: "Turn off" })).toHaveAttribute("title", "Wait for the running Inspector job to finish.");
  });
});

function serverRoutes(s: Server, inspector: Inspector, extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    "GET /servers/{id}": () => jsonResponse(200, s),
    "GET /inspector": () => jsonResponse(200, inspector),
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

async function serverSection() {
  const sections = await screen.findAllByRole("region", { name: "Inspector" });
  return sections[0];
}

describe("server page: the Inspector", () => {
  it("turns the server's switch on and off with a PATCH, and says nothing more while it is off", async () => {
    let current = serverFixture();
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(current, inspectorFixture(), {
        "GET /servers/{id}": () => jsonResponse(200, current),
        "PATCH /servers/{id}": (req) => {
          current = serverFixture({ inspector: (req.body as { inspector: boolean }).inspector, restart_required: true });
          return jsonResponse(200, current);
        },
      }),
    });
    const section = await serverSection();
    const toggle = within(section).getByRole("switch", { name: "Connect this server to the Inspector" });
    expect(toggle).not.toBeChecked();
    expect(within(section).getByText(/Takes effect at the next start or restart\. It needs the Inspector of this installation/)).toBeInTheDocument();
    expect(within(section).queryByRole("status")).toBeNull();
    await user.click(toggle);
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH").map((c) => c.body)).toEqual([{ inspector: true }]));
    expect(await screen.findByText("Inspector switch turned on. It takes effect at the next start or restart.")).toBeInTheDocument();
    await waitFor(() => expect(within(section).getByRole("switch")).toBeChecked());
    await user.click(within(section).getByRole("switch"));
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH").map((c) => c.body)).toEqual([{ inspector: true }, { inspector: false }]));
  });

  it("says that the installation's Inspector is off, with the way to Settings for admins", async () => {
    const s = serverFixture({ inspector: true });
    renderApp({ path: "/servers/3", signedInAs: developer, routes: serverRoutes(s, inspectorOff()) });
    const section = await serverSection();
    expect(await within(section).findByText(/The Inspector of this installation is off, so this server cannot connect to it\./)).toBeInTheDocument();
    expect(within(section).getByText(/An administrator turns it on in Settings\./)).toBeInTheDocument();
  });

  it("links admins to the Settings section", async () => {
    renderApp({ path: "/servers/3", signedInAs: admin, routes: serverRoutes(serverFixture({ inspector: true }), inspectorOff()) });
    const section = await serverSection();
    expect(await within(section).findByRole("link", { name: "Settings" })).toHaveAttribute("href", "/admin/settings#inspector");
  });

  it("says that the installation's Inspector is not running, with the daemon's reason", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture({ inspector: true, inspector_active: true }), inspectorFixture({ state: "stopped", detail: "the Inspector container exited", targets: null })),
    });
    const section = await serverSection();
    expect(await within(section).findByText(/turned on but is not running/)).toBeInTheDocument();
    expect(within(section).getByText("the Inspector container exited")).toBeInTheDocument();
  });

  it("waits for the next start of a server that is not running", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture({ inspector: true, state: "exited", desired_state: "stopped", started_at: null }), inspectorFixture()),
    });
    const section = await serverSection();
    expect(await within(section).findByText("The server is not running. It connects to the Inspector when it starts.")).toBeInTheDocument();
  });

  it("offers a restart when the running container was started without the Inspector", async () => {
    const restartJob = job({ id: 96, kind: "server.restart", subject_type: "server", subject_id: "3", status: "running" });
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture({ inspector: true, inspector_active: false, restart_required: true }), inspectorFixture(), {
        "POST /servers/{id}/restart": () => jsonResponse(202, restartJob),
        "GET /jobs/{id}": () => jsonResponse(200, restartJob),
      }),
    });
    const section = await serverSection();
    expect(await within(section).findByText("The running game server was started without the Inspector. It connects at the next restart.")).toBeInTheDocument();
    await user.click(within(section).getByRole("button", { name: "Restart echo" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/servers/3/restart")).toBe(true));
    expect(await screen.findByLabelText("Job 96")).toBeInTheDocument();
  });

  it("asks for a restart after the Inspector settings changed (secrets replaced)", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture({ inspector: true, inspector_active: true, restart_required: true }), inspectorFixture({ targets: [] })),
    });
    const section = await serverSection();
    expect(await within(section).findByText("The Inspector settings changed since the game server started. It connects at the next restart.")).toBeInTheDocument();
    expect(within(section).getByRole("button", { name: "Restart echo" })).toBeInTheDocument();
  });

  it("shows a connected server with Open in Inspector", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture({ inspector: true, inspector_active: true }), inspectorFixture({ targets: [targetFixture()] })),
    });
    const section = await serverSection();
    expect(await within(section).findByText("Connected")).toBeInTheDocument();
    expect(within(section).getByRole("button", { name: /Open in Inspector/ })).toBeInTheDocument();
  });

  it("says that the server has not connected yet, with the firewall and engine hints", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(
        serverFixture({ inspector: true, inspector_active: true }),
        inspectorFixture({ targets: [targetFixture({ connected: false, connected_at: null })] }),
      ),
    });
    const section = await serverSection();
    expect(await within(section).findByText("Not connected yet")).toBeInTheDocument();
    expect(within(section).getByText(/a host firewall has to allow TCP port 7700 from Docker's networks/)).toBeInTheDocument();
    expect(within(section).getByText(/Golem Engine version with Inspector support/)).toBeInTheDocument();
    expect(within(section).queryByRole("button", { name: /Open in Inspector/ })).toBeNull();
  });

  it("says that the state is unknown when the Inspector did not answer", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverRoutes(serverFixture({ inspector: true, inspector_active: true }), inspectorFixture({ targets: null })),
    });
    const section = await serverSection();
    expect(await within(section).findByText("The Inspector did not answer, so it is not known whether this server is connected.")).toBeInTheDocument();
  });
});

describe("opening the Inspector", () => {
  afterEach(() => vi.restoreAllMocks());

  const connected = () => serverRoutes(serverFixture({ inspector: true, inspector_active: true }), inspectorFixture({ targets: [targetFixture()] }));

  it("opens the tab during the click, cuts it off from this page and navigates it to the one-time link", async () => {
    const tab = fakeTab();
    const open = vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    let answer!: (r: Response) => void;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: { ...connected(), "POST /inspector/sessions": () => new Promise<Response>((r) => (answer = r)) },
    });
    const section = await serverSection();
    await user.click(await within(section).findByRole("button", { name: /Open in Inspector/ }));
    expect(open).toHaveBeenCalledWith("about:blank", "_blank");
    expect(tab.opener).toBeNull();
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/inspector/sessions")?.body).toEqual({ server_id: 3 }));
    expect(tab.location.replace).not.toHaveBeenCalled();
    answer(jsonResponse(200, session()));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith(`${ADDRESS}/_golem-deploy/enter#t=abc`));
    expect(tab.close).not.toHaveBeenCalled();
    // The link is nowhere on the page.
    expect(document.body.innerHTML).not.toContain("#t=abc");
  });

  it("uses the public address when the daemon has one", async () => {
    const tab = fakeTab();
    vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: { ...connected(), "POST /inspector/sessions": () => jsonResponse(200, session({ public_url: "https://inspector.example.com" })) },
    });
    await user.click(await within(await serverSection()).findByRole("button", { name: /Open in Inspector/ }));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith("https://inspector.example.com/_golem-deploy/enter#t=abc"));
  });

  it("closes the tab again and says why when the request fails", async () => {
    const tab = fakeTab();
    vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: { ...connected(), "POST /inspector/sessions": () => errorResponse(409, "conflict", "the Inspector is not running") },
    });
    await user.click(await within(await serverSection()).findByRole("button", { name: /Open in Inspector/ }));
    expect(await screen.findByText("Could not open the Inspector: The Inspector is not running.")).toBeInTheDocument();
    expect(tab.close).toHaveBeenCalled();
    expect(tab.location.replace).not.toHaveBeenCalled();
  });

  it("offers the link to click when the browser blocked the new tab", async () => {
    vi.spyOn(window, "open").mockReturnValue(null);
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: { ...connected(), "POST /inspector/sessions": () => jsonResponse(200, session()) },
    });
    await user.click(await within(await serverSection()).findByRole("button", { name: /Open in Inspector/ }));
    const dialog = await screen.findByRole("dialog", { name: "Open the Inspector" });
    expect(within(dialog).getByText("Use this link instead. It works once, within a minute.")).toBeInTheDocument();
    const link = within(dialog).getByRole("link", { name: /Open the Inspector/ });
    expect(link).toHaveAttribute("href", `${ADDRESS}/_golem-deploy/enter#t=abc`);
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    link.addEventListener("click", (e) => e.preventDefault());
    await user.click(link);
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Open the Inspector" })).toBeNull());
  });

  it("offers Open Inspector on the Servers page to every account while it runs, and not otherwise", async () => {
    const tab = fakeTab();
    vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    const { user, calls } = renderApp({
      path: "/servers",
      signedInAs: developer,
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [serverFixture()] }),
        "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
        "GET /inspector": () => jsonResponse(200, inspectorFixture()),
        "POST /inspector/sessions": () => jsonResponse(200, session()),
      },
    });
    await user.click(await screen.findByRole("button", { name: /Open Inspector/ }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/inspector/sessions")?.body).toEqual({}));
    await waitFor(() => expect(tab.location.replace).toHaveBeenCalled());
  });

  it("hides Open Inspector on the Servers page while the Inspector is off", async () => {
    renderApp({
      path: "/servers",
      signedInAs: developer,
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [serverFixture()] }),
        "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
      },
    });
    await screen.findByRole("table", { name: "Servers" });
    await waitFor(() => expect(screen.getByRole("button", { name: "New server" })).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: /Open Inspector/ })).toBeNull();
  });
});

describe("the Inspector switch on create and import", () => {
  it("sends inspector on create only when it is ticked", async () => {
    const { user, calls } = renderApp({
      path: "/servers",
      signedInAs: developer,
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
        "GET /builds": () => jsonResponse(200, { items: [buildFixture()] }),
        "POST /servers": () => jsonResponse(201, serverFixture({ inspector: true, state: "missing" })),
        "GET /servers/{id}": () => jsonResponse(200, serverFixture({ inspector: true, state: "missing" })),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New server" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    await user.type(await within(dialog).findByLabelText("Name"), "echo");
    const box = within(dialog).getByRole("checkbox", { name: "Connect to the Inspector" });
    expect(box).toHaveAccessibleDescription(/Takes effect at the next start or restart\./);
    await user.click(box);
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST" && c.path === "/servers")?.body).toEqual({ name: "echo", build_id: 12, desired_state: "stopped", inspector: true }),
    );
  });

  it("accepts the optional boolean inspector in an import", () => {
    const doc = (spec: Record<string, unknown>) => JSON.stringify({ format_version: 2, servers: [{ name: "a", project: "arena", ...spec }] });
    expect(checkServerImport(doc({ inspector: true })).errors).toEqual([]);
    expect(checkServerImport(doc({ inspector: false })).errors).toEqual([]);
    expect(checkServerImport(doc({})).errors).toEqual([]);
    expect(checkServerImport(doc({ inspector: "yes" })).errors).toEqual(["Server 1 (a): inspector must be true or false."]);
    expect(checkServerImport(doc({ inspector: null })).errors).toEqual(["Server 1 (a): inspector must be true or false."]);
  });
});

describe("Inspector helpers and jobs", () => {
  it("labels the job kind in words and keeps its log and cancel for admins", () => {
    expect(jobKindLabel("inspector.apply")).toBe("Update the Inspector");
    const j = job({ kind: "inspector.apply", subject_type: "inspector", subject_id: "hub", restricted: true, created_by: 2 });
    expect(jobPermissions(j, { id: 2, role: "developer" })).toMatchObject({ canReadLog: false, canCancel: false });
    expect(jobPermissions(j, { id: 1, role: "admin" })).toMatchObject({ canReadLog: true, canCancel: true });
  });

  it("builds the Inspector's address and entry link like the contract says", () => {
    const loc = { protocol: "https:", hostname: "[::1]" };
    expect(inspectorAddress("", 7700, loc)).toBe("https://[::1]:7700");
    expect(inspectorAddress("https://inspector.example.com/", 7700, loc)).toBe("https://inspector.example.com");
    expect(inspectorAddress("javascript:alert(1)", 7700, loc)).toBeNull();
    expect(inspectorEntryUrl(session(), { protocol: "http:", hostname: "10.0.0.5" })).toBe("http://10.0.0.5:7700/_golem-deploy/enter#t=abc");
    expect(inspectorEntryUrl(session({ path: "//evil.example.com/x" }), loc)).toBeNull();
    expect(inspectorEntryUrl(session({ path: "https://evil.example.com/x" }), loc)).toBeNull();
    expect(inspectorEntryUrl(session({ path: "/\\evil.example.com" }), loc)).toBeNull();
    // The ticket is in the fragment and stays there, with every base64url character.
    const tok = "A-b_".repeat(10) + "xyz";
    expect(inspectorEntryUrl(session({ path: `/_golem-deploy/enter#t=${tok}`, public_url: "https://inspector.example.com" }), loc)).toBe(
      `https://inspector.example.com/_golem-deploy/enter#t=${tok}`,
    );
  });
});
