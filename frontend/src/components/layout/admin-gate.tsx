import { Link, Outlet } from "@tanstack/react-router";
import { ShieldAlert } from "lucide-react";
import { useAccount } from "@/api/queries";
import { ErrorState, LoadingState } from "@/components/ui/states";

/**
 * Admin pages render only for admin accounts. This is presentation only;
 * the daemon rejects non-admin requests with 403 regardless.
 */
export function AdminGate() {
  const account = useAccount();
  if (account.isPending) return <LoadingState />;
  if (account.isError) return <ErrorState error={account.error} onRetry={() => void account.refetch()} />;
  if (account.data.role !== "admin") return <Forbidden />;
  return <Outlet />;
}

export function Forbidden() {
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-3 py-16 text-center">
      <ShieldAlert aria-hidden className="size-8 text-muted-foreground" />
      <h1 className="text-lg font-semibold">Administrator access required</h1>
      <p className="text-sm text-muted-foreground">
        This page is only available to administrator accounts. Ask an administrator if you need access.
      </p>
      <Link to="/projects" className="text-sm font-medium underline underline-offset-4">
        Go to projects
      </Link>
    </div>
  );
}
