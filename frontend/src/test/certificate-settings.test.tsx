import { screen, waitFor, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { ACME, GameCertificate, PanelCertificate } from "@/api/types.gen";
import { account, errorResponse, job, jsonResponse, streamResponse, type Handler } from "@/test/fake-api";
import { certFixture } from "@/test/fixtures";
import { renderApp } from "@/test/render-app";

const game: GameCertificate = { mode: "self_signed", certificate: null, game_host: "game.example.com" };
const panel: PanelCertificate = {
  mode: "self_signed", configurable: true, hosts: ["panel.example.com", "mirror.example.com"],
  certificate: certFixture(), covers_hosts: false,
};
const acme: ACME = {
  email: "", staging: false, terms_agreed: true,
  directory_url: "https://acme-v02.api.letsencrypt.org/directory",
  http01_available: true, tls_alpn01_available: false, certificates: [],
};
const issued = certFixture({ issuer: "CN=Test CA", self_signed: false, fingerprint_sha256: "88".repeat(32), dns_names: [game.game_host] });
const issuing = job({ kind: "certificate.issue", subject_type: "certificate", subject_id: "acme", restricted: true, status: "running" });

function settings(extra: Record<string, Handler> = {}) {
  return renderApp({
    path: "/admin/settings", signedInAs: account(),
    routes: {
      "GET /settings": () => jsonResponse(200, { registration_enabled: false, game_host: "", effective_game_host: game.game_host }),
      "GET /certificates/game": () => jsonResponse(200, game),
      "GET /certificates/panel": () => jsonResponse(200, panel),
      "GET /certificates/acme": () => jsonResponse(200, acme),
      ...extra,
    },
  });
}

describe("automatic game certificates", () => {
  it.each([
    ["acme", true, true],
    ["custom", true, true],
    ["self_signed", true, false],
    ["acme", false, false],
    ["custom", false, false],
  ] as const)("warns about the mounted panel-valid key in %s mode with matching host %s", async (mode, sameHost, warns) => {
    settings({
      "GET /certificates/game": () => jsonResponse(200, { ...game, mode, certificate: mode === "self_signed" ? null : issued }),
      "GET /certificates/panel": () => jsonResponse(200, { ...panel, hosts: sameHost ? [game.game_host] : panel.hosts }),
    });
    const region = within(await screen.findByRole("region", { name: "Game certificates" }));
    const panelRegion = within(await screen.findByRole("region", { name: "Panel certificate" }));
    await panelRegion.findAllByText(sameHost ? game.game_host : panel.hosts.join(", "));
    if (warns) {
      expect(await region.findByText(/Every game container can read/)).toHaveTextContent("--public-host");
    } else {
      expect(region.queryByText(/Every game container can read/)).not.toBeInTheDocument();
    }
  });

  it.each([
    ["acme", "panel.example.com", ["PANEL.example.com"], [], true],
    ["custom", "panel.example.com", ["*.example.com"], [], true],
    ["custom", "deep.panel.example.com", ["*.example.com"], [], false],
    ["custom", "example.com", ["*.example.com"], [], false],
    ["acme", "2001:db8::1", [], ["2001:db8::1"], true],
    ["custom", "192.0.2.5", ["192.0.2.5"], [], false],
  ] satisfies [GameCertificate["mode"], string, string[], string[], boolean][])(
    "matches the installed %s certificate's SANs against panel host %s",
    async (mode, host, dns_names, ip_addresses, warns) => {
      settings({
        "GET /certificates/game": () => jsonResponse(200, { ...game, mode, certificate: { ...issued, dns_names, ip_addresses } }),
        "GET /certificates/panel": () => jsonResponse(200, { ...panel, hosts: [host] }),
      });
      const region = within(await screen.findByRole("region", { name: "Game certificates" }));
      const panelRegion = within(await screen.findByRole("region", { name: "Panel certificate" }));
      await panelRegion.findAllByText(host);
      if (warns) {
        expect(await region.findByText(/Every game container can read/)).toBeInTheDocument();
      } else {
        expect(region.queryByText(/Every game container can read/)).not.toBeInTheDocument();
      }
    },
  );

  it.each([
    [{ game_host: "" }, {}, /Game host missing/],
    [{}, { terms_agreed: false }, /Accept and save the subscriber agreement/],
    [{}, { http01_available: false }, /Make the daemon reachable on port 80/],
  ] satisfies [Partial<GameCertificate>, Partial<ACME>, RegExp][])("explains unmet requirements before issuance", async (gameOverrides, acmeOverrides, detail) => {
    settings({
      "GET /certificates/game": () => jsonResponse(200, { ...game, ...gameOverrides }),
      "GET /certificates/acme": () => jsonResponse(200, { ...acme, ...acmeOverrides }),
    });
    const region = within(await screen.findByRole("region", { name: "Game certificates" }, { timeout: 5000 }));
    await region.findByText(detail);
    expect(region.getByRole("button", { name: "Switch game to automatic certificates" })).toBeDisabled();
  });

  it.each(["success", "error"] as const)("follows the 202 response through %s", async (outcome) => {
    let completed = false;
    const log = streamResponse();
    const automatic = { ...game, mode: "acme", certificate: issued };
    const { user, calls } = settings({
      "GET /certificates/game": () => jsonResponse(200, completed && outcome === "success" ? automatic : game),
      "PUT /certificates/game": () => jsonResponse(202, { certificate: game, job: issuing }),
      "GET /jobs/{id}": () => jsonResponse(200, completed
        ? { ...issuing, status: outcome, detail: outcome === "error" ? "HTTP-01 challenge timed out. Check port 80." : "" }
        : issuing),
      "GET /jobs/{id}/log": () => log.response,
    });
    const region = within(await screen.findByRole("region", { name: "Game certificates" }, { timeout: 5000 }));
    await user.click(await region.findByRole("button", { name: "Switch game to automatic certificates" }));
    const inline = within(await region.findByLabelText("Job 77"));
    expect(inline.getByText("Running")).toBeInTheDocument();
    expect(region.getByText("Self-signed")).toBeInTheDocument();
    log.push("Checking the challenge\n");
    expect(await inline.findByText("Checking the challenge")).toBeInTheDocument();
    expect(calls.find((call) => call.method === "PUT")?.body).toEqual({ mode: "acme" });
    expect(calls.find((call) => call.path === "/jobs/77/log")?.query.get("follow")).toBe("1");
    completed = true;
    log.close();
    if (outcome === "error") {
      expect(await inline.findByRole("alert", {}, { timeout: 2600 })).toHaveTextContent("HTTP-01 challenge timed out. Check port 80.");
      expect(region.getByText("Self-signed")).toBeInTheDocument();
    } else {
      expect(await region.findByText("Automatic", {}, { timeout: 2600 })).toBeInTheDocument();
      expect(region.getByText("CN=Test CA")).toBeInTheDocument();
    }
  });

  it("shows a refused issuance request", async () => {
    const { user } = settings({
      "PUT /certificates/game": () => errorResponse(409, "job_in_progress", "another certificate job is running"),
    });
    const region = within(await screen.findByRole("region", { name: "Game certificates" }, { timeout: 5000 }));
    await user.click(await region.findByRole("button", { name: "Switch game to automatic certificates" }));
    expect(await region.findByText("Another certificate job is running.")).toBeInTheDocument();
    expect(region.getByText("Self-signed")).toBeInTheDocument();
  });
});

describe("panel TLS and ACME settings", () => {
  it.each([
    ["none", /plain HTTP on loopback or behind a reverse proxy/],
    ["files", /operator supplies this certificate with --tls-cert and --tls-key/],
  ] as const)("explains a fixed panel TLS mode (%s)", async (mode, detail) => {
    settings({
      "GET /certificates/panel": () => jsonResponse(200, { ...panel, mode, configurable: false, certificate: mode === "none" ? null : panel.certificate }),
    });
    const region = within(await screen.findByRole("region", { name: "Panel certificate" }, { timeout: 5000 }));
    expect(await region.findByText(detail)).toBeInTheDocument();
    expect(region.getByText("panel.example.com, mirror.example.com")).toBeInTheDocument();
    expect(region.queryByRole("button", { name: /switch panel/i })).toBeNull();
  });

  it("follows panel issuance and prompts reload when the installed certificate changes", async () => {
    let completed = false;
    const log = streamResponse();
    const automatic = { ...panel, mode: "acme", certificate: issued, covers_hosts: true };
    const { user } = settings({
      "GET /certificates/panel": () => jsonResponse(200, completed ? automatic : panel),
      "PUT /certificates/panel": () => jsonResponse(202, { certificate: panel, job: issuing }),
      "GET /jobs/{id}": () => jsonResponse(200, completed ? { ...issuing, status: "success" } : issuing),
      "GET /jobs/{id}/log": () => log.response,
    });
    const region = within(await screen.findByRole("region", { name: "Panel certificate" }, { timeout: 5000 }));
    expect(await region.findByText("Does not cover every panel host")).toBeInTheDocument();
    await user.click(await region.findByRole("button", { name: "Switch panel to automatic certificates" }));
    const inline = within(await region.findByLabelText("Job 77"));
    expect(inline.getByText("Running")).toBeInTheDocument();
    expect(region.queryByText("Reload the panel")).toBeNull();
    completed = true;
    log.close();
    expect(await region.findByText("Covers every panel host", {}, { timeout: 2600 })).toBeInTheDocument();
    expect(region.getByText("Reload the panel")).toBeInTheDocument();
  });

  it("applies the synchronous panel response and prompts reload", async () => {
    const { user, calls } = settings({
      "GET /certificates/panel": () => jsonResponse(200, { ...panel, mode: "acme", certificate: issued }),
      "PUT /certificates/panel": () => jsonResponse(200, { certificate: panel, job: null }),
    });
    const region = within(await screen.findByRole("region", { name: "Panel certificate" }, { timeout: 5000 }));
    await user.click(await region.findByRole("button", { name: "Switch panel to self-signed" }));
    expect(await region.findByText("Reload the panel")).toBeInTheDocument();
    expect(calls.find((call) => call.method === "PUT")?.body).toEqual({ mode: "self_signed" });
  });

  it("saves explicit agreement, staging and email, and handles a field error", async () => {
    let attempts = 0;
    const { user, calls } = settings({
      "GET /certificates/acme": () => jsonResponse(200, { ...acme, terms_agreed: false }),
      "PUT /certificates/acme": (req) => ++attempts === 1
        ? errorResponse(400, "validation_failed", "enter a single contact address", { field: "email" })
        : jsonResponse(200, { ...acme, ...(req.body as object), directory_url: "https://acme-staging-v02.api.letsencrypt.org/directory" }),
    });
    const region = within(await screen.findByRole("region", { name: "Automatic certificates" }, { timeout: 5000 }));
    const email = await region.findByRole("textbox", { name: "Contact email" });
    expect(region.getByRole("link", { name: "Let's Encrypt subscriber agreement" })).toHaveAttribute("href", "https://letsencrypt.org/repository/");
    await user.type(email, "ops@example.com");
    await user.click(region.getByRole("switch", { name: "Use staging" }));
    await user.click(region.getByRole("checkbox", { name: /I agree to the/ }));
    const save = region.getByRole("button", { name: "Save automatic certificate settings" });
    await user.click(save);
    expect(await region.findByText("Enter a single contact address.")).toBeInTheDocument();
    expect(calls.find((call) => call.method === "PUT")?.body).toEqual({ email: "ops@example.com", staging: true, terms_agreed: true });
    await user.click(save);
    await waitFor(() => expect(save).toBeDisabled());
    expect(await screen.findByRole("button", { name: "Switch game to automatic certificates" })).toBeEnabled();
  });

  it("shows a managed certificate, its last failure and expiry, and refreshes it after renewal", async () => {
    let renewed = false;
    const log = streamResponse();
    const lastAttempt = "2026-11-22T12:00:00Z";
    const nearExpiry = new Date(Date.now() + 48 * 3600 * 1000).toISOString();
    const { user } = settings({
      "GET /certificates/acme": () => jsonResponse(200, {
        ...acme, directory_url: "https://acme.example.test/directory", certificates: [{
          identifier: "game.example.com", used_by: ["game", "panel"],
          status: renewed ? "issued" : "error", certificate: renewed ? issued : { ...issued, not_after: nearExpiry },
          last_error: renewed ? "" : "Port 80 was unreachable", last_attempt_at: lastAttempt,
        }],
      }),
      "POST /certificates/acme/renew": () => jsonResponse(202, { ...issuing, kind: "certificate.renew" }),
      "GET /jobs/{id}": () => jsonResponse(200, { ...issuing, kind: "certificate.renew", status: renewed ? "success" : "running" }),
      "GET /jobs/{id}/log": () => log.response,
    });
    const region = within(await screen.findByRole("region", { name: "Automatic certificates" }, { timeout: 5000 }));
    const table = within(await region.findByRole("table", { name: "Managed certificates" }));
    expect(table.getByText("Game servers, Panel")).toBeInTheDocument();
    expect(table.getByText("Port 80 was unreachable")).toBeInTheDocument();
    expect(table.getByText(/Expiring soon/)).toBeInTheDocument();
    expect(region.getByText("https://acme.example.test/directory")).toBeInTheDocument();
    expect(region.getByRole("switch", { name: "Use staging" })).toBeDisabled();
    await user.click(region.getByRole("button", { name: "Renew now" }));
    expect(await region.findByText("Renew certificates")).toBeInTheDocument();
    renewed = true;
    log.close();
    expect(await table.findByText("Issued", {}, { timeout: 2600 })).toBeInTheDocument();
    expect(table.queryByText("Port 80 was unreachable")).toBeNull();
  });

  it("shows ACME and panel read failures and keeps the actions unavailable", async () => {
    settings({
      "GET /certificates/panel": () => errorResponse(503, "service_unavailable", "panel certificate unavailable"),
      "GET /certificates/acme": () => errorResponse(503, "service_unavailable", "automatic settings unavailable"),
    });
    expect(await screen.findByText("Panel certificate unavailable.")).toBeInTheDocument();
    expect(await within(await screen.findByRole("region", { name: "Automatic certificates" }, { timeout: 5000 })).findByText("Automatic settings unavailable.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Switch game to automatic certificates" })).toBeDisabled();
  });

  it("keeps certificate configuration off the developer route", async () => {
    const { calls } = renderApp({ path: "/admin/settings", signedInAs: account({ role: "developer" }) });
    expect(await screen.findByText(/administrator access/i)).toBeInTheDocument();
    expect(calls.some((call) => call.path.startsWith("/certificates"))).toBe(false);
  });
});
