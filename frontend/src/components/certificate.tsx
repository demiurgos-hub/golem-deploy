import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { CopyButton } from "@/components/ui/copy-button";
import { DetailList } from "@/components/ui/page";
import type { CertificateInfo } from "@/api/types.gen";
import { formatDateTime, formatRelative } from "@/lib/format";

/** Days until a certificate expires (negative once expired). */
export function daysLeft(notAfter: string, now = Date.now()): number {
  return Math.floor((new Date(notAfter).getTime() - now) / 86_400_000);
}

export function CertificateExpiry({ at }: { at: string }) {
  const [now] = useState(Date.now);
  const expired = new Date(at).getTime() <= now;
  const near = daysLeft(at, now) < 5;
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <time dateTime={at}>{formatDateTime(at)}</time>
      {expired || near ? (
        <Badge tone={expired ? "danger" : "warning"}>{expired ? "Expired" : "Expiring soon"} ({formatRelative(at, now)})</Badge>
      ) : (
        <span className="text-xs text-muted-foreground">{formatRelative(at, now)}</span>
      )}
    </span>
  );
}

/** One certificate's facts, shared by the server page and the settings page. */
export function CertificateDetails({ cert, showGameHost = true }: { cert: CertificateInfo; showGameHost?: boolean }) {
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
        { term: "Valid until", value: <CertificateExpiry at={cert.not_after} /> },
        ...(showGameHost ? [{
          term: "Game host",
          value: cert.covers_game_host ? (
            <span>Covered</span>
          ) : (
            <Badge tone="danger" title="Clients connecting by the game host name will reject this certificate.">
              Not covered
            </Badge>
          ),
        }] : []),
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

