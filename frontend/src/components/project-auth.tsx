import { isApiError, sentence } from "@/api/errors";
import type { ProjectAuth, ProjectAuthType } from "@/api/types.gen";
import { Field, Input, Textarea } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { checkRepoUrl, type RepoKind } from "@/lib/validators";

export interface AuthDraft {
  type: ProjectAuthType;
  username: string;
  password: string;
  privateKey: string;
}

export const emptyAuth = (type: ProjectAuthType = "none"): AuthDraft => ({ type, username: "", password: "", privateKey: "" });

export type AuthErrors = Partial<Record<"username" | "password" | "privateKey" | "type", string | null>>;

/** The daemon's message for passphrase-protected keys (internal/git). */
export const ENCRYPTED_KEY_MESSAGE = "Passphrase-protected SSH keys are not supported; provide an unencrypted deploy key.";

/**
 * Detects passphrase-protected keys, which the daemon rejects: PEM keys say so
 * in their headers; OpenSSH keys name their cipher right after the
 * "openssh-key-v1" magic ("none" when unencrypted).
 */
export function isEncryptedKey(key: string): boolean {
  if (/Proc-Type:\s*4,ENCRYPTED/i.test(key) || /-----BEGIN ENCRYPTED PRIVATE KEY-----/.test(key)) return true;
  const m = /-----BEGIN OPENSSH PRIVATE KEY-----([\s\S]*?)-----END OPENSSH PRIVATE KEY-----/.exec(key);
  if (!m) return false;
  let bin: string;
  try {
    bin = atob(m[1].replace(/\s+/g, ""));
  } catch {
    return false;
  }
  const magic = "openssh-key-v1\0";
  if (!bin.startsWith(magic)) return false;
  const at = magic.length;
  const len = (bin.charCodeAt(at) << 24) | (bin.charCodeAt(at + 1) << 16) | (bin.charCodeAt(at + 2) << 8) | bin.charCodeAt(at + 3);
  return bin.slice(at + 4, at + 4 + len) !== "none";
}

// eslint-disable-next-line no-control-regex -- rejecting control characters is the point
const SPACE_OR_CONTROL = /[\s\u0000-\u001f\u007f-\u009f]/u;
// eslint-disable-next-line no-control-regex -- rejecting control characters is the point
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

/** Validates the draft against the repository URL it will be used with (the daemon's rules and wording). */
export function validateAuth(draft: AuthDraft, repoUrl: string): AuthErrors {
  const { kind } = checkRepoUrl(repoUrl.trim());
  const errors: AuthErrors = {};
  if (draft.type === "none" && kind === "ssh") {
    // The daemon's rule: without a key of its own, ssh would offer the daemon user's keys.
    errors.type = "SSH repository URLs need a private key: choose SSH and paste a deploy key.";
  } else if (draft.type === "https") {
    if (kind === "ssh") errors.type = "HTTPS credentials require an https:// repository URL.";
    if (!draft.username.trim()) errors.username = "Username is required for HTTPS authentication.";
    else if (SPACE_OR_CONTROL.test(draft.username.trim())) {
      errors.username = "Username must not contain whitespace or control characters.";
    }
    if (!draft.password) errors.password = "Password is required for HTTPS authentication.";
    else if (CONTROL.test(draft.password)) {
      errors.password = "Password must not contain control characters or line breaks.";
    }
  } else if (draft.type === "ssh") {
    if (kind === "https") errors.type = "An SSH key requires an ssh:// or user@host:path repository URL.";
    const key = draft.privateKey.trim();
    if (!key) errors.privateKey = "Paste the private key.";
    else if (!/^-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(key)) {
      errors.privateKey = "This does not look like a private key. Paste the whole file, including the BEGIN and END lines.";
    } else if (isEncryptedKey(key)) {
      errors.privateKey = ENCRYPTED_KEY_MESSAGE;
    }
  }
  return errors;
}

/** Maps the daemon's auth.* field errors onto the credential fields; null when the error is about something else. */
export function authErrorsFrom(err: unknown): AuthErrors | null {
  if (!isApiError(err) || err.code !== "validation_failed") return null;
  const msg = sentence(err.message);
  switch (err.field) {
    case "auth.type":
      return { type: msg };
    case "auth.username":
      return { username: msg };
    case "auth.password":
      return { password: msg };
    case "auth.private_key":
      return { privateKey: msg };
  }
  return null;
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

/** The auth types the daemon accepts with a URL of this kind (all while the URL is not valid yet). */
export function authTypesFor(kind: RepoKind | null): ProjectAuthType[] {
  if (kind === "ssh") return ["ssh"];
  if (kind === "https") return ["none", "https"];
  return ["none", "https", "ssh"];
}

/** The draft adjusted to a (new) repository URL: an SSH URL needs a key, an https URL cannot use one. */
export function authForUrl(draft: AuthDraft, repoUrl: string): AuthDraft {
  const allowed = authTypesFor(checkRepoUrl(repoUrl.trim()).kind);
  if (allowed.includes(draft.type)) return draft;
  return { ...draft, type: allowed.includes("none") ? "none" : allowed[0] };
}

export function ProjectAuthFields({
  draft,
  onChange,
  errors,
  repoUrl,
  allowNone = true,
}: {
  draft: AuthDraft;
  onChange: (d: AuthDraft) => void;
  errors: AuthErrors;
  /** The URL the credentials are for: decides which types are offered. */
  repoUrl: string;
  /** False where "no credentials" is a separate choice (the edit dialog). */
  allowNone?: boolean;
}) {
  const kind = checkRepoUrl(repoUrl.trim()).kind;
  const types = TYPES.filter((t) => authTypesFor(kind).includes(t.value) && (allowNone || t.value !== "none"));
  return (
    <div className="flex flex-col gap-4">
      <fieldset>
        <legend className="mb-1.5 text-sm font-medium">Authentication</legend>
        <div className={cn("grid gap-2", types.length === 1 ? "grid-cols-1" : types.length === 2 ? "grid-cols-2" : "grid-cols-3")} role="radiogroup">
          {types.map((t) => (
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
        {kind === "ssh" && (
          <p className="mt-1.5 text-xs text-muted-foreground">SSH repositories always need a deploy key of their own.</p>
        )}
        {errors.type && (
          <p className="mt-1.5 text-xs text-destructive" role="alert">
            {errors.type}
          </p>
        )}
      </fieldset>
      {draft.type === "https" && (
        <>
          <Field
            label="Username"
            error={errors.username}
            hint="Required. With an access token, use the username your Git host expects (for example x-access-token on GitHub, oauth2 on GitLab)."
          >
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
          hint="Paste an unencrypted OpenSSH or PEM private key (a read-only deploy key); passphrase-protected keys are rejected. Put the SSH user in the repository URL. Stored encrypted; it is never shown again."
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
