/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** "1" enables the dev-only mock of the Docker- and git-backed routes. Rejected in production builds. */
  readonly VITE_API_MOCK?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
