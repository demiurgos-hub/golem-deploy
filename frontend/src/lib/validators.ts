/**
 * Client-side mirrors of the daemon's validation rules (docs/API.md). They
 * give immediate feedback; the daemon remains the authority.
 */

export const USERNAME_RE = /^[a-z0-9][a-z0-9._-]{2,31}$/;
export const RESOURCE_NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
export const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
export const STATIC_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

export const PASSWORD_MIN_BYTES = 12;
export const PASSWORD_MAX_BYTES = 72;
/** POST /static accepts at most 1 GiB per file. */
export const STATIC_UPLOAD_LIMIT_BYTES = 1024 * 1024 * 1024;

const encoder = new TextEncoder();

export function utf8Length(s: string): number {
  return encoder.encode(s).length;
}

/** Returns an error message, or null when valid. */
export type Validator = (value: string) => string | null;

/** The daemon trims and lower-cases usernames before checking them. */
export function normalizeUsername(u: string): string {
  return u.trim().toLowerCase();
}

/** A daemon message ("repository URL is required") as a sentence. */
function asSentence(message: string): string {
  return message.charAt(0).toUpperCase() + message.slice(1) + ".";
}

export const validateUsername: Validator = (value) => {
  const n = normalizeUsername(value);
  if (!n) return "Enter a username.";
  if (!USERNAME_RE.test(n)) {
    return "Use 3 to 32 characters: lower-case letters, digits, '.', '_' or '-', starting with a letter or digit.";
  }
  return null;
};

export const validatePassword: Validator = (value) => {
  const n = utf8Length(value);
  if (n === 0) return "Enter a password.";
  if (n < PASSWORD_MIN_BYTES) return `Use at least ${PASSWORD_MIN_BYTES} bytes (currently ${n}).`;
  if (n > PASSWORD_MAX_BYTES) {
    return `Use at most ${PASSWORD_MAX_BYTES} bytes (currently ${n}). Characters outside basic Latin take 2 to 4 bytes each.`;
  }
  return null;
};

export const validateResourceName: Validator = (value) => {
  if (!value) return "Enter a name.";
  if (!RESOURCE_NAME_RE.test(value)) {
    return "Use 1 to 63 characters: lower-case letters, digits and '-', starting with a letter or digit.";
  }
  return null;
};

/**
 * Names that would steer the docker CLI the daemon runs (internal/docker),
 * compared case-insensitively like the daemon does.
 */
const RESERVED_ENV = new Set([
  "PATH",
  "HOME",
  "XDG_RUNTIME_DIR",
  "HTTP_PROXY",
  "HTTPS_PROXY",
  "NO_PROXY",
  "ALL_PROXY",
  "SSH_AUTH_SOCK",
  "SYSTEMROOT",
  "SYSTEMDRIVE",
  "WINDIR",
  "COMSPEC",
  "PATHEXT",
  "TEMP",
  "TMP",
  "USERPROFILE",
  "APPDATA",
  "LOCALAPPDATA",
  "PROGRAMDATA",
  "PROGRAMFILES",
  "PROGRAMFILES(X86)",
  "PROGRAMW6432",
  "HOMEDRIVE",
  "HOMEPATH",
  "USERNAME",
  "USERDOMAIN",
  "COMPUTERNAME",
  "NUMBER_OF_PROCESSORS",
  "PROCESSOR_ARCHITECTURE",
  "OS",
]);

export function isReservedEnvKey(key: string): boolean {
  const up = key.toUpperCase();
  return up.startsWith("DOCKER_") || up.startsWith("BUILDKIT_") || RESERVED_ENV.has(up);
}

export const validateEnvKey: Validator = (value) => {
  if (!value) return "Enter a variable name.";
  if (!ENV_KEY_RE.test(value)) return "Use up to 128 letters, digits and '_', not starting with a digit.";
  if (isReservedEnvKey(value)) return `${value} is reserved by the daemon and cannot be used.`;
  return null;
};

const WINDOWS_DEVICES = new Set(["CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"]);

export const validateStaticName: Validator = (value) => {
  if (!value) return "The file has no name.";
  if (value.length > 128) return "File names can be at most 128 characters.";
  if (!STATIC_NAME_RE.test(value)) {
    return "File names may only contain letters, digits, '.', '_' and '-', and must start with a letter or digit.";
  }
  if (value.endsWith(".")) return "File names must not end with a dot.";
  const base = value.toUpperCase().split(".")[0];
  if (WINDOWS_DEVICES.has(base) || /^(COM|LPT)[0-9]$/.test(base)) return "This file name is a reserved device name.";
  return null;
};

// eslint-disable-next-line no-control-regex -- rejecting control characters is the point
const CONTROL_CHARS = /[\u0000-\u001f\u007f-\u009f]/;
// eslint-disable-next-line no-control-regex -- rejecting control characters is the point
const SPACE_OR_CONTROL = /[\s\u0000-\u001f\u007f-\u009f]/u;

export const validateTokenName: Validator = (value) => {
  const name = value.trim();
  const n = [...name].length;
  if (n === 0) return "Enter a name.";
  if (n > 64) return "Use at most 64 characters.";
  if (CONTROL_CHARS.test(name)) return "Use printable characters only.";
  return null;
};

