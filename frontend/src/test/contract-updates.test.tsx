import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import type { Job, Project, Server, StaticFileList } from "@/api/types.gen";
import { buildFixture, projectFixture, serverFixture } from "@/test/fixtures";
import { account, errorResponse, job, jsonResponse, streamResponse, type FakeRequest } from "@/test/fake-api";
import { renderApp } from "@/test/render-app";

// Behaviour that follows the contract changes of the security-fix branches
// (docs/API.md): SSH auth and origin changes for projects, env rules, port
// and state rules for servers, static store usage, quota and upload limits,
// escaped log output, and list polling.

const project: Project = projectFixture({ auth_type: "https", auth_username: "deploy-bot", has_secret: true });
const server: Server = serverFixture({ port: 7830, public_url: "https://localhost:7830/" });

const KEY = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ==\n-----END OPENSSH PRIVATE KEY-----";

function serverRoutes(s: Server, extra: Record<string, (req: FakeRequest) => Response | Promise<Response>> = {}) {
  return {
    "GET /servers/{id}": () => jsonResponse(200, s),
    "GET /projects": () => jsonResponse(200, { items: [] }),
    "GET /jobs": () => jsonResponse(200, { items: [] }),
    "GET /servers/{id}/stats": () => errorResponse(409, "conflict", "the server is not running"),
    "GET /servers/{id}/runtime": () => errorResponse(409, "conflict", "the server is not running"),
    "GET /servers/{id}/logs": () => streamResponse().response,
    ...extra,
  };
}

afterEach(() => {
  vi.useRealTimers();
});

describe("projects: SSH needs a key", () => {
  it("offers only SSH for an SSH URL and requires a key", async () => {
    const { user, calls } = renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: {
        "GET /projects": () => jsonResponse(200, { items: [] }),
        "POST /projects": () => jsonResponse(201, { project: { ...project, id: 9 }, job: job({ id: 1, kind: "project.clone", subject_id: "9" }) }),
        "GET /projects/{id}": () => jsonResponse(200, { ...project, id: 9 }),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
        "GET /jobs/{id}": () => jsonResponse(200, job({ id: 1, kind: "project.clone", subject_id: "9" })),
        "GET /jobs/{id}/log": () => streamResponse().response,
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New project" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New project" });
    expect(within(dialog).getByRole("radio", { name: /None/ })).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText("Name"), "kart");
    await user.type(within(dialog).getByLabelText("Repository URL"), "git@github.com:acme/kart.git");
    expect(within(dialog).queryByRole("radio", { name: /None/ })).toBeNull();
    expect(within(dialog).queryByRole("radio", { name: /HTTPS/ })).toBeNull();
    expect(within(dialog).getByRole("radio", { name: /SSH/ })).toBeChecked();
    await user.click(within(dialog).getByRole("button", { name: "Create and clone" }));
    expect(within(dialog).getByText("Paste the private key.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST" && c.path === "/projects")).toBe(false);

    await user.click(within(dialog).getByLabelText("Private key"));
    await user.paste(KEY);
    await user.click(within(dialog).getByRole("button", { name: "Create and clone" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/projects")?.body).toMatchObject({ auth: { type: "ssh" } }));
  });

  it("shows the daemon's auth.type error for none with an SSH URL", async () => {
    const { user } = renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: {
        "GET /projects": () => jsonResponse(200, { items: [] }),
        "POST /projects": () =>
          errorResponse(400, "validation_failed", 'SSH repository URLs need a private key: use auth type "ssh" with a deploy key', { field: "auth.type" }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New project" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New project" });
    await user.type(within(dialog).getByLabelText("Name"), "kart");
    await user.type(within(dialog).getByLabelText("Repository URL"), "ssh://git@host/acme/kart.git");
    await user.click(within(dialog).getByLabelText("Private key"));
    await user.paste(KEY);
    await user.click(within(dialog).getByRole("button", { name: "Create and clone" }));
    expect(await within(dialog).findByText(/SSH repository URLs need a private key: use auth type "ssh"/)).toBeInTheDocument();
  });
});

describe("projects: credentials never follow a URL to another host", () => {
  function setup(patch: (req: FakeRequest) => Response = () => jsonResponse(200, project)) {
    return renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: {
        "GET /projects/{id}": () => jsonResponse(200, project),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
        "PATCH /projects/{id}": patch,
      },
    });
  }

  it("keeps credentials for a path-only change", async () => {
    const { user, calls } = setup();
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit arena" });
    const url = within(dialog).getByLabelText("Repository URL");
    await user.clear(url);
    await user.type(url, "https://GITHUB.com:443/example/arena-renamed.git");
    expect(within(dialog).queryByText("Credentials needed for the new host")).toBeNull();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ repo_url: "https://GITHUB.com:443/example/arena-renamed.git" }),
    );
  });

  it("requires replacing or clearing credentials when the host changes", async () => {
    const { user, calls } = setup();
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit arena" });
    const url = within(dialog).getByLabelText("Repository URL");
    await user.clear(url);
    await user.type(url, "https://gitlab.example.com/example/arena.git");
    expect(within(dialog).getByText("Credentials needed for the new host")).toBeInTheDocument();
    expect(within(dialog).getByText(/never sent to a new one/)).toBeInTheDocument();
    expect(within(dialog).getByRole("radio", { name: /Keep current/ })).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(within(dialog).getByText(/Stored credentials are never sent to a new host/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);

    await user.click(within(dialog).getByRole("radio", { name: /Remove/ }));
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({
        repo_url: "https://gitlab.example.com/example/arena.git",
        auth: { type: "none" },
      }),
    );
  });

  it("asks for a key when moving to an SSH URL and shows a field auth error from the daemon", async () => {
    const { user } = setup(() =>
      errorResponse(400, "validation_failed", "the new repository URL has a different scheme, host or port; send auth in the same request", { field: "auth" }),
    );
    await user.click(await screen.findByRole("button", { name: "Edit" }));
    const dialog = await screen.findByRole("dialog", { name: "Edit arena" });
    const url = within(dialog).getByLabelText("Repository URL");
    await user.clear(url);
    await user.type(url, "git@github.com:example/arena.git");
    expect(within(dialog).queryByRole("radio", { name: /Remove/ })).toBeNull();
    await user.click(within(dialog).getByRole("radio", { name: /Replace/ }));
    expect(within(dialog).getByRole("radio", { name: /SSH/ })).toBeChecked();
    expect(within(dialog).queryByRole("radio", { name: /HTTPS/ })).toBeNull();
    await user.click(within(dialog).getByLabelText("Private key"));
    await user.paste(KEY);
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await within(dialog).findByText(/The new repository URL has a different scheme, host or port/)).toBeInTheDocument();
  });

  it("shows long job failure details in full", async () => {
    const detail =
      "the checkout exceeds the maximum repository size configured on the host (--max-repo-size-mb); the previous checkout was kept and nothing else was changed";
    renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: {
        "GET /projects/{id}": () => jsonResponse(200, project),
        "GET /jobs": () => jsonResponse(200, { items: [job({ id: 8, status: "error", detail, finished_at: "2026-11-20T10:00:00Z" })] }),
      },
    });
    const text = await screen.findByText(detail);
    expect(text.className).not.toMatch(/truncate/);
    expect(text.className).toMatch(/break-words/);
  });
});

