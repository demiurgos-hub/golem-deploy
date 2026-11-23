import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { browserTransport, type Transport } from "@/api/client";
import { AppRoot } from "@/app/app-root";
import { createServices } from "@/app/services";
import "@/index.css";

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
