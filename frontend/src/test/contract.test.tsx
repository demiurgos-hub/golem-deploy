import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import type { Project, Server } from "@/api/types.gen";
import { buildFixture, projectFixture, serverFixture } from "@/test/fixtures";
import { account, errorResponse, job, jsonResponse, streamResponse } from "@/test/fake-api";
import { renderApp } from "@/test/render-app";

// Behaviour that follows the final API v1 contract (docs/API.md).

const project: Project = projectFixture({ last_commit: "", last_pulled_at: null, status: "cloning" });
const build = buildFixture();
const server: Server = serverFixture({ restart_required: true });

describe("login page and /auth/options", () => {
  it("offers registration only when it is enabled", async () => {
    const view = renderApp({ path: "/login", routes: { "GET /auth/options": () => jsonResponse(200, { registration_enabled: true }) } });
    expect(await screen.findByRole("link", { name: "Request one" })).toBeInTheDocument();
    view.unmount();

    renderApp({ path: "/login" });
    expect(await screen.findByText(/Ask an administrator to create one/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Request one" })).toBeNull();
  });

  it("explains on /register that registration is off", async () => {
    renderApp({ path: "/register" });
    expect(await screen.findByText(/Self-registration is turned off on this host/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Request account" })).toBeNull();
  });

  it("shows the daemon's message for concurrent logins and busy password checks", async () => {
    let n = 0;
    const { user } = renderApp({
      path: "/login",
      routes: {
        "POST /auth/login": () =>
          ++n === 1
            ? errorResponse(429, "rate_limited", "too many concurrent logins from your address; try again in a moment", { headers: { "Retry-After": "1" } })
            : errorResponse(503, "service_unavailable", "the server is busy; try again", { headers: { "Retry-After": "2" } }),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "alice");
    await user.type(screen.getByLabelText("Password"), "whatever password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(/Too many concurrent logins from your address/)).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Sign in" })).toBeEnabled(), { timeout: 3000 });
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(/The server is busy; try again in 2 seconds\./)).toBeInTheDocument();
  });
});

describe("account", () => {
  it("asks for the current password to create a token and reports a wrong one on the field", async () => {
    const { user, calls } = renderApp({
      path: "/account",
      signedInAs: account(),
      routes: {
        "GET /account/tokens": () => jsonResponse(200, { items: [] }),
        "POST /account/tokens": () => errorResponse(400, "invalid_current_password", "current password is incorrect"),
      },
    });
    await user.click(await screen.findByRole("button", { name: "New token" }));
    await user.type(screen.getByLabelText("Name"), "ci");
    await user.click(screen.getByRole("button", { name: "Create token" }));
    expect(screen.getByText("Enter your password.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "POST" && c.path === "/account/tokens")).toBe(false);

    await user.type(screen.getByLabelText("Your password"), "not my password");
    await user.click(screen.getByRole("button", { name: "Create token" }));
    expect(await screen.findByText("The password is wrong.")).toBeInTheDocument();
  });

  it("revokes tokens on password change by default and says how many", async () => {
    const { user, calls } = renderApp({
      path: "/account",
      signedInAs: account(),
      routes: {
        "GET /account/tokens": () => jsonResponse(200, { items: [] }),
        "POST /account/password": () =>
          jsonResponse(200, { token: "t2", expires_at: "", session_expires_at: "", account: account(), tokens_revoked: 2 }),
      },
    });
    const revoke = await screen.findByLabelText("Also revoke all my API tokens");
    expect(revoke).toBeChecked();
    await user.type(screen.getByLabelText("Current password"), "old password 123");
    await user.type(screen.getByLabelText("New password"), "new password 456");
    await user.type(screen.getByLabelText("Repeat new password"), "new password 456");
    await user.click(screen.getByRole("button", { name: "Change password" }));
    expect(await screen.findByText(/2 API tokens were revoked/)).toBeInTheDocument();
    expect(calls.find((c) => c.path === "/account/password")?.body).toMatchObject({ revoke_tokens: true });
  });
});

describe("jobs and the developer role", () => {
  it("hides the log and cancel of a restricted job from developers", async () => {
    const clone = job({ id: 41, kind: "project.clone", status: "running", restricted: true, created_by: 9, created_by_username: "root" });
    const { calls } = renderApp({
      path: "/projects/5",
      signedInAs: account({ id: 2, role: "developer", username: "dev" }),
      routes: {
        "GET /projects/{id}": () => jsonResponse(200, project),
        "GET /jobs": () => jsonResponse(200, { items: [clone] }),
        "GET /jobs/{id}": () => jsonResponse(200, clone),
      },
    });
    const inline = await screen.findByLabelText("Job 41");
    expect(await within(inline).findByText(/only administrators can see its log or cancel it/)).toBeInTheDocument();
    expect(within(inline).getByText(/started by root/)).toBeInTheDocument();
    expect(within(inline).queryByRole("button", { name: "Cancel job" })).toBeNull();
    expect(calls.some((c) => c.path === "/jobs/41/log")).toBe(false);
  });

  it("streams another account's pull for a developer but offers no cancel", async () => {
    const pull = job({ id: 42, status: "running", created_by: 9, created_by_username: "root" });
    const log = streamResponse();
    renderApp({
      path: "/projects/5",
      signedInAs: account({ id: 2, role: "developer", username: "dev" }),
      routes: {
        "GET /projects/{id}": () => jsonResponse(200, { ...project, status: "pulling", last_commit: "abc" }),
        "GET /jobs": () => jsonResponse(200, { items: [pull] }),
        "GET /jobs/{id}": () => jsonResponse(200, pull),
        "GET /jobs/{id}/log": () => log.response,
      },
    });
    const inline = await screen.findByLabelText("Job 42");
    log.push("Fetching origin\n");
    expect(await within(inline).findByText("Fetching origin")).toBeInTheDocument();
    expect(within(inline).queryByRole("button", { name: "Cancel job" })).toBeNull();
    expect(within(inline).getByText("Cannot cancel")).toBeInTheDocument();
  });
});

describe("servers", () => {
  it("explains that servers run builds when no successful build exists", async () => {
    const { user } = renderApp({
      path: "/servers",
      signedInAs: account(),
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New server" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    expect(await within(dialog).findByText("No successful builds yet")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Create server" })).toBeDisabled();
  });

  it("creates a server from the newest successful build of a project", async () => {
    const older = buildFixture({ id: 9, commit: "0d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e" });
    const { user, calls } = renderApp({
      path: "/servers",
      signedInAs: account(),
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [projectFixture()] }),
        "GET /builds": () => jsonResponse(200, { items: [build, older] }),
        "POST /servers": () => jsonResponse(201, { ...server, restart_required: false, state: "missing" }),
        "GET /servers/{id}": () => jsonResponse(200, { ...server, restart_required: false, state: "missing" }),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
      },
    });
    await user.click((await screen.findAllByRole("button", { name: "New server" }))[0]);
    const dialog = await screen.findByRole("dialog", { name: "New server" });
    const select = await within(dialog).findByLabelText("Build");
    expect(within(select).getAllByRole("option").map((o) => (o as HTMLOptionElement).value)).toEqual(["12", "9"]);
    expect(select).toHaveValue("12");
    await user.type(within(dialog).getByLabelText("Name"), "echo");
    await user.click(within(dialog).getByRole("button", { name: "Create server" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST" && c.path === "/servers")?.body).toEqual({ name: "echo", build_id: 12, desired_state: "stopped" }));
  });

  it("shows restart_required and a 429 on the log stream inside the viewer with a retry", async () => {
    let attempts = 0;
    const { user, calls } = renderApp({
      path: "/servers/3",
      signedInAs: account(),
      routes: {
        "GET /servers/{id}": () => jsonResponse(200, server),
        "GET /projects": () => jsonResponse(200, { items: [] }),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
        "GET /servers/{id}/stats": () => errorResponse(409, "conflict", "the server is not running"),
        "GET /servers/{id}/runtime": () => errorResponse(409, "conflict", "the server is not running"),
        "GET /servers/{id}/logs": () => {
          attempts++;
          return errorResponse(429, "rate_limited", "too many open log streams; close one and retry", { headers: { "Retry-After": "5" } });
        },
      },
    });
    expect(await screen.findByText("Restart required")).toBeInTheDocument();
    const viewer = (await screen.findByRole("log")).parentElement as HTMLElement;
    expect(await within(viewer).findByText(/Too many open log streams; close one and retry\. Try again in 5 seconds\./)).toBeInTheDocument();
    const logCall = calls.find((c) => c.path === "/servers/3/logs");
    expect(logCall?.query.get("tail")).toBe("1000");
    expect(logCall?.query.get("follow")).toBe("1");
    await user.click(within(viewer).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(attempts).toBe(2));
    // A race with a stopped container shows as text, not an error.
    expect(await screen.findByText(/The server is not running\. Statistics are available/)).toBeInTheDocument();
    expect(await screen.findByText(/The server is not running\. The game reports its runtime/)).toBeInTheDocument();
  });

  it("shows the import error the daemon returns, naming the entry", async () => {
    const { user, container } = renderApp({
      path: "/servers",
      signedInAs: account(),
      routes: {
        "GET /servers": () => jsonResponse(200, { items: [] }),
        "GET /projects": () => jsonResponse(200, { items: [] }),
        "POST /servers/import": () =>
          errorResponse(400, "validation_failed", 'project "kart" has no successful build', { field: "servers[1].project" }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Import" }));
    const doc = { format_version: 2, servers: [{ name: "a", project: "arena" }, { name: "b", project: "kart" }] };
    const input = container.ownerDocument.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, new File([JSON.stringify(doc)], "servers.json", { type: "application/json" }));
    await user.click(await screen.findByRole("button", { name: "Import 2 servers" }));
    expect(await screen.findByText("Server 2 (b), project:")).toBeInTheDocument();
    expect(screen.getByText(/Project "kart" has no successful build\. Nothing was imported\./)).toBeInTheDocument();
  });
});

describe("static file replace semantics", () => {
  it("sends replace=1 only after confirmation and offers Replace on a 409", async () => {
    const uploads: string[] = [];
    const { user, container } = renderApp({
      path: "/static",
      signedInAs: account(),
      routes: {
        "GET /static": () => jsonResponse(200, { items: [{ name: "maps.zip", size_bytes: 10, modified_at: "2026-11-01T00:00:00Z", is_zip: true }], used_bytes: 10, quota_bytes: 0, file_count: 1, max_files: 2000 }),
        "POST /static": (req) => {
          const name = (req.body as FormData).get("files") instanceof File ? ((req.body as FormData).get("files") as File).name : "";
          const replace = req.query.get("replace") === "1";
          uploads.push(`${name}${replace ? " replace" : ""}`);
          if (name === "late.bin" && !replace) return errorResponse(409, "conflict", "a file with that name already exists; upload with replace=1 to overwrite it");
          return jsonResponse(201, { items: [{ name, size_bytes: 1, modified_at: "2026-11-02T00:00:00Z", is_zip: false }], used_bytes: 11, quota_bytes: 0, file_count: 2, max_files: 2000 });
        },
      },
    });
    await screen.findByText("ZIP archive");
    const input = container.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, [new File(["a"], "maps.zip"), new File(["b"], "late.bin")]);
    await user.click(screen.getByRole("button", { name: /Upload 2 files/ }));
    const confirm = await screen.findByRole("alertdialog", { name: "Replace existing files" });
    await user.click(within(confirm).getByRole("button", { name: "Replace and upload" }));
    const queue = screen.getByRole("list", { name: "Upload queue" });
    expect(await within(queue).findByText("Name exists")).toBeInTheDocument();
    expect(uploads).toEqual(["maps.zip replace", "late.bin"]);
    await user.click(within(queue).getByRole("button", { name: "Replace the stored late.bin" }));
    await waitFor(() => expect(uploads).toEqual(["maps.zip replace", "late.bin", "late.bin replace"]));
  });
});
