import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import type { Build, GameCertificate, Server, StaticFileList } from "@/api/types.gen";
import { account, errorResponse, job, jsonResponse, streamResponse, type FakeRequest, type Handler } from "@/test/fake-api";
import { COMMIT, buildFixture, certFixture, imageFixture, projectFixture, runtimeFixture, serverFixture } from "@/test/fixtures";
import { renderApp } from "@/test/render-app";

// The milestone 3 contract (docs/API.md): builds, servers that run builds,
// the runtime view, the game certificate and the rules around them.

const developer = account({ id: 2, role: "developer", username: "dev" });
const project = projectFixture();
const OTHER_COMMIT = "0d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e";

function projectRoutes(builds: Build[], extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    "GET /projects/{id}": () => jsonResponse(200, project),
    "GET /projects/{id}/builds": () => jsonResponse(200, { items: builds }),
    "GET /jobs": () => jsonResponse(200, { items: [] }),
    "GET /servers": () => jsonResponse(200, { items: [serverFixture()] }),
    ...extra,
  };
}

describe("project builds", () => {
  it("starts a build with a pull and follows its job inline", async () => {
    const buildJob = job({ id: 300, kind: "project.build", status: "running", started_at: "2026-11-20T10:00:00Z" });
    const log = streamResponse();
    const { user, calls } = renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: projectRoutes([], {
        "POST /projects/{id}/builds": () => jsonResponse(202, { build: buildFixture({ status: "queued", commit: "", job_id: 300 }), job: buildJob }),
        "GET /jobs/{id}": () => jsonResponse(200, buildJob),
        "GET /jobs/{id}/log": () => log.response,
      }),
    });
    expect(await screen.findByText("No builds yet")).toBeInTheDocument();
    await user.click(screen.getByLabelText("Pull first"));
    await user.click(screen.getByRole("button", { name: "Start build" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/projects/5/builds")?.body).toEqual({ pull: true }));
    const inline = await screen.findByLabelText("Job 300");
    expect(within(inline).getByText("Build")).toBeInTheDocument();
    log.push("#14 [build 8/9] RUN go build\n");
    expect(await within(inline).findByText("#14 [build 8/9] RUN go build")).toBeInTheDocument();
    expect(calls.find((c) => c.path === "/jobs/300/log")?.query.get("follow")).toBe("1");
  });

  it("builds the current checkout without a body flag and shows the daemon's 409 verbatim", async () => {
    const { user, calls } = renderApp({
      path: "/projects/5",
      signedInAs: developer,
      routes: projectRoutes([], {
        "POST /projects/{id}/builds": () =>
          errorResponse(409, "conflict", "the project's checkout is in an error state; pull the project first (or build with pull)"),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Start build" }));
    expect(await screen.findByText("The project's checkout is in an error state; pull the project first (or build with pull).")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST" && c.path === "/projects/5/builds")?.body).toEqual({});
  });

  it("lists builds newest first with status, failure detail, commit, engine and servers", async () => {
    const failed = buildFixture({
      id: 13,
      status: "error",
      detail: "go.mod requires github.com/demiurgos-hub/golem-engine v0.3.2; Golem Deploy needs v0.4.0-rc.1 or newer",
      image_id: "",
      image_tag: "",
      engine_version: "v0.3.2",
      created_by: 9,
      job_id: 301,
    });
    const ok = buildFixture({ server_ids: [3] });
    renderApp({
      path: "/projects/5",
      signedInAs: developer,
      routes: projectRoutes([ok, failed], {
        "GET /jobs": () => jsonResponse(200, { items: [job({ id: 301, kind: "project.build", created_by: 9, created_by_username: "root", status: "error" })] }),
      }),
    });
    const table = await screen.findByRole("table", { name: "Builds" });
    const rows = within(table).getAllByRole("row").slice(1);
    expect(within(rows[0]).getByRole("link", { name: "#13" })).toHaveAttribute("href", "/builds/13");
    expect(within(rows[0]).getByText(/go\.mod requires github\.com\/demiurgos-hub\/golem-engine v0\.3\.2/)).toBeInTheDocument();
    expect(within(rows[0]).getByText(/by root/)).toBeInTheDocument();
    expect(within(rows[1]).getByText(COMMIT.slice(0, 12))).toBeInTheDocument();
    expect(within(rows[1]).getByRole("link", { name: "echo" })).toHaveAttribute("href", "/servers/3");
    // Developers cannot delete builds.
    expect(screen.queryByRole("button", { name: /Delete build/ })).toBeNull();
  });

  it("lets an admin delete a build and shows the daemon's reason when it is in use", async () => {
    const { user } = renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: projectRoutes([buildFixture({ server_ids: [3] })], {
        "DELETE /builds/{id}": () =>
          errorResponse(409, "conflict", "servers are configured with this build; assign them another build or delete them first"),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete build #12" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete build" });
    await user.click(within(confirm).getByRole("button", { name: "Delete build" }));
    expect(await within(confirm).findByText("Servers are configured with this build; assign them another build or delete them first.")).toBeInTheDocument();
  });
});

describe("build page", () => {
  it("shows the build's details and its live log", async () => {
    const running = job({ id: 300, kind: "project.build", status: "running", started_at: "2026-11-20T10:00:00Z" });
    const log = streamResponse();
    renderApp({
      path: "/builds/12",
      signedInAs: developer,
      routes: {
        "GET /builds/{id}": () => jsonResponse(200, buildFixture({ server_ids: [3] })),
        "GET /projects/{id}": () => jsonResponse(200, project),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
        "GET /jobs/{id}": () => jsonResponse(200, running),
        "GET /jobs/{id}/log": () => log.response,
        "GET /servers": () => jsonResponse(200, { items: [serverFixture()] }),
      },
    });
    expect(await screen.findByRole("heading", { name: /Build #12/ })).toBeInTheDocument();
    expect(screen.getByText(`golem-deploy/arena:${COMMIT.slice(0, 12)}-b12`)).toBeInTheDocument();
    expect(screen.getByText(/recorded when a server running this build first reports ready/)).toBeInTheDocument();
    expect(screen.getByText("/wt")).toBeInTheDocument();
    expect(screen.getByText("/api/ws")).toBeInTheDocument();
    const inline = await screen.findByLabelText("Job 300");
    log.push("#18 naming to golem-deploy/arena done\n");
    expect(await within(inline).findByText("#18 naming to golem-deploy/arena done")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  it("says when the build's job is gone", async () => {
    renderApp({
      path: "/builds/12",
      signedInAs: account(),
      routes: {
        "GET /builds/{id}": () => jsonResponse(200, buildFixture({ job_id: null, schema_fingerprint: "3b6f0c2a91d4e8f7" })),
        "GET /projects/{id}": () => jsonResponse(200, project),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
        "GET /servers": () => jsonResponse(200, { items: [] }),
      },
    });
    expect(await screen.findByText(/The job record of this build is gone/)).toBeInTheDocument();
    expect(screen.getByText("3b6f0c2a91d4e8f7")).toBeInTheDocument();
    expect(screen.getByText("Ready to run")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete" })).toBeEnabled();
  });
});

describe("static add-ons and the custom Dockerfile switch", () => {
  const statics: StaticFileList = {
    items: [
      { name: "maps.zip", size_bytes: 10, modified_at: "2026-11-01T00:00:00Z", is_zip: true },
      { name: "notes.txt", size_bytes: 10, modified_at: "2026-11-01T00:00:00Z", is_zip: false },
    ],
    used_bytes: 20,
    quota_bytes: 0,
    file_count: 2,
    max_files: 2000,
  };

  it("edits the add-ons and routes the daemon's static_addons[i] errors to the row", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: projectRoutes([], {
        "GET /static": () => jsonResponse(200, statics),
        "PATCH /projects/{id}": (req: FakeRequest) =>
          ++n === 1
            ? errorResponse(400, "validation_failed", "target must be a relative path below the game root", { field: "static_addons[0].target" })
            : jsonResponse(200, { ...project, static_addons: (req.body as { static_addons: unknown }).static_addons }),
      }),
    });
    expect(await screen.findByText(/No static add-ons/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Edit add-ons" }));
    const dialog = await screen.findByRole("dialog", { name: "Static add-ons of arena" });
    await user.click(await within(dialog).findByRole("button", { name: "Add add-on" }));
    const zip = within(dialog).getByLabelText("ZIP file of add-on 1");
    // Only ZIP archives are offered.
    expect(within(zip).queryByRole("option", { name: "notes.txt" })).toBeNull();
    await user.type(within(dialog).getByLabelText("Target path of add-on 1"), "../up");
    await user.click(within(dialog).getByRole("button", { name: "Save add-ons" }));
    expect(within(dialog).getByText(/may not leave the game root/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);

    await user.clear(within(dialog).getByLabelText("Target path of add-on 1"));
    await user.type(within(dialog).getByLabelText("Target path of add-on 1"), "assets/maps");
    await user.click(within(dialog).getByRole("button", { name: "Save add-ons" }));
    expect(await within(dialog).findByText("Target must be a relative path below the game root.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ static_addons: [{ name: "maps.zip", target: "assets/maps" }] });
    await user.click(within(dialog).getByRole("button", { name: "Save add-ons" }));
    expect(await screen.findByText("assets/maps")).toBeInTheDocument();
  });

  it("lets an admin allow the repository's Dockerfile, with the shared-cache warning", async () => {
    const { user, calls } = renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: projectRoutes([], { "PATCH /projects/{id}": () => jsonResponse(200, { ...project, allow_custom_dockerfile: true }) }),
    });
    const box = await screen.findByLabelText("Allow the repository's own Dockerfile");
    expect(box).not.toBeChecked();
    expect(screen.getByText(/trusting this repository, and everyone who can push to it, with that shared build cache/)).toBeInTheDocument();
    await user.click(box);
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ allow_custom_dockerfile: true }));
  });

  it("shows developers the settings read-only", async () => {
    renderApp({
      path: "/projects/5",
      signedInAs: developer,
      routes: projectRoutes([], { "GET /projects/{id}": () => jsonResponse(200, { ...project, static_addons: [{ name: "maps.zip", target: "." }] }) }),
    });
    expect(await screen.findByText(". (the game root)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit add-ons" })).toBeNull();
    expect(screen.queryByLabelText("Allow the repository's own Dockerfile")).toBeNull();
    expect(screen.getByText("not allowed")).toBeInTheDocument();
  });

  it("shows why a project cannot be deleted", async () => {
    const { user } = renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: projectRoutes([], {
        "DELETE /projects/{id}": () =>
          errorResponse(409, "conflict", "servers are configured with builds of this project (server ids 3); assign them another build or delete them first"),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete project" });
    expect(within(confirm).getByText(/every\s+build of the project with its image are removed/)).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Delete project" }));
    expect(await within(confirm).findByText(/Servers are configured with builds of this project \(server ids 3\)/)).toBeInTheDocument();
  });
});

describe("creating a server", () => {
  const kart = projectFixture({ id: 6, name: "kart" });
  const arenaBuilds = [buildFixture({ id: 12 }), buildFixture({ id: 10, commit: OTHER_COMMIT })];
  const kartBuild = buildFixture({ id: 11, project_id: 6 });

  it("offers projects with a successful build and their builds, and routes origin errors to the row", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/servers",
      signedInAs: developer,
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [project, kart, projectFixture({ id: 7, name: "empty" })] }),
        "GET /builds": () => jsonResponse(200, { items: [...arenaBuilds, kartBuild] }),
        "POST /servers": () =>
          ++n === 1
            ? errorResponse(400, "validation_failed", "origin https://b.example.com:8443 is not reachable from this game host", { field: "allowed_origins[1]" })
            : jsonResponse(201, serverFixture({ id: 40, name: "eu", build_id: 11, project_id: 6, state: "missing" })),
        "GET /servers/{id}": () => jsonResponse(200, serverFixture({ id: 40, name: "eu", build_id: 11, project_id: 6, state: "missing", desired_state: "stopped" })),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New server" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    const projectSelect = await within(dialog).findByLabelText("Project");
    expect(within(projectSelect).getAllByRole("option").map((o) => o.textContent)).toEqual(["arena", "kart"]);
    await user.selectOptions(projectSelect, "6");
    expect(within(dialog).getByLabelText("Build")).toHaveValue("11");
    await user.type(within(dialog).getByLabelText("Name"), "eu");
    await user.click(within(dialog).getByRole("button", { name: "Add origin" }));
    await user.type(within(dialog).getByLabelText("Allowed origin 1"), "https://play.example.com/path");
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    expect(within(dialog).getByText(/An origin has no path/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST")).toBe(false);

    await user.clear(within(dialog).getByLabelText("Allowed origin 1"));
    await user.type(within(dialog).getByLabelText("Allowed origin 1"), "https://play.example.com");
    await user.click(within(dialog).getByRole("button", { name: "Add origin" }));
    await user.type(within(dialog).getByLabelText("Allowed origin 2"), "https://b.example.com:8443");
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    expect(await within(dialog).findByText("Origin https://b.example.com:8443 is not reachable from this game host.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST")?.body).toEqual({
      name: "eu",
      build_id: 11,
      allowed_origins: ["https://play.example.com", "https://b.example.com:8443"],
      desired_state: "stopped",
    });
    await user.click(within(dialog).getByRole("button", { name: "Remove allowed origin 2" }));
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    expect(await screen.findByRole("heading", { name: /eu/ })).toBeInTheDocument();
  });

  it("opens preselected for a project from ?create=", async () => {
    renderApp({
      path: "/servers?create=6",
      signedInAs: account(),
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [project, kart] }),
        "GET /builds": () => jsonResponse(200, { items: [...arenaBuilds, kartBuild] }),
      },
    });
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    expect(await within(dialog).findByLabelText("Project")).toHaveValue("6");
  });
});

function serverPageRoutes(s: Server, extra: Record<string, Handler> = {}): Record<string, Handler> {
  return {
    "GET /servers/{id}": () => jsonResponse(200, s),
    "GET /projects": () => jsonResponse(200, { items: [project] }),
    "GET /jobs": () => jsonResponse(200, { items: [] }),
    "GET /servers/{id}/stats": () => errorResponse(409, "conflict", "the server is not running"),
    "GET /servers/{id}/runtime": () => jsonResponse(200, runtimeFixture()),
    "GET /servers/{id}/logs": () => streamResponse().response,
    ...extra,
  };
}

describe("server page", () => {
  it("links the game URL and shows the runtime view of a ready game", async () => {
    renderApp({ path: "/servers/3", signedInAs: developer, routes: serverPageRoutes(serverFixture()) });
    const links = await screen.findAllByRole("link", { name: /https:\/\/localhost:7810\// });
    expect(links[0]).toHaveAttribute("href", "https://localhost:7810/");
    expect(links[0]).toHaveAttribute("target", "_blank");
    const runtime = screen.getByRole("region", { name: "Game runtime" });
    expect(await within(runtime).findByText("Ready")).toBeInTheDocument();
    expect(within(runtime).getByText("7")).toBeInTheDocument();
    expect(within(runtime).getByText("431")).toBeInTheDocument();
    expect(within(runtime).getByText("1h 2m")).toBeInTheDocument();
    expect(within(runtime).getByText(/30 Hz/)).toBeInTheDocument();
    expect(within(runtime).getByText(/last 4\.2 ms, max 21\.7 ms of a 33\.3 ms budget, no overruns/)).toBeInTheDocument();
    expect(within(runtime).getByText("webtransport")).toBeInTheDocument();
    expect(within(runtime).getByText("3b6f0c2a91d4e8f7")).toBeInTheDocument();
  });

  it("shows the not-ready reason and degrades on 503 and 409", async () => {
    const view = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture(), { "GET /servers/{id}/runtime": () => jsonResponse(200, runtimeFixture({ ready: false, not_ready_reason: "loading world" })) }),
    });
    expect(await screen.findByText("Not ready")).toBeInTheDocument();
    expect(screen.getByText("loading world")).toBeInTheDocument();
    view.unmount();

    const second = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture(), {
        "GET /servers/{id}/runtime": () => errorResponse(503, "service_unavailable", "the game's admin endpoint did not answer; see the server log"),
      }),
    });
    expect(await screen.findByText("The game did not answer")).toBeInTheDocument();
    expect(screen.getByText("The game's admin endpoint did not answer; see the server log.")).toBeInTheDocument();
    second.unmount();

    const { calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture({ state: "exited", exit_code: 0, desired_state: "stopped", started_at: null })),
    });
    expect(await screen.findByText(/The game reports its runtime while the server is running/)).toBeInTheDocument();
    expect(calls.some((c) => c.path === "/servers/3/runtime")).toBe(false);
  });

  it("explains a missing game host and a missing certificate", async () => {
    renderApp({ path: "/servers/3", signedInAs: developer, routes: serverPageRoutes(serverFixture({ public_url: "", state: "missing", container_id: "", desired_state: "stopped" })) });
    expect(await screen.findByText(/No game host is configured/)).toBeInTheDocument();
    expect(screen.getByText(/No certificate yet/)).toBeInTheDocument();
  });

  it("shows the server's own self-signed certificate", async () => {
    renderApp({ path: "/servers/3", signedInAs: developer, routes: serverPageRoutes(serverFixture({ certificate: certFixture() })) });
    const section = await screen.findByRole("region", { name: "Certificate" });
    expect(within(section).getByText("Self-signed")).toBeInTheDocument();
    expect(within(section).getByText("9f".repeat(32))).toBeInTheDocument();
    expect(within(section).getByText(/browsers opening the game URL directly show a certificate warning/)).toBeInTheDocument();
  });

  it("shows a failed start with its detail and opens its log", async () => {
    const failed = job({
      id: 71,
      kind: "server.start",
      subject_type: "server",
      subject_id: "3",
      status: "error",
      detail: "the game did not report ready within 1m0s; its admin endpoint never answered: the game must use the Golem Engine deploy contract",
      finished_at: "2026-11-20T10:01:00Z",
    });
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture({ state: "exited", exit_code: 1, started_at: null }), {
        "GET /jobs": () => jsonResponse(200, { items: [failed] }),
        "GET /jobs/{id}": () => jsonResponse(200, failed),
        "GET /jobs/{id}/log": () => {
          const s = streamResponse();
          s.push("container | listen: bad config\n");
          s.close();
          return s.response;
        },
      }),
    });
    const notice = await screen.findByText("The last start failed");
    const box = notice.closest("[role=alert]") as HTMLElement;
    expect(within(box).getByText(/the game must use the Golem Engine deploy contract/)).toBeInTheDocument();
    expect(within(box).getByRole("link", { name: "Server log" })).toHaveAttribute("href", "#logs");
    await user.click(within(box).getByRole("button", { name: "Show job log" }));
    const inline = await screen.findByLabelText("Job 71");
    expect(await within(inline).findByText("listen: bad config", { exact: false })).toBeInTheDocument();
  });

  it("deploys another build of the same project and restarts, saying players are disconnected", async () => {
    const restart = job({ id: 80, kind: "server.restart", subject_type: "server", subject_id: "3", status: "queued" });
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture(), {
        "GET /builds": () => jsonResponse(200, { items: [buildFixture({ id: 14, commit: OTHER_COMMIT }), buildFixture({ id: 12 }), buildFixture({ id: 13, project_id: 6 })] }),
        "PATCH /servers/{id}": () => jsonResponse(200, serverFixture({ build_id: 14, commit: OTHER_COMMIT, restart_required: true })),
        "POST /servers/{id}/restart": () => jsonResponse(202, restart),
        "GET /jobs/{id}": () => jsonResponse(200, restart),
        "GET /jobs/{id}/log": () => streamResponse().response,
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Deploy another build" }));
    const dialog = await screen.findByRole("dialog", { name: "Deploy a build to echo" });
    const select = await within(dialog).findByLabelText("Build");
    // Only other successful builds of the server's project.
    expect(within(select).getAllByRole("option").map((o) => (o as HTMLOptionElement).value)).toEqual(["14"]);
    expect(within(dialog).getByText(/every connected player is disconnected/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Deploy and restart" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/servers/3/restart")).toBe(true));
    const patch = calls.findIndex((c) => c.method === "PATCH");
    expect(calls[patch].body).toEqual({ build_id: 14 });
    expect(patch).toBeLessThan(calls.findIndex((c) => c.path === "/servers/3/restart"));
    expect(await screen.findByLabelText("Job 80")).toBeInTheDocument();
  });

  it("assigns a build to a stopped server without restarting and shows job_in_progress as sent", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture({ state: "exited", exit_code: 0, desired_state: "stopped", started_at: null }), {
        "GET /builds": () => jsonResponse(200, { items: [buildFixture({ id: 14, commit: OTHER_COMMIT }), buildFixture({ id: 12 })] }),
        "PATCH /servers/{id}": () =>
          ++n === 1
            ? errorResponse(409, "job_in_progress", "a job is running for this server; wait for it to finish before changing its port or build")
            : jsonResponse(200, serverFixture({ build_id: 14, state: "exited" })),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Deploy another build" }));
    const dialog = await screen.findByRole("dialog", { name: "Deploy a build to echo" });
    expect(await within(dialog).findByText(/the next start runs the new build/)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Deploy and restart" })).toBeNull();
    await user.click(within(dialog).getByRole("button", { name: "Assign build" }));
    expect(await within(dialog).findByText("A job is running for this server; wait for it to finish before changing its port or build.")).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Assign build" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Deploy a build to echo" })).toBeNull());
    expect(calls.some((c) => c.path === "/servers/3/restart")).toBe(false);
  });

  it("offers a server without a build any successful build", async () => {
    renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture({ build_id: null, project_id: null, image: "", commit: "", engine_version: "", state: "missing", container_id: "", desired_state: "stopped" }), {
        "GET /builds": () => jsonResponse(200, { items: [buildFixture({ id: 12 }), buildFixture({ id: 13, project_id: 6 })] }),
      }),
    });
    expect(await screen.findByText("No build assigned")).toBeInTheDocument();
    screen.getByRole("button", { name: "Choose a build" }).click();
    const dialog = await screen.findByRole("dialog", { name: "Deploy a build to echo" });
    const select = await within(dialog).findByLabelText("Build");
    expect(within(select).getAllByRole("option").map((o) => (o as HTMLOptionElement).value)).toEqual(["13", "12"]);
  });

  it("saves allowed origins and routes the daemon's error to the row", async () => {
    let n = 0;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: developer,
      routes: serverPageRoutes(serverFixture({ allowed_origins: ["https://a.example.com"] }), {
        "PATCH /servers/{id}": () =>
          ++n === 1
            ? errorResponse(400, "validation_failed", "origin http://b.example.com: a DNS name or IP address is required", { field: "allowed_origins[1]" })
            : jsonResponse(200, serverFixture({ allowed_origins: ["https://a.example.com"] })),
      }),
    });
    const section = await screen.findByRole("region", { name: "Allowed origins" });
    expect(within(section).getByLabelText("Allowed origin 1")).toHaveValue("https://a.example.com");
    expect(within(section).getByRole("button", { name: "Save origins" })).toBeDisabled();
    await user.click(within(section).getByRole("button", { name: "Add origin" }));
    await user.type(within(section).getByLabelText("Allowed origin 2"), "http://b.example.com");
    await user.click(within(section).getByRole("button", { name: "Save origins" }));
    expect(await within(section).findByText("Origin http://b.example.com: a DNS name or IP address is required.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ allowed_origins: ["https://a.example.com", "http://b.example.com"] });
  });
});

