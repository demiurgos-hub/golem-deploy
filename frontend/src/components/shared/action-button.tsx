import { cn } from "@/lib/utils";

interface ActionButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: "default" | "destructive" | "ghost";
  size?: "sm" | "default";
}

export function ActionButton({
  variant = "default",
  size = "sm",
  className,
  ...props
}: ActionButtonProps) {
  return (
    <button
      className={cn(
        "inline-flex items-center justify-center rounded-md text-sm font-medium transition-colors disabled:opacity-50",
        size === "sm" ? "px-2 py-1 text-xs" : "px-3 py-1.5",
        variant === "destructive" &&
          "bg-destructive text-white hover:bg-destructive/90",
        variant === "default" &&
          "bg-secondary text-secondary-foreground hover:bg-secondary/80",
        variant === "ghost" &&
          "text-muted-foreground hover:bg-accent hover:text-foreground",
        className,
      )}
      {...props}
    />
  );
}
