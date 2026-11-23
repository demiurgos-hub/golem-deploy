import { ExternalLink } from "lucide-react";
import { safeHttpUrl } from "@/lib/url";
import { cn } from "@/lib/utils";

/** Repository URL as a link when it is http(s); ssh URLs are shown as text. */
export function RepoLink({ url, className }: { url: string; className?: string }) {
  const href = safeHttpUrl(url);
  const text = <span className={cn("block font-mono text-xs", className)} title={url}>{url}</span>;
  if (!href) return text;
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" className="inline-flex max-w-full items-center gap-1 hover:underline">
      {text}
      <ExternalLink aria-hidden className="size-3 shrink-0 text-muted-foreground" />
      <span className="sr-only">(opens in a new tab)</span>
    </a>
  );
}
