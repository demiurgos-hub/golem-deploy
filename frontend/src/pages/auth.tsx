import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useSearch } from "@tanstack/react-router";
import { Moon, Sun } from "lucide-react";
import { useServices, useSession } from "@/app/services";
import { describeError, isApiError, readyAgain, withWait } from "@/api/errors";
import { useAuthOptions } from "@/api/queries";
import type { CurrentAccount } from "@/api/types.gen";
import { qk } from "@/api/query-keys";
import { useTheme } from "@/hooks/use-theme";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { Checkbox, Field, Input } from "@/components/ui/input";
import { ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { normalizeUsername, validatePassword, validateUsername } from "@/lib/validators";
import { clearSetupToken, heldSetupToken } from "@/lib/setup-token";
import { safeRedirectPath } from "@/lib/url";

function AuthLayout({ title, children }: { title: string; children: ReactNode }) {
  const theme = useTheme();
  return (
    <div className="flex min-h-screen flex-col items-center justify-center bg-background px-4 py-10">
      <div className="absolute right-4 top-4">
        <Button
          variant="ghost"
          size="icon"
          onClick={theme.toggle}
          aria-label={theme.dark ? "Switch to light theme" : "Switch to dark theme"}
        >
          {theme.dark ? <Sun /> : <Moon />}
        </Button>
      </div>
      <main className="w-full max-w-sm">
        <p className="mb-6 text-center text-lg font-semibold tracking-tight">Golem Deploy</p>
        <div className="rounded-lg border border-border bg-card p-6 shadow-sm">
          <h1 className="mb-5 text-base font-semibold">{title}</h1>
          {children}
        </div>
      </main>
    </div>
  );
}

/** A per-second countdown for rate limits: returns the seconds left and a function to start it. */
function useCountdown(): [number, (seconds: number) => void] {
  const [state, setState] = useState<{ until: number; now: number } | null>(null);
  useEffect(() => {
    if (state === null || state.now >= state.until) return;
    const t = setTimeout(() => setState((s) => (s ? { ...s, now: Date.now() } : s)), 1000);
    return () => clearTimeout(t);
  }, [state]);
  const remaining = state ? Math.max(0, Math.ceil((state.until - state.now) / 1000)) : 0;
  const start = (seconds: number) => {
    const now = Date.now();
    setState({ until: now + seconds * 1000, now });
  };
  return [remaining, start];
}

type FieldErrors = Partial<Record<"username" | "password" | "confirm", string | null>>;

/** Errors after which the form waits for Retry-After before it can be sent again. */
function waitsForRetry(err: unknown): boolean {
  return isApiError(err) && (err.code === "rate_limited" || err.code === "service_unavailable") && !!err.retryAfter;
}

/** The daemon's message with a live countdown instead of the static Retry-After. */
function errorWithCountdown(err: unknown, wait: number): string {
  if (waitsForRetry(err) && isApiError(err)) {
    return wait > 0 ? withWait(err.message, wait) : readyAgain(err.message);
  }
  return describeError(err);
}

/** The host command that prints a new setup link (golem-deploy admin setup-link). */
export const SETUP_LINK_COMMAND =
  "sudo runuser -u golem-deploy -- golem-deploy admin setup-link --data-dir /var/lib/golem-deploy";

function SetupLinkCommand() {
  return (
    <span className="flex items-start gap-1">
      <code className="min-w-0 flex-1 break-all rounded-md bg-muted px-2 py-1 font-mono text-xs text-foreground">
        {SETUP_LINK_COMMAND}
      </code>
      <CopyButton value={SETUP_LINK_COMMAND} size="icon" variant="ghost" label="Copy the command" />
    </span>
  );
}

/** Shown on the login page while no admin account exists. */
function SetupRequiredNotice() {
  return (
    <Notice title="No admin account yet">
      <span className="flex flex-col gap-2">
        <span>
          This installation has no admin account yet. The setup link creates it. The installer prints the link when it
          finishes, or make a new one on the host with:
        </span>
        <SetupLinkCommand />
      </span>
    </Notice>
  );
}

export function LoginPage() {
  const { api, session, queryClient } = useServices();
  const snapshot = useSession();
  const navigate = useNavigate();
  const search: { redirect?: string } = useSearch({ strict: false });
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [persist, setPersist] = useState(false);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);
  const [wait, startWait] = useCountdown();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const next: FieldErrors = { username: validateUsername(username), password: validatePassword(password) };
    setErrors(next);
    setFormError(null);
    if (next.username || next.password) return;
    setPending(true);
    try {
      const res = await api.auth.login({ username: normalizeUsername(username), password });
      queryClient.clear();
      const me: CurrentAccount = { ...res.account, auth: "session", api_token: null };
      queryClient.setQueryData(qk.account, me);
      session.start(res.token, persist);
      await navigate({ href: safeRedirectPath(search.redirect) ?? "/projects", replace: true });
    } catch (err) {
      setPending(false);
      if (waitsForRetry(err) && isApiError(err) && err.retryAfter) startWait(err.retryAfter);
      if (isApiError(err) && err.code === "validation_failed" && (err.field === "username" || err.field === "password")) {
        setErrors({ [err.field]: err.message });
      } else {
        setFormError(err);
      }
    }
  }

  const waiting = waitsForRetry(formError) && wait > 0;
  const options = useAuthOptions();
  return (
    <AuthLayout title="Sign in">
      <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
        {snapshot.notice && <Notice tone="warning">{snapshot.notice}</Notice>}
        {options.data?.setup_required && <SetupRequiredNotice />}
        <Field label="Username" error={errors.username}>
          {(p) => (
            <Input
              {...p}
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
            />
          )}
        </Field>
        <Field label="Password" error={errors.password}>
          {(p) => (
            <Input
              {...p}
              name="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </Field>
        <Checkbox
          label="Keep me signed in on this device"
          checked={persist}
          onChange={(e) => setPersist(e.target.checked)}
        />
        {formError !== null && <ErrorText error={formError}>{errorWithCountdown(formError, wait)}</ErrorText>}
        <Button type="submit" loading={pending} disabled={waiting}>
          Sign in
        </Button>
        {options.data && (
          <p className="text-center text-xs text-muted-foreground">
            {options.data.registration_enabled ? (
              <>
                No account?{" "}
                <Link to="/register" search={search} className="font-medium text-foreground underline underline-offset-4">
                  Request one
                </Link>
                . An administrator approves it before you can sign in.
              </>
            ) : (
              "No account? Ask an administrator to create one for you."
            )}
          </p>
        )}
      </form>
    </AuthLayout>
  );
}

export function RegisterPage() {
  const { api } = useServices();
  const options = useAuthOptions();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [errors, setErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);
  const [created, setCreated] = useState<string | null>(null);
  const [wait, startWait] = useCountdown();

  async function submit(e: FormEvent) {
    e.preventDefault();
    const next: FieldErrors = {
      username: validateUsername(username),
      password: validatePassword(password),
      confirm: confirm !== password ? "The passwords do not match." : null,
    };
    setErrors(next);
    setFormError(null);
    if (next.username || next.password || next.confirm) return;
    setPending(true);
    try {
      const account = await api.auth.register({ username: normalizeUsername(username), password });
      setCreated(account.username);
    } catch (err) {
      if (waitsForRetry(err) && isApiError(err) && err.retryAfter) startWait(err.retryAfter);
      if (isApiError(err) && err.code === "username_taken") {
        setErrors({ username: "This username is taken. Choose another." });
      } else if (isApiError(err) && err.code === "validation_failed" && (err.field === "username" || err.field === "password")) {
        setErrors({ [err.field]: err.message });
      } else {
        setFormError(err);
      }
    } finally {
      setPending(false);
    }
  }

  if (created) {
    return (
      <AuthLayout title="Account requested">
        <div className="flex flex-col gap-4 text-sm">
          <p>
            The account <span className="font-medium">{created}</span> was created. An administrator must approve it before
            you can sign in.
          </p>
          <Link to="/login" className="font-medium underline underline-offset-4">
            Back to sign in
          </Link>
        </div>
      </AuthLayout>
    );
  }

  const disabled =
    (isApiError(formError) && formError.code === "registration_disabled") || options.data?.registration_enabled === false;
  const waiting = waitsForRetry(formError) && wait > 0;
  if (disabled) {
    return (
      <AuthLayout title="Request an account">
        <div className="flex flex-col gap-4 text-sm">
          <p>Self-registration is turned off on this host. Ask an administrator to create an account for you.</p>
          <Link to="/login" className="font-medium underline underline-offset-4">
            Back to sign in
          </Link>
        </div>
      </AuthLayout>
    );
  }
  return (
    <AuthLayout title="Request an account">
      <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
        <Field label="Username" error={errors.username} hint="3 to 32 characters: lower-case letters, digits, '.', '_' or '-'.">
          {(p) => (
            <Input
              {...p}
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
            />
          )}
        </Field>
        <Field label="Password" error={errors.password} hint="12 to 72 bytes.">
          {(p) => (
            <Input
              {...p}
              name="new-password"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </Field>
        <Field label="Repeat password" error={errors.confirm}>
          {(p) => (
            <Input
              {...p}
              name="confirm-password"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
          )}
        </Field>
        {formError !== null && <ErrorText error={formError}>{errorWithCountdown(formError, wait)}</ErrorText>}
        <Button type="submit" loading={pending} disabled={waiting}>
          Request account
        </Button>
        <p className="text-center text-xs text-muted-foreground">
          Already have an account?{" "}
          <Link to="/login" className="font-medium text-foreground underline underline-offset-4">
            Sign in
          </Link>
        </p>
      </form>
    </AuthLayout>
  );
}

/**
 * Creates the first admin account with the one-time setup link
 * (golem-deploy admin setup-link prints "<origin>/setup#t=<token>"). main.tsx
 * has already moved the token from the address bar into the holder in
 * lib/setup-token.ts; the page takes it from there and clears it once the
 * token is used up or refused.
 */
export function SetupPage() {
  const { api, session, queryClient } = useServices();
  const navigate = useNavigate();
  const options = useAuthOptions();
  const [token, setToken] = useState<string | null>(heldSetupToken);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [persist, setPersist] = useState(false);
  const [errors, setErrors] = useState<FieldErrors>({});
  const [formError, setFormError] = useState<unknown>(null);
  const [pending, setPending] = useState(false);
  const [wait, startWait] = useCountdown();

  // Once an admin exists, no setup link works any more.
  const setupDone = options.data?.setup_required === false;
  useEffect(() => {
    if (setupDone) clearSetupToken();
  }, [setupDone]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const next: FieldErrors = {
      username: validateUsername(username),
      password: validatePassword(password),
      confirm: confirm !== password ? "The passwords do not match." : null,
    };
    setErrors(next);
    setFormError(null);
    if (next.username || next.password || next.confirm || !token) return;
    setPending(true);
    try {
      const res = await api.auth.setup({ token, username: normalizeUsername(username), password });
      clearSetupToken();
      queryClient.clear();
      const me: CurrentAccount = { ...res.account, auth: "session", api_token: null };
      queryClient.setQueryData(qk.account, me);
      session.start(res.token, persist);
      await navigate({ to: "/", replace: true });
    } catch (err) {
      setPending(false);
      if (waitsForRetry(err) && isApiError(err) && err.retryAfter) startWait(err.retryAfter);
      if (isApiError(err) && err.code === "setup_link_invalid") {
        clearSetupToken();
        setToken(null);
      } else if (isApiError(err) && err.code === "username_taken") {
        setErrors({ username: "This username is taken. Choose another." });
      } else if (isApiError(err) && err.code === "validation_failed" && (err.field === "username" || err.field === "password")) {
        setErrors({ [err.field]: err.message });
      } else {
        setFormError(err);
      }
    }
  }

  if (setupDone) {
    return (
      <AuthLayout title="Set up Golem Deploy">
        <div className="flex flex-col gap-4 text-sm">
          <p>This setup link is missing, expired or already used. A setup link works once and only until it expires.</p>
          <p>
            This installation already has an admin account, so no setup link is needed.{" "}
            <Link to="/login" className="font-medium underline underline-offset-4">
              Sign in
            </Link>{" "}
            instead.
          </p>
        </div>
      </AuthLayout>
    );
  }

  // Wait for the options rather than show a form or advice that may not
  // apply. If they cannot be read, go on: the daemon decides the token.
  if (options.isPending) {
    return (
      <AuthLayout title="Set up Golem Deploy">
        <LoadingState />
      </AuthLayout>
    );
  }

  if (!token) {
    return (
      <AuthLayout title="Set up Golem Deploy">
        <div className="flex flex-col gap-4 text-sm">
          <p>This setup link is missing, expired or already used. A setup link works once and only until it expires.</p>
          <p>The installer prints a setup link when it finishes. To make a new one, run this command on the host:</p>
          <SetupLinkCommand />
          <p>Then open the new link in your browser.</p>
          <p>
            If an admin account already exists,{" "}
            <Link to="/login" className="font-medium underline underline-offset-4">
              sign in
            </Link>{" "}
            instead.
          </p>
        </div>
      </AuthLayout>
    );
  }

  const waiting = waitsForRetry(formError) && wait > 0;
  return (
    <AuthLayout title="Create the admin account">
      <form onSubmit={(e) => void submit(e)} noValidate className="flex flex-col gap-4">
        <p className="text-sm text-muted-foreground">
          Choose the username and password of the first administrator of this installation.
        </p>
        <Field label="Username" error={errors.username} hint="3 to 32 characters: lower-case letters, digits, '.', '_' or '-'.">
          {(p) => (
            <Input
              {...p}
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
            />
          )}
        </Field>
        <Field label="Password" error={errors.password} hint="12 to 72 bytes.">
          {(p) => (
            <Input
              {...p}
              name="new-password"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          )}
        </Field>
        <Field label="Repeat password" error={errors.confirm}>
          {(p) => (
            <Input
              {...p}
              name="confirm-password"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
          )}
        </Field>
        <Checkbox
          label="Keep me signed in on this device"
          checked={persist}
          onChange={(e) => setPersist(e.target.checked)}
        />
        {formError !== null && <ErrorText error={formError}>{errorWithCountdown(formError, wait)}</ErrorText>}
        <Button type="submit" loading={pending} disabled={waiting}>
          Create admin account
        </Button>
      </form>
    </AuthLayout>
  );
}
