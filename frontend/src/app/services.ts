import { createContext, useContext, useSyncExternalStore } from "react";
import { QueryClient } from "@tanstack/react-query";
import { createApiClient, type ApiClient, type Transport } from "@/api/client";
import { createEndpoints, type Endpoints } from "@/api/endpoints";
import { isApiError } from "@/api/errors";
import { SessionRefresher, SessionStore, type ChannelLike, type StorageLike } from "@/api/session";

/** Everything the UI needs from the outside world, created once per app instance. */
export interface AppServices {
  client: ApiClient;
  api: Endpoints;
  session: SessionStore;
  refresher: SessionRefresher;
  queryClient: QueryClient;
}

export interface CreateServicesOptions {
  transport: Transport;
  local?: StorageLike | null;
  session?: StorageLike | null;
  channel?: ChannelLike | null;
  document?: Document | null;
}

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        // Client errors (4xx) do not get better by retrying.
        retry: (count, err) => !(isApiError(err) && (err.isClientError || err.code === "aborted")) && count < 2,
        refetchOnWindowFocus: true,
        refetchIntervalInBackground: false,
      },
      mutations: { retry: false },
    },
  });
}

export function createServices(opts: CreateServicesOptions): AppServices {
  const session = new SessionStore({ local: opts.local, session: opts.session, channel: opts.channel });
  const client = createApiClient({
    transport: opts.transport,
    getToken: () => session.getToken(),
    onUnauthenticated: (tokenUsed) => session.handleUnauthenticated(tokenUsed),
  });
  const api = createEndpoints(client);
  const refresher = new SessionRefresher({
    store: session,
    refresh: () => api.auth.refresh(),
    document: opts.document ?? null,
  });
  const queryClient = createQueryClient();
  return { client, api, session, refresher, queryClient };
}

export const ServicesContext = createContext<AppServices | null>(null);

export function useServices(): AppServices {
  const s = useContext(ServicesContext);
  if (!s) throw new Error("useServices must be used inside ServicesContext");
  return s;
}

export function useApi(): Endpoints {
  return useServices().api;
}

export function useSession() {
  const { session } = useServices();
  return useSyncExternalStore(session.subscribeSnapshot, session.getSnapshot);
}
