import { validateEnvKey } from "@/lib/validators";

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

export interface EnvDiffResult {
  diff: EnvDiff;
  /** Errors per new row id. */
  rowErrors: Record<string, string>;
  hasChanges: boolean;
  valid: boolean;
}

export function emptyDraft(): EnvDraft {
  return { existing: {}, added: [] };
}

export function computeEnvDiff(existingKeys: readonly string[], draft: EnvDraft): EnvDiffResult {
  const env_set: Record<string, string> = {};
  const env_unset: string[] = [];
  const rowErrors: Record<string, string> = {};
  // Names are unique ignoring case on the daemon (portable to Windows hosts).
  const existing = new Map(existingKeys.map((k) => [k.toUpperCase(), k]));

  for (const key of existingKeys) {
    const action = draft.existing[key];
    if (!action || action.kind === "keep") continue;
    if (action.kind === "remove") env_unset.push(key);
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
    env_set[key] = row.value;
  }

  const hasChanges = Object.keys(env_set).length > 0 || env_unset.length > 0;
  return {
    diff: { env_set, env_unset },
    rowErrors,
    hasChanges,
    valid: Object.keys(rowErrors).length === 0,
  };
}
