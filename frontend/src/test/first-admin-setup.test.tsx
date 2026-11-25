import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { STORAGE_KEY } from "@/api/session";
import { captureSetupToken, clearSetupToken, heldSetupToken, setupTokenFromHash } from "@/lib/setup-token";
import { SETUP_LINK_COMMAND } from "@/pages/auth";
import { account, errorResponse, fakeJwt, jsonResponse, type Handler } from "@/test/fake-api";
import { renderApp } from "@/test/render-app";

const TOKEN = "ZCcxF9XoheDIpXxaidEeplkXKPKqtZJkA4dNKJYFXeg";

function sessionFor(username: string) {
  return {
    token: fakeJwt(Math.floor(Date.now() / 1000) + 3600),
    expires_at: "",
    session_expires_at: "",
    account: account({ username }),
  };
}

describe("setup token in the fragment", () => {
  it("reads t= and nothing else", () => {
    expect(setupTokenFromHash(`#t=${TOKEN}`)).toBe(TOKEN);
    expect(setupTokenFromHash(`t=${TOKEN}`)).toBe(TOKEN);
    expect(setupTokenFromHash("")).toBeNull();
    expect(setupTokenFromHash("#t=")).toBeNull();
    expect(setupTokenFromHash("#x=1")).toBeNull();
  });

  it("is taken only on the setup page, and the address is replaced without the fragment", () => {
    clearSetupToken();
    const replaced: string[] = [];
    const replace = (url: string) => replaced.push(url);
    expect(captureSetupToken({ pathname: "/login", search: "", hash: `#t=${TOKEN}` }, replace)).toBe(false);
    expect(captureSetupToken({ pathname: "/setup", search: "", hash: "#x=1" }, replace)).toBe(false);
    expect(captureSetupToken({ pathname: "/setup", search: "", hash: "" }, replace)).toBe(false);
    expect(replaced).toEqual([]);
    expect(heldSetupToken()).toBeNull();

    expect(captureSetupToken({ pathname: "/setup", search: "?a=1", hash: `#t=${TOKEN}` }, replace)).toBe(true);
    expect(replaced).toEqual(["/setup?a=1"]);
    expect(heldSetupToken()).toBe(TOKEN);
    clearSetupToken();
    expect(heldSetupToken()).toBeNull();
  });
});

/** GET /auth/options while no admin exists. */
const setupRequired: Record<string, Handler> = {
  "GET /auth/options": () => jsonResponse(200, { registration_enabled: false, setup_required: true }),
};