describe("server environment", () => {
  it("rejects GOLEM_ names and oversized values on their rows, and accepts names like PATH", async () => {
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes(server, { "PATCH /servers/{id}": () => jsonResponse(200, server) }),
    });
    await screen.findByText("GREETING");
    await user.click(screen.getByRole("button", { name: "Add variable" }));
    await user.type(screen.getAllByLabelText("Variable name")[0], "GOLEM_X");
    await user.type(screen.getByLabelText("Value for GOLEM_X"), "v");
    expect(screen.getByText("Names starting with GOLEM_ are reserved for golem-deploy.")).toBeInTheDocument();
    await user.clear(screen.getAllByLabelText("Variable name")[0]);
    await user.type(screen.getAllByLabelText("Variable name")[0], "PATH");
    expect(screen.queryByText("Names starting with GOLEM_ are reserved for golem-deploy.")).toBeNull();

    await user.click(screen.getByRole("button", { name: "Replace value" }));
    const value = screen.getByLabelText("New value for GREETING");
    // Single-line inputs drop line breaks on their own (unit tests cover that rule); the size limit counts bytes.
    await user.click(value);
    await user.paste("x".repeat(32 * 1024));
    expect(await screen.findByText(/The variable exceeds 32768 bytes/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save environment" })).toBeDisabled();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
  });

  it("routes the daemon's env_set.<KEY> error to that row and explains a 409 retry", async () => {
    let n = 0;
    const { user } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes(server, {
        "PATCH /servers/{id}": () =>
          ++n === 1
            ? errorResponse(400, "validation_failed", "the value is not valid UTF-8", { field: "env_set.MOTD" })
            : errorResponse(409, "conflict", "the change conflicts with a concurrent change; retry"),
      }),
    });
    await screen.findByText("GREETING");
    await user.click(screen.getByRole("button", { name: "Add variable" }));
    await user.type(screen.getAllByLabelText("Variable name")[0], "MOTD");
    await user.type(screen.getByLabelText("Value for MOTD"), "hi");
    await user.click(screen.getByRole("button", { name: "Save environment" }));
    const row = (await screen.findByText("The value is not valid UTF-8.")).closest("[data-env-row]");
    expect(row?.getAttribute("data-env-row")).toBe("MOTD");
    await user.type(screen.getByLabelText("Value for MOTD"), "!");
    expect(screen.queryByText("The value is not valid UTF-8.")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Save environment" }));
    expect(await screen.findByText(/was saved at the same moment, so yours was not.*save again/)).toBeInTheDocument();
  });

  it("routes env.<KEY> errors on create to the row and a build_id error to the build field", async () => {
    let n = 0;
    const { user } = renderApp({
      path: "/servers",
      signedInAs: account(),
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [project] }),
        "GET /builds": () => jsonResponse(200, { items: [buildFixture()] }),
        "POST /servers": () =>
          ++n === 1
            ? errorResponse(400, "validation_failed", "the variable exceeds 32768 bytes", { field: "env.SECRET" })
            : errorResponse(400, "validation_failed", "the build was deleted meanwhile; choose another build", { field: "build_id" }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New server" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    await user.type(await within(dialog).findByLabelText("Name"), "echo");
    await user.click(within(dialog).getByRole("button", { name: "Add variable" }));
    await user.type(within(dialog).getByLabelText("Variable name"), "SECRET");
    await user.type(within(dialog).getByLabelText("Value for SECRET"), "s3cret");
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    const row = (await within(dialog).findByText("The variable exceeds 32768 bytes.")).closest("[data-env-row]");
    expect(row?.getAttribute("data-env-row")).toBe("SECRET");
    expect(dialog.textContent).not.toContain("s3cret");
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    const buildField = within(dialog).getByLabelText("Build");
    expect(await within(dialog).findByText("The build was deleted meanwhile; choose another build.")).toBeInTheDocument();
    expect(buildField.getAttribute("aria-describedby")).toBeTruthy();
  });

  it("names the variable of an import value error", async () => {
    const { user, container } = renderApp({
      path: "/servers",
      signedInAs: account(),
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [] }),
        "POST /servers/import": () => errorResponse(400, "validation_failed", "the value contains a NUL byte", { field: "servers[0].env.MOTD" }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Import" }));
    const input = container.ownerDocument.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, new File([JSON.stringify({ format_version: 2, servers: [{ name: "a", project: "arena", env: { MOTD: "ok" } }] })], "s.json"));
    await user.click(await screen.findByRole("button", { name: "Import 1 server" }));
    expect(await screen.findByText("Server 1 (a), environment variable MOTD:")).toBeInTheDocument();
  });
});

