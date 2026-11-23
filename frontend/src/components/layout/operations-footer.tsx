import { useOperationTracker } from "@/hooks/use-operation-tracker";

export function OperationsFooter() {
  const { operations } = useOperationTracker();

  if (operations.size === 0) return null;

  const labels = Array.from(operations.values());

  return (
    <div className="border-t border-border bg-muted/50 px-4 py-1.5 text-xs text-muted-foreground">
      {labels.length === 1
        ? labels[0]
        : `${labels.length} operations in progress`}
    </div>
  );
}
