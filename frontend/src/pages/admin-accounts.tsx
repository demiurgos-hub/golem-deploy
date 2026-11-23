import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, Trash2 } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError } from "@/api/errors";
import { useAccount, useAccountTokens, useAccounts } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { APIToken, Account, Role, UpdateAccountRequest } from "@/api/types.gen";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { ConfirmDialog, Dialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Select } from "@/components/ui/input";
import { PageHeader } from "@/components/ui/page";
import { EmptyState, ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, formatRelative } from "@/lib/format";
import { normalizeUsername, validatePassword, validateUsername } from "@/lib/validators";

type PendingChange =
  | { kind: "update"; account: Account; body: UpdateAccountRequest; title: string; description: string; label: string }
  | { kind: "delete"; account: Account };

export function AdminAccountsPage() {
  const accounts = useAccounts();
  const me = useAccount();
  const [creating, setCreating] = useState(false);
  const [pending, setPending] = useState<PendingChange | null>(null);
  const [tokensOf, setTokensOf] = useState<Account | null>(null);

  const sorted = accounts.data
    ? [...accounts.data.items].sort((a, b) => Number(a.approved) - Number(b.approved) || a.username.localeCompare(b.username))
    : [];

  return (
    <>
      <PageHeader
        title="Accounts"
        description="Approve registrations, change roles and remove accounts. Role changes and un-approval sign the account out everywhere."
        actions={
          <Button onClick={() => setCreating(true)}>
            <Plus />
            New account
          </Button>
        }
      />
      {accounts.isPending ? (
        <LoadingState label="Loading accounts" />
      ) : accounts.isError ? (
        <ErrorState error={accounts.error} title="Could not load accounts" onRetry={() => void accounts.refetch()} />
      ) : sorted.length === 0 ? (
        <EmptyState title="No accounts" />
      ) : (
        <DataTable
          label="Accounts"
          data={sorted}
          rowKey={(a) => a.id}
          columns={[
            {
              key: "username",
              header: "Username",
              sortValue: (a) => a.username,
              cell: (a) => (
                <span className="font-medium">
                  {a.username}
                  {a.id === me.data?.id && <span className="ml-1.5 text-xs font-normal text-muted-foreground">(you)</span>}
                </span>
              ),
            },
            {
              key: "role",
              header: "Role",
              sortValue: (a) => a.role,
              cell: (a) => <Badge tone={a.role === "admin" ? "info" : "neutral"}>{a.role}</Badge>,
            },
            {
              key: "approved",
              header: "Status",
              cell: (a) => (a.approved ? <Badge tone="success">Approved</Badge> : <Badge tone="warning">Awaiting approval</Badge>),
            },
            {
              key: "created",
              header: "Created",
              hideOnSmall: true,
              sortValue: (a) => a.created_at,
              cell: (a) => <span title={formatDateTime(a.created_at)}>{formatRelative(a.created_at)}</span>,
            },
          ]}
          actions={(a) => (
            <>
              <Button size="sm" variant="outline" aria-label={`API tokens of ${a.username}`} onClick={() => setTokensOf(a)}>
                <KeyRound />
                Tokens
              </Button>
              <AccountActions account={a} onRequest={setPending} />
            </>
          )}
        />
      )}
      {creating && <CreateAccountDialog onClose={() => setCreating(false)} />}
      {tokensOf && <AccountTokensDialog account={tokensOf} onClose={() => setTokensOf(null)} />}
      <PendingChangeDialog change={pending} onClose={() => setPending(null)} />
    </>
  );
}

