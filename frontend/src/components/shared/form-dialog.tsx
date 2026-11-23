import * as Dialog from "@radix-ui/react-dialog";
import { X } from "lucide-react";

interface FormDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  children: React.ReactNode;
  wide?: boolean;
}

export function FormDialog({
  open,
  onOpenChange,
  title,
  children,
  wide = false,
}: FormDialogProps) {
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-50 bg-black/50" />
        <Dialog.Content
          className={`fixed left-1/2 top-1/2 z-50 w-full -translate-x-1/2 -translate-y-1/2 rounded-lg border border-border bg-card p-6 shadow-lg ${wide ? "max-w-2xl" : "max-w-md"} max-h-[85vh] overflow-y-auto`}
        >
          <div className="flex items-center justify-between">
            <Dialog.Title className="text-lg font-semibold text-foreground">
              {title}
            </Dialog.Title>
            <Dialog.Close className="rounded-sm p-1 text-muted-foreground hover:text-foreground">
              <X className="h-4 w-4" />
            </Dialog.Close>
          </div>
          <div className="mt-4">{children}</div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