describe("first admin setup page", () => {
  it("reads the token, removes it from the address and signs the new admin in", async () => {
    const { user, history, calls, services } = renderApp({
      path: `/setup#t=${TOKEN}`,
      routes: {
        ...setupRequired,
        "POST /setup": (req) => jsonResponse(201, sessionFor((req.body as { username: string }).username)),
        "GET /projects": () => jsonResponse(200, { items: [] }),
        "GET /servers": () => jsonResponse(200, { items: [] }),
      },
    });
    // The token left the address before the router started.
    expect(history.location.hash).toBe("");
    expect(history.location.pathname).toBe("/setup");
    expect(history.location.href).not.toContain(TOKEN);
    expect(await screen.findByRole("heading", { name: "Create the admin account" })).toBeInTheDocument();
    expect(history.location.href).not.toContain(TOKEN);

    // The rules of the registration form.
    await user.type(screen.getByLabelText("Username"), "a!");
    await user.type(screen.getByLabelText("Password"), "short");
    await user.type(screen.getByLabelText("Repeat password"), "other");
    await user.click(screen.getByRole("button", { name: "Create admin account" }));
    expect(screen.getByText(/Use 3 to 32 characters/)).toBeInTheDocument();
    expect(screen.getByText(/at least 12 bytes/)).toBeInTheDocument();
    expect(screen.getByText("The passwords do not match.")).toBeInTheDocument();
    expect(calls.some((c) => c.path === "/setup")).toBe(false);

    await user.clear(screen.getByLabelText("Username"));
    await user.type(screen.getByLabelText("Username"), "Root");
    await user.clear(screen.getByLabelText("Password"));
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.clear(screen.getByLabelText("Repeat password"));
    await user.type(screen.getByLabelText("Repeat password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Create admin account" }));

    await screen.findByRole("heading", { name: "Projects" });
    expect(history.location.pathname).toBe("/projects");
    const sent = calls.find((c) => c.path === "/setup");
    expect(sent?.body).toEqual({ token: TOKEN, username: "root", password: "correct horse battery" });
    // A public route: no credentials are sent.
    expect(sent?.headers.Authorization).toBeUndefined();
    expect(services.session.getToken()).toBeTruthy();
    expect(sessionStorage.getItem(STORAGE_KEY)).toBeTruthy();
    expect(localStorage.getItem(STORAGE_KEY)).toBeNull();
    // The token is not kept in storage.
    for (const store of [sessionStorage, localStorage]) {
      for (let i = 0; i < store.length; i++) expect(store.getItem(store.key(i) ?? "")).not.toContain(TOKEN);
    }
    // Used up: the holder is empty and no address has the token.
    expect(heldSetupToken()).toBeNull();
    expect(history.location.href).not.toContain(TOKEN);
  });

  it("explains an invalid link and shows the command for a new one", async () => {
    const { user } = renderApp({
      path: `/setup#t=${TOKEN}`,
      routes: {
        ...setupRequired,
        "POST /setup": () => errorResponse(403, "setup_link_invalid", "this setup link is missing, expired or already used"),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "root");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.type(screen.getByLabelText("Repeat password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Create admin account" }));
    expect(await screen.findByText(/This setup link is missing, expired or already used/)).toBeInTheDocument();
    expect(screen.getByText(SETUP_LINK_COMMAND)).toBeInTheDocument();
    expect(screen.getByText(/installer prints a setup link/)).toBeInTheDocument();
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
    expect(heldSetupToken()).toBeNull();
  });

  it("keeps the form for field errors from the daemon", async () => {
    const { user } = renderApp({
      path: `/setup#t=${TOKEN}`,
      routes: {
        ...setupRequired,
        "POST /setup": () => errorResponse(400, "validation_failed", "password must be at most 72 bytes", { field: "password" }),
      },
    });
    await user.type(await screen.findByLabelText("Username"), "root");
    await user.type(screen.getByLabelText("Password"), "correct horse battery");
    await user.type(screen.getByLabelText("Repeat password"), "correct horse battery");
    await user.click(screen.getByRole("button", { name: "Create admin account" }));
    expect(await screen.findByText("password must be at most 72 bytes")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create admin account" })).toBeEnabled();
  });

  it("explains a missing token without calling the daemon", async () => {
    const { calls } = renderApp({ path: "/setup", routes: setupRequired });
    expect(await screen.findByText(/This setup link is missing, expired or already used/)).toBeInTheDocument();
    expect(screen.getByText(SETUP_LINK_COMMAND)).toBeInTheDocument();
    expect(calls.some((c) => c.path === "/setup")).toBe(false);
  });

  it("says at once that an old link is used up once an admin exists", async () => {
    const { calls, history } = renderApp({ path: `/setup#t=${TOKEN}` });
    expect(await screen.findByText(/already has an admin account/)).toBeInTheDocument();
    expect(screen.getByText(/This setup link is missing, expired or already used/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute("href", "/login");
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
    expect(screen.queryByText(SETUP_LINK_COMMAND)).not.toBeInTheDocument();
    expect(calls.some((c) => c.path === "/setup")).toBe(false);
    expect(heldSetupToken()).toBeNull();
    expect(history.location.href).not.toContain(TOKEN);
  });

  it("does not show the form while the options load", async () => {
    let answer: (r: Response) => void = () => {};
    renderApp({
      path: `/setup#t=${TOKEN}`,
      routes: { "GET /auth/options": () => new Promise<Response>((resolve) => (answer = resolve)) },
    });
    expect(await screen.findByRole("status")).toBeInTheDocument();
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
    answer(jsonResponse(200, { registration_enabled: false, setup_required: true }));
    expect(await screen.findByRole("heading", { name: "Create the admin account" })).toBeInTheDocument();
  });

  it("shows the form when the options cannot be read", async () => {
    renderApp({
      path: `/setup#t=${TOKEN}`,
      routes: { "GET /auth/options": () => errorResponse(500, "internal_error", "internal error") },
    });
    // After the query's retries.
    expect(await screen.findByRole("heading", { name: "Create the admin account" }, { timeout: 8000 })).toBeInTheDocument();
  }, 10_000);

  it("sends a signed-in visitor to the home page", async () => {
    const { history } = renderApp({
      path: `/setup#t=${TOKEN}`,
      signedInAs: account(),
      routes: { "GET /projects": () => jsonResponse(200, { items: [] }), "GET /servers": () => jsonResponse(200, { items: [] }) },
    });
    await screen.findByRole("heading", { name: "Projects" });
    expect(history.location.pathname).toBe("/projects");
    expect(history.location.href).not.toContain(TOKEN);
  });
});

describe("login page while no admin exists", () => {
  it("says how to create the first admin", async () => {
    renderApp({
      path: "/login",
      routes: { "GET /auth/options": () => jsonResponse(200, { registration_enabled: false, setup_required: true }) },
    });
    expect(await screen.findByText(/This installation has no admin account yet/)).toBeInTheDocument();
    expect(screen.getByText(SETUP_LINK_COMMAND)).toBeInTheDocument();
  });

  it("says nothing about it once an admin exists", async () => {
    renderApp({ path: "/login" });
    await screen.findByText(/No account\? Ask an administrator/);
    expect(screen.queryByText(/no admin account yet/)).not.toBeInTheDocument();
  });
});
