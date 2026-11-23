import type { ProjectAuth, ProjectAuthType } from "@/api/types.gen";
import { Field, Input, Textarea } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export interface AuthDraft {
  type: ProjectAuthType;
  username: string;
  password: string;
  privateKey: string;
}

export const emptyAuth = (type: ProjectAuthType = "none"): AuthDraft => ({ type, username: "", password: "", privateKey: "" });

export type AuthErrors = Partial<Record<"username" | "password" | "privateKey" | "type", string | null>>;

/** Validates the draft against the repository URL it will be used with. */
export function validateAuth(draft: AuthDraft, repoUrl: string): AuthErrors {
  const url = repoUrl.trim();
  const isHttps = url.startsWith("https://");
  const errors: AuthErrors = {};
  if (draft.type === "https") {
    if (!isHttps && url) errors.type = "HTTPS credentials need an https:// repository URL.";
    if (!draft.username.trim()) errors.username = "Enter the username.";
    if (!draft.password) errors.password = "Enter the password or access token.";
  } else if (draft.type === "ssh") {
    if (isHttps) errors.type = "An SSH key needs an ssh:// or git@host:path repository URL.";
    const key = draft.privateKey.trim();
    if (!key) errors.privateKey = "Paste the private key.";
    else if (!/^-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(key)) {
      errors.privateKey = "This does not look like a private key. Paste the whole file, including the BEGIN and END lines.";
    }
  }
  return errors;
}

export function hasErrors(errors: AuthErrors): boolean {
  return Object.values(errors).some(Boolean);
}

/** The write-only wire value. Secrets go to the daemon and are never read back. */
export function toProjectAuth(draft: AuthDraft): ProjectAuth {
  switch (draft.type) {
    case "https":
      return { type: "https", username: draft.username.trim(), password: draft.password };
    case "ssh":
      // Keep the key byte-exact apart from surrounding whitespace, and end it with a newline as OpenSSH expects.
      return { type: "ssh", private_key: draft.privateKey.trim() + "\n" };
    default:
      return { type: "none" };
  }
}

const TYPES: { value: ProjectAuthType; label: string; hint: string }[] = [
  { value: "none", label: "None", hint: "Public repository" },
  { value: "https", label: "HTTPS", hint: "Username and password or token" },
  { value: "ssh", label: "SSH", hint: "Deploy key" },
];

export function ProjectAuthFields({
  draft,
  onChange,
  errors,
}: {
  draft: AuthDraft;
  onChange: (d: AuthDraft) => void;
  errors: AuthErrors;
}) {
  return (
    <div className="flex flex-col gap-4">
      <fieldset>
        <legend className="mb-1.5 text-sm font-medium">Authentication</legend>
        <div className="grid grid-cols-3 gap-2" role="radiogroup">
          {TYPES.map((t) => (
            <label
              key={t.value}
              className={cn(
                "flex cursor-pointer flex-col rounded-md border px-3 py-2 text-sm focus-within:ring-2 focus-within:ring-ring",
                draft.type === t.value ? "border-primary bg-accent" : "border-input",
              )}
            >
              <input
                type="radio"
                name="auth-type"
                value={t.value}
                checked={draft.type === t.value}
                onChange={() => onChange({ ...draft, type: t.value })}
                className="sr-only"
              />
              <span className="font-medium">{t.label}</span>
              <span className="text-xs text-muted-foreground">{t.hint}</span>
            </label>
          ))}
        </div>
        {errors.type && (
          <p className="mt-1.5 text-xs text-destructive" role="alert">
            {errors.type}
          </p>
        )}
      </fieldset>
      {draft.type === "https" && (
        <>
          <Field label="Username" error={errors.username} hint="With an access token, use the username your Git host expects (for example oauth2 on GitLab).">
            {(p) => (
              <Input {...p} autoComplete="off" spellCheck={false} value={draft.username} onChange={(e) => onChange({ ...draft, username: e.target.value })} />
            )}
          </Field>
          <Field label="Password or access token" error={errors.password} hint="Stored encrypted. It is never shown again.">
            {(p) => (
              <Input
                {...p}
                type="password"
                autoComplete="new-password"
                value={draft.password}
                onChange={(e) => onChange({ ...draft, password: e.target.value })}
              />
            )}
          </Field>
        </>
      )}
      {draft.type === "ssh" && (
        <Field
          label="Private key"
          error={errors.privateKey}
          hint="Paste the contents of an unencrypted OpenSSH or PEM private key (a deploy key with read access). Stored encrypted; it is never shown again."
        >
          {(p) => (
            <Textarea
              {...p}
              className="min-h-36 font-mono text-xs"
              spellCheck={false}
              autoComplete="off"
              autoCorrect="off"
              autoCapitalize="none"
              placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
              value={draft.privateKey}
              onChange={(e) => onChange({ ...draft, privateKey: e.target.value })}
            />
          )}
        </Field>
      )}
    </div>
  );
}
