/**
 * Client-side mirrors of the daemon's validation rules (docs/API.md). They
 * give immediate feedback; the daemon remains the authority.
 */

import type { ProfileKind } from "@/api/types.gen";

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

/** Env names starting with this (any case) are reserved for variables the daemon injects. */
export const RESERVED_ENV_PREFIX = "GOLEM_";
/** Limits on server environment (docs/API.md, Servers); sizes count UTF-8 bytes of key plus value. */
export const ENV_MAX_VARS = 128;
export const ENV_MAX_VAR_BYTES = 32 * 1024;
export const ENV_MAX_TOTAL_BYTES = 64 * 1024;

export function isReservedEnvKey(key: string): boolean {
  return key.toUpperCase().startsWith(RESERVED_ENV_PREFIX);
}

export const validateEnvKey: Validator = (value) => {
  if (!value) return "Enter a variable name.";
  if (!ENV_KEY_RE.test(value)) return "Use up to 128 letters, digits and '_', not starting with a digit.";
  if (isReservedEnvKey(value)) return `Names starting with ${RESERVED_ENV_PREFIX} are reserved for golem-deploy.`;
  return null;
};

/**
 * A value as the daemon checks it (internal/docker ValidateEnvValue): one
 * line of UTF-8, at most 32 KiB together with its name. The wording follows
 * the daemon's; values are never echoed.
 */
export function validateEnvValue(key: string, value: string): string | null {
  if (utf8Length(key) + utf8Length(value) > ENV_MAX_VAR_BYTES) {
    return `The variable exceeds ${ENV_MAX_VAR_BYTES} bytes (name and value, currently ${utf8Length(key) + utf8Length(value)}).`;
  }
  if (value.includes("\0")) return "The value contains a NUL byte.";
  if (/[\r\n]/.test(value)) return "The value contains a line break (values must be a single line).";
  return null;
}

/** Limits over the whole set (count and total size), or null. */
export function validateEnvTotals(env: Record<string, string>): string | null {
  const keys = Object.keys(env);
  if (keys.length > ENV_MAX_VARS) return `At most ${ENV_MAX_VARS} variables are allowed (currently ${keys.length}).`;
  const total = keys.reduce((n, k) => n + utf8Length(k) + utf8Length(env[k]), 0);
  if (total > ENV_MAX_TOTAL_BYTES) return `The variables exceed ${ENV_MAX_TOTAL_BYTES} bytes in total (currently ${total}).`;
  return null;
}

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

/**
 * Scheme kind, lower-case host and port (default port made explicit; an
 * scp-style URL is SSH on port 22) of a URL checkRepoUrl accepts, like the
 * daemon's git.SameOrigin. Null for invalid URLs.
 */
export function repoOrigin(raw: string): { kind: RepoKind; host: string; port: string } | null {
  const url = raw.trim();
  const { kind } = checkRepoUrl(url);
  if (kind === null) return null;
  const def = kind === "ssh" ? "22" : "443";
  const sep = url.indexOf("://");
  if (sep >= 0) {
    const rest = url.slice(sep + 3);
    const slash = rest.indexOf("/");
    const authority = slash < 0 ? rest : rest.slice(0, slash);
    const hostPort = authority.slice(authority.lastIndexOf("@") + 1);
    if (hostPort.startsWith("[")) {
      const close = hostPort.indexOf("]");
      const port = hostPort.slice(close + 2);
      return { kind, host: hostPort.slice(1, close).toLowerCase(), port: port || def };
    }
    const colon = hostPort.indexOf(":");
    return colon < 0
      ? { kind, host: hostPort.toLowerCase(), port: def }
      : { kind, host: hostPort.slice(0, colon).toLowerCase(), port: hostPort.slice(colon + 1) || def };
  }
  const rest = url.slice(url.indexOf("@") + 1);
  const host = rest.startsWith("[") ? rest.slice(1, rest.indexOf("]")) : rest.slice(0, rest.indexOf(":"));
  return { kind, host: host.toLowerCase(), port: def };
}

/**
 * Whether two repository URLs share scheme, host and port. Stored
 * credentials are only kept for a new URL with the same origin; otherwise
 * the daemon requires auth in the same request (validation_failed, field auth).
 */
