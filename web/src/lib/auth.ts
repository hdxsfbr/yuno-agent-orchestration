import { useSyncExternalStore } from "react";

// The platform's API token. It lives in localStorage so a reload keeps the operator signed
// in, and is only ever sent as an Authorization header: never in a URL, a log line or a
// query key.

export const TOKEN_KEY = "yuno.apiToken";

// Holds the token for this tab when the browser refuses storage (private mode, blocked data).
let fallback = "";
// True from the moment the API refuses the token in use until a new one is saved.
let refused = false;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());

export function getToken(): string {
  try {
    const stored = localStorage.getItem(TOKEN_KEY);
    if (stored !== null) return stored;
  } catch {
    /* storage unavailable: use the in-memory copy */
  }
  return fallback;
}

export function setToken(token: string): void {
  fallback = token.trim();
  try {
    if (fallback) localStorage.setItem(TOKEN_KEY, fallback);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* kept in memory only */
  }
  refused = false;
  emit();
}

export const clearToken = () => setToken("");

/** The `Authorization` header for the token in use, or nothing when there is none. */
export function authHeaders(token = getToken()): Record<string, string> {
  return token ? { Authorization: `Bearer ${token}` } : {};
}

/**
 * Called when the API answers 401 to a request sent with `sent`. A response to a token that
 * has been replaced since is stale and says nothing about the current one.
 */
export function reportUnauthorized(sent: string): void {
  if (refused || sent !== getToken()) return;
  refused = true;
  emit();
}

export function subscribeAuth(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

// Another tab signing in or out changes the stored token under this one.
if (typeof window !== "undefined") {
  window.addEventListener("storage", (e) => {
    if (e.key === TOKEN_KEY || e.key === null) emit();
  });
}

/** The token in use, kept current across sign-in, sign-out and other tabs. */
export const useToken = () => useSyncExternalStore(subscribeAuth, getToken);

/** Whether the API has refused the request for want of a valid token. */
export const signInRequired = () => refused;

export const useSignInRequired = () => useSyncExternalStore(subscribeAuth, signInRequired);
