import type { ServerExport, ServerSpec } from "@/api/types.gen";
import { RESOURCE_NAME_RE, validateEnvKey } from "@/lib/validators";

export interface ImportCheck {
  value: ServerExport | null;
  errors: string[];
}

/** The daemon reads at most 1 MiB of JSON per request. */
export const IMPORT_LIMIT_BYTES = 1024 * 1024;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const SPEC_FIELDS = new Set(["name", "project", "image", "port", "env", "desired_state"]);

/**
 * Checks a JSON export (GET /servers/export) before anything is sent, listing
 * every problem the client can detect. The daemon re-validates everything,
 * including what only it can know (existing names and ports, the port range,
 * project names, that each image is a managed image on this host), stops at
 * the first problem and creates all servers or none.
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
  if (data.format_version !== 1) errors.push("format_version must be 1.");
  if (!Array.isArray(data.servers)) {
    errors.push("servers must be a list.");
    return { value: null, errors };
  }
  if (data.servers.length === 0) errors.push("The file contains no servers.");

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
    if (typeof raw.image !== "string" || !raw.image.trim()) errors.push(`${label}: image is required.`);
    if (raw.project !== undefined && (typeof raw.project !== "string" || (raw.project !== "" && !RESOURCE_NAME_RE.test(raw.project)))) {
      errors.push(`${label}: project must be a project name.`);
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
        }
      }
    }
    servers.push(raw as unknown as ServerSpec);
  });

  return errors.length ? { value: null, errors } : { value: { format_version: 1, servers }, errors };
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
  return `Server ${i + 1}${name ? ` (${name})` : ""}${m[2] ? `, ${m[2]}` : ""}`;
}
