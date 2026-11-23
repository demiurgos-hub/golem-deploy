import { Link, type ErrorComponentProps } from "@tanstack/react-router";
import { Button } from "@/components/ui/button";

export function NotFoundPage() {
  return (
    <div className="mx-auto flex min-h-[60vh] max-w-md flex-col items-center justify-center gap-3 px-4 text-center">
      <p className="text-sm font-medium text-muted-foreground">404</p>
      <h1 className="text-xl font-semibold">Page not found</h1>
      <p className="text-sm text-muted-foreground">There is nothing at this address. It may have been moved or deleted.</p>
      <Link to="/projects" className="text-sm font-medium underline underline-offset-4">
        Go to projects
      </Link>
    </div>
  );
}

/** Last-resort boundary for render errors and failed route chunk loads. */
export function RouteErrorPage({ reset }: ErrorComponentProps) {
  return (
    <div className="mx-auto flex min-h-[60vh] max-w-md flex-col items-center justify-center gap-3 px-4 text-center">
      <h1 className="text-xl font-semibold">This page failed to load</h1>
      <p className="text-sm text-muted-foreground">
        The web client hit an unexpected problem. If the daemon was just upgraded, reloading picks up the new version.
      </p>
      <div className="flex gap-2">
        <Button variant="outline" onClick={reset}>
          Try again
        </Button>
        <Button onClick={() => window.location.reload()}>Reload</Button>
      </div>
    </div>
  );
}
