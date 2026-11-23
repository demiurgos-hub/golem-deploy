import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

const tones = {
  neutral: "border-border bg-muted text-muted-foreground",
  success: "border-emerald-600/30 bg-emerald-600/10 text-emerald-700 dark:text-emerald-400",
  warning: "border-amber-600/30 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  danger: "border-red-600/30 bg-red-600/10 text-red-700 dark:text-red-400",
  info: "border-sky-600/30 bg-sky-600/10 text-sky-700 dark:text-sky-400",
} as const;

export type BadgeTone = keyof typeof tones;

export function Badge({
  tone = "neutral",
  className,
  children,
  title,
}: {
  tone?: BadgeTone;
  className?: string;
  children: ReactNode;
  title?: string;
}) {
  return (
    <span
      title={title}
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap rounded-full border px-2 py-0.5 text-xs font-medium [&_svg]:size-3",
        tones[tone],
        className,
      )}
    >
      {children}
    </span>
  );
}
