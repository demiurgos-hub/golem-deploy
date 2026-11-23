import {
  Outlet,
  createRootRouteWithContext,
  createRoute,
  createRouter,
  lazyRouteComponent,
  redirect,
  type RouterHistory,
} from "@tanstack/react-router";
import type { AppServices } from "@/app/services";
import { AppShell } from "@/components/layout/app-shell";
import { AdminGate } from "@/components/layout/admin-gate";
import { NotFoundPage, RouteErrorPage } from "@/pages/not-found";
import { safeRedirectPath } from "@/lib/url";

export interface RouterContext {
  services: AppServices;
}

type AuthSearch = { redirect?: string };

function validateAuthSearch(search: Record<string, unknown>): AuthSearch {
  const r = safeRedirectPath(search.redirect);
  return r ? { redirect: r } : {};
}

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: NotFoundPage,
  errorComponent: RouteErrorPage,
});

// Signed-in users have no business on the auth pages.
function leaveAuthPages({ context, search }: { context: RouterContext; search: AuthSearch }) {
  if (context.services.session.getToken()) {
    throw redirect({ href: search.redirect ?? "/projects", replace: true });
  }
}

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  validateSearch: validateAuthSearch,
  beforeLoad: leaveAuthPages,
  component: lazyRouteComponent(() => import("@/pages/auth"), "LoginPage"),
});

const registerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/register",
  validateSearch: validateAuthSearch,
  beforeLoad: leaveAuthPages,
  component: lazyRouteComponent(() => import("@/pages/auth"), "RegisterPage"),
});

/** Everything behind sign-in. */
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "app",
  beforeLoad: ({ context, location }) => {
    if (!context.services.session.getToken()) {
      throw redirect({ to: "/login", search: { redirect: safeRedirectPath(location.href) ?? undefined } });
    }
  },
  component: AppShell,
});

const indexRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/projects", replace: true });
  },
});

const projectsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/projects",
  component: lazyRouteComponent(() => import("@/pages/projects"), "ProjectsPage"),
});

const projectRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/projects/$id",
  component: lazyRouteComponent(() => import("@/pages/project-detail"), "ProjectDetailPage"),
});

const serversRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/servers",
  component: lazyRouteComponent(() => import("@/pages/servers"), "ServersPage"),
});

const serverRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/servers/$id",
  component: lazyRouteComponent(() => import("@/pages/server-detail"), "ServerDetailPage"),
});

const imagesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/images",
  component: lazyRouteComponent(() => import("@/pages/images"), "ImagesPage"),
});

const staticRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/static",
  component: lazyRouteComponent(() => import("@/pages/static-files"), "StaticFilesPage"),
});

const volumesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/volumes",
  component: lazyRouteComponent(() => import("@/pages/volumes"), "VolumesPage"),
});

const accountRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/account",
  component: lazyRouteComponent(() => import("@/pages/account"), "AccountPage"),
});

/** Admin pages. Hidden from developers in the navigation; the API enforces access regardless. */
const adminRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/admin",
  component: AdminGate,
});

const adminIndexRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/admin/accounts", replace: true });
  },
});

const adminAccountsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/accounts",
  component: lazyRouteComponent(() => import("@/pages/admin-accounts"), "AdminAccountsPage"),
});

const adminSettingsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/settings",
  component: lazyRouteComponent(() => import("@/pages/admin-settings"), "AdminSettingsPage"),
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  registerRoute,
  appRoute.addChildren([
    indexRoute,
    projectsRoute,
    projectRoute,
    serversRoute,
    serverRoute,
    imagesRoute,
    staticRoute,
    volumesRoute,
    accountRoute,
    adminRoute.addChildren([adminIndexRoute, adminAccountsRoute, adminSettingsRoute]),
  ]),
]);

export function createAppRouter(services: AppServices, history?: RouterHistory) {
  return createRouter({
    routeTree,
    history,
    context: { services },
    defaultPreload: "intent",
    scrollRestoration: true,
  });
}

export type AppRouter = ReturnType<typeof createAppRouter>;

declare module "@tanstack/react-router" {
  interface Register {
    router: AppRouter;
  }
}