describe("server port and state", () => {
  it("locks the port of a running server and handles a 409 from a race", async () => {
    let current: Server = server;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes(server, {
        "GET /servers/{id}": () => jsonResponse(200, current),
        "PATCH /servers/{id}": () => errorResponse(409, "conflict", "stop the server before changing its port"),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Change port" }));
    let dialog = await screen.findByRole("dialog", { name: "Change the port of echo" });
    expect(within(dialog).getByLabelText("Port")).toBeDisabled();
    expect(within(dialog).getByText(/Stop the server to change its port/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));

    // Stopped now (as far as this page knows); the container started again elsewhere meanwhile.
    current = { ...server, state: "exited", exit_code: 0, desired_state: "stopped" };
    await act(async () => {
      await new Promise((r) => setTimeout(r, 4200));
    });
    await user.click(screen.getByRole("button", { name: "Change port" }));
    dialog = await screen.findByRole("dialog", { name: "Change the port of echo" });
    const port = within(dialog).getByLabelText("Port");
    expect(port).toBeEnabled();
    await user.clear(port);
    await user.type(port, "7831");
    await user.click(within(dialog).getByRole("button", { name: "Save" }));
    expect(await within(dialog).findByText("Stop the server before changing its port.")).toBeInTheDocument();
    expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ port: 7831 });
  }, 10_000);

  it("disables actions and log reads while Docker cannot be queried (unknown)", async () => {
    const { calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes({ ...server, state: "unknown", started_at: null }),
    });
    expect(await screen.findByText("State unknown")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Restart" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled();
    expect(await screen.findByText(/Docker cannot be reached right now/)).toBeInTheDocument();
    expect(calls.some((c) => c.path === "/servers/3/logs")).toBe(false);
  });

  it("offers start but not restart for a server without a container (missing)", async () => {
    const { calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes({ ...server, state: "missing", container_id: "", desired_state: "stopped", started_at: null }),
    });
    expect(await screen.findByRole("button", { name: "Start" })).toBeEnabled();
    expect(screen.queryByRole("button", { name: "Restart" })).toBeNull();
    expect(screen.getAllByText("No container").length).toBeGreaterThan(0);
    expect(calls.some((c) => c.path === "/servers/3/logs")).toBe(false);
  });
});