function AccountActions({ account, onRequest }: { account: Account; onRequest: (c: PendingChange) => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  // Approving is not destructive, so it runs directly with its own per-row pending state.
  const approve = useMutation({
    mutationFn: () => api.accounts.update(account.id, { approved: true }),
    onSuccess: () => {
      toast.success(`${account.username} can now sign in.`);
      void queryClient.invalidateQueries({ queryKey: qk.accounts });
      void queryClient.invalidateQueries({ queryKey: qk.status });
    },
    onError: (err) => toast.error(err, `Could not approve ${account.username}`),
  });
  const otherRole: Role = account.role === "admin" ? "developer" : "admin";
  return (
    <>
      {account.approved ? (
        <Button
          size="sm"
          variant="outline"
          onClick={() =>
            onRequest({
              kind: "update",
              account,
              body: { approved: false },
              title: "Unapprove account",
              description: `${account.username} will be signed out everywhere and cannot sign in until approved again. Their API tokens are deleted and do not come back when the account is approved again.`,
              label: "Unapprove",
            })
          }
        >
          Unapprove
        </Button>
      ) : (
        <Button size="sm" variant="outline" loading={approve.isPending} onClick={() => approve.mutate()}>
          Approve
        </Button>
      )}
      <Button
        size="sm"
        variant="outline"
        onClick={() =>
          onRequest({
            kind: "update",
            account,
            body: { role: otherRole },
            title: otherRole === "admin" ? "Make administrator" : "Make developer",
            description:
              otherRole === "admin"
                ? `${account.username} will be able to manage accounts, settings and host resources. Their current sessions end and they sign in again; their API tokens keep working (as developer).`
                : `${account.username} loses administrator rights. Their current sessions end and they sign in again; their API tokens keep working.`,
            label: otherRole === "admin" ? "Make administrator" : "Make developer",
          })
        }
      >
        {otherRole === "admin" ? "Make admin" : "Make developer"}
      </Button>
      <Button size="sm" variant="outline" aria-label={`Delete ${account.username}`} onClick={() => onRequest({ kind: "delete", account })}>
        <Trash2 />
      </Button>
    </>
  );
}

/** Another account's API tokens (metadata only): an admin can revoke any of them, e.g. after a leak. */
function AccountTokensDialog({ account, onClose }: { account: Account; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const tokens = useAccountTokens(account.id);
  const [revoking, setRevoking] = useState<APIToken | null>(null);
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={`API tokens of ${account.username}`}
      description="Tokens act as a developer at most. Secrets are never shown; revoking a token stops everything that uses it."
      size="lg"
      footer={<Button onClick={onClose}>Close</Button>}
    >
      {tokens.isPending ? (
        <LoadingState label="Loading tokens" />
      ) : tokens.isError ? (
        <ErrorState error={tokens.error} title="Could not load tokens" onRetry={() => void tokens.refetch()} />
      ) : tokens.data.items.length === 0 ? (
        <EmptyState title="No API tokens" description={`${account.username} has no API tokens.`} />
      ) : (
        <DataTable
          label={`API tokens of ${account.username}`}
          data={tokens.data.items}
          rowKey={(t) => t.id}
          columns={[
            { key: "name", header: "Name", cell: (t) => <span className="font-medium">{t.name}</span>, sortValue: (t) => t.name },
            { key: "prefix", header: "Prefix", cell: (t) => <code className="font-mono text-xs">{t.prefix}</code> },
            {
              key: "used",
              header: "Last used",
              cell: (t) => <span title={formatDateTime(t.last_used_at)}>{formatRelative(t.last_used_at)}</span>,
            },
            {
              key: "expires",
              header: "Expires",
              cell: (t) => (t.expires_at ? <span title={formatDateTime(t.expires_at)}>{formatRelative(t.expires_at)}</span> : "Never"),
            },
          ]}
          actions={(t) => (
            <Button size="sm" variant="outline" onClick={() => setRevoking(t)} aria-label={`Revoke ${t.name}`}>
              <Trash2 />
              Revoke
            </Button>
          )}
        />
      )}
      {account.approved ? null : (
        <div className="mt-3">
          <Notice tone="info">Un-approving an account deletes its tokens; re-approval does not bring them back.</Notice>
        </div>
      )}
      <ConfirmDialog
        open={revoking !== null}
        onOpenChange={(o) => !o && setRevoking(null)}
        title="Revoke API token"
        description={
          <>
            Revoke <span className="font-medium text-foreground">{revoking?.name}</span> ({revoking?.prefix}) of {account.username}?
            Anything using it stops working immediately.
          </>
        }
        confirmLabel="Revoke token"
        destructive
        onConfirm={async () => {
          if (!revoking) return;
          await api.accounts.deleteToken(account.id, revoking.id);
          toast.success(`Token ${revoking.name} of ${account.username} revoked.`);
          await queryClient.invalidateQueries({ queryKey: qk.accountTokens(account.id) });
        }}
      />
    </Dialog>
  );
}

function PendingChangeDialog({ change, onClose }: { change: PendingChange | null; onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const name = change?.account.username ?? "";
  return (
    <ConfirmDialog
      open={change !== null}
      onOpenChange={(o) => !o && onClose()}
      title={change?.kind === "delete" ? "Delete account" : (change?.title ?? "")}
      description={
        change?.kind === "delete" ? (
          <>
            Delete <span className="font-medium text-foreground">{name}</span>? Their sessions and API tokens stop working. This
            cannot be undone.
          </>
        ) : (
          (change?.description ?? "")
        )
      }
      confirmLabel={change?.kind === "delete" ? "Delete account" : (change?.label ?? "Confirm")}
      destructive={change?.kind === "delete"}
      onConfirm={async () => {
        if (!change) return;
        if (change.kind === "delete") {
          await api.accounts.remove(change.account.id);
          toast.success(`Account ${name} deleted.`);
        } else {
          await api.accounts.update(change.account.id, change.body);
          toast.success(`Account ${name} updated.`);
        }
        void queryClient.invalidateQueries({ queryKey: qk.accounts });
        void queryClient.invalidateQueries({ queryKey: qk.status });
      }}
    />
  );
}

function CreateAccountDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("developer");
  const [approved, setApproved] = useState(true);
  const [errors, setErrors] = useState<Partial<Record<"username" | "password", string | null>>>({});

  const create = useMutation({
    mutationFn: () => api.accounts.create({ username: normalizeUsername(username), password, role, approved }),
    onSuccess: (acct) => {
      toast.success(`Account ${acct.username} created.`);
      void queryClient.invalidateQueries({ queryKey: qk.accounts });
      void queryClient.invalidateQueries({ queryKey: qk.status });
      onClose();
    },
    onError: (err) => {
      if (isApiError(err) && err.code === "username_taken") setErrors({ username: "This username is taken." });
      else if (isApiError(err) && err.code === "validation_failed" && (err.field === "username" || err.field === "password")) {
        setErrors({ [err.field]: err.message });
      }
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs = { username: validateUsername(username), password: validatePassword(password) };
    setErrors(errs);
    if (!errs.username && !errs.password) create.mutate();
  }

  const shownInline = isApiError(create.error) && (create.error.code === "username_taken" || create.error.field);
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title="New account"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" form="create-account" loading={create.isPending}>
            Create account
          </Button>
        </>
      }
    >
      <form id="create-account" onSubmit={submit} noValidate className="flex flex-col gap-4">
        <Field label="Username" error={errors.username}>
          {(p) => (
            <Input {...p} autoComplete="off" spellCheck={false} value={username} onChange={(e) => setUsername(e.target.value)} autoFocus />
          )}
        </Field>
        <Field label="Initial password" error={errors.password} hint="12 to 72 bytes. Share it securely; the user can change it under Account.">
          {(p) => <Input {...p} type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />}
        </Field>
        <Field label="Role">
          {(p) => (
            <Select {...p} value={role} onChange={(e) => setRole(e.target.value === "admin" ? "admin" : "developer")}>
              <option value="developer">Developer</option>
              <option value="admin">Administrator</option>
            </Select>
          )}
        </Field>
        <Checkbox label="Approved (can sign in right away)" checked={approved} onChange={(e) => setApproved(e.target.checked)} />
        {create.isError && !shownInline && <ErrorText error={create.error} />}
      </form>
    </Dialog>
  );
}
