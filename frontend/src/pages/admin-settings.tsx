import { useRef, useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ShieldCheck, Upload } from "lucide-react";
import { useApi } from "@/app/services";
import { isApiError, sentence } from "@/api/errors";
import { useACME, useGameCertificate, useSettings } from "@/api/queries";
import { qk } from "@/api/query-keys";
import type { CertificateMode, GameCertificate, Job, UpdateGameCertificateRequest } from "@/api/types.gen";
import { CertificateDetails } from "@/components/certificate";
import { ACMESettings, AutomaticCertificateCheck, PanelCertificateSettings } from "@/components/certificate-settings";
import { InlineJob } from "@/components/jobs";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Checkbox, Field, Input, Textarea } from "@/components/ui/input";
import { PageHeader, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { Spinner } from "@/components/ui/spinner";
import { useToast } from "@/components/ui/toast";

export function AdminSettingsPage() {
  const acme = useACME();
  return (
    <>
      <PageHeader title="Settings" description="Host-wide settings stored by the daemon." />
      <div className="flex flex-col gap-6">
        <GameHostSection />
        <CertificatesSection acme={acme} />
        <PanelCertificateSettings acme={acme} />
        <ACMESettings query={acme} />
        <RegistrationSection />
      </div>
    </>
  );
}

function RegistrationSection() {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const settings = useSettings();

  const update = useMutation({
    mutationFn: (registration_enabled: boolean) => api.system.updateSettings({ registration_enabled }),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.settings, s);
      void queryClient.invalidateQueries({ queryKey: qk.status });
      toast.success(s.registration_enabled ? "Self-registration turned on." : "Self-registration turned off.");
    },
    onError: (err) => toast.error(err, "Could not save the setting"),
  });

  return (
    <Section
      title="Self-registration"
      description="When on, anyone who can reach this page can request a developer account at /register. New accounts cannot sign in until an administrator approves them."
    >
      {settings.isPending ? (
        <LoadingState />
      ) : settings.isError ? (
        <ErrorState error={settings.error} onRetry={() => void settings.refetch()} />
      ) : (
        <div className="flex items-center gap-3">
          <Checkbox
            label="Allow self-registration"
            checked={update.isPending ? (update.variables ?? settings.data.registration_enabled) : settings.data.registration_enabled}
            disabled={update.isPending}
            onChange={(e) => update.mutate(e.target.checked)}
          />
          {update.isPending && <Spinner label="Saving" />}
        </div>
      )}
    </Section>
  );
}

/** Mirrors the daemon's game_host rule for early feedback: a DNS name or IP address, no scheme, port or path. */
export function validateGameHost(raw: string): string | null {
  const v = raw.trim();
  if (v === "") return null;
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(v)) return "Enter only the host name or IP address, without https://.";
  if (v.includes("/")) return "Enter only the host name or IP address, without a path.";
  if (/\s/.test(v)) return "The host name cannot contain spaces.";
  if (/^\[.*\]$/.test(v) || (/:\d+$/.test(v) && !v.includes("::") && (v.match(/:/g) ?? []).length === 1)) {
    return "Enter the host without a port: each server has its own port.";
  }
  return null;
}

