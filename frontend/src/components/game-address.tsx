import { safeHttpUrl } from "@/lib/url";

export function GameAddress({ url }: { url: string }) {
  const href = safeHttpUrl(url);
  if (!href) return null;
  return (
    <a href={href} target="_blank" rel="noreferrer" className="break-all font-mono text-xs text-muted-foreground underline-offset-4 hover:underline">
      {url}
    </a>
  );
}
