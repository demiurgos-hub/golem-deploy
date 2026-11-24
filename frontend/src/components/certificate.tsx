import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { CopyButton } from "@/components/ui/copy-button";
import { DetailList } from "@/components/ui/page";
import type { CertificateInfo } from "@/api/types.gen";
import { formatDateTime } from "@/lib/format";

/** Days until a certificate expires (negative once expired). */
export function daysLeft(notAfter: string, now = Date.now()): number {
  return Math.floor((new Date(notAfter).getTime() - now) / 86_400_000);
}

/** One certificate's facts, shared by the server page and the settings page. */
export function CertificateDetails({ cert }: { cert: CertificateInfo }) {
  // When the page opened is precise enough for days of validity.
  const [now] = useState(() => Date.now());
  const left = daysLeft(cert.not_after, now);
  const expired = new Date(cert.not_after).getTime() <= now;
  return (
    <DetailList
      items={[
        {
          term: "Type",
          value: cert.self_signed ? (
            <Badge tone="warning">Self-signed</Badge>
          ) : (
            <span className="flex flex-col gap-0.5">
              <Badge tone="success" className="w-fit">
                Issued by a CA
              </Badge>
              <span className="break-words text-xs text-muted-foreground">{cert.issuer}</span>
            </span>
          ),
        },
        { term: "Subject", value: <span className="break-words font-mono text-xs">{cert.subject || "-"}</span> },
        {
          term: "Names",
          value: (
            <span className="break-words font-mono text-xs">{[...cert.dns_names, ...cert.ip_addresses].join(", ") || "-"}</span>
          ),
        },
        {
          term: "Valid until",
          value: (
            <span className="inline-flex flex-wrap items-center gap-2">
              <span title={formatDateTime(cert.not_after)}>{formatDateTime(cert.not_after)}</span>
              {expired ? (
                <Badge tone="danger">Expired</Badge>
              ) : left < 5 ? (
                <Badge tone="warning">{left < 1 ? "Expires within a day" : `${left} days left`}</Badge>
              ) : (
                <span className="text-xs text-muted-foreground">{left} days left</span>
              )}
            </span>
          ),
        },
        {
          term: "Game host",
          value: cert.covers_game_host ? (
            <span>Covered</span>
          ) : (
            <Badge tone="danger" title="Clients connecting by the game host name will reject this certificate.">
              Not covered
            </Badge>
          ),
        },
        {
          term: "SHA-256",
          value: (
            <span className="inline-flex max-w-full items-start gap-1">
              <code className="break-all font-mono text-xs">{cert.fingerprint_sha256}</code>
              <CopyButton value={cert.fingerprint_sha256} size="icon" variant="ghost" label="Copy certificate fingerprint" />
            </span>
          ),
        },
        ...(cert.chain_length > 1 ? [{ term: "Chain", value: `${cert.chain_length} certificates` }] : []),
      ]}
    />
  );
}