function GameHostSection() {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const settings = useSettings();
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (game_host: string) => api.system.updateSettings({ game_host }),
    onSuccess: (s) => {
      queryClient.setQueryData(qk.settings, s);
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      void queryClient.invalidateQueries({ queryKey: qk.certificates.all });
      setDraft(null);
      toast.success(
        s.effective_game_host
          ? `Game host saved. Restart running servers and reload the panel before testing connections to ${s.effective_game_host}.`
          : "Game host cleared. Servers cannot start until a game host is configured.",
      );
    },
    onError: (err) => {
      if (isApiError(err) && err.field === "game_host") setError(sentence(err.message));
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    if (draft === null) return;
    const err = validateGameHost(draft);
    setError(err);
    if (!err) save.mutate(draft.trim());
  }

  return (
    <Section
      title="Game host"
      description="The DNS name or IP address players use to reach game servers. It is part of every server's game URL (https://<game host>:<port>/), of the origins servers accept, and the name self-signed game certificates are issued for."
    >
      {settings.isPending ? (
        <LoadingState />
      ) : settings.isError ? (
        <ErrorState error={settings.error} onRetry={() => void settings.refetch()} />
      ) : (
        <form onSubmit={submit} noValidate className="flex flex-col gap-3">
          <Field
            label="Game host"
            error={error}
            hint="Leave empty to use the first --public-host the daemon was started with. Changing it renews self-signed certificates and marks running servers as needing a restart."
          >
            {(p) => (
              <Input
                {...p}
                spellCheck={false}
                autoComplete="off"
                placeholder="play.example.com"
                className="max-w-md"
                value={draft ?? settings.data.game_host}
                onChange={(e) => {
                  setDraft(e.target.value);
                  setError(null);
                }}
              />
            )}
          </Field>
          <p className="text-sm">
            Servers use:{" "}
            {settings.data.effective_game_host ? (
              <>
                <code className="font-mono text-xs">{settings.data.effective_game_host}</code>
                {!settings.data.game_host && <span className="text-muted-foreground"> (the daemon&apos;s first --public-host)</span>}
              </>
            ) : (
              <Badge tone="danger">Nothing: servers cannot start</Badge>
            )}
          </p>
          <div className="flex gap-2">
            <Button type="submit" size="sm" loading={save.isPending} disabled={draft === null || draft.trim() === settings.data.game_host}>
              Save game host
            </Button>
            {draft !== null && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => {
                  setDraft(null);
                  setError(null);
                  save.reset();
                }}
                disabled={save.isPending}
              >
                Discard
              </Button>
            )}
          </div>
          {save.isError && !(isApiError(save.error) && save.error.field === "game_host") && <ErrorText error={save.error} />}
        </form>
      )}
    </Section>
  );
}

const MODE_LABELS: Record<CertificateMode, string> = { self_signed: "Self-signed", custom: "Custom certificate", acme: "Automatic" };

/**
 * The game certificate: the mode, what it means, the installed custom
 * certificate and an upload form. The private key is held only in the form
 * while typing; it is never pre-filled and cleared after every upload.
 */
function CertificatesSection({ acme }: { acme: ReturnType<typeof useACME> }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const cert = useGameCertificate();
  const [confirmSelfSigned, setConfirmSelfSigned] = useState(false);
  const [job, setJob] = useState<Job | null>(null);
  const automatic = useMutation({
    mutationFn: () => api.certificates.updateGame({ mode: "acme" }),
    onSuccess: (res) => {
      queryClient.setQueryData(qk.certificates.game, res.certificate);
      setJob(res.job);
      if (!res.job) {
        void queryClient.invalidateQueries({ queryKey: qk.certificates.acme });
        void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      }
    },
  });
  // In custom mode with no valid certificate, the current daemon answers GET with 409 conflict.
  const missingCustom = isApiError(cert.error) && cert.error.code === "conflict";
  const mode: CertificateMode | null = cert.data?.mode ?? (missingCustom ? "custom" : null);

  return (
    <Section
      title="Game certificates"
      description="The TLS certificate game servers serve for WebTransport and HTTPS on their ports. The panel's own TLS is separate."
    >
      {cert.isPending ? (
        <LoadingState label="Loading the certificate" />
      ) : cert.isError && !missingCustom ? (
        <ErrorState error={cert.error} title="Could not load the game certificate" onRetry={() => void cert.refetch()} />
      ) : (
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap items-center gap-2 text-sm">
            <span>Mode:</span>
            <Badge tone={mode === "self_signed" ? "warning" : "success"}>{mode ? MODE_LABELS[mode] : "-"}</Badge>
            {cert.data?.game_host && (
              <span className="text-muted-foreground">
                for <code className="font-mono text-xs">{cert.data.game_host}</code>
              </span>
            )}
          </div>
          {mode === "self_signed" ? (
            <SelfSignedExplanation data={cert.data} />
          ) : mode === "custom" ? (
            <CustomCertificate data={cert.data} missingMessage={missingCustom && isApiError(cert.error) ? cert.error.message : null} />
          ) : (
            <>
              <p className="text-sm">The daemon obtains and renews a shared certificate for public play. Players need a browser that trusts its certificate authority.</p>
              {cert.data?.certificate ? <CertificateDetails cert={cert.data.certificate} /> : (
                <Notice tone="danger" title="No automatic certificate is installed">Check the automatic certificate status below.</Notice>
              )}
            </>
          )}
          {mode !== "self_signed" && (
            <div>
              <Button size="sm" variant="outline" onClick={() => setConfirmSelfSigned(true)}>
                Switch to self-signed certificates
              </Button>
            </div>
          )}
          {mode !== "acme" && (
            <AutomaticCertificateCheck acme={acme} hosts={cert.data?.game_host ? [cert.data.game_host] : []} hostLabel="Game host">
              {(ready) => (
                <>
                  <p className="text-sm">For public play, the daemon can obtain and renew certificates trusted by players&apos; browsers.</p>
                  {mode === "self_signed" && (
                    <p className="text-xs text-muted-foreground">Restart running servers after issuance to apply the new pinning setting.</p>
                  )}
                  <Button size="sm" variant="outline" disabled={!ready} loading={automatic.isPending} onClick={() => automatic.mutate()}>
                    Switch game to automatic certificates
                  </Button>
                </>
              )}
            </AutomaticCertificateCheck>
          )}
          {automatic.isError && <ErrorText error={automatic.error} />}
          <UploadCertificate switching={mode !== "custom"} restartNeeded={mode === "self_signed"} />
        </div>
      )}
      {job && <InlineJob key={job.id} job={job} onDismiss={() => setJob(null)} />}
      <ConfirmDialog
        open={confirmSelfSigned}
        onOpenChange={setConfirmSelfSigned}
        title="Switch to self-signed certificates"
        description={
          <>
            Each server gets its own short-lived self-signed certificate. Running servers show &quot;Restart required&quot; because the
            pinning setting changes on restart. Restarting disconnects players.
          </>
        }
        confirmLabel="Switch to self-signed"
        onConfirm={async () => {
          const res = await api.certificates.updateGame({ mode: "self_signed" });
          queryClient.setQueryData(qk.certificates.game, res.certificate);
          void queryClient.invalidateQueries({ queryKey: qk.certificates.acme });
          void queryClient.invalidateQueries({ queryKey: qk.servers.all });
          toast.success("Switched to self-signed certificates. Restart running servers to apply it.");
        }}
      />
    </Section>
  );
}

