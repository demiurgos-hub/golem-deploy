import type { ReactNode } from "react";
import { AlertTriangle, Inbox, RotateCw } from "lucide-react";
import { describeError, isApiError } from "@/api/errors";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { cn } from "@/lib/utils";

export function LoadingState({ label = "Loading", className }: { label?: string; className?: string }) {
  return (
    <div role="status" className={cn("flex items-center justify-center gap-2 py-10 text-sm text-muted-foreground", className)}>
      <Spinner />
      <span>{label}</span>
    </div>
  );
}

export function EmptyState({
  title,
  description,
  action,
  className,
}: {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("flex flex-col items-center gap-2 rounded-md border border-dashed border-border px-6 py-10 text-center", className)}>
      <Inbox aria-hidden className="size-6 text-muted-foreground" />
      <p className="text-sm font-medium">{title}</p>
      {description && <p className="max-w-md text-sm text-muted-foreground">{description}</p>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  );
}

/** Shows an API error's message and request id, with an optional retry. */
export function ErrorState({
  error,
  title = "Could not load this",
  onRetry,
  className,
}: {
  error: unknown;
  title?: string;
  onRetry?: () => void;
  className?: string;
}) {
  const requestId = isApiError(error) ? error.requestId : "";
  return (
    <div role="alert" className={cn("flex flex-col items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-4 py-4", className)}>
      <div className="flex items-center gap-2 text-sm font-medium text-destructive">
        <AlertTriangle aria-hidden className="size-4" />
        {title}
      </div>
      <p className="text-sm text-foreground">{describeError(error)}</p>
      {requestId && <p className="font-mono text-xs text-muted-foreground">Request id: {requestId}</p>}
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RotateCw />
          Retry
        </Button>
      )}
    </div>
  );
}

/** Compact inline error, e.g. inside forms and dialogs. */
export function ErrorText({ error, children }: { error: unknown; children?: ReactNode }) {
  const requestId = isApiError(error) ? error.requestId : "";
  return (
    <div role="alert" className="rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm">
      <p className="whitespace-pre-wrap break-words text-foreground">{children ?? describeError(error)}</p>
      {requestId && <p className="mt-0.5 font-mono text-xs text-muted-foreground">Request id: {requestId}</p>}
    </div>
  );
}

export function Notice({
  tone = "info",
  title,
  children,
  action,
}: {
  tone?: "info" | "warning" | "danger";
  title?: string;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div
      role={tone === "info" ? "status" : "alert"}
      className={cn(
        "flex flex-wrap items-start justify-between gap-3 rounded-md border px-4 py-3 text-sm",
        tone === "info" && "border-sky-600/30 bg-sky-600/5",
        tone === "warning" && "border-amber-600/40 bg-amber-500/10",
        tone === "danger" && "border-destructive/40 bg-destructive/10",
      )}
    >
      <div className="min-w-0">
        {title && <p className="font-medium">{title}</p>}
        {children && <div className={cn(title && "mt-0.5", "text-muted-foreground")}>{children}</div>}
      </div>
      {action}
    </div>
  );
}
