import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import type { APIToken, Job, Project } from "@/api/types.gen";
import { STORAGE_KEY } from "@/api/session";
import { account, errorResponse, fakeJwt, job as makeJob, jsonResponse, streamResponse } from "@/test/fake-api";
import { renderApp } from "@/test/render-app";
import { projectFixture } from "@/test/fixtures";

const project: Project = projectFixture();

describe("login", () => {
  it("redirects to login, validates fields and returns to the intended page", async () => {
    const { user, history, calls } = renderApp({
      path: "/servers",
      routes: {
        "POST /auth/login": (req) =>
          jsonResponse(200, {
            token: fakeJwt(Math.floor(Date.now() / 1000) + 3600),
            expires_at: "",
            session_expires_at: "",
            account: account({ username: (req.body as { username: string }).username }),
          }),
        "GET /servers": () => jsonResponse(200, { items: [] }),
      },
    });

    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(history.location.pathname).toBe("/login");
    expect(history.location.search).toContain("redirect=%2Fservers");

    await user.type(screen.getByLabelText("Username"), "a!");
    await user.type(screen.getByLabelText("Password"), "short");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(screen.getByText(/Use 3 to 32 characters/)).toBeInTheDocument();
    expect(screen.getByText(/at least 12 bytes/)).toBeInTheDocument();
    expect(calls.some((c) => c.path === "/auth/login")).toBe(false);

    await user.clear(screen.getByLabelText("Username"));
    await user.type(screen.getByLabelText("Username"), "Alice");
    await user.clear(screen.getByLabelText("Password"));
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByRole("heading", { name: "Servers" })).toBeInTheDocument();
    expect(history.location.pathname).toBe("/servers");
    // The username is normalised like the daemon does; the token is per-tab by default.
    expect(calls.find((c) => c.path === "/auth/login")?.body).toEqual({ username: "alice", password: "correct horse battery" });
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeTruthy();
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
  });

  it("shows a generic message for invalid credentials", async () => {
    const { user } = renderApp({
      path: "/login",
      routes: { "POST /auth/login": () => errorResponse(401, "invalid_credentials", "invalid username or password") },
    });
    await user.type(await screen.findByLabelText("Username"), "alice");
    await user.type(screen.getByLabelText("Password"), "wrong password!!");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(/Sign-in failed. Check the username and password/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Sign in" })).toBeInTheDocument();
  });

  it("shows the wait from Retry-After when rate limited", async () => {
    const { user } = renderApp({
      path: "/login",
      routes: {
        "POST /auth/login": () => errorResponse(429, "rate_limited", "too many attempts", { headers: { "Retry-After": "30" } }),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "alice");
    await user.type(screen.getByLabelText("Password"), "whatever password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByText(/Try again in 30 seconds/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeDisabled();
  });

  it("keeps the token in localStorage with 'Keep me signed in'", async () => {
    const { user } = renderApp({
      path: "/login",
      routes: {
        "POST /auth/login": () =>
          jsonResponse(200, { token: fakeJwt(Math.floor(Date.now() / 1000) + 3600), expires_at: "", session_expires_at: "", account: account() }),
        "GET /projects": () => jsonResponse(200, { items: [] }),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "alice");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.click(screen.getByLabelText(/Keep me signed in/));
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Projects" });
    expect(localStorage.getItem(STORAGE_KEY)).toBeTruthy();
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeNull();
  });
});

describe("expired session", () => {
  it("returns to login once with a notice when requests fail with 401", async () => {
    const expired = () => errorResponse(401, "unauthenticated", "session expired");
    const { history } = renderApp({
      path: "/projects",
      signedInAs: account(),
      routes: { "GET /projects": expired, "GET /account": expired, "GET /status": expired },
    });
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.getAllByText(/Your session ended/)).toHaveLength(1);
    expect(history.location.search).toContain("redirect=%2Fprojects");
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeNull();
  });
});

describe("role-based navigation", () => {
  it("hides admin navigation from developers and explains a forced admin URL", async () => {
    renderApp({ path: "/account", signedInAs: account({ role: "developer", username: "dev" }), routes: { "GET /account/tokens": () => jsonResponse(200, { items: [] }) } });
    const nav = await screen.findByRole("complementary", { name: "Main navigation" });
    await within(nav).findByText("dev");
    expect(within(nav).getByRole("link", { name: "Projects" })).toBeInTheDocument();
    expect(within(nav).queryByRole("link", { name: /Accounts/ })).toBeNull();
    expect(within(nav).queryByText("Administration")).toBeNull();
  });

  it("shows a clean message on /admin/accounts for a developer", async () => {
    renderApp({ path: "/admin/accounts", signedInAs: account({ role: "developer" }) });
    expect(await screen.findByRole("heading", { name: "Administrator access required" })).toBeInTheDocument();
  });

  it("shows admin navigation to admins", async () => {
    renderApp({
      path: "/admin/accounts",
      signedInAs: account({ role: "admin" }),
      routes: { "GET /accounts": () => jsonResponse(200, { items: [account()] }) },
    });
    const nav = await screen.findByRole("complementary", { name: "Main navigation" });
    expect(await within(nav).findByRole("link", { name: /Accounts/ })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Accounts" })).toBeInTheDocument();
  });
});

describe("API tokens", () => {
  it("shows the secret exactly once", async () => {
    const created: APIToken = { id: 9, name: "ci", prefix: "gdp_1234abcd", created_at: new Date().toISOString(), last_used_at: null, expires_at: null };
    let items: APIToken[] = [];
    const { user, calls } = renderApp({
      path: "/account",
      signedInAs: account(),
      routes: {
        "GET /account/tokens": () => jsonResponse(200, { items }),
        "POST /account/tokens": () => {
          items = [created];
          return jsonResponse(201, { token: created, secret: "gdp_1234abcd5678ef901234abcd5678ef901234abcd5678ef90" });
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "New token" }));
    await user.type(screen.getByLabelText("Name"), "ci");
    await user.selectOptions(screen.getByLabelText("Expires"), "never");
    await user.type(screen.getByLabelText("Your password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Create token" }));

    const dialog = await screen.findByRole("dialog", { name: "Copy your new token" });
    expect(within(dialog).getByDisplayValue(/^gdp_1234abcd5678/)).toBeInTheDocument();
    expect(within(dialog).getByText(/only time the secret is shown/)).toBeInTheDocument();
    expect(calls.find((c) => c.method === "POST" && c.path === "/account/tokens")?.body).toEqual({
      name: "ci",
      current_password: "correct horse battery",
    });

    // Escape does not dismiss it by accident.
    await user.keyboard("{Escape}");
    expect(screen.getByRole("dialog", { name: "Copy your new token" })).toBeInTheDocument();

    await user.click(within(dialog).getByRole("button", { name: "I have copied the token" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.queryByDisplayValue(/^gdp_1234abcd5678/)).toBeNull();
    // The list shows only the prefix.
    expect(await screen.findByText("gdp_1234abcd")).toBeInTheDocument();
  });
});

describe("job-returning actions", () => {
  it("shows the started job inline with its live log", async () => {
    const job: Job = makeJob();
    const log = streamResponse();
    let jobState: Job = job;
    const { user, calls } = renderApp({
      path: "/projects/5",
      signedInAs: account(),
      routes: {
        "GET /projects/{id}": () => jsonResponse(200, project),
        "GET /jobs": () => jsonResponse(200, { items: [] }),
        "POST /projects/{id}/pull": () => {
          jobState = { ...job, status: "running", started_at: new Date().toISOString() };
          return jsonResponse(202, job);
        },
        "GET /jobs/{id}": () => jsonResponse(200, jobState),
        "GET /jobs/{id}/log": () => log.response,
      },
    });

    await screen.findByRole("heading", { name: /arena/ });
    await user.click(screen.getByRole("button", { name: "Pull" }));

    const inline = await screen.findByLabelText("Job 77");
    expect(within(inline).getByText("Pull")).toBeInTheDocument();
    expect(await within(inline).findByText("Running")).toBeInTheDocument();

    log.push("Fetching origin\nFast-forward");
    log.push("ing to 4f1c2a9\n");
    expect(await within(inline).findByText("Fetching origin")).toBeInTheDocument();
    expect(await within(inline).findByText("Fast-forwarding to 4f1c2a9")).toBeInTheDocument();
    expect(calls.find((c) => c.path === "/jobs/77/log")?.query.get("follow")).toBe("1");

    jobState = { ...jobState, status: "success", finished_at: new Date().toISOString() };
    log.close();
    expect(await within(inline).findByText("Succeeded", {}, { timeout: 4000 })).toBeInTheDocument();
  });
});

describe("static file uploads", () => {
  it("queues files from separate selections and rejects bad names before uploading", async () => {
    const { user, container } = renderApp({
      path: "/static",
      signedInAs: account(),
      routes: {
        "GET /static": () =>
          jsonResponse(200, { items: [{ name: "maps.zip", size_bytes: 10, modified_at: "2026-11-01T00:00:00Z", is_zip: true }], used_bytes: 10, quota_bytes: 0, file_count: 1, max_files: 2000 }),
      },
    });
    await screen.findByText("maps.zip");
    const input = container.querySelector<HTMLInputElement>("input[type=file]");
    if (!input) throw new Error("no file input");
    await user.upload(input, new File(["a"], "level.bin"));
    await user.upload(input, [new File(["b"], "maps.zip"), new File(["c"], "bad name.txt")]);
    const queue = screen.getByRole("list", { name: "Upload queue" });
    expect(within(queue).getByText("level.bin")).toBeInTheDocument();
    expect(within(queue).getByText("maps.zip")).toBeInTheDocument();
    expect(within(queue).getByText("Replaces existing")).toBeInTheDocument();
    expect(within(queue).getByText(/may only contain letters/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Upload 2 files/ })).toBeEnabled();
  });
});
