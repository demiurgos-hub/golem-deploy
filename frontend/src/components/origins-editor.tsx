import { Plus, X } from "lucide-react";
import { isApiError, sentence } from "@/api/errors";
import { Button } from "@/components/ui/button";
import { FieldError, Input } from "@/components/ui/input";

/** The daemon's limit on extra allowed origins per server. */
export const MAX_ALLOWED_ORIGINS = 16;

/**
 * Early feedback for one origin: http(s)://host[:port], nothing else. The
 * daemon normalises (lower case, default ports dropped, duplicates removed)
 * and has the final word.
 */
export function validateOrigin(raw: string): string | null {
  const v = raw.trim();
  if (!v) return "Enter an origin such as https://play.example.com, or remove the row.";
  if (!/^https?:\/\//i.test(v)) return "An origin starts with https:// (or http://).";
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    return "This is not a valid origin.";
  }
  if (u.username || u.password) return "An origin cannot contain a user name or password.";
  const rest = v.replace(/^https?:\/\//i, "");
  if (rest.includes("/") || u.search || u.hash) return "An origin has no path, query or fragment: just scheme, host and optional port.";
  return null;
}

export interface OriginErrors {
  /** Per row index. */
  rows: Record<number, string>;
  list: string | null;
}

export const noOriginErrors = (): OriginErrors => ({ rows: {}, list: null });

export function validateOrigins(origins: readonly string[]): OriginErrors {
  const rows: Record<number, string> = {};
  origins.forEach((o, i) => {
    const e = validateOrigin(o);
    if (e) rows[i] = e;
  });
  return { rows, list: origins.length > MAX_ALLOWED_ORIGINS ? `At most ${MAX_ALLOWED_ORIGINS} extra origins are allowed.` : null };
}

export function hasOriginErrors(e: OriginErrors): boolean {
  return e.list !== null || Object.keys(e.rows).length > 0;
}

/** Routes the daemon's `allowed_origins[i]` / `allowed_origins` error to the editor, or null when it is about something else. */
export function originErrorsFrom(err: unknown): OriginErrors | null {
  if (!isApiError(err) || err.code !== "validation_failed" || !err.field) return null;
  const m = /^allowed_origins\[(\d+)\]$/.exec(err.field);
  if (m) return { rows: { [Number(m[1])]: sentence(err.message) }, list: null };
  if (err.field === "allowed_origins") return { rows: {}, list: sentence(err.message) };
  return null;
}

/** Editable list of extra browser origins, one input per row. */
export function OriginsEditor({
  origins,
  onChange,
  errors,
  disabled,
}: {
  origins: string[];
  onChange: (next: string[]) => void;
  errors: OriginErrors;
  disabled?: boolean;
}) {
  return (
    <div className="flex flex-col gap-2">
      {origins.length === 0 && <p className="text-sm text-muted-foreground">No extra origins.</p>}
      <ul className="flex flex-col gap-2" aria-label="Allowed origins">
        {origins.map((o, i) => (
          <li key={i} className="flex flex-col gap-1">
            <div className="flex items-center gap-2">
              <Input
                aria-label={`Allowed origin ${i + 1}`}
                aria-invalid={errors.rows[i] ? true : undefined}
                spellCheck={false}
                autoComplete="off"
                placeholder="https://play.example.com"
                value={o}
                disabled={disabled}
                onChange={(e) => onChange(origins.map((x, j) => (j === i ? e.target.value : x)))}
              />
              <Button
                variant="ghost"
                size="icon"
                aria-label={`Remove allowed origin ${i + 1}`}
                disabled={disabled}
                onClick={() => onChange(origins.filter((_, j) => j !== i))}
              >
                <X />
              </Button>
            </div>
            <FieldError>{errors.rows[i]}</FieldError>
          </li>
        ))}
      </ul>
      <div>
        <Button size="sm" variant="outline" disabled={disabled || origins.length >= MAX_ALLOWED_ORIGINS} onClick={() => onChange([...origins, ""])}>
          <Plus />
          Add origin
        </Button>
      </div>
      <FieldError>{errors.list}</FieldError>
    </div>
  );
}
