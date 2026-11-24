import { useState, type FormEvent, type ReactNode } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { isApiError, sentence } from "@/api/errors";
import { qk } from "@/api/query-keys";
import { usePanelCertificate, type useACME } from "@/api/queries";
import type { ACME, Job, PanelCertificate, PanelCertificateMode, UpdateACMERequest } from "@/api/types.gen";
import { useApi } from "@/app/services";
import { CertificateDetails, CertificateExpiry } from "@/components/certificate";
import { InlineJob } from "@/components/jobs";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DataTable } from "@/components/ui/data-table";
import { Checkbox, Field, Input } from "@/components/ui/input";
import { DetailList, Section } from "@/components/ui/page";
import { ErrorState, ErrorText, LoadingState, Notice } from "@/components/ui/states";
import { formatDateTime } from "@/lib/format";

const letsEncryptDirectories = [
  "https://acme-v02.api.letsencrypt.org/directory",
  "https://acme-staging-v02.api.letsencrypt.org/directory",
];

const noChallenge = "Make the daemon reachable on port 80 through --http-listen, or have the panel serve HTTPS on port 443.";

export function AutomaticCertificateCheck({
  acme, hosts, hostLabel, children,
}: {
  acme: ReturnType<typeof useACME>;
  hosts: string[];
  hostLabel: string;
  children: (ready: boolean) => ReactNode;
}) {
  const challenge = acme.data?.http01_available || acme.data?.tls_alpn01_available;
  const ready = !acme.isError && Boolean(hosts.length && acme.data?.terms_agreed && challenge);
  return (
    <div className="flex flex-col items-start gap-2">
      {acme.isPending ? <LoadingState label="Checking automatic certificate requirements" /> :
        acme.isError ? <ErrorState error={acme.error} title="Could not check automatic certificate requirements" onRetry={() => void acme.refetch()} /> : (
          <ul className="list-disc space-y-1 pl-5 text-sm text-muted-foreground">
            <li>{hostLabel} {hosts.length ? <code className="break-all font-mono text-xs">{hosts.join(", ")}</code> : "missing. Set it before requesting a certificate."}</li>
            <li>{acme.data.terms_agreed ? "Subscriber agreement accepted." : "Accept and save the subscriber agreement under Automatic certificates."}</li>
            <li>{challenge ? "At least one challenge method is available." : noChallenge}</li>
            {acme.data.staging && letsEncryptDirectories.includes(acme.data.directory_url) && <li>Staging is on. Browsers will warn about the test certificate.</li>}
          </ul>
        )}
      {children(ready)}
    </div>
  );
}

const panelModes: Record<PanelCertificateMode, string> = {
  none: "Plain HTTP",
  files: "Operator-supplied files",
  self_signed: "Self-signed",
  acme: "Automatic",
};

export function PanelCertificateSettings({ acme }: { acme: ReturnType<typeof useACME> }) {
  const panel = usePanelCertificate();
  return (
    <Section title="Panel certificate" description="The TLS certificate used to open this control panel.">
      {panel.isPending && <LoadingState label="Loading the panel certificate" />}
      {panel.isError && <ErrorState error={panel.error} title="Could not load the panel certificate" onRetry={() => void panel.refetch()} />}
      {panel.data && <PanelCertificateControls panel={panel.data} readFailed={panel.isError} acme={acme} />}
    </Section>
  );
}