export function sameRepoOrigin(a: string, b: string): boolean {
  const oa = repoOrigin(a);
  const ob = repoOrigin(b);
  return oa !== null && ob !== null && oa.kind === ob.kind && oa.host === ob.host && Number(oa.port) === Number(ob.port);
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

/** Server.snapshot_interval_seconds: 0 (autosave off) or 10 to 86400; the daemon's default is 300. */
export const SNAPSHOT_INTERVAL_DEFAULT = 300;
export const SNAPSHOT_INTERVAL_MIN = 10;
export const SNAPSHOT_INTERVAL_MAX = 86_400;
/** BackupSchedule: interval_hours 0 (off) or 1 to 720; keep 1 to 100, default 7. */
export const BACKUP_INTERVAL_MAX_HOURS = 720;
export const BACKUP_KEEP_DEFAULT = 7;
export const BACKUP_KEEP_MAX = 100;
/** CreateBackupRequest.note: one line of at most 200 characters. */
export const BACKUP_NOTE_MAX = 200;

function wholeNumber(value: string): number | null {
  const v = value.trim();
  return /^\d+$/.test(v) ? Number(v) : null;
}

export const validateSnapshotInterval: Validator = (value) => {
  const n = wholeNumber(value);
  if (n === null) return "Enter a whole number of seconds.";
  if (n !== 0 && (n < SNAPSHOT_INTERVAL_MIN || n > SNAPSHOT_INTERVAL_MAX)) {
    return `Use 0 to turn autosave off, or ${SNAPSHOT_INTERVAL_MIN} to ${SNAPSHOT_INTERVAL_MAX.toLocaleString("en-US")} seconds (24 hours).`;
  }
  return null;
};

export const validateBackupInterval: Validator = (value) => {
  const n = wholeNumber(value);
  if (n === null) return "Enter a whole number of hours.";
  if (n > BACKUP_INTERVAL_MAX_HOURS) return `Use 0 to turn scheduled backups off, or 1 to ${BACKUP_INTERVAL_MAX_HOURS} hours (30 days).`;
  return null;
};

export const validateBackupKeep: Validator = (value) => {
  const n = wholeNumber(value);
  if (n === null) return "Enter a whole number.";
  if (n < 1 || n > BACKUP_KEEP_MAX) return `Keep 1 to ${BACKUP_KEEP_MAX} scheduled backups.`;
  return null;
};

/**
 * GET /servers/{id}/profile: the capture length in seconds of the timed kinds, from 1 to `max`
 * (servers.ProfileSeconds); the other kinds take no `seconds`.
 */
export const PROFILE_SECONDS: Partial<Record<ProfileKind, { default: number; max: number }>> = {
  cpu: { default: 30, max: 60 },
  trace: { default: 5, max: 15 },
};

export function validateProfileSeconds(kind: ProfileKind, value: string): string | null {
  const limits = PROFILE_SECONDS[kind];
  if (!limits) return null;
  const n = wholeNumber(value);
  if (n === null || n < 1 || n > limits.max) return `Enter a whole number of seconds from 1 to ${limits.max}.`;
  return null;
}

/** A regular expression character class of code points and inclusive ranges. */
function charClass(points: readonly (number | readonly [number, number])[]): string {
  const hex = (n: number) => `\\u{${n.toString(16)}}`;
  return `[${points.map((p) => (typeof p === "number" ? hex(p) : `${hex(p[0])}-${hex(p[1])}`)).join("")}]`;
}

/** The space Go's strings.TrimSpace removes around a note (unicode.IsSpace). */
const NOTE_SPACE = charClass([[0x09, 0x0d], 0x20, 0x85, 0xa0, 0x1680, [0x2000, 0x200a], 0x2028, 0x2029, 0x202f, 0x205f, 0x3000]);
const NOTE_TRIM = new RegExp(`^${NOTE_SPACE}+|${NOTE_SPACE}+$`, "gu");

/**
 * Characters the daemon refuses in a backup note (servers.ValidateNote):
 * C0 controls, DEL, C1 controls, the line and paragraph separators and the
 * bidirectional formatting characters.
 */
export const BACKUP_NOTE_FORBIDDEN = new RegExp(charClass([[0x00, 0x1f], [0x7f, 0x9f], 0x2028, 0x2029, [0x202a, 0x202e], [0x2066, 0x2069]]), "u");
const LINE_BREAK = new RegExp(charClass([0x0a, 0x0d, 0x2028, 0x2029]), "u");

/** Removes the surrounding space the daemon removes from a note before checking it. */
export function trimBackupNote(value: string): string {
  return value.replace(NOTE_TRIM, "");
}

/** Checks a backup note as the daemon does, after removing the surrounding space. */
export const validateBackupNote: Validator = (value) => {
  const note = trimBackupNote(value);
  if (LINE_BREAK.test(note)) return "Use a single line.";
  if (BACKUP_NOTE_FORBIDDEN.test(note)) return "Remove the control characters and text direction marks.";
  if ([...note].length > BACKUP_NOTE_MAX) return `Use at most ${BACKUP_NOTE_MAX} characters.`;
  return null;
};
