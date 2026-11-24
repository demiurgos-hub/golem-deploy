import { Plus, Trash2, Undo2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { FieldError, Input } from "@/components/ui/input";
import type { EnvDraft, EnvErrors, ExistingAction } from "@/lib/env-diff";
import { localId } from "@/lib/utils";

/**
 * Edits server environment variables without ever showing stored values:
 * existing variables are listed by key and can get a new value or be
 * removed; new variables are added as rows. Values are typed into password
 * fields (single line: the daemon rejects line breaks). Turn the draft into
 * a request with computeEnvDiff; errors (client-side or routed from the
 * daemon with envErrorsFrom) appear on the row they concern.
 */
export function EnvEditor({
  existingKeys,
  draft,
  onChange,
  errors,
  disabled = false,
}: {
  existingKeys: readonly string[];
  draft: EnvDraft;
  onChange: (d: EnvDraft) => void;
  errors: EnvErrors;
  disabled?: boolean;
}) {
  const setExisting = (key: string, action: ExistingAction) =>
    onChange({ ...draft, existing: { ...draft.existing, [key]: action } });

  return (
    <div className="flex flex-col gap-3">
      {existingKeys.length > 0 && (
        <ul className="flex flex-col divide-y divide-border rounded-md border border-border" aria-label="Stored variables">
          {existingKeys.map((key) => {
            const action = draft.existing[key] ?? { kind: "keep" };
            const error = errors.existingErrors[key];
            return (
              <li key={key} className="flex flex-col gap-1 px-3 py-2">
                <div className="flex flex-wrap items-center gap-2">
                  <code className={action.kind === "remove" ? "font-mono text-xs line-through opacity-60" : "font-mono text-xs"}>{key}</code>
                  {action.kind === "keep" && <span className="text-xs text-muted-foreground">value hidden</span>}
                  {action.kind === "remove" && <Badge tone="danger">will be removed</Badge>}
                  {action.kind === "replace" && (
                    <Input
                      type="password"
                      autoComplete="new-password"
                      aria-label={`New value for ${key}`}
                      aria-invalid={error ? true : undefined}
                      placeholder="New value"
                      className="h-8 min-w-40 flex-1"
                      value={action.value}
                      disabled={disabled}
                      onChange={(e) => setExisting(key, { kind: "replace", value: e.target.value })}
                      autoFocus
                    />
                  )}
                  <div className="ml-auto flex gap-1.5">
                    {action.kind === "keep" ? (
                      <>
                        <Button size="sm" variant="outline" disabled={disabled} onClick={() => setExisting(key, { kind: "replace", value: "" })}>
                          Replace value
                        </Button>
                        <Button size="sm" variant="outline" disabled={disabled} onClick={() => setExisting(key, { kind: "remove" })}>
                          <Trash2 />
                          Remove
                        </Button>
                      </>
                    ) : (
                      <Button size="sm" variant="ghost" disabled={disabled} onClick={() => setExisting(key, { kind: "keep" })}>
                        <Undo2 />
                        Undo
                      </Button>
                    )}
                  </div>
                </div>
                <FieldError>{error}</FieldError>
              </li>
            );
          })}
        </ul>
      )}
      {draft.added.map((row) => (
        <div key={row.id} className="flex flex-col gap-1" data-env-row={row.key || undefined}>
          <div className="flex flex-wrap items-center gap-2">
            <Input
              aria-label="Variable name"
              placeholder="NAME"
              spellCheck={false}
              autoComplete="off"
              className="h-8 w-48 font-mono text-xs"
              value={row.key}
              disabled={disabled}
              aria-invalid={errors.rowErrors[row.id] ? true : undefined}
              onChange={(e) =>
                onChange({ ...draft, added: draft.added.map((r) => (r.id === row.id ? { ...r, key: e.target.value } : r)) })
              }
            />
            <Input
              type="password"
              autoComplete="new-password"
              aria-label={`Value for ${row.key || "new variable"}`}
              placeholder="Value"
              className="h-8 min-w-40 flex-1"
              value={row.value}
              disabled={disabled}
              onChange={(e) =>
                onChange({ ...draft, added: draft.added.map((r) => (r.id === row.id ? { ...r, value: e.target.value } : r)) })
              }
            />
            <Button
              size="icon"
              variant="ghost"
              aria-label={`Remove row ${row.key}`}
              disabled={disabled}
              onClick={() => onChange({ ...draft, added: draft.added.filter((r) => r.id !== row.id) })}
            >
              <Trash2 />
            </Button>
          </div>
          <FieldError>{errors.rowErrors[row.id]}</FieldError>
        </div>
      ))}
      {errors.general && (
        <p className="text-xs text-destructive" role="alert">
          {errors.general}
        </p>
      )}
      <div>
        <Button
          size="sm"
          variant="outline"
          disabled={disabled}
          onClick={() => onChange({ ...draft, added: [...draft.added, { id: localId("env"), key: "", value: "" }] })}
        >
          <Plus />
          Add variable
        </Button>
      </div>
      <p className="text-xs text-muted-foreground">
        Names starting with GOLEM_ are reserved. Values are a single line, at most 32 KiB each.
      </p>
    </div>
  );
}
