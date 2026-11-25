/**
 * The one-time setup token of a "/setup#t=<token>" link (golem-deploy admin
 * setup-link). main.tsx moves it out of the address bar into this holder as
 * the first thing the application does, before the router starts. The setup
 * page reads it from here. It is never put back into a URL or into storage.
 */

let held: string | null = null;

/** The setup token in a "#t=<token>" fragment, or null. */
export function setupTokenFromHash(hash: string): string | null {
  const params = new URLSearchParams(hash.startsWith("#") ? hash.slice(1) : hash);
  return params.get("t") || null;
}

/**
 * On the setup page with a "t" parameter in the fragment: keeps the token
 * and replaces the history entry with the same address without the
 * fragment. Returns whether it took a token.
 */
export function captureSetupToken(
  location: { pathname: string; search: string; hash: string },
  replace: (pathAndSearch: string) => void,
): boolean {
  if (location.pathname !== "/setup") return false;
  const token = setupTokenFromHash(location.hash);
  if (!token) return false;
  replace(location.pathname + location.search);
  held = token;
  return true;
}

/** The kept setup token, or null. */
export function heldSetupToken(): string | null {
  return held;
}

/** Forgets the kept token once it is used up or refused. */
export function clearSetupToken(): void {
  held = null;
}
