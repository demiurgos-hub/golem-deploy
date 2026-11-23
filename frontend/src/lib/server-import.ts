import type { ServerExport, ServerSpec } from "@/api/types.gen";
import { ENV_KEY_RE, RESOURCE_NAME_RE } from "@/lib/validators";

export interface ImportCheck {
  value: ServerExport | null;
  errors: string[];
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

const SPEC_FIELDS = new Set(["name", "project", "image", "port", "env", "desired_state"]);

/**
 * Checks an export file before anything is sent, listing every problem the
 * client can detect (the daemon re-validates and also checks conflicts with
 * existing servers; import is all-or-nothing).
 */
export function checkServerImport(text: string): ImportCheck {
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return { value: null, errors: ["The file is not valid JSON."] };
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
    if (raw.project !== undefined && (typeof raw.project !== "string" || !RESOURCE_NAME_RE.test(raw.project))) {
      errors.push(`${label}: project must be a project name.`);
    }
    if (raw.port !== undefined) {
      if (typeof raw.port !== "number" || !Number.isInteger(raw.port) || raw.port < 1 || raw.port > 65535) {
        errors.push(`${label}: port must be a whole number from 1 to 65535.`);
      } else if (ports.has(raw.port)) {
        errors.push(`${label}: port ${raw.port} is also used by server ${(ports.get(raw.port) ?? 0) + 1}.`);
      } else {
        ports.set(raw.port, i);
      }
    }
    if (raw.desired_state !== "running" && raw.desired_state !== "stopped") {
      errors.push(`${label}: desired_state must be "running" or "stopped".`);
    }
    if (raw.env !== undefined) {
      if (!isRecord(raw.env)) errors.push(`${label}: env must be an object of names to values.`);
      else {
        for (const [k, v] of Object.entries(raw.env)) {
          if (!ENV_KEY_RE.test(k)) errors.push(`${label}: environment variable name "${k}" is invalid.`);
          if (typeof v !== "string") errors.push(`${label}: the value of ${k} must be a string.`);
        }
      }
    }
    servers.push(raw as unknown as ServerSpec);
  });

  return errors.length ? { value: null, errors } : { value: { format_version: 1, servers }, errors };
}