describe("settings: game host and certificates", () => {
  const settings = { registration_enabled: false, game_host: "", effective_game_host: "panel.example.com" };
  const selfSigned: GameCertificate = { mode: "self_signed", game_host: "panel.example.com", certificate: null };

  it("shows the effective game host and routes a game_host error to the field", async () => {
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: account(),
      routes: {
        "GET /settings": () => jsonResponse(200, settings),
        "GET /certificates/game": () => jsonResponse(200, selfSigned),
        "PATCH /settings": () => errorResponse(400, "validation_failed", "game_host must be a DNS name or an IP address (no scheme, port or path)", { field: "game_host" }),
      },
    });
    expect(await screen.findByText("panel.example.com", { selector: "code" })).toBeInTheDocument();
    expect(screen.getByText(/the daemon's first --public-host/)).toBeInTheDocument();
    const input = screen.getByRole("textbox", { name: "Game host" });
    await user.type(input, "https://play.example.com");
    await user.click(screen.getByRole("button", { name: "Save game host" }));
    expect(screen.getByText("Enter only the host name or IP address, without https://.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
    await user.clear(input);
    await user.type(input, "play_example");
    await user.click(screen.getByRole("button", { name: "Save game host" }));
    expect(await screen.findByText("Game_host must be a DNS name or an IP address (no scheme, port or path).")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ game_host: "play_example" });
  });

  it("explains self-signed mode and installs a custom certificate after a confirmation, never keeping the key", async () => {
    let n = 0;
    const custom: GameCertificate = {
      mode: "custom",
      game_host: "panel.example.com",
      certificate: certFixture({ subject: "CN=panel.example.com", issuer: "CN=Example CA", self_signed: false, dns_names: ["panel.example.com"], chain_length: 2 }),
    };
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: account(),
      routes: {
        "GET /settings": () => jsonResponse(200, settings),
        "GET /certificates/game": () => jsonResponse(200, n >= 2 ? custom : selfSigned),
        "PUT /certificates/game": () =>
          ++n === 1 ? errorResponse(400, "validation_failed", "the private key does not match the certificate", { field: "private_key_pem" }) : jsonResponse(200, custom),
      },
    });
    expect(await screen.findByText(/Each server has its own self-signed certificate/)).toBeInTheDocument();
    expect(screen.getByText(/Browsers opening a game URL directly from\s+the internet show a certificate warning/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Upload a trusted certificate" }));
    const form = screen.getByRole("form", { name: "Upload a certificate" });
    const key = within(form).getByLabelText("Private key (PEM)");
    expect(key).toHaveValue("");
    await user.click(within(form).getByRole("button", { name: "Install certificate" }));
    expect(within(form).getByText(/Paste the PEM certificate chain/)).toBeInTheDocument();
    expect(within(form).getByText(/Paste the unencrypted PEM private key/)).toBeInTheDocument();

    await user.click(within(form).getByLabelText("Certificate chain (PEM)"));
    await user.paste("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----");
    await user.click(key);
    await user.paste("-----BEGIN PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END PRIVATE KEY-----");
    await user.click(within(form).getByRole("button", { name: "Install certificate" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Switch to the custom certificate" });
    expect(within(confirm).getByText(/a restart\s+disconnects their players/)).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Install and switch" }));
    expect(await within(form).findByText("The private key does not match the certificate.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PUT")?.body).toEqual({
      mode: "custom",
      certificate_pem: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----",
      private_key_pem: "-----BEGIN PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END PRIVATE KEY-----",
    });

    await user.click(within(form).getByRole("button", { name: "Install certificate" }));
    await user.click(within(await screen.findByRole("alertdialog", { name: "Switch to the custom certificate" })).getByRole("button", { name: "Install and switch" }));
    expect(await screen.findByText("Custom certificate")).toBeInTheDocument();
    expect(screen.getByText("CN=panel.example.com")).toBeInTheDocument();
    expect(screen.getByText("Issued by a CA")).toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Upload a certificate" })).toBeNull();
    // The key is gone from the page once installed.
    expect(document.body.textContent).not.toContain("SECRETKEYMATERIAL");
  });

  it("shows the installed custom certificate, warns when it misses the game host and switches back after a warning", async () => {
    const { user, calls } = renderApp({
      path: "/admin/settings",
      signedInAs: account(),
      routes: {
        "GET /settings": () => jsonResponse(200, settings),
        "GET /certificates/game": () =>
          jsonResponse(200, { mode: "custom", game_host: "panel.example.com", certificate: certFixture({ self_signed: false, covers_game_host: false, issuer: "CN=CA" }) }),
        "PUT /certificates/game": () => jsonResponse(200, selfSigned),
      },
    });
    expect(await screen.findByText("The certificate does not cover the game host")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Switch to self-signed certificates" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Switch to self-signed certificates" });
    expect(within(confirm).getByText(/Restart required/)).toBeInTheDocument();
    await user.click(within(confirm).getByRole("button", { name: "Switch to self-signed" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ mode: "self_signed" }));
  });

  it("handles custom mode without an installed certificate (null or 409)", async () => {
    const view = renderApp({
      path: "/admin/settings",
      signedInAs: account(),
      routes: {
        "GET /settings": () => jsonResponse(200, settings),
        "GET /certificates/game": () => jsonResponse(200, { mode: "custom", game_host: "panel.example.com", certificate: null }),
      },
    });
    expect(await screen.findByText("No valid certificate is installed")).toBeInTheDocument();
    view.unmount();
    renderApp({
      path: "/admin/settings",
      signedInAs: account(),
      routes: {
        "GET /settings": () => jsonResponse(200, settings),
        "GET /certificates/game": () => errorResponse(409, "conflict", "no valid custom game certificate is installed; upload one or switch to self_signed"),
      },
    });
    expect(await screen.findByText(/No valid custom game certificate is installed; upload one or switch to self_signed\./)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Upload a new certificate" })).toBeInTheDocument();
  });
});

describe("images and static files", () => {
  it("lists the builds that record an image and shows the daemon's 409 on delete", async () => {
    const { user } = renderApp({
      path: "/images",
      signedInAs: account(),
      routes: {
        "GET /images": () => jsonResponse(200, { items: [imageFixture()] }),
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [project] }),
        "DELETE /images/{id}": () => errorResponse(409, "conflict", "the image belongs to build 12; delete the build instead (DELETE /api/v1/builds/{id})"),
      },
    });
    expect(await screen.findByRole("link", { name: "#12" })).toHaveAttribute("href", "/builds/12");
    await user.click(screen.getByRole("button", { name: /Delete image/ }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete image" });
    await user.click(within(confirm).getByRole("button", { name: "Delete image" }));
    expect(await within(confirm).findByText(/The image belongs to build 12; delete the build instead/)).toBeInTheDocument();
  });

  const list: StaticFileList = {
    items: [
      { name: "maps.zip", size_bytes: 10, modified_at: "2026-11-01T00:00:00Z", is_zip: true },
      { name: "free.zip", size_bytes: 10, modified_at: "2026-11-01T00:00:00Z", is_zip: true },
    ],
    used_bytes: 20,
    quota_bytes: 0,
    file_count: 2,
    max_files: 2000,
  };
  const users = [projectFixture({ static_addons: [{ name: "maps.zip", target: "assets" }] })];

  it("shows which projects use a file, refuses deleting it and tells a developer only admins replace it", async () => {
    const { user, container, calls } = renderApp({
      path: "/static",
      signedInAs: developer,
      routes: { "GET /static": () => jsonResponse(200, list), "GET /projects": () => jsonResponse(200, { items: users }) },
    });
    const table = await screen.findByRole("table", { name: "Static files" });
    const usedRow = (await within(table).findByRole("link", { name: "arena" })).closest("tr") as HTMLElement;
    expect(within(usedRow).getByRole("button", { name: "Delete maps.zip" })).toBeDisabled();
    expect(within(table).getByRole("button", { name: "Delete free.zip" })).toBeEnabled();
    const input = container.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, new File(["x"], "maps.zip"));
    const queue = screen.getByRole("list", { name: "Upload queue" });
    expect(within(queue).getByText("Not allowed")).toBeInTheDocument();
    expect(within(queue).getByText(/static add-on of arena; only an administrator can replace it/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST")).toBe(false);
  });

  it("tells an admin that replacing an add-on takes effect at the next build", async () => {
    const { user, container } = renderApp({
      path: "/static",
      signedInAs: account(),
      routes: { "GET /static": () => jsonResponse(200, list), "GET /projects": () => jsonResponse(200, { items: users }) },
    });
    await screen.findByRole("link", { name: "arena" });
    const input = container.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, new File(["x"], "maps.zip"));
    await user.click(screen.getByRole("button", { name: /Upload 1 file/ }));
    const confirm = await screen.findByRole("alertdialog", { name: "Replace existing files" });
    expect(within(confirm).getByText(/add-on of arena: their next build uses the new file/)).toBeInTheDocument();
  });
});

describe("regressions found against the real daemon", () => {
  it("does not keep Pull first ticked once the clone that forced it has finished", async () => {
    let current = projectFixture({ status: "cloning", last_commit: "", last_pulled_at: null });
    renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: projectRoutes([], { "GET /projects/{id}": () => jsonResponse(200, current) }),
    });
    const box = await screen.findByLabelText("Pull first");
    expect(box).toBeChecked();
    expect(box).toBeDisabled();
    current = projectFixture();
    await waitFor(() => expect(screen.getByLabelText("Pull first")).not.toBeChecked(), { timeout: 4000 });
    expect(screen.getByLabelText("Pull first")).toBeEnabled();
  });

  it("clears the game host error on Discard", async () => {
    const { user } = renderApp({
      path: "/admin/settings",
      signedInAs: account(),
      routes: {
        "GET /settings": () => jsonResponse(200, { registration_enabled: false, game_host: "", effective_game_host: "localhost" }),
        "GET /certificates/game": () => jsonResponse(200, { mode: "self_signed", game_host: "localhost", certificate: null }),
      },
    });
    const input = await screen.findByRole("textbox", { name: "Game host" });
    await user.type(input, "bad host");
    await user.click(screen.getByRole("button", { name: "Save game host" }));
    expect(screen.getByText("The host name cannot contain spaces.")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Discard" }));
    expect(screen.queryByText("The host name cannot contain spaces.")).toBeNull();
    expect(input).toHaveValue("");
  });
});

describe("first run", () => {
  it("shows the order of the steps and links the next one", async () => {
    const view = renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: { "GET /projects": () => jsonResponse(200, { items: [] }), "GET /servers": () => jsonResponse(200, { items: [] }) },
    });
    const guide = await screen.findByRole("region", { name: "Getting started" });
    expect(within(guide).getByText("1. Add a project")).toBeInTheDocument();
    expect(within(guide).getByText("2. Build it")).toBeInTheDocument();
    expect(within(guide).getByText("3. Create a server")).toBeInTheDocument();
    view.unmount();

    renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: {
        "GET /projects": () => jsonResponse(200, { items: [project] }),
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /builds": () => jsonResponse(200, { items: [buildFixture()] }),
      },
    });
    const next = await screen.findByRole("region", { name: "Getting started" });
    expect(await within(next).findByRole("link", { name: "Create a server" })).toHaveAttribute("href", "/servers?create=5");
  });

  it("hides the guide once a server exists", async () => {
    renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: { "GET /projects": () => jsonResponse(200, { items: [project] }), "GET /servers": () => jsonResponse(200, { items: [serverFixture()] }) },
    });
    await screen.findByText("arena");
    expect(screen.queryByRole("region", { name: "Getting started" })).toBeNull();
  });
});