function PanelCertificateControls({
  panel, readFailed, acme,
}: {
  panel: PanelCertificate;
  readFailed: boolean;
  acme: ReturnType<typeof useACME>;
}) {
  const api = useApi();
  const queryClient = useQueryClient();
  const [initialFingerprint] = useState(panel.certificate?.fingerprint_sha256);
  const [initialMode] = useState(panel.mode);
  const [job, setJob] = useState<Job | null>(null);
  const update = useMutation({
    mutationFn: (mode: "self_signed" | "acme") => api.certificates.updatePanel({ mode }),
    onSuccess: (res) => {
      queryClient.setQueryData(qk.certificates.panel, res.certificate);
      setJob(res.job);
      if (!res.job) void queryClient.invalidateQueries({ queryKey: qk.certificates.acme });
    },
  });
  const changed = panel.mode !== initialMode || panel.certificate?.fingerprint_sha256 !== initialFingerprint;
  return (
    <div className="flex flex-col gap-4">
      <DetailList items={[
        { term: "Mode", value: <Badge tone={panel.mode === "self_signed" ? "warning" : "neutral"}>{panelModes[panel.mode]}</Badge> },
        { term: "Hosts", value: <code className="break-all font-mono text-xs">{panel.hosts.join(", ") || "None configured"}</code> },
        { term: "Host coverage", value: panel.certificate ? (
          <Badge tone={panel.covers_hosts ? "success" : "warning"}>
            {panel.covers_hosts ? "Covers every panel host" : "Does not cover every panel host"}
          </Badge>
        ) : "No certificate in use" },
      ]} />
      {panel.certificate && <CertificateDetails cert={panel.certificate} showGameHost={false} />}
      {changed && (
        <Notice tone="info" title="Reload the panel">
          The panel certificate changed after this page loaded. Reload to connect with the new certificate.
        </Notice>
      )}
      {!panel.configurable ? (
        <p className="text-sm text-muted-foreground">
          {panel.mode === "files"
            ? "The operator supplies this certificate with --tls-cert and --tls-key. Change those files on the host."
            : "The daemon serves plain HTTP on loopback or behind a reverse proxy. If a proxy provides TLS, manage its certificate there."}
        </p>
      ) : panel.mode === "acme" ? (
        <div className="flex flex-col items-start gap-2">
          <p className="text-sm">A self-signed panel certificate is suitable for local administration. Browsers show a certificate warning.</p>
          <Button size="sm" variant="outline" disabled={readFailed} loading={update.isPending} onClick={() => update.mutate("self_signed")}>
            Switch panel to self-signed
          </Button>
        </div>
      ) : (
        <>
          <p className="text-sm">Browsers show a warning for the panel&apos;s self-signed certificate. With automatic certificates the daemon handles issuance and renewal.</p>
          <AutomaticCertificateCheck acme={acme} hosts={panel.hosts} hostLabel="Panel hosts">
            {(ready) => (
              <Button size="sm" variant="outline" disabled={!ready || readFailed} loading={update.isPending} onClick={() => update.mutate("acme")}>
                Switch panel to automatic certificates
              </Button>
            )}
          </AutomaticCertificateCheck>
        </>
      )}
      {update.isError && <ErrorText error={update.error} />}
      {job && <InlineJob key={job.id} job={job} onDismiss={() => setJob(null)} />}
    </div>
  );
}

export function ACMESettings({ query }: { query: ReturnType<typeof useACME> }) {
  return (
    <Section title="Automatic certificates" description="Configure the ACME account and see which certificates the daemon manages.">
      {query.isPending && <LoadingState label="Loading automatic certificate settings" />}
      {query.isError && <ErrorState error={query.error} title="Could not load automatic certificate settings" onRetry={() => void query.refetch()} />}
      {query.data && <ACMEControls acme={query.data} readFailed={query.isError} />}
    </Section>
  );
}