function SelfSignedExplanation({ data }: { data: GameCertificate | undefined }) {
  return (
    <div className="flex flex-col gap-3 text-sm">
      <p>Each server has its own self-signed certificate for development, LAN play and clients that pin its certificate hash. Browsers opening a game URL directly from the internet show a certificate warning.</p>
      {/* The decided contract leaves this null in self-signed mode; older daemons still return one shared certificate. */}
      {data?.certificate && (
        <details className="rounded-md border border-border px-3 py-2">
          <summary className="cursor-pointer text-xs font-medium">Shared certificate reported by the daemon</summary>
          <div className="mt-3">
            <CertificateDetails cert={data.certificate} />
          </div>
        </details>
      )}
    </div>
  );
}

function CustomCertificate({ data, missingMessage }: { data: GameCertificate | undefined; missingMessage: string | null }) {
  const c = data?.certificate ?? null;
  if (!c) {
    return (
      <Notice tone="danger" title="No valid certificate is installed">
        {missingMessage ? `${sentence(missingMessage)} ` : ""}Servers cannot start until a certificate is uploaded below, or until you
        switch back to self-signed certificates.
      </Notice>
    );
  }
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm">Use a custom certificate if you manage your own TLS certificate. Players need a device that trusts the certificate you upload.</p>
      <CertificateDetails cert={c} />
      {!c.covers_game_host && (
        <Notice tone="warning" title="The certificate does not cover the game host">
          Clients connecting to {data?.game_host ? <code className="font-mono text-xs">{data.game_host}</code> : "the game host"} will
          reject it. Upload a certificate for that name, or change the game host.
        </Notice>
      )}
    </div>
  );
}

type UploadErrors = Partial<Record<"certificate_pem" | "private_key_pem", string | null>>;

