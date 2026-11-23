import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { useServices } from "@/app/services";
import { isApiError } from "@/api/errors";
import { useAccount, useTokens } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { APIToken, CreateAPITokenResponse, CurrentAccount } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { DetailList, PageHeader, Section } from "@/components/ui/page";
import { EmptyState, ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative } from "@/lib/format";
import { validatePassword, validateTokenName } from "@/lib/validators";

export function AccountPage() {
  const account = useAccount();
  return (
    <>
      <PageHeader title="Account" description="Your profile, password and API tokens." />
      <div className="flex flex-col gap-6">
        <Section title="Profile">
          {account.isPending ? (
            <LoadingState />
          ) : account.isError ? (
            <ErrorState error={account.error} onRetry={() => void account.refetch()} />
          ) : (
            <DetailList
              items={[
                { term: "Username", value: account.data.username },
                { term: "Role", value: <Badge tone={account.data.role === "admin" ? "info" : "neutral"}>{account.data.role}</Badge> },
                { term: "Created", value: formatDateTime(account.data.created_at) },
              ]}
            />
          )}
        </Section>
        <ChangePassword />
        <Tokens />
      </div>
    </>
  );
}

function ChangePassword() {
  const { api, session, queryClient } = useServices();
  const toast = useToast();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [revokeTokens, setRevokeTokens] = useState(true);
  const [errors, setErrors] = useState<Partial<Record<"current" | "next" | "confirm", string | null>>>({});

  const change = useMutation({
    mutationFn: () =>
      api.account.changePassword({ current_password: current, new_password: next, revoke_tokens: revokeTokens }),
    onSuccess: (res) => {
      // The old token is now invalid everywhere; adopt the new one (other tabs of this browser get it too).
      session.update(res.token);
      queryClient.setQueryData(qk.account, (old: CurrentAccount | undefined) =>
        old ? { ...old, ...res.account } : { ...res.account, auth: "session" as const, api_token: null },
      );
      void queryClient.invalidateQueries({ queryKey: qk.tokens });
      setCurrent("");
      setNext("");
      setConfirm("");
      toast.success(`Password changed. All other sessions were signed out.${revokedText(res.tokens_revoked, revokeTokens)}`);
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "invalid_current_password") {
        setErrors({ current: "The current password is wrong." });
      } else if (isApiError(err) && err.code === "validation_failed" && err.field === "current_password") {
        setErrors({ current: err.message });
      } else if (isApiError(err) && err.code === "validation_failed" && err.field === "new_password") {
        setErrors({ next: err.message });
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = {
      current: current ? null : "Enter your current password.",
      next: validatePassword(next) ?? (next === current ? "Choose a password different from the current one." : null),
      confirm: confirm === next ? null : "The passwords do not match.",
    };
    setErrors(errs);
    if (errs.current || errs.next || errs.confirm) return;
    change.mutate();
  }

  const showFormError =
    change.isError &&
    !(
      isApiError(change.error) &&
      (change.error.code === "invalid_current_password" ||
        change.error.field === "new_password" ||
        change.error.field === "current_password")
    );

  return (
    <Section
      title="Change password"
      description="Changing the password signs out every other session of this account. This tab stays signed in."
    >
      <form onSubmit={submit} noValidate className="flex max-w-sm flex-col gap-4">
        <Field label="Current password" error={errors.current}>
          {(p) => (
            <Input {...p} type="password" autoComplete="current-password" value={current} onChange={(e) => setCurrent(e.target.value)} />
          )}
        </Field>
        <Field label="New password" error={errors.next} hint="12 to 72 bytes.">
          {(p) => <Input {...p} type="password" autoComplete="new-password" value={next} onChange={(e) => setNext(e.target.value)} />}
        </Field>
        <Field label="Repeat new password" error={errors.confirm}>
          {(p) => (
            <Input {...p} type="password" autoComplete="new-password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
          )}
        </Field>
        <div className="flex flex-col gap-1">
          <Checkbox
            label="Also revoke all my API tokens"
            checked={revokeTokens}
            onChange={(e) => setRevokeTokens(e.target.checked)}
          />
          <p className="pl-6 text-xs text-muted-foreground">
            Recommended if the password may have leaked: scripts using your tokens stop working until you create new ones.
          </p>
        </div>
        {showFormError && <ErrorText error={change.error} />}
        <div>
          <Button type="submit" loading={change.isPending}>
            Change password
          </Button>
        </div>
      </form>
    </Section>
  );
}

/** How the password change treated API tokens, as a sentence to append. */
export function revokedText(revoked: number, requested: boolean): string {
  if (revoked > 0) return ` ${revoked} API token${revoked === 1 ? " was" : "s were"} revoked.`;
  return requested ? " You had no API tokens to revoke." : " Your API tokens keep working.";
}

const EXPIRY_OPTIONS = [
  { value: "never", label: "Never" },
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
  { value: "365", label: "1 year" },
] as const;

function isExpired(t: APIToken): boolean {
  return t.expires_at !== null && new Date(t.expires_at).getTime() <= Date.now();
}

function Tokens() {
  const { api } = useServices();
  const toast = useToast();
  const queryClient = useQueryClient();
  const tokens = useTokens();
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<CreateAPITokenResponse | null>(null);
  const [revoking, setRevoking] = useState<APIToken | null>(null);

  return (
    <Section
      title="API tokens"
      description="Tokens let scripts and CI use the API. A token acts as a developer at most, even when an administrator creates it, and cannot manage accounts, tokens or settings."
      actions={
        <Button size="sm" onClick={() => setCreating(true)}>
          <Plus />
          New token
        </Button>
      }
    >
      {tokens.isPending ? (
        <LoadingState label="Loading tokens" />
      ) : tokens.isError ? (
        <ErrorState error={tokens.error} title="Could not load tokens" onRetry={() => void tokens.refetch()} />
      ) : tokens.data.items.length === 0 ? (
        <EmptyState title="No API tokens" description="Create one for automation. Its secret is shown only once." />
      ) : (
        <DataTable
          label="API tokens"
          data={tokens.data.items}
          rowKey={(t) => t.id}
          columns={[
            { key: "name", header: "Name", cell: (t) => <span className="font-medium">{t.name}</span>, sortValue: (t) => t.name },
            { key: "prefix", header: "Prefix", cell: (t) => <code className="font-mono text-xs">{t.prefix}</code> },
            {
              key: "created",
              header: "Created",
              cell: (t) => <span title={formatDateTime(t.created_at)}>{formatRelative(t.created_at)}</span>,
              sortValue: (t) => t.created_at,
              hideOnSmall: true,
            },
            {
              key: "used",
              header: "Last used",
              cell: (t) => <span title={formatDateTime(t.last_used_at)}>{formatRelative(t.last_used_at)}</span>,
            },
            {
              key: "expires",
              header: "Expires",
              cell: (t) =>
                isExpired(t) ? (
                  <Badge tone="danger">Expired</Badge>
                ) : t.expires_at ? (
                  <span title={formatDateTime(t.expires_at)}>{formatRelative(t.expires_at)}</span>
                ) : (
                  <span className="text-muted-foreground">Never</span>
                ),
            },
          ]}
          actions={(t) => (
            <Button size="sm" variant="outline" onClick={() => setRevoking(t)}>
              <Trash2 />
              Revoke
            </Button>
          )}
        />
      )}

      {creating && (
        <CreateTokenDialog
          onClose={() => setCreating(false)}
          onCreated={(res) => {
            setCreating(false);
            setCreated(res);
            void queryClient.invalidateQueries({ queryKey: qk.tokens });
          }}
        />
      )}
      {created && <TokenSecretDialog created={created} onClose={() => setCreated(null)} />}
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(o) => !o && setRevoking(null)}
        title="Revoke API token"
        description={
          <>
            Revoke <span className="font-medium text-foreground">{revoking?.name}</span> ({revoking?.prefix})? Anything using it
            stops working immediately.
          </>
        }
        confirmLabel="Revoke token"
        destructive
        onConfirm={async () => {
          if (!revoking) return;
          await api.account.deleteToken(revoking.id);
          toast.success(`Token ${revoking.name} revoked.`);
          await queryClient.invalidateQueries({ queryKey: qk.tokens });
        }}
      />
    </Section>
  );
}

function CreateTokenDialog({ onClose, onCreated }: { onClose: () => void; onCreated: (r: CreateAPITokenResponse) => void }) {
  const { api } = useServices();
  const [name, setName] = useState("");
  const [expiry, setExpiry] = useState<string>("90");
  const [password, setPassword] = useState("");
  const [errors, setErrors] = useState<Partial<Record<"name" | "password", string | null>>>({});
  const create = useMutation({
    mutationFn: () => {
      const days = expiry === "never" ? null : Number(expiry);
      return api.account.createToken({
        name: name.trim(),
        current_password: password,
        ...(days ? { expires_at: new Date(Date.now() + days * 86_400_000).toISOString() } : {}),
      });
    },
    onSuccess: (res) => {
      setPassword("");
      onCreated(res);
    },
    onError: (err) => {
      if (!isApiError(err)) return;
      if (err.code === "invalid_current_password") setErrors({ password: "The password is wrong." });
      else if (err.code === "validation_failed" && err.field === "current_password") setErrors({ password: err.message });
      else if (err.code === "validation_failed" && err.field === "name") setErrors({ name: err.message });
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = { name: validateTokenName(name), password: password ? null : "Enter your password." };
    setErrors(errs);
    if (!errs.name && !errs.password) create.mutate();
  }

  const shownInline =
    isApiError(create.error) &&
    (create.error.code === "invalid_current_password" || create.error.field === "name" || create.error.field === "current_password");
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="New API token"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="create-token" loading={create.isPending}>
            Create token
          </Button>
        </>
      }
    >
      <form id="create-token" onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Name" error={errors.name} hint="What uses this token, for example ci or deploy-script.">
          {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} maxLength={64} autoFocus />}
        </Field>
        <Field label="Expires">
          {(p) => (
            <Select {...p} value={expiry} onChange={(e) => setExpiry(e.target.value)}>
              {EXPIRY_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </Select>
          )}
        </Field>
        <Field
          label="Your password"
          error={errors.password}
          hint="Confirms it is you: a token is a long-lived credential."
        >
          {(p) => (
            <Input {...p} type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          )}
        </Field>
        {create.isError && !shownInline && <ErrorText error={create.error} />}
      </form>
    </Dialog>
  );
}

/** Shows a new token's secret exactly once. Closing requires an explicit click. */
export function TokenSecretDialog({ created, onClose }: { created: CreateAPITokenResponse; onClose: () => void }) {
  return (
    <Dialog
      open
      // Only the button closes this dialog, so the secret is not lost by a stray click or Escape.
      dismissible={false}
      onOpenChange={onClose}
      title="Copy your new token"
      description={`Token "${created.token.name}" was created.`}
      footer={<Button onClick={onClose}>I have copied the token</Button>}
    >
      <div className="flex flex-col gap-4">
        <Notice tone="warning" title="This is the only time the secret is shown">
          Store it somewhere safe now, for example in your CI secret store. If you lose it, revoke the token and create a
          new one.
        </Notice>
        <div className="flex items-center gap-2">
          <KeyRound aria-hidden className="size-4 shrink-0 text-muted-foreground" />
          <Input
            readOnly
            value={created.secret}
            aria-label="Token secret"
            className="font-mono text-xs"
            onFocus={(e) => e.currentTarget.select()}
          />
          <CopyButton value={created.secret} />
        </div>
        <p className="text-xs text-muted-foreground">
          Use it as <code className="font-mono">Authorization: Bearer &lt;token&gt;</code>.
        </p>
      </div>
    </Dialog>
  );
}
