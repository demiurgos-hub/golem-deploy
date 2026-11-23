/** Returns the URL when it is an absolute http(s) URL, otherwise null. */
export function safeHttpUrl(value: string): string | null {
  try {
    const u = new URL(value);
    return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
  } catch {
    return null;
  }
}

/** Accepts only same-origin paths (no protocol-relative "//host"), never the auth pages. */
export function safeRedirectPath(value: unknown): string | null {
  if (typeof value !== "string") return null;
  if (!value.startsWith("/") || value.startsWith("//") || value.startsWith("/\\")) return null;
  if (value.startsWith("/login") || value.startsWith("/register")) return null;
  return value;
}
