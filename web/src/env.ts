// Build-time configuration. Nothing here is a secret.
const base = (import.meta.env.VITE_API_BASE ?? '').replace(/\/+$/, '');

/** API base URL. Empty means same origin (dev proxy). */
export function apiBase(): string {
  return base || window.location.origin;
}

/** The one origin markdown images may load from. */
export function apiOrigin(): string {
  try {
    return new URL(apiBase(), window.location.origin).origin;
  } catch {
    return window.location.origin;
  }
}

/**
 * Dev-only bearer token. `import.meta.env.DEV` is replaced by `false` in a
 * production build, so this expression is dead code there; vite.config.ts also
 * refuses to build while VITE_DEV_TOKEN is set.
 */
export const DEV_TOKEN: string | undefined = import.meta.env.DEV
  ? import.meta.env.VITE_DEV_TOKEN || undefined
  : undefined;
