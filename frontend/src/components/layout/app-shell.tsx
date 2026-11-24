import { useState, type ComponentType } from "react";
import { Link, Outlet } from "@tanstack/react-router";
import {
  Boxes,
  DatabaseBackup,
  FileArchive,
  FolderGit2,
  HardDrive,
  LogOut,
  Menu,
  Moon,
  Server,
  Settings,
  Sun,
  User,
  Users,
  X,
} from "lucide-react";
import { useServices } from "@/app/services";
import { useAccount, useStatus } from "@/api/queries";
import { useTheme } from "@/hooks/use-theme";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/ui/states";
import { cn } from "@/lib/utils";

interface NavItem {
  to: "/projects" | "/servers" | "/backups" | "/images" | "/static" | "/volumes" | "/admin/accounts" | "/admin/settings";
  label: string;
  icon: ComponentType<{ className?: string }>;
}

const MAIN_NAV: NavItem[] = [
  { to: "/projects", label: "Projects", icon: FolderGit2 },
  { to: "/servers", label: "Servers", icon: Server },
  { to: "/backups", label: "Backups", icon: DatabaseBackup },
  { to: "/images", label: "Images", icon: Boxes },
  { to: "/static", label: "Static files", icon: FileArchive },
  { to: "/volumes", label: "Volumes", icon: HardDrive },
];

const ADMIN_NAV: NavItem[] = [
  { to: "/admin/accounts", label: "Accounts", icon: Users },
  { to: "/admin/settings", label: "Settings", icon: Settings },
];

function NavLink({ item, badge, onNavigate }: { item: NavItem; badge?: number; onNavigate: () => void }) {
  const Icon = item.icon;
  return (
    <Link
      to={item.to}
      onClick={onNavigate}
      className="flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm text-muted-foreground hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      activeProps={{ className: "bg-accent font-medium text-foreground", "aria-current": "page" }}
    >
      <Icon className="size-4" />
      <span className="flex-1">{item.label}</span>
      {badge ? (
        <Badge tone="warning" title={`${badge} awaiting approval`}>
          {badge}
        </Badge>
      ) : null}
    </Link>
  );
}

export function AppShell() {
  const { api, session } = useServices();
  const account = useAccount();
  const status = useStatus();
  const theme = useTheme();
  const [menuOpen, setMenuOpen] = useState(false);
  const [signingOut, setSigningOut] = useState(false);
  const isAdmin = account.data?.role === "admin";

  async function signOut() {
    setSigningOut(true);
    try {
      await api.auth.logout();
    } catch {
      // Local state is cleared regardless; the token expires on its own.
    }
    session.end("logout");
  }

  const close = () => setMenuOpen(false);

  return (
    <div className="flex min-h-screen bg-background">
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:left-2 focus:top-2 focus:z-[70] focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:shadow"
      >
        Skip to content
      </a>
      <aside
        className={cn(
          "fixed inset-y-0 left-0 z-40 flex w-60 flex-col border-r border-border bg-card transition-transform md:sticky md:top-0 md:h-screen md:translate-x-0",
          menuOpen ? "translate-x-0" : "-translate-x-full",
        )}
        aria-label="Main navigation"
      >
        <div className="flex h-14 items-center justify-between border-b border-border px-4">
          <Link to="/projects" onClick={close} className="font-semibold tracking-tight focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
            Golem Deploy
          </Link>
          <Button variant="ghost" size="icon" className="md:hidden" aria-label="Close menu" onClick={close}>
            <X />
          </Button>
        </div>
        <nav className="flex flex-1 flex-col gap-0.5 overflow-y-auto p-3">
          {MAIN_NAV.map((item) => (
            <NavLink key={item.to} item={item} onNavigate={close} />
          ))}
          {isAdmin && (
            <>
              <p className="mb-1 mt-4 px-2.5 text-xs font-medium uppercase tracking-wide text-muted-foreground">Administration</p>
              {ADMIN_NAV.map((item) => (
                <NavLink
                  key={item.to}
                  item={item}
                  onNavigate={close}
                  badge={item.to === "/admin/accounts" ? status.data?.counts.pending_accounts : undefined}
                />
              ))}
            </>
          )}
        </nav>
        <div className="border-t border-border p-3">
          <Link
            to="/account"
            onClick={close}
            className="flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            activeProps={{ className: "bg-accent", "aria-current": "page" }}
          >
            <User className="size-4 text-muted-foreground" />
            <span className="min-w-0 flex-1 truncate">{account.data?.username ?? "Account"}</span>
            {account.data && <Badge tone={isAdmin ? "info" : "neutral"}>{account.data.role}</Badge>}
          </Link>
          <div className="mt-2 flex gap-2">
            <Button
              variant="outline"
              size="sm"
              className="flex-1"
              onClick={theme.toggle}
              aria-label={theme.dark ? "Switch to light theme" : "Switch to dark theme"}
            >
              {theme.dark ? <Sun /> : <Moon />}
              {theme.dark ? "Light" : "Dark"}
            </Button>
            <Button variant="outline" size="sm" className="flex-1" onClick={() => void signOut()} loading={signingOut}>
              {!signingOut && <LogOut />}
              Sign out
            </Button>
          </div>
        </div>
      </aside>
      {menuOpen && <div className="fixed inset-0 z-30 bg-black/40 md:hidden" onClick={close} aria-hidden />}

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-card px-4 md:hidden">
          <Button variant="ghost" size="icon" aria-label="Open menu" aria-expanded={menuOpen} onClick={() => setMenuOpen(true)}>
            <Menu />
          </Button>
          <span className="font-semibold">Golem Deploy</span>
        </header>
        <main id="main" className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 md:px-8" tabIndex={-1}>
          {status.data && !status.data.docker_reachable && (
            <div className="mb-4">
              <Notice tone="danger" title="Docker is not reachable">
                The daemon cannot talk to Docker. Servers cannot be started, stopped or inspected, and their state shows as
                unknown until Docker is back.
              </Notice>
            </div>
          )}
          <Outlet />
        </main>
        <footer className="border-t border-border px-4 py-2 text-xs text-muted-foreground md:px-8">
          <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-x-4 gap-y-1">
            <span>Golem Deploy {status.data?.version ?? ""}</span>
            {status.data && (
              <span className="flex items-center gap-1.5">
                <span
                  aria-hidden
                  className={cn("size-2 rounded-full", status.data.docker_reachable ? "bg-emerald-500" : "bg-red-500")}
                />
                Docker {status.data.docker_reachable ? "reachable" : "not reachable"}
              </span>
            )}
            {status.data && status.data.counts.active_jobs > 0 && (
              <span>
                {status.data.counts.active_jobs} active job{status.data.counts.active_jobs === 1 ? "" : "s"}
              </span>
            )}
          </div>
        </footer>
      </div>
    </div>
  );
}