function UploadCertificate({ switching, restartNeeded }: { switching: boolean; restartNeeded: boolean }) {
  const api = useApi();
  const toast = useToast();
  const queryClient = useQueryClient();
  const [open, setOpen] = useState(false);
  const [chain, setChain] = useState("");
  // Held only while typing; never pre-filled, cleared after every attempt that succeeded and when the form closes.
  const [key, setKey] = useState("");
  const [errors, setErrors] = useState<UploadErrors>({});
  const [confirm, setConfirm] = useState(false);
  const request = useRef<UpdateGameCertificateRequest | null>(null);

  const upload = useMutation({
    networkMode: "always",
    mutationFn: () => {
      const body = request.current;
      request.current = null;
      if (!body) throw new Error("Missing certificate upload");
      return api.certificates.updateGame(body);
    },
    onSuccess: (res) => {
      queryClient.setQueryData(qk.certificates.game, res.certificate);
      void queryClient.invalidateQueries({ queryKey: qk.certificates.acme });
      void queryClient.invalidateQueries({ queryKey: qk.servers.all });
      setChain("");
      setKey("");
      setOpen(false);
      toast.success(
        restartNeeded
          ? "Certificate installed and custom mode turned on. Restart running servers to apply it."
          : "Certificate replaced. Running servers pick it up within about 30 seconds.",
      );
    },
    onError: (err) => {
      if (isApiError(err) && (err.field === "certificate_pem" || err.field === "private_key_pem")) {
        setErrors({ [err.field]: sentence(err.message) });
      }
    },
  });

  function close() {
    setOpen(false);
    setKey("");
    setChain("");
    setErrors({});
    upload.reset();
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    const errs: UploadErrors = {
      certificate_pem: chain.includes("-----BEGIN CERTIFICATE-----") ? null : "Paste the PEM certificate chain, leaf first (-----BEGIN CERTIFICATE-----).",
      private_key_pem: /-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(key) ? null : "Paste the unencrypted PEM private key (-----BEGIN PRIVATE KEY-----).",
    };
    setErrors(errs);
    if (errs.certificate_pem || errs.private_key_pem) return;
    if (switching) setConfirm(true);
    else send();
  }

  function send() {
    // Mutation variables remain in the query cache, so keep PEM out of them.
    request.current = { mode: "custom", certificate_pem: chain, private_key_pem: key };
    upload.mutate();
  }

  if (!open) {
    return (
      <div>
        <Button size="sm" variant={switching ? "outline" : "primary"} onClick={() => setOpen(true)}>
          <Upload />
          {switching ? "Upload a trusted certificate" : "Upload a new certificate"}
        </Button>
        {switching && <p className="mt-1 text-xs text-muted-foreground">Use a custom certificate if you manage your own TLS certificate.</p>}
      </div>
    );
  }

  const inline = isApiError(upload.error) && (upload.error.field === "certificate_pem" || upload.error.field === "private_key_pem");
  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-3 rounded-md border border-border p-4" aria-label="Upload a certificate">
      <p className="flex items-center gap-2 text-sm font-medium">
        <ShieldCheck aria-hidden className="size-4" />
        {switching ? "Install a custom certificate" : "Replace the custom certificate"}
      </p>
      <p className="text-xs text-muted-foreground">
        A chain of PEM certificates (leaf first, at most 8) and its unencrypted private key (PKCS#8, PKCS#1 or SEC 1; RSA of at
        least 2048 bits or ECDSA). The leaf must be valid now and usable for TLS servers. Renew it yourself before it expires: the
        daemon never changes a custom certificate. The private key is never shown again after the upload.
      </p>
      <Field label="Certificate chain (PEM)" error={errors.certificate_pem}>
        {(p) => (
          <Textarea
            {...p}
            spellCheck={false}
            autoComplete="off"
            className="min-h-32 font-mono text-xs"
            placeholder="-----BEGIN CERTIFICATE-----"
            value={chain}
            onChange={(e) => {
              setChain(e.target.value);
              setErrors((x) => ({ ...x, certificate_pem: null }));
            }}
          />
        )}
      </Field>
      <Field label="Private key (PEM)" error={errors.private_key_pem}>
        {(p) => (
          <Textarea
            {...p}
            spellCheck={false}
            autoComplete="off"
            data-1p-ignore
            data-lpignore="true"
            className="min-h-32 font-mono text-xs"
            placeholder="-----BEGIN PRIVATE KEY-----"
            value={key}
            onChange={(e) => {
              setKey(e.target.value);
              setErrors((x) => ({ ...x, private_key_pem: null }));
            }}
          />
        )}
      </Field>
      {restartNeeded && (
        <p className="text-xs text-muted-foreground">
          Installing it switches the game certificate to custom mode: running servers need a restart to use it.
        </p>
      )}
      {upload.isError && !inline && <ErrorText error={upload.error} />}
      <div className="flex gap-2">
        <Button type="submit" size="sm" loading={upload.isPending}>
          Install certificate
        </Button>
        <Button size="sm" variant="ghost" onClick={close} disabled={upload.isPending}>
          Cancel
        </Button>
      </div>
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Switch to the custom certificate"
        description={
          <>
            Every server will serve this certificate, and clients will validate it normally.{" "}
            {restartNeeded ? (
              <>Running servers show &quot;Restart required&quot;, and a restart disconnects their players.</>
            ) : "Running servers pick it up within a minute."}
          </>
        }
        confirmLabel="Install and switch"
        onConfirm={() => send()}
      />
    </form>
  );
}
