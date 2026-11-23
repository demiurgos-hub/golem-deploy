/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** "1" enables the dev-only mock layer for routes the daemon does not serve yet. Rejected in production builds. */
  readonly VITE_API_MOCK?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
