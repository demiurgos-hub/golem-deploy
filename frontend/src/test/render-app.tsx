import { render } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory } from "@tanstack/react-router";
import { AppRoot } from "@/app/app-root";
import { createServices } from "@/app/services";
import type { Account, Status } from "@/api/types.gen";
import { account, currentAccount, fakeJwt, fakeTransport, jsonResponse, type Handler } from "@/test/fake-api";
import { inspectorOff } from "@/test/fixtures";
import { captureSetupToken, clearSetupToken } from "@/lib/setup-token";

export const STATUS: Status = {
  version: "v0.0.0-test",
  docker_reachable: true,
  registration_enabled: false,
  counts: { projects: 0, servers: 0, active_jobs: 0, pending_accounts: 0 },
};

export function renderApp({
  path,
  routes = {},
  signedInAs,
}: {
  path: string;
  routes?: Record<string, Handler>;
  /** Start with a session for this account. */
  signedInAs?: Account;
}) {
  const me = signedInAs ?? account();
  const { transport, calls } = fakeTransport({
    "GET /account": () => jsonResponse(200, currentAccount(me)),
    "GET /status": () => jsonResponse(200, STATUS),
    "GET /auth/options": () => jsonResponse(200, { registration_enabled: false, setup_required: false }),
    // Pages list builds on the side (first-run guide, project page); tests that care override these.
    "GET /builds": () => jsonResponse(200, { items: [] }),
    "GET /projects/{id}/builds": () => jsonResponse(200, { items: [] }),
    "GET /backups": () => jsonResponse(200, { items: [], total_bytes: 0, max_content_bytes: 2048 * 1024 * 1024 }),
    // The server pages and Settings read the Inspector; it is off unless a test says otherwise.
    "GET /inspector": () => jsonResponse(200, inspectorOff()),
    ...routes,
  });
  const services = createServices({ transport, local: localStorage, session: sessionStorage, channel: null, document: null });
  if (signedInAs) services.session.start(fakeJwt(Math.floor(Date.now() / 1000) + 12 * 3600), false);
  const history = createMemoryHistory({ initialEntries: [path] });
  // What main.tsx does with the browser's address before the router starts.
  clearSetupToken();
  captureSetupToken(history.location, (url) => history.replace(url));
  const user = userEvent.setup();
  const view = render(<AppRoot services={services} history={history} />);
  return { ...view, services, calls, history, user };
}
