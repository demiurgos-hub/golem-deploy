import { isApiError, sentence } from "@/api/errors";
import { validateEnvKey, validateEnvTotals, validateEnvValue } from "@/lib/validators";

/**
 * Environment editing without ever reading values back. Existing variables
 * are known by key only; each can be kept, given a new value, or removed.
 * New rows add variables. The result is the PATCH body's env_set/env_unset
 * (or, with no existing keys, the create body's env).
 */

export type ExistingAction = { kind: "keep" } | { kind: "replace"; value: string } | { kind: "remove" };

export interface NewEnvRow {
  id: string;
  key: string;
  value: string;
}

export interface EnvDraft {
  existing: Record<string, ExistingAction>;
  added: NewEnvRow[];
}

export interface EnvDiff {
  env_set: Record<string, string>;
  env_unset: string[];
}

/** Where env problems are shown: per new row (by id), per stored variable (by key), or for the whole set. */
export interface EnvErrors {
  rowErrors: Record<string, string>;
  existingErrors: Record<string, string>;
  general: string | null;
}

export interface EnvDiffResult extends EnvErrors {
  diff: EnvDiff;
  hasChanges: boolean;
  valid: boolean;
}

export function emptyDraft(): EnvDraft {
  return { existing: {}, added: [] };
}

export function noEnvErrors(): EnvErrors {
  return { rowErrors: {}, existingErrors: {}, general: null };
}

export function hasEnvErrors(e: EnvErrors): boolean {
  return Object.keys(e.rowErrors).length > 0 || Object.keys(e.existingErrors).length > 0 || e.general !== null;
}

/** Client-side errors and server-side ones (cleared when the draft changes) together. */
export function mergeEnvErrors(a: EnvErrors, b: EnvErrors): EnvErrors {
  return {
    rowErrors: { ...b.rowErrors, ...a.rowErrors },
    existingErrors: { ...b.existingErrors, ...a.existingErrors },
    general: a.general ?? b.general,
  };
}

export function computeEnvDiff(existingKeys: readonly string[], draft: EnvDraft): EnvDiffResult {
  const env_set: Record<string, string> = {};
  const env_unset: string[] = [];
  const rowErrors: Record<string, string> = {};
  const existingErrors: Record<string, string> = {};
  // Names are unique ignoring case on the daemon (portable to Windows hosts).
  const existing = new Map(existingKeys.map((k) => [k.toUpperCase(), k]));

  for (const key of existingKeys) {
    const action = draft.existing[key];
    if (!action || action.kind === "keep") continue;
    if (action.kind === "remove") {
      env_unset.push(key);
      continue;
    }
    const valueError = validateEnvValue(key, action.value);
    if (valueError) existingErrors[key] = valueError;
    else env_set[key] = action.value;
  }

  const seen = new Map<string, { id: string; key: string }>();
  for (const row of draft.added) {
    const key = row.key.trim();
    if (!key && !row.value) continue; // an untouched blank row
    const keyError = validateEnvKey(key);
    if (keyError) {
      rowErrors[row.id] = keyError;
      continue;
    }
    const stored = existing.get(key.toUpperCase());
    if (stored !== undefined) {
      rowErrors[row.id] =
        stored === key
          ? `${key} already exists. Use "Replace value" on it instead.`
          : `${stored} already exists; names are unique ignoring case. Use "Replace value" on it instead.`;
      continue;
    }
    const first = seen.get(key.toUpperCase());
    if (first !== undefined) {
      rowErrors[row.id] = `${key} is listed twice (names are unique ignoring case).`;
      rowErrors[first.id] = `${first.key} is listed twice (names are unique ignoring case).`;
      delete env_set[first.key];
      continue;
    }
    seen.set(key.toUpperCase(), { id: row.id, key });
    const valueError = validateEnvValue(key, row.value);
    if (valueError) {
      rowErrors[row.id] = valueError;
      continue;
    }
    env_set[key] = row.value;
  }

  // Count and total size are only known for a complete set (a new server):
  // stored values are never read back, so a PATCH leaves those limits to the daemon.
  const general = existingKeys.length === 0 ? validateEnvTotals(env_set) : null;
  const hasChanges = Object.keys(env_set).length > 0 || env_unset.length > 0 || Object.keys(existingErrors).length > 0;
  const errors = { rowErrors, existingErrors, general };
  return {
    diff: { env_set, env_unset },
    ...errors,
    hasChanges: hasChanges || Object.keys(rowErrors).length > 0,
    valid: !hasEnvErrors(errors),
  };
}

/**
 * Routes a daemon env error to where it belongs. Value errors name the key
 * in `field` (`env.MOTD` on create, `env_set.MOTD` on update); name and
 * limit errors name only `env`, `env_set` or `env_unset`. Returns null when
 * the error is not about the environment.
 */
export function envErrorsFrom(err: unknown, draft: EnvDraft, existingKeys: readonly string[]): EnvErrors | null {
  if (!isApiError(err) || err.code !== "validation_failed" || !err.field) return null;
  const m = /^(env|env_set|env_unset)(?:\.(.+))?$/.exec(err.field);
  if (!m) return null;
  const msg = sentence(err.message);
  const out = noEnvErrors();
  const key = m[2];
  if (key !== undefined) {
    const upper = key.toUpperCase();
    const row = draft.added.find((r) => r.key.trim() === key) ?? draft.added.find((r) => r.key.trim().toUpperCase() === upper);
    const stored = existingKeys.find((k) => k === key) ?? existingKeys.find((k) => k.toUpperCase() === upper);
    if (row) out.rowErrors[row.id] = msg;
    else if (stored !== undefined) out.existingErrors[stored] = msg;
    else out.general = `${key}: ${msg}`;
    return out;
  }
  out.general = msg;
  return out;
}
