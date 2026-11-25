import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { browserTransport, type Transport } from "@/api/client";
import { AppRoot } from "@/app/app-root";
import { createServices } from "@/app/services";
import { captureSetupToken, setupTokenFromHash } from "@/lib/setup-token";
import "@/index.css";

// A setup link's token leaves the address bar and the browser's history
// before anything else runs (see lib/setup-token.ts).
captureSetupToken(window.location, (url) => window.history.replaceState(window.history.state, "", url));
// A setup link pasted into an open /setup page changes only the fragment and
// does not load the page: load it again so the token is taken the same way.
window.addEventListener("hashchange", () => {
  if (window.location.pathname === "/setup" && setupTokenFromHash(window.location.hash)) window.location.reload();
});

function storage(kind: "localStorage" | "sessionStorage"): Storage | null {
  try {
    return window[kind];
  } catch {
    return null;
  }
}

async function transport(): Promise<Transport> {
  const real = browserTransport();
  // import.meta.env.DEV is a build-time constant: in production this branch
  // and the mock module are removed from the bundle (vite.config.ts checks).
  if (import.meta.env.DEV && import.meta.env.VITE_API_MOCK === "1") {
    const { createMockTransport } = await import("@/mock/transport");
    return createMockTransport(real);
  }
  return real;
}

async function main() {
  const services = createServices({
    transport: await transport(),
    local: storage("localStorage"),
    session: storage("sessionStorage"),
    channel: typeof BroadcastChannel === "function" ? new BroadcastChannel("golem_deploy_session") : null,
    document,
  });
  // A new tab in the default (per-tab) storage mode asks open tabs for their session.
  await services.session.requestFromPeers();

  const root = document.getElementById("root");
  if (!root) throw new Error("missing #root");
  createRoot(root).render(
    <StrictMode>
      <AppRoot services={services} />
    </StrictMode>,
  );
}

void main();
