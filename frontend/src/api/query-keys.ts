import type { SubjectType } from "@/api/types.gen";

/** Every React Query key in the app. Invalidate by the narrowest prefix that covers the change. */
export const qk = {
  account: ["account"] as const,
  tokens: ["account", "tokens"] as const,
  accounts: ["accounts"] as const,
  accountTokens: (id: number) => ["accounts", "tokens", id] as const,
  authOptions: ["auth-options"] as const,
  settings: ["settings"] as const,
  status: ["status"] as const,
  jobs: {
    all: ["jobs"] as const,
    detail: (id: number) => ["jobs", "detail", id] as const,
    forSubject: (type: SubjectType, id: number) => ["jobs", "subject", type, String(id)] as const,
  },
  projects: {
    all: ["projects"] as const,
    list: ["projects", "list"] as const,
    detail: (id: number) => ["projects", "detail", id] as const,
    branches: (id: number) => ["projects", "branches", id] as const,
  },
  servers: {
    all: ["servers"] as const,
    list: ["servers", "list"] as const,
    detail: (id: number) => ["servers", "detail", id] as const,
    stats: (id: number) => ["servers", "stats", id] as const,
  },
  images: ["images"] as const,
  staticFiles: ["static"] as const,
  volumes: ["volumes"] as const,
};