describe("server deletion outcome", () => {
  it("shows a failed delete as a failure, with its job", async () => {
    const failed: Job = job({
      id: 61,
      kind: "server.delete",
      subject_type: "server",
      subject_id: "3",
      status: "error",
      detail: "Docker did not answer in time while trying to remove the container",
      finished_at: "2026-11-20T10:00:00Z",
    });
    const { user, history } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes(server, {
        "DELETE /servers/{id}": () => errorResponse(500, "internal_error", "deleting the server failed; see the log of job 61"),
        "GET /jobs": (req) => jsonResponse(200, { items: req.query.get("limit") === "5" ? [failed] : [] }),
        "GET /jobs/{id}": () => jsonResponse(200, failed),
        "GET /jobs/{id}/log": () => {
          const s = streamResponse();
          s.push("deleting server \"echo\"\nerror: timeout\n");
          s.close();
          return s.response;
        },
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    expect(await screen.findByText(/Could not delete echo: Deleting the server failed; see the log of job 61\./)).toBeInTheDocument();
    const inline = await screen.findByLabelText("Job 61");
    expect(within(inline).getByText("Failed")).toBeInTheDocument();
    expect(within(inline).getByText(/Docker did not answer in time/)).toBeInTheDocument();
    expect(history.location.pathname).toBe("/servers/3");
  });

  it("reports a successful delete and returns to the list", async () => {
    const { user, history } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes(server, {
        "DELETE /servers/{id}": () => jsonResponse(204, null),
        "GET /servers": () => jsonResponse(200, { items: [] }),
      }),
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Delete server" });
    await user.click(within(confirm).getByRole("button", { name: "Delete server" }));
    expect(await screen.findByText(/Server echo deleted: container and saved data removed, port 7830 freed\. Its backups are kept\./)).toBeInTheDocument();
    await waitFor(() => expect(history.location.pathname).toBe("/servers"));
  });
});

describe("server logs", () => {
  it("shows the daemon's escaped control sequences dimmed, not interpreted", async () => {
    const log = streamResponse();
    renderApp({ path: "/servers/3", signedInAs: account(), routes: serverRoutes(server, { "GET /servers/{id}/logs": () => log.response }) });
    await screen.findByRole("log");
    log.push("2026-11-24T10:00:00Z \\x1b[31mred\\x1b[0m tick\n");
    const esc = await screen.findAllByText("\\x1b[31m");
    expect(esc[0].className).toMatch(/text-muted-foreground/);
    expect(esc[0].parentElement?.textContent).toBe("2026-11-24T10:00:00Z \\x1b[31mred\\x1b[0m tick");
  });

  it("dims the container prefix of job log lines", async () => {
    const running = job({ id: 70, kind: "server.start", subject_type: "server", subject_id: "3", status: "running", started_at: "2026-11-20T10:00:00Z" });
    const log = streamResponse();
    renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: serverRoutes({ ...server, state: "exited", exit_code: 1 }, {
        "GET /jobs": () => jsonResponse(200, { items: [running] }),
        "GET /jobs/{id}": () => jsonResponse(200, running),
        "GET /jobs/{id}/log": () => log.response,
      }),
    });
    const inline = await screen.findByLabelText("Job 70");
    log.push("container | panic: boom\n");
    const prefix = await within(inline).findByText("container |", { exact: false, selector: "span" });
    expect(prefix.className).toMatch(/text-muted-foreground/);
  });
});

function staticList(extra: Partial<StaticFileList> = {}): StaticFileList {
  return {
    items: [{ name: "maps.zip", size_bytes: 1_000_000, modified_at: "2026-11-01T00:00:00Z", is_zip: true }],
    used_bytes: 1_000_000,
    quota_bytes: 2 * 1024 * 1024,
    file_count: 1,
    max_files: 2000,
    ...extra,
  };
}