function ACMEControls({ acme, readFailed }: { acme: ACME; readFailed: boolean }) {
  const api = useApi();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState<UpdateACMERequest>({});
  const [job, setJob] = useState<Job | null>(null);
  const save = useMutation({
    mutationFn: () => api.certificates.updateACME(draft),
    onSuccess: (updated) => {
      queryClient.setQueryData(qk.certificates.acme, updated);
      setDraft({});
    },
  });
  const renew = useMutation({ mutationFn: api.certificates.renew, onSuccess: setJob });
  const customDirectory = !letsEncryptDirectories.includes(acme.directory_url);
  const stagingLocked = acme.certificates.length > 0 || customDirectory;
  const emailError = isApiError(save.error) && save.error.field === "email" ? sentence(save.error.message) : null;

  function submit(event: FormEvent) {
    event.preventDefault();
    save.mutate();
  }

  return (
    <div className="flex flex-col gap-4">
      <form onSubmit={submit} className="flex flex-col items-start gap-3">
        <Field label="Contact email" hint="Optional. The daemon shares it with the certificate authority." error={emailError}>
          {(p) => <Input {...p} type="email" autoComplete="email" maxLength={254} disabled={save.isPending} value={draft.email ?? acme.email}
            onChange={(event) => { save.reset(); setDraft({ ...draft, email: event.target.value }); }} />}
        </Field>
        <div className="space-y-1">
          <Checkbox role="switch" label="Use staging" checked={draft.staging ?? acme.staging} disabled={stagingLocked || save.isPending}
            aria-describedby="acme-staging-hint"
            onChange={(event) => setDraft({ ...draft, staging: event.target.checked })} />
          <p id="acme-staging-hint" className="text-xs text-muted-foreground">
            Staging issues test certificates that browsers do not trust.{" "}
            {customDirectory
              ? "The configured directory controls the environment."
              : stagingLocked && "Switch the game and panel away from automatic certificates to change this."}
          </p>
        </div>
        <Checkbox
          label={<>I agree to the <a className="underline" href="https://letsencrypt.org/repository/" target="_blank" rel="noreferrer">Let&apos;s Encrypt subscriber agreement</a></>}
          checked={draft.terms_agreed ?? acme.terms_agreed} disabled={save.isPending}
          onChange={(event) => setDraft({ ...draft, terms_agreed: event.target.checked })}
        />
        {save.isError && !emailError && <ErrorText error={save.error} />}
        <Button type="submit" size="sm" disabled={readFailed || Object.keys(draft).length === 0} loading={save.isPending}>
          Save automatic certificate settings
        </Button>
      </form>
      <DetailList items={[
        { term: "HTTP-01 (port 80)", value: acme.http01_available ? "Available" : "Unavailable" },
        { term: "TLS-ALPN-01 (port 443)", value: acme.tls_alpn01_available ? "Available" : "Unavailable" },
        ...(customDirectory ? [{ term: "ACME directory", value: <code className="break-all font-mono text-xs">{acme.directory_url}</code> }] : []),
      ]} />
      {!acme.http01_available && !acme.tls_alpn01_available && (
        <Notice tone="warning" title="No ACME challenge methods are available">{noChallenge}</Notice>
      )}
      {acme.certificates.length === 0 ? <p className="text-sm text-muted-foreground">No managed certificates yet.</p> : (
        <>
          <DataTable
            label="Managed certificates"
            data={acme.certificates}
            rowKey={(cert) => cert.identifier}
            columns={[
              { key: "identifier", header: "Identifier", cell: (cert) => <code className="break-all font-mono text-xs">{cert.identifier}</code> },
              { key: "used-by", header: "Used by", cell: (cert) => cert.used_by.map((use) => use === "game" ? "Game servers" : "Panel").join(", ") },
              { key: "status", header: "Status", cell: (cert) => (
                <Badge tone={cert.status === "issued" ? "success" : cert.status === "error" ? "danger" : "warning"}>
                  {{ issued: "Issued", error: "Failed", pending: "Pending" }[cert.status]}
                </Badge>
              ) },
              { key: "expiry", header: "Expires", cell: (cert) => cert.certificate ? <CertificateExpiry at={cert.certificate.not_after} /> : "—" },
              { key: "error", header: "Last error", cell: (cert) => cert.last_error ? <span className="break-words text-destructive">{cert.last_error}</span> : "—" },
              { key: "attempt", header: "Last attempt", cell: (cert) => cert.last_attempt_at ? formatDateTime(cert.last_attempt_at) : "Never" },
            ]}
          />
          <div><Button size="sm" variant="outline" disabled={readFailed} loading={renew.isPending} onClick={() => renew.mutate()}>Renew now</Button></div>
        </>
      )}
      {renew.isError && <ErrorText error={renew.error} />}
      {job && <InlineJob key={job.id} job={job} onDismiss={() => setJob(null)} />}
    </div>
  );
}
