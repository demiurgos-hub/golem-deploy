import type { ServerExport, ServerSpec } from "@/api/types.gen";
import { RESOURCE_NAME_RE, validateEnvKey, validateEnvTotals, validateEnvValue } from "@/lib/validators";

export interface ImportCheck {
  value: ServerExport | null;
  errors: string[];
}

/** POST /servers/import reads at most 64 MiB of JSON (docs/API.md). */
export const IMPORT_LIMIT_BYTES = 64 * 1024 * 1024;
/** One import document holds at most 128 servers. */
export const IMPORT_MAX_SERVERS = 128;
/** The export format this client reads and the daemon accepts (servers reference project + commit). */
export const EXPORT_FORMAT_VERSION = 2;
const MAX_ORIGINS = 16;
const COMMIT_RE = /^[0-9a-f]{40}([0-9a-f]{24})?$/;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const SPEC_FIELDS = new Set(["name", "project", "commit", "port", "allowed_origins", "env", "desired_state"]);

/**
 * Checks a JSON export (GET /servers/export) before anything is sent, listing
 * every problem the client can detect. The daemon re-validates everything,
 * including what only it can know (existing names and ports, the port range,
 * that each project exists and has a successful build of the commit, the
 * allowed origins' exact rules), stops at the first problem and creates all
 * servers or none.
 */
export function checkServerImport(text: string): ImportCheck {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return { value: null, errors: ["The file is not valid JSON. Choose a file saved with Export."] };
  }
  const errors: string[] = [];
  if (!isRecord(data)) return { value: null, errors: ["The file must contain a JSON object."] };
  for (const k of Object.keys(data)) {
    if (k !== "format_version" && k !== "servers") errors.push(`Unknown top-level field "${k}".`);
  }
  if (data.format_version !== EXPORT_FORMAT_VERSION) {
    errors.push(
      data.format_version === 1
        ? "This is a format version 1 export (servers named an image). This version needs format 2, where each server names its project and commit: export again from an up-to-date Golem Deploy."
        : `format_version must be ${EXPORT_FORMAT_VERSION}.`,
    );
  }
  if (!Array.isArray(data.servers)) {
    errors.push("servers must be a list.");
    return { value: null, errors };
  }
  if (data.servers.length === 0) errors.push("The file contains no servers.");
  if (data.servers.length > IMPORT_MAX_SERVERS) {
    errors.push(`The file contains ${data.servers.length} servers; one import accepts at most ${IMPORT_MAX_SERVERS}. Split it into several files.`);
    return { value: null, errors };
  }

  const names = new Map<string, number>();
  const ports = new Map<number, number>();
  const servers: ServerSpec[] = [];
  data.servers.forEach((raw: unknown, i: number) => {
    const at = `Server ${i + 1}`;
    if (!isRecord(raw)) {
      errors.push(`${at}: must be an object.`);
      return;
    }
    const label = typeof raw.name === "string" && raw.name ? `${at} (${raw.name})` : at;
    for (const k of Object.keys(raw)) if (!SPEC_FIELDS.has(k)) errors.push(`${label}: unknown field "${k}".`);
    if (typeof raw.name !== "string" || !RESOURCE_NAME_RE.test(raw.name)) {
      errors.push(`${label}: name must be 1 to 63 lower-case letters, digits or '-', starting with a letter or digit.`);
    } else if (names.has(raw.name)) {
      errors.push(`${label}: the name is also used by server ${(names.get(raw.name) ?? 0) + 1}.`);
    } else {
      names.set(raw.name, i);
    }
    if (typeof raw.project !== "string" || raw.project === "") {
      errors.push(`${label}: project is required (the server runs a build of that project). A server exported without a build has an empty project: fill it in.`);
    } else if (!RESOURCE_NAME_RE.test(raw.project)) {
      errors.push(`${label}: project must be a project name.`);
    }
    if (raw.commit !== undefined && raw.commit !== "" && (typeof raw.commit !== "string" || !COMMIT_RE.test(raw.commit))) {
      errors.push(`${label}: commit must be a full lower-case commit hash (or empty for the newest successful build).`);
    }
    if (raw.allowed_origins !== undefined && raw.allowed_origins !== null) {
      if (!Array.isArray(raw.allowed_origins) || raw.allowed_origins.some((o) => typeof o !== "string")) {
        errors.push(`${label}: allowed_origins must be a list of origins.`);
      } else if (raw.allowed_origins.length > MAX_ORIGINS) {
        errors.push(`${label}: at most ${MAX_ORIGINS} allowed origins.`);
      }
    }
    if (raw.port !== undefined && raw.port !== null) {
      if (typeof raw.port !== "number" || !Number.isInteger(raw.port) || raw.port < 1024 || raw.port > 32767) {
        errors.push(`${label}: port must be a whole number within the daemon's port range (always 1024 to 32767).`);
      } else if (ports.has(raw.port)) {
        errors.push(`${label}: port ${raw.port} is also used by server ${(ports.get(raw.port) ?? 0) + 1}.`);
      } else {
        ports.set(raw.port, i);
      }
    }
    if (raw.desired_state !== undefined && raw.desired_state !== "" && raw.desired_state !== "running" && raw.desired_state !== "stopped") {
      errors.push(`${label}: desired_state must be "running" or "stopped".`);
    }
    if (raw.env !== undefined && raw.env !== null) {
      if (!isRecord(raw.env)) errors.push(`${label}: env must be an object of names to values.`);
      else {
        const seen = new Set<string>();
        for (const [k, v] of Object.entries(raw.env)) {
          const keyError = validateEnvKey(k);
          if (keyError) errors.push(`${label}: environment variable "${k}": ${keyError}`);
          else if (seen.has(k.toUpperCase())) errors.push(`${label}: environment variable "${k}" is listed twice (names are unique ignoring case).`);
          seen.add(k.toUpperCase());
          if (typeof v !== "string") errors.push(`${label}: the value of ${k} must be a string.`);
          else if (!keyError) {
            const valueError = validateEnvValue(k, v);
            if (valueError) errors.push(`${label}: environment variable ${k}: ${valueError}`);
          }
        }
        const stringEnv = Object.fromEntries(Object.entries(raw.env).filter(([, v]) => typeof v === "string")) as Record<string, string>;
        const totals = validateEnvTotals(stringEnv);
        if (totals) errors.push(`${label}: environment: ${totals}`);
      }
    }
    servers.push(raw as unknown as ServerSpec);
  });

  return errors.length ? { value: null, errors } : { value: { format_version: EXPORT_FORMAT_VERSION, servers }, errors };
}

/**
 * Names the entry an import error points at: the daemon reports one problem,
 * with `field` like "servers[1].env". Returns e.g. "Server 2 (eu-arena), env".
 */
export function describeImportField(field: string | undefined, doc: ServerExport | null): string | null {
  if (!field) return null;
  const m = /^servers\[(\d+)\](?:\.(.+))?$/.exec(field);
  if (!m) return field;
  const i = Number(m[1]);
  const name = doc?.servers[i]?.name;
  // Value errors name the variable (servers[0].env.MOTD) or the entry (servers[0].allowed_origins[1]).
  const origin = m[2] ? /^allowed_origins\[(\d+)\]$/.exec(m[2]) : null;
  const attr = m[2]?.startsWith("env.") ? `environment variable ${m[2].slice(4)}` : origin ? `allowed origin ${Number(origin[1]) + 1}` : m[2];
  return `Server ${i + 1}${name ? ` (${name})` : ""}${attr ? `, ${attr}` : ""}`;
}
