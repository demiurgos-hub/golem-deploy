import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient } from "@/api/client";
import type { APIToken, UnapprovedAccount } from "@/api/types";
import { useAuth } from "@/hooks/use-auth";
import { ConfirmDialog } from "@/components/shared/confirm-dialog";
import { useDashboard } from "@/components/layout/dashboard-layout";
import { Check, Copy, X, Trash2 } from "lucide-react";

export function AccountTab() {
  const { isAdmin } = useAuth();
  const { activeTab, refreshKey } = useDashboard();
  const queryClient = useQueryClient();

  // Change password state
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmNewPassword, setConfirmNewPassword] = useState("");
  const [passwordError, setPasswordError] = useState<string | null>(null);
  const [passwordSuccess, setPasswordSuccess] = useState(false);

  // API tokens
  const [tokenName, setTokenName] = useState("");
  const [createdToken, setCreatedToken] = useState<string | null>(null);
  const [tokenCopied, setTokenCopied] = useState(false);
  const [tokenError, setTokenError] = useState<string | null>(null);
  const [confirmRevokeToken, setConfirmRevokeToken] = useState<APIToken | null>(null);

  // Admin state
  const [confirmDeleteAccount, setConfirmDeleteAccount] = useState<string | null>(null);

  const { data: unapprovedAccounts = [] } = useQuery({
    queryKey: ["unapproved-accounts", refreshKey],
    queryFn: () => apiClient.getUnapprovedAccounts(),
    enabled: activeTab === "account" && isAdmin,
    // No refetchInterval — manual refresh only
  });

  const { data: allAccounts = [] } = useQuery({
    queryKey: ["all-accounts", refreshKey],
    queryFn: () => apiClient.getAllAccounts(),
    enabled: activeTab === "account" && isAdmin,
  });

  const { data: apiTokens = [] } = useQuery({
    queryKey: ["api-tokens", refreshKey],
    queryFn: () => apiClient.listAPITokens(),
    enabled: activeTab === "account",
  });

  const changePasswordMutation = useMutation({
    mutationFn: () => apiClient.changePassword(oldPassword, newPassword),
    onSuccess: () => {
      setOldPassword("");
      setNewPassword("");
      setConfirmNewPassword("");
      setPasswordError(null);
      setPasswordSuccess(true);
    },
    onError: (err) => {
      setPasswordError(
        err instanceof Error ? err.message : "Failed to change password",
      );
    },
  });

  const approveMutation = useMutation({
    mutationFn: ({ username, approve }: { username: string; approve: boolean }) =>
      apiClient.approveAccount(username, approve),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["unapproved-accounts"] });
      queryClient.invalidateQueries({ queryKey: ["all-accounts"] });
    },
  });

  const deleteAccountMutation = useMutation({
    mutationFn: (username: string) => apiClient.deleteAccount(username),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["all-accounts"] });
      queryClient.invalidateQueries({ queryKey: ["unapproved-accounts"] });
    },
  });

  const createTokenMutation = useMutation({
    mutationFn: (name: string) => apiClient.createAPIToken(name),
    onSuccess: (resp) => {
      setTokenName("");
      setTokenError(null);
      setCreatedToken(resp.token ?? null);
      setTokenCopied(false);
      queryClient.invalidateQueries({ queryKey: ["api-tokens"] });
    },
    onError: (err) => {
      setTokenError(err instanceof Error ? err.message : "Failed to create token");
    },
  });

  const revokeTokenMutation = useMutation({
    mutationFn: (id: number) => apiClient.revokeAPIToken(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["api-tokens"] });
    },
  });

  const handleChangePassword = (e: React.FormEvent) => {
    e.preventDefault();
    setPasswordError(null);
    setPasswordSuccess(false);
    if (newPassword.length < 8) {
      setPasswordError("New password must be at least 8 characters");
      return;
    }
    if (newPassword !== confirmNewPassword) {
      setPasswordError("New passwords do not match");
      return;
    }
    changePasswordMutation.mutate();
  };

  const handleCreateToken = (e: React.FormEvent) => {
    e.preventDefault();
    const name = tokenName.trim();
    if (!name) {
      setTokenError("Token name is required");
      return;
    }
    setTokenError(null);
    createTokenMutation.mutate(name);
  };

  const copyCreatedToken = async () => {
    if (!createdToken) return;
    await navigator.clipboard.writeText(createdToken);
    setTokenCopied(true);
  };

  return (
    <div className="space-y-6 max-w-2xl">
      <h2 className="text-lg font-semibold">Account</h2>

      {/* Change Password */}
      <div className="rounded-lg border border-border p-4">
        <h3 className="text-sm font-medium text-muted-foreground mb-3">
          Change Password
        </h3>
        <form onSubmit={handleChangePassword} className="space-y-3">
          <div className="space-y-1">
            <label className="text-sm font-medium">Current Password</label>
            <input
              type="password"
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              required
            />
          </div>
          <div className="space-y-1">
            <label className="text-sm font-medium">New Password</label>
            <input
              type="password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              placeholder="Min 8 characters"
              required
            />
          </div>
          <div className="space-y-1">
            <label className="text-sm font-medium">Confirm New Password</label>
            <input
              type="password"
              value={confirmNewPassword}
              onChange={(e) => setConfirmNewPassword(e.target.value)}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
              required
            />
          </div>
          {passwordError && (
            <p className="text-sm text-destructive">{passwordError}</p>
          )}
          {passwordSuccess && (
            <p className="text-sm text-green-600 dark:text-green-400">
              Password changed successfully
            </p>
          )}
          <button
            type="submit"
            disabled={changePasswordMutation.isPending}
            className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
          >
            {changePasswordMutation.isPending
              ? "Changing..."
              : "Change Password"}
          </button>
        </form>
      </div>

      {/* API Tokens */}
      <div className="rounded-lg border border-border p-4">
        <h3 className="text-sm font-medium text-muted-foreground mb-3">
          API Tokens
        </h3>
        <p className="text-sm text-muted-foreground mb-3">
          Tokens authenticate as a developer and cannot manage accounts or
          refresh a login session.
        </p>
        <form onSubmit={handleCreateToken} className="flex gap-2 mb-3">
          <input
            type="text"
            value={tokenName}
            onChange={(e) => setTokenName(e.target.value)}
            className="flex-1 rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring"
            placeholder="Token name"
            required
          />
          <button
            type="submit"
            disabled={createTokenMutation.isPending}
            className="rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
          >
            {createTokenMutation.isPending ? "Creating..." : "Create"}
          </button>
        </form>
        {tokenError && <p className="text-sm text-destructive mb-3">{tokenError}</p>}
        {createdToken && (
          <div className="mb-3 rounded-md border border-amber-500/40 bg-amber-500/10 p-3">
            <p className="text-sm font-medium mb-1">
              Copy this token now. It will not be shown again.
            </p>
            <div className="flex items-center gap-2">
              <code className="flex-1 break-all text-xs">{createdToken}</code>
              <button
                type="button"
                onClick={copyCreatedToken}
                className="rounded-md border border-border p-1.5 hover:bg-muted"
                title="Copy token"
              >
                <Copy className="h-3 w-3" />
              </button>
            </div>
            {tokenCopied && (
              <p className="text-xs text-green-600 dark:text-green-400 mt-1">
                Copied to clipboard
              </p>
            )}
          </div>
        )}
        {apiTokens.length === 0 ? (
          <p className="text-sm text-muted-foreground">No API tokens</p>
        ) : (
          <div className="space-y-2">
            {apiTokens.map((tok) => (
              <div
                key={tok.id}
                className="flex items-center justify-between rounded-md border border-border px-3 py-2"
              >
                <div>
                  <span className="font-medium text-sm">{tok.name}</span>
                  <span className="text-xs text-muted-foreground ml-2">
                    {tok.prefix} · {tok.created_at}
                  </span>
                </div>
                <button
                  onClick={() => setConfirmRevokeToken(tok)}
                  className="rounded-md p-1.5 text-muted-foreground hover:text-destructive"
                  title="Revoke"
                >
                  <Trash2 className="h-3 w-3" />
                </button>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Admin: Pending Approvals */}
      {isAdmin && (
        <div className="rounded-lg border border-border p-4">
          <h3 className="text-sm font-medium text-muted-foreground mb-3">
            Pending Approvals ({unapprovedAccounts.length})
          </h3>
          {unapprovedAccounts.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No pending approvals
            </p>
          ) : (
            <div className="space-y-2">
              {unapprovedAccounts.map((acc: UnapprovedAccount) => (
                <div
                  key={acc.id}
                  className="flex items-center justify-between rounded-md border border-border px-3 py-2"
                >
                  <div>
                    <span className="font-medium text-sm">{acc.username}</span>
                    <span className="text-xs text-muted-foreground ml-2">
                      Created: {acc.created_at}
                    </span>
                  </div>
                  <div className="flex gap-1">
                    <button
                      onClick={() =>
                        approveMutation.mutate({
                          username: acc.username,
                          approve: true,
                        })
                      }
                      className="rounded-md bg-green-600 p-1.5 text-white hover:bg-green-700"
                      title="Approve"
                    >
                      <Check className="h-3 w-3" />
                    </button>
                    <button
                      onClick={() =>
                        approveMutation.mutate({
                          username: acc.username,
                          approve: false,
                        })
                      }
                      className="rounded-md bg-destructive p-1.5 text-white hover:bg-destructive/90"
                      title="Deny"
                    >
                      <X className="h-3 w-3" />
                    </button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Admin: All Accounts */}
      {isAdmin && (
        <div className="rounded-lg border border-border p-4">
          <h3 className="text-sm font-medium text-muted-foreground mb-3">
            All Accounts ({allAccounts.length})
          </h3>
          {allAccounts.length === 0 ? (
            <p className="text-sm text-muted-foreground">No accounts</p>
          ) : (
            <div className="space-y-2">
              {allAccounts.map((acc: UnapprovedAccount) => (
                <div
                  key={acc.id}
                  className="flex items-center justify-between rounded-md border border-border px-3 py-2"
                >
                  <div>
                    <span className="font-medium text-sm">{acc.username}</span>
                    <span className="text-xs text-muted-foreground ml-2">
                      Level: {acc.access_level}
                    </span>
                  </div>
                  <button
                    onClick={() => setConfirmDeleteAccount(acc.username)}
                    className="rounded-md p-1.5 text-muted-foreground hover:text-destructive"
                    title="Delete"
                  >
                    <Trash2 className="h-3 w-3" />
                  </button>
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      <ConfirmDialog
        open={!!confirmRevokeToken}
        onOpenChange={() => setConfirmRevokeToken(null)}
        title="Revoke API Token"
        description={`Revoke "${confirmRevokeToken?.name}"? Requests using this token will fail immediately.`}
        confirmLabel="Revoke"
        destructive
        onConfirm={() => {
          if (confirmRevokeToken) revokeTokenMutation.mutate(confirmRevokeToken.id);
          setConfirmRevokeToken(null);
        }}
      />

      <ConfirmDialog
        open={!!confirmDeleteAccount}
        onOpenChange={() => setConfirmDeleteAccount(null)}
        title="Delete Account"
        description={`Permanently delete the account "${confirmDeleteAccount}"?`}
        confirmLabel="Delete"
        destructive
        onConfirm={() => {
          if (confirmDeleteAccount)
            deleteAccountMutation.mutate(confirmDeleteAccount);
          setConfirmDeleteAccount(null);
        }}
      />
    </div>
  );
}
