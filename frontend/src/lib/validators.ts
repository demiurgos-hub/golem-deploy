/**
 * Client-side mirrors of the daemon's validation rules (docs/API.md). They
 * give immediate feedback; the daemon remains the authority.
 */

export const USERNAME_RE = /^[a-z0-9][a-z0-9._-]{2,31}$/;
export const RESOURCE_NAME_RE = /^[a-z0-9][a-z0-9-]{0,62}$/;
export const ENV_KEY_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
export const STATIC_NAME_RE = /^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$/;

export const PASSWORD_MIN_BYTES = 12;
export const PASSWORD_MAX_BYTES = 72;
/** POST /static accepts at most 1 GiB per request; keep room for multipart framing. */
export const STATIC_UPLOAD_LIMIT_BYTES = 1024 * 1024 * 1024 - 1024 * 1024;

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

export const validateEnvKey: Validator = (value) => {
  if (!value) return "Enter a variable name.";
  if (!ENV_KEY_RE.test(value)) {
    return "Use letters, digits and '_', not starting with a digit.";
  }
  return null;
};

export const validateStaticName: Validator = (value) => {
  if (!value) return "The file has no name.";
  if (value.length > 128) return "File names can be at most 128 characters.";
  if (value.startsWith(".")) return "File names cannot start with a dot.";
  if (!STATIC_NAME_RE.test(value)) return "File names may only contain letters, digits, '.', '_' and '-'.";
  return null;
};

// eslint-disable-next-line no-control-regex -- rejecting control characters is the point
const CONTROL_CHARS = /[\u0000-\u001f\u007f-\u009f]/;

export const validateTokenName: Validator = (value) => {
  const name = value.trim();
  const n = [...name].length;
  if (n === 0) return "Enter a name.";
  if (n > 64) return "Use at most 64 characters.";
  if (CONTROL_CHARS.test(name)) return "Use printable characters only.";
  return null;
};

/** Repository URLs the daemon accepts: https://, ssh:// or scp-style user@host:path. */
export const validateRepoUrl: Validator = (value) => {
  const v = value.trim();
  if (!v) return "Enter the repository URL.";
  if (/^https:\/\/[^/\s]+\/\S+$/.test(v)) return null;
  if (/^ssh:\/\/[^/\s]+\/\S+$/.test(v)) return null;
  if (/^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s/]\S*$/.test(v)) return null;
  return "Use an https:// URL, an ssh:// URL, or the git@host:owner/repo form.";
};

export function validatePort(value: string, { optional }: { optional: boolean }): string | null {
  const v = value.trim();
  if (!v) return optional ? null : "Enter a port.";
  if (!/^\d+$/.test(v)) return "Enter a whole number.";
  const n = Number(v);
  if (n < 1 || n > 65535) return "Use a port from 1 to 65535.";
  return null;
}
