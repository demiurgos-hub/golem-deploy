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
  const existing = new Set(existingKeys);

  for (const key of existingKeys) {
    const action = draft.existing[key];
    if (!action || action.kind === "keep") continue;
    if (action.kind === "remove") env_unset.push(key);
    else env_set[key] = action.value;
  }

  const seen = new Map<string, string>();
  for (const row of draft.added) {
    const key = row.key.trim();
    if (!key && !row.value) continue; // an untouched blank row
    const keyError = validateEnvKey(key);
    if (keyError) {
      rowErrors[row.id] = keyError;
      continue;
    }
    if (existing.has(key)) {
      rowErrors[row.id] = `${key} already exists. Use "Replace value" on it instead.`;
      continue;
    }
    const first = seen.get(key);
    if (first !== undefined) {
      rowErrors[row.id] = `${key} is listed twice.`;
      rowErrors[first] = `${key} is listed twice.`;
      delete env_set[key];
      continue;
    }
    seen.set(key, row.id);
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