const HOSTNAME_RE = /^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$/;
const SSH_USER_RE = /^[A-Za-z0-9_][A-Za-z0-9._-]*$/;

function checkHost(host: string, bracketed: boolean): string | null {
  if (!host) return "repository URL must include a host";
  if (bracketed) return /^[0-9A-Fa-f:.]+$/.test(host) ? null : "repository URL has an invalid host";
  if (host.length > 253 || !HOSTNAME_RE.test(host) || host.includes("..")) return "repository URL has an invalid host";
  return null;
}

function checkSSHUser(user: string): string | null {
  return user && user.length <= 64 && SSH_USER_RE.test(user) ? null : "repository URL has an invalid SSH user name";
}

export type RepoKind = "https" | "ssh";

/**
 * Repository URLs the daemon accepts (internal/git ValidateRemoteURL,
 * docs/REPOSITORIES.md): https://host[:port]/path, ssh://[user@]host[:port]/path
 * or scp-style user@host:path. Messages are the daemon's own, so the form
 * says the same thing whichever side rejects the URL.
 */
export function checkRepoUrl(raw: string): { kind: RepoKind; error: null } | { kind: null; error: string } {
  const fail = (error: string) => ({ kind: null, error }) as const;
  if (!raw) return fail("repository URL is required");
  if (raw.length > 2048) return fail("repository URL is too long");
  if (raw.startsWith("-")) return fail("repository URL must not start with '-'");
  if (raw.includes("\\")) return fail("repository URL must not contain backslashes");
  if (SPACE_OR_CONTROL.test(raw)) return fail("repository URL must not contain whitespace or control characters");

  const sep = raw.indexOf("://");
  if (sep >= 0 && !/[/@:]/.test(raw.slice(0, sep))) {
    const scheme = raw.slice(0, sep).toLowerCase();
    if (scheme !== "https" && scheme !== "ssh") {
      return fail(`repository URL must use https:// or ssh (${scheme === "http" ? "plain http is not allowed" : "scheme not allowed"})`);
    }
    if (/[?#]/.test(raw)) return fail("repository URL must not contain a query or fragment");
    const rest = raw.slice(sep + 3);
    const slash = rest.indexOf("/");
    const authority = slash < 0 ? rest : rest.slice(0, slash);
    const path = slash < 0 ? "" : rest.slice(slash);
    const at = authority.lastIndexOf("@");
    if (at >= 0) {
      if (scheme === "https") return fail("https repository URLs must not contain a user name or password; use the auth fields");
      const userInfo = authority.slice(0, at);
      if (userInfo.includes(":")) return fail("repository URL must not contain a password");
      const userError = checkSSHUser(userInfo);
      if (userError) return fail(userError);
    }
    const hostPort = authority.slice(at + 1);
    const bracketed = hostPort.startsWith("[");
    const close = hostPort.indexOf("]");
    if (bracketed && close < 0) return fail("repository URL is malformed");
    const host = bracketed ? hostPort.slice(1, close) : hostPort.split(":")[0];
    const hostError = checkHost(host, bracketed);
    if (hostError) return fail(hostError);
    const portPart = bracketed ? hostPort.slice(close + 1) : hostPort.slice(host.length);
    if (portPart) {
      const port = portPart.slice(1);
      if (!portPart.startsWith(":") || !/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535) {
        return fail("repository URL has an invalid port");
      }
    }
    if (path.replace(/\//g, "") === "") return fail("repository URL must include a repository path");
    return { kind: scheme, error: null };
  }
  if (/^[A-Za-z0-9+.-]*::/.test(raw)) return fail("repository URL must use https:// or ssh");

  const form = "repository URL must be https://host/path, ssh://[user@]host/path or user@host:path; local paths are not allowed";
  const at = raw.indexOf("@");
  if (at <= 0) return fail(form);
  const user = raw.slice(0, at);
  const rest = raw.slice(at + 1);
  const bracketed = rest.startsWith("[");
  let host: string;
  let path: string;
  if (bracketed) {
    const end = rest.indexOf("]:");
    if (end < 0) return fail(form);
    host = rest.slice(1, end);
    path = rest.slice(end + 2);
  } else {
    const colon = rest.indexOf(":");
    if (colon < 0) return fail(form);
    host = rest.slice(0, colon);
    path = rest.slice(colon + 1);
  }
  if (/[/:]/.test(user) || host.includes("/")) return fail(form);
  const error = checkSSHUser(user) ?? checkHost(host, bracketed);
  if (error) return fail(error);
  if (path.replace(/\//g, "") === "") return fail("repository URL must include a repository path");
  if (path.startsWith("-")) return fail("repository path must not start with '-'");
  return { kind: "ssh", error: null };
}

export const validateRepoUrl: Validator = (value) => {
  const { error } = checkRepoUrl(value.trim());
  return error ? asSentence(error) : null;
};

export function validatePort(value: string, { optional }: { optional: boolean }): string | null {
  const v = value.trim();
  if (!v) return optional ? null : "Enter a port.";
  if (!/^\d+$/.test(v)) return "Enter a whole number.";
  const n = Number(v);
  if (n < 1024 || n > 32767) return "Use a port from the daemon's port range (always within 1024 to 32767).";
  return null;
}