describe("static store usage, quota and upload concurrency", () => {
  it("shows usage against the quota, or no limit", async () => {
    const view = renderApp({ path: "/static", signedInAs: account(), routes: { "GET /static": () => jsonResponse(200, staticList()) } });
    const meter = await screen.findByRole("meter", { name: "Static store usage" });
    expect(meter).toHaveAttribute("aria-valuenow", "1000000");
    expect(meter).toHaveAttribute("aria-valuemax", String(2 * 1024 * 1024));
    expect(screen.getByText(/Used 1.000.000 bytes of 2.097.152 bytes\./)).toBeInTheDocument();
    expect(screen.getByText(/^1 of 2.000 files$/)).toBeInTheDocument();
    view.unmount();

    renderApp({ path: "/static", signedInAs: account(), routes: { "GET /static": () => jsonResponse(200, staticList({ quota_bytes: 0 })) } });
    expect(await screen.findByText(/used, no size limit/)).toBeInTheDocument();
    expect(screen.queryByRole("meter")).toBeNull();
  });

  it("warns before an upload that cannot fit and reports a 507 as not stored", async () => {
    const { user, container } = renderApp({
      path: "/static",
      signedInAs: account(),
      routes: {
        "GET /static": () => jsonResponse(200, staticList()),
        "POST /static": () => errorResponse(507, "insufficient_storage", "the upload would exceed the static store quota of 2 MiB"),
      },
    });
    await screen.findByRole("meter");
    const input = container.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, new File([new Uint8Array(1_500_000)], "big.bin"));
    expect(screen.getByText("Not enough room")).toBeInTheDocument();
    expect(screen.getByText(/only 1\.1 MB of the 2\.1 MB quota is left|of the .* quota is left/)).toBeInTheDocument();
    expect(screen.getByText("Larger than the space left")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Upload 1 file/ }));
    const queue = screen.getByRole("list", { name: "Upload queue" });
    expect(await within(queue).findByText("Not stored")).toBeInTheDocument();
    expect(within(queue).getByText("The upload would exceed the static store quota of 2 MiB. The file was not stored.")).toBeInTheDocument();
  });

  it("never sends more than two uploads at once and waits out a 429", async () => {
    let inflight = 0;
    let peak = 0;
    let rateLimited = false;
    const pending: (() => void)[] = [];
    const { user, container } = renderApp({
      path: "/static",
      signedInAs: account(),
      routes: {
        "GET /static": () => jsonResponse(200, staticList({ quota_bytes: 0 })),
        "POST /static": async (req) => {
          const name = ((req.body as FormData).get("files") as File).name;
          if (name === "f3.bin" && !rateLimited) {
            rateLimited = true; // another tab of the same account holds a slot
            return errorResponse(429, "rate_limited", "too many uploads in progress for this account; wait for one to finish", { headers: { "Retry-After": "1" } });
          }
          inflight++;
          peak = Math.max(peak, inflight);
          await new Promise<void>((resolve) => pending.push(resolve));
          inflight--;
          return jsonResponse(201, { ...staticList({ quota_bytes: 0 }), items: [{ name, size_bytes: 1, modified_at: "2026-11-02T00:00:00Z", is_zip: false }] });
        },
      },
    });
    await screen.findByText(/used, no size limit/);
    const input = container.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, [1, 2, 3, 4, 5].map((i) => new File(["x"], `f${i}.bin`)));
    await user.click(screen.getByRole("button", { name: /Upload 5 files/ }));
    const queue = screen.getByRole("list", { name: "Upload queue" });
    for (let done = 0; done < 5; ) {
      await waitFor(() => expect(pending.length).toBeGreaterThan(0), { timeout: 3000 });
      expect(inflight).toBeLessThanOrEqual(2);
      pending.shift()?.();
      done++;
    }
    await waitFor(() => expect(within(queue).getAllByText("Uploaded")).toHaveLength(5), { timeout: 4000 });
    expect(peak).toBe(2);
    expect(rateLimited).toBe(true);
  }, 15_000);
});

describe("lists refresh on their own", () => {
  it("polls the project list while idle, like job lists", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { calls } = renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: { "GET /projects": () => jsonResponse(200, { items: [project] }) },
    });
    await screen.findByText("arena");
    const before = calls.filter((c) => c.path === "/projects").length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_500);
    });
    expect(calls.filter((c) => c.path === "/projects").length).toBeGreaterThan(before);
  });
});
