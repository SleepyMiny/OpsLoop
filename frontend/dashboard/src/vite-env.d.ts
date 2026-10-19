/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 'demo' (default), 'live' or 'replay'. */
  readonly VITE_OPSLOOP_SOURCE?: string;
  /** Base URL of the read-only BFF serving GET /api/v1/snapshot. */
  readonly VITE_OPSLOOP_API_BASE?: string;
}
