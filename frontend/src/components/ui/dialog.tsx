import * as RadixDialog from "@radix-ui/react-dialog";
import * as AlertDialog from "@radix-ui/react-alert-dialog";
import { useState, type ReactNode } from "react";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { describeError } from "@/api/errors";
import { ErrorText } from "@/components/ui/states";

const overlay = "fixed inset-0 z-50 bg-black/50";
const content =
  "fixed left-1/2 top-1/2 z-50 flex max-h-[90vh] w-[calc(100%-2rem)] -translate-x-1/2 -translate-y-1/2 flex-col " +
  "rounded-lg border border-border bg-card text-card-foreground shadow-lg focus:outline-none";

interface DialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  size?: "md" | "lg" | "xl";
  /** When false there is no close button and Escape or outside clicks do nothing. */
  dismissible?: boolean;
}

/** A modal dialog; Radix traps focus and restores it on close. */
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  size = "md",
  dismissible = true,
}: DialogProps) {
  return (
    <RadixDialog.Root open={open} onOpenChange={(o) => (dismissible || o) && onOpenChange(o)}>
      <RadixDialog.Portal>
        <RadixDialog.Overlay className={overlay} />
        <RadixDialog.Content
          className={cn(content, size === "md" && "max-w-md", size === "lg" && "max-w-2xl", size === "xl" && "max-w-4xl")}
          // Without a description, tell Radix there is intentionally none.
          {...(description ? {} : { "aria-describedby": undefined })}
          onEscapeKeyDown={dismissible ? undefined : (e) => e.preventDefault()}
          onInteractOutside={dismissible ? undefined : (e) => e.preventDefault()}
        >
          <div className="flex items-start justify-between gap-4 border-b border-border px-5 py-4">
            <div className="min-w-0">
              <RadixDialog.Title className="text-base font-semibold">{title}</RadixDialog.Title>
              {description && (
                <RadixDialog.Description className="mt-1 text-sm text-muted-foreground">
                  {description}
                </RadixDialog.Description>
              )}
            </div>
            {dismissible && (
              <RadixDialog.Close asChild>
                <Button variant="ghost" size="icon" aria-label="Close">
                  <X />
                </Button>
              </RadixDialog.Close>
            )}
          </div>
          <div className="min-h-0 overflow-y-auto px-5 py-4">{children}</div>
          {footer && <div className="flex justify-end gap-2 border-t border-border px-5 py-3">{footer}</div>}
        </RadixDialog.Content>
      </RadixDialog.Portal>
    </RadixDialog.Root>
  );
}

interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: ReactNode;
  confirmLabel: string;
  destructive?: boolean;
  /** May return a promise; the dialog stays open and shows the error if it rejects. */
  onConfirm: () => unknown;
  /** Extra content under the description, such as a secondary option. */
  children?: ReactNode;
  /** Explains what to do about an error onConfirm rejected with, shown under it. */
  errorHint?: (error: unknown) => ReactNode;
}

/** Confirmation for consequential actions. Name the thing being changed in the description. */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  destructive = false,
  onConfirm,
  children,
  errorHint,
}: ConfirmDialogProps) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>(null);

  function change(next: boolean) {
    if (pending) return;
    if (!next) setError(null);
    onOpenChange(next);
  }

  async function confirm() {
    setPending(true);
    setError(null);
    try {
      await onConfirm();
      setPending(false);
      onOpenChange(false);
    } catch (err) {
      setPending(false);
      setError(err);
    }
  }

  return (
    <AlertDialog.Root open={open} onOpenChange={change}>
      <AlertDialog.Portal>
        <AlertDialog.Overlay className={overlay} />
        <AlertDialog.Content className={cn(content, "max-w-md")}>
          <div className="px-5 pt-5">
            <AlertDialog.Title className="text-base font-semibold">{title}</AlertDialog.Title>
            <AlertDialog.Description asChild>
              <div className="mt-2 text-sm text-muted-foreground">{description}</div>
            </AlertDialog.Description>
            {children && <div className="mt-3">{children}</div>}
            {error !== null && (
              <div className="mt-3 flex flex-col gap-1">
                <ErrorText error={error}>{describeError(error)}</ErrorText>
                {errorHint?.(error)}
              </div>
            )}
          </div>
          <div className="mt-4 flex justify-end gap-2 border-t border-border px-5 py-3">
            <AlertDialog.Cancel asChild>
              <Button variant="outline" disabled={pending}>
                Cancel
              </Button>
            </AlertDialog.Cancel>
            <Button
              variant={destructive ? "destructive" : "primary"}
              loading={pending}
              onClick={() => void confirm()}
            >
              {confirmLabel}
            </Button>
          </div>
        </AlertDialog.Content>
      </AlertDialog.Portal>
    </AlertDialog.Root>
  );
}
