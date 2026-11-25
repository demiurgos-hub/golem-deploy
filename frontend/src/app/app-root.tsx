import { useEffect, useState } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider, type RouterHistory } from "@tanstack/react-router";
import { ServicesContext, type AppServices } from "@/app/services";
import { createAppRouter } from "@/router";
import { ToastProvider } from "@/components/ui/toast";
import { ThemeProvider } from "@/hooks/use-theme";
import { safeRedirectPath } from "@/lib/url";

/**
 * Wires the services to the UI: providers, the router, token refresh, and
 * the reaction to session changes (a session that ends anywhere cancels and
 * drops all cached data and sends this tab to the login page once).
 */
export function AppRoot({ services, history }: { services: AppServices; history?: RouterHistory }) {
  const [router] = useState(() => createAppRouter(services, history));

  useEffect(() => {
    const { session, refresher, queryClient } = services;
    refresher.start();
    const unsubscribe = session.subscribe((event) => {
      if (event.type === "ended") {
        void queryClient.cancelQueries();
        queryClient.clear();
        const here = router.state.location.href;
        const back = event.reason === "logout" ? undefined : (safeRedirectPath(here) ?? undefined);
        void router.navigate({ to: "/login", search: { redirect: back }, replace: true });
      } else if (event.source === "remote") {
        // Another tab signed in (possibly as someone else) or refreshed.
        void queryClient.invalidateQueries();
        const path = router.state.location.pathname;
        if (path === "/login" || path === "/register" || path === "/setup") void router.navigate({ to: "/projects", replace: true });
      }
    });
    return () => {
      unsubscribe();
      refresher.stop();
    };
  }, [services, router]);

  return (
    <ServicesContext.Provider value={services}>
      <QueryClientProvider client={services.queryClient}>
        <ThemeProvider>
          <ToastProvider>
            <RouterProvider router={router} />
          </ToastProvider>
        </ThemeProvider>
      </QueryClientProvider>
    </ServicesContext.Provider>
  );
}
