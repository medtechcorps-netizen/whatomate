import axios, {
  AxiosError,
  type AxiosInstance,
  type AxiosRequestConfig,
  type AxiosResponse,
  type InternalAxiosRequestConfig,
} from "axios";
import { toast } from "vue-sonner";
import { i18n } from "@/i18n";
import { resolveOrganizationHeader } from "@/lib/organizationHeaders";
import {
  clearBrowserOrganizationIdentity,
  readSelectedOrganizationId,
} from "@/lib/browserIdentity";
import { clearSignOutReason, recordSignOutReason } from "@/lib/sessionSignout";
import { registerTabSession } from "@/lib/tabSession";
import {
  announceSessionEnding,
  clearLastActivity,
  currentPolicyIdentity,
  isIdleSessionStale,
  readSessionIdlePolicy,
  recordActivity,
  rememberSessionIdlePolicy,
} from "@/lib/sessionActivity";

// Get base path from server-injected config or fallback
const basePath = ((window as any).__BASE_PATH__ ?? "").replace(/\/$/, "");
const API_BASE_URL = import.meta.env.VITE_API_URL || `${basePath}/api`;

export const api: AxiosInstance = axios.create({
  baseURL: API_BASE_URL,
  timeout: 30000,
  withCredentials: true,
  headers: {
    "Content-Type": "application/json",
  },
});

// Helper to read a cookie by name
function getCookie(name: string): string | null {
  const match = document.cookie.match(
    new RegExp("(?:^|; )" + name + "=([^;]*)"),
  );
  return match ? decodeURIComponent(match[1]) : null;
}

/**
 * Build standard headers for native fetch() calls.
 * Includes X-Organization-ID (for org switching) and optionally X-CSRF-Token (for mutating requests).
 */
export function getRequestHeaders(opts?: {
  csrf?: boolean;
}): Record<string, string> {
  const headers: Record<string, string> = {};
  const selectedOrgId = readSelectedOrganizationId();
  if (selectedOrgId) {
    headers["X-Organization-ID"] = selectedOrgId;
  }
  if (opts?.csrf) {
    const csrfToken = getCookie("whm_csrf");
    if (csrfToken) {
      headers["X-CSRF-Token"] = csrfToken;
    }
  }
  return headers;
}

// ---------------------------------------------------------------------------
// Session refresh coordination
//
// The access token lives in an httpOnly cookie that every tab shares. When it
// expires, the first 401 rotates it via POST /auth/refresh (single-use refresh
// token). The rules below keep a burst of 401s — across tabs, or staggered in
// one tab right after a refresh — from logging the user out:
//
// - Every request is stamped with the shared "auth epoch" it was sent under.
//   The epoch advances (in localStorage, so all tabs see it) after each
//   successful login or refresh. A 401 for a request sent under an older epoch
//   is stale: the cookie has already been rotated, so it is retried once
//   without refreshing again.
// - Refreshes are serialized across tabs with the Web Locks API, and inside the
//   lock a tab skips its own refresh when another tab advanced the epoch while
//   it waited.
// - Only a definitive 401/403 from /auth/refresh ends the session. Network
//   errors, 5xx and 429 are retried with backoff behind a non-blocking
//   "Reconnecting…" toast, and the user stays signed in if they still fail.
// - While a tab is visible the access token is refreshed shortly before it
//   expires, using expires_in from the login/refresh responses.
// ---------------------------------------------------------------------------

const AUTH_EPOCH_KEY = "whm_auth_epoch";
const AUTH_EXPIRES_AT_KEY = "whm_auth_expires_at";
// Same lock name as earlier builds so tabs on old and new bundles still
// serialize their refreshes during a deploy.
const REFRESH_LOCK_NAME = "whm-token-refresh";
const PROACTIVE_REFRESH_LEAD_MS = 60_000;
// Delays before each retry of a transient refresh failure (~10 s in total).
const REFRESH_RETRY_DELAYS_MS = [1_000, 3_000, 6_000];
const REFRESH_REQUEST_TIMEOUT_MS = 15_000;
const REVOKE_REQUEST_TIMEOUT_MS = 5_000;
// Waits before retrying a rate-limited revoke.
const REVOKE_RETRY_DELAYS_MS = [1_000, 2_000];
// Session changes (logout, sign-in, org switch) are ordered after other work
// on the shared refresh lock and wait at most this long for it. Most holders
// finish well inside it: a refresh attempt and the sign-in or org switch
// request itself time out after 15 s, a logout after 7 + 5 s. A sign-in that
// first has to finish an earlier logout can hold longer. A wait that runs out
// never goes ahead unlocked: a logout is deferred (its pending record is
// finished by the next sign-in or page load, under the lock), and a sign-in
// is refused with SESSION_BUSY_CODE ("please try again").
const SESSION_CHANGE_LOCK_WAIT_MS = 20_000;
const SESSION_ISSUING_TIMEOUT_MS = 15_000;
// What a background logout may spend revoking before it aborts the revoke and
// logs out anyway.
const LOGOUT_REVOKE_BUDGET_MS = 7_000;
const LOGOUT_REQUEST_TIMEOUT_MS = 5_000;
// A held sign-in lock is released by this backstop even if its response is
// never seen. It is above the longest hold: a pending logout finished first
// (7 + 5 s), the request (15 s) and undoing what a late answer issued
// (5 + 5 s).
const SESSION_LOCK_BACKSTOP_MS = 60_000;
// The marker and CSRF value of the session that began last (kept current
// across its refreshes): tells whether the jar still holds that session's
// cookies.
const SESSION_BEGUN_KEY = "whm_session_begun";
// Each open tab refreshes its own liveness stamp; a frozen or discarded tab's
// stamp goes stale, so a closing tab does not count on it.
const TAB_ALIVE_PREFIX = "whm_tab_alive:";
const TAB_HEARTBEAT_MS = 5_000;
const TAB_ALIVE_WINDOW_MS = 15_000;
const SESSION_MARKER_KEY = "whm_session_id";
// The ended marker and CSRF value of a logout that has not got through yet.
const LOGOUT_PENDING_KEY = "whm_logout_pending";
// Same-origin tabs announce themselves here, so a closing tab knows whether
// another one can finish its logout in order.
const TAB_CHANNEL_NAME = "whm-tabs";
/** Error code of a sign-in refused because the session lock stayed busy. */
export const SESSION_BUSY_CODE = "ERR_SESSION_BUSY";
/**
 * Error code of a sign-in or org switch refused because this browser has no
 * Web Locks (an outdated browser, or plain HTTP off localhost): sessions could
 * not be kept apart across its tabs.
 */
export const SESSION_UNSUPPORTED_CODE = "ERR_SESSION_UNSUPPORTED";
const RECONNECT_TOAST_ID = "whm-session-reconnect";
const MAX_TIMER_DELAY_MS = 2_147_483_647;

import { createRouterMatcher, START_LOCATION, type RouterMatcher } from "vue-router";
import { routes } from "@/router/routes";
import { AUTH_SESSION_EVENT } from "@/lib/authSessionEvent";

export { AUTH_SESSION_EVENT };
export type AuthSessionState =
  | "refreshed"
  | "reconnecting"
  | "reconnected"
  | "unavailable"
  | "expired";

type AuthAwareRequestConfig = InternalAxiosRequestConfig & {
  _retry?: boolean;
  _authEpoch?: number;
  // The logical session marker the request was sent under: a request is never
  // refreshed for, or replayed under, another session.
  _sessionMarker?: string | null;
  // A sign-in or org switch holds the refresh lock until its response.
  _releaseSessionLock?: () => void;
  // The marker and CSRF cookie when a sign-in or org switch was sent, with
  // the lock held.
  _issueMarker?: string | null;
  _issueCsrf?: string | null;
  _switchIdentity?: string;
  _switchDispatched?: boolean;
  _switchAdapter?: ReturnType<typeof axios.getAdapter>;
  // An SSO callback page's code exchange (POST /auth/refresh): issues a
  // session like a sign-in.
  _ssoExchange?: boolean;
  // A 401 is returned as is: no refresh, never a sign-out.
  skipAuthRefresh?: boolean;
  // The logical session and identity a GET /me was sent under.
  _sessionScope?: string;
};

// Login/refresh responses whose Set-Cookie starts a new auth epoch.
const SWITCH_ORGANIZATION_PATH = "/auth/refresh/switch-org";
const SESSION_ISSUING_PATHS = ["/auth/login", "/auth/register", SWITCH_ORGANIZATION_PATH];

function isOrganizationSwitch(path: string): boolean {
  return path === SWITCH_ORGANIZATION_PATH;
}

function isSessionIssuing(config: AuthAwareRequestConfig | undefined, path: string): boolean {
  return SESSION_ISSUING_PATHS.includes(path) || config?._ssoExchange === true;
}

let memoryAuthEpoch = 0;
let memorySessionMarker: string | null = null;
// The logout still being completed in the background, if any.
let pendingLogout: Promise<void> | null = null;
let pendingLogoutMarker: string | null = null;
// The refresh request this tab has in flight: another tab's session change
// aborts it, so its late Set-Cookie can never land after that change.
let refreshAbort: AbortController | null = null;
// The logout request (revoke or /auth/logout) this tab has in flight: a new
// session begun in another tab aborts it, so its cookie-clearing answer cannot
// land on that session. (Every session now begins under the lock, after any
// logout queued before it, so this is defensive.)
let logoutRequests: AbortController | null = null;
// The logical session this document shows: set when it restores or
// establishes an identity, cleared when it ends or leaves. A tab acts only on
// ends of this session, and never sends this session's work under another.
let tabSession: string | null = null;
// The pending logout this tab is responsible for: one it started, took over
// from a closing tab or resumed on load (kept when its lock wait was deferred).
let ownedLogout: string | null = null;
// Set the moment a session starts to end (sign-out, expiry, idle end): from
// then on no API request goes out and no 401 triggers a refresh that could
// race the revoke. A new sign-in clears it.
let sessionEnding = false;
// A key earlier builds kept (the CSRF value of their last session); nothing
// reads it any more, and a logout clears it.
const SESSION_CSRF_KEY = "whm_session_csrf";
let refreshInFlight: Promise<void> | null = null;
let proactiveRefreshTimer: ReturnType<typeof setTimeout> | null = null;

function readStoredNumber(key: string): number {
  try {
    const value = Number(localStorage.getItem(key));
    return Number.isFinite(value) && value > 0 ? value : 0;
  } catch {
    return 0;
  }
}

function writeStoredValue(key: string, value: string | null) {
  try {
    if (value === null) {
      localStorage.removeItem(key);
    } else {
      localStorage.setItem(key, value);
    }
  } catch {
    // In-memory state still covers this tab when storage is denied.
  }
}

// The logical session marker changes when a session begins (a successful
// login or register, an SSO sign-in) or ends (logout, expiry); never on token
// rotation, a CSRF change or an org switch. A live marker is the session id;
// an ended one is "ended:<that id>", so ending the same session twice writes
// the same value. Refresh work captures the marker when it starts and may only
// act while it is unchanged and live.
const ENDED_MARKER_PREFIX = "ended:";

function readSessionMarker(): string | null {
  try {
    return localStorage.getItem(SESSION_MARKER_KEY);
  } catch {
    return memorySessionMarker;
  }
}

function setSessionMarker(marker: string): string {
  memorySessionMarker = marker;
  writeStoredValue(SESSION_MARKER_KEY, marker);
  return marker;
}

function isEndedMarker(marker: string | null | undefined): boolean {
  return !!marker && marker.startsWith(ENDED_MARKER_PREFIX);
}

/**
 * The id of the logical session this browser is in, or has just ended ("" for
 * a session from before ids existed). Stable across token rotation.
 */
export function readSessionId(): string {
  const marker = readSessionMarker() ?? "";
  return isEndedMarker(marker) ? marker.slice(ENDED_MARKER_PREFIX.length) : marker;
}

// beginSessionMarker starts a new logical session. An older logout that has
// not got through is moot: the jar now holds the new session's cookies.
function beginSessionMarker(): string {
  writeStoredValue(LOGOUT_PENDING_KEY, null);
  const id = Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
  tabSession = id;
  const marker = setSessionMarker(id);
  writeStoredValue(SESSION_BEGUN_KEY, JSON.stringify({ marker, csrf: getCookie("whm_csrf") }));
  return marker;
}

function readBegunSession(): { marker: string; csrf: string | null } | null {
  try {
    const parsed = JSON.parse(localStorage.getItem(SESSION_BEGUN_KEY) || "null");
    return parsed && typeof parsed.marker === "string" ? parsed : null;
  } catch {
    return null;
  }
}

// A refresh rotated the current session's cookies: note its new CSRF value.
function noteBegunCsrf(marker: string | null) {
  const begun = readBegunSession();
  if (begun && begun.marker === marker) {
    writeStoredValue(SESSION_BEGUN_KEY, JSON.stringify({ marker, csrf: getCookie("whm_csrf") }));
  }
}

// endSessionMarker ends the logical session `expected` (an id) and returns its
// ended marker. It returns null, and writes nothing, when the browser has
// moved on to another session: that one is not this end's to end.
function endSessionMarker(expected: string): string | null {
  const current = readSessionMarker() ?? "";
  const ended = ENDED_MARKER_PREFIX + expected;
  if (current === ended) return ended;
  if (current !== expected) return null;
  return setSessionMarker(ended);
}

function sessionIdOf(marker: string | null): string {
  const value = marker ?? "";
  return isEndedMarker(value) ? value.slice(ENDED_MARKER_PREFIX.length) : value;
}

/** The logical session this tab shows, or null when it shows none. */
export function readTabSession(): string | null {
  return tabSession;
}

/**
 * Whether this tab shows the browser's current, live session. Only then may
 * it end that session or touch the shared state; otherwise it leaves locally.
 */
export function isTabSessionCurrent(): boolean {
  const marker = readSessionMarker();
  return tabSession !== null && !isEndedMarker(marker) && sessionIdOf(marker) === tabSession;
}

/**
 * Records that this tab now shows the browser's current session (the store
 * restored or applied an identity, say after a sign-in elsewhere and a Back
 * navigation). Returns false, adopting nothing, when that session has ended,
 * or when this tab still shows another session (a newer sign-in replaced it):
 * then the identity at hand is not this tab's to show, and the tab leaves.
 */
export function adoptTabSession(): boolean {
  const marker = readSessionMarker();
  if (isEndedMarker(marker)) {
    tabSession = null;
    return false;
  }
  if (tabSession !== null && tabSession !== sessionIdOf(marker)) {
    leaveReplacedSession();
    return false;
  }
  tabSession = sessionIdOf(marker);
  return true;
}

/** The session this tab shows when it is the browser's current one, else null. */
export function currentTabSession(): string | null {
  return isTabSessionCurrent() ? tabSession : null;
}

/**
 * Whether the shared state still belongs to `session` (a session this tab
 * showed as current): the browser is still in it, or has just ended it. Once
 * another session began (in any tab), that one owns it.
 */
export function sharedStateBelongsTo(session: string | null): boolean {
  if (session === null) return false;
  const marker = readSessionMarker() ?? "";
  return marker === session || marker === ENDED_MARKER_PREFIX + session;
}

/** Whether the browser's session has ended (no newer one began since). */
export function browserSessionEnded(): boolean {
  return isEndedMarker(readSessionMarker());
}

registerTabSession({
  adopt: adoptTabSession,
  isCurrent: isTabSessionCurrent,
  current: currentTabSession,
  owns: sharedStateBelongsTo,
  ended: browserSessionEnded,
});

type PendingLogout = { marker: string; csrf: string | null };

function readPendingLogout(): PendingLogout | null {
  try {
    const parsed = JSON.parse(localStorage.getItem(LOGOUT_PENDING_KEY) || "null");
    return parsed && typeof parsed.marker === "string" ? parsed : null;
  } catch {
    return null;
  }
}

// recordPendingCsrf notes the jar's current CSRF value in the pending logout
// of `ended` (a refresh of that session rotated it after the end).
function recordPendingCsrf(ended: string) {
  const pending = readPendingLogout();
  const csrf = getCookie("whm_csrf");
  if (pending?.marker === ended && csrf && csrf !== pending.csrf) {
    writeStoredValue(LOGOUT_PENDING_KEY, JSON.stringify({ ...pending, csrf }));
  }
}

// The CSRF value recorded with the pending logout of `ended`, if any.
function recordedLogoutCsrf(ended?: string): string | null {
  const pending = readPendingLogout();
  return ended !== undefined && pending?.marker === ended ? pending.csrf : null;
}

// A pending logout still applies while no session began after it.
function pendingLogoutApplies(): PendingLogout | null {
  const pending = readPendingLogout();
  return pending && pending.marker === readSessionMarker() ? pending : null;
}

function sessionChangedError(config?: InternalAxiosRequestConfig) {
  return new AxiosError("The session changed", AxiosError.ERR_CANCELED, config);
}

function sessionBusyError(config?: InternalAxiosRequestConfig) {
  return new AxiosError("The session is busy, please try again", SESSION_BUSY_CODE, config);
}

function sessionUnsupportedError(config?: InternalAxiosRequestConfig) {
  return new AxiosError(
    "This browser cannot keep sessions apart across tabs",
    SESSION_UNSUPPORTED_CODE,
    config,
  );
}

function readAuthEpoch(): number {
  return Math.max(memoryAuthEpoch, readStoredNumber(AUTH_EPOCH_KEY));
}

function advanceAuthEpoch() {
  memoryAuthEpoch = Math.max(Date.now(), readAuthEpoch() + 1);
  writeStoredValue(AUTH_EPOCH_KEY, String(memoryAuthEpoch));
}

function emitAuthSessionState(state: AuthSessionState) {
  try {
    window.dispatchEvent(
      new CustomEvent(AUTH_SESSION_EVENT, { detail: { state } }),
    );
  } catch {
    // Observers are optional.
  }
}

function showReconnectToast(state: "reconnecting" | "dismiss" | "unavailable") {
  const t = i18n.global.t;
  try {
    if (state === "reconnecting") {
      toast.loading(t("sessionSecurity.reconnecting"), {
        id: RECONNECT_TOAST_ID,
      });
    } else if (state === "dismiss") {
      toast.dismiss(RECONNECT_TOAST_ID);
    } else {
      toast.error(t("sessionSecurity.reconnectFailed"), {
        id: RECONNECT_TOAST_ID,
        description: t("sessionSecurity.reconnectFailedDesc"),
      });
    }
  } catch {
    // A toast failure must never affect the request flow.
  }
}

function requestPath(url?: string): string {
  if (!url) return "";
  const path = url.split("?")[0];
  const apiIndex = path.indexOf("/api/auth/");
  return apiIndex >= 0 ? path.slice(apiIndex + 4) : path;
}

// Endpoints that must never trigger a refresh: they either establish or end
// the session themselves, or are public.
function isSessionlessAuthEndpoint(url?: string): boolean {
  const path = requestPath(url);
  return (
    path === "/auth/login" ||
    path === "/auth/register" ||
    path === "/auth/refresh" ||
    path === "/auth/logout" ||
    path === "/auth/sso" ||
    path.startsWith("/auth/sso/")
  );
}

// A 401 from /auth/refresh is the server's verdict. A 403 counts only when it
// carries the app's own JSON error envelope; an HTML page from a proxy, WAF or
// edge is treated as transient.
function isDefinitiveRefreshFailure(error: unknown): boolean {
  if (!axios.isAxiosError(error)) return false;
  const status = error.response?.status;
  if (status === 401) return true;
  if (status !== 403) return false;
  const data = error.response?.data as { status?: unknown } | null | undefined;
  return !!data && typeof data === "object" && data.status === "error";
}

function hasStoredUser(): boolean {
  try {
    return !!localStorage.getItem("user");
  } catch {
    return false;
  }
}

function clearSessionTiming() {
  if (proactiveRefreshTimer !== null) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }
  writeStoredValue(AUTH_EXPIRES_AT_KEY, null);
}

function recordSessionExpiry(expiresInSeconds: unknown) {
  if (typeof expiresInSeconds !== "number" || !(expiresInSeconds > 0)) return;
  writeStoredValue(
    AUTH_EXPIRES_AT_KEY,
    String(Date.now() + expiresInSeconds * 1000),
  );
  scheduleProactiveRefresh();
}

function onSessionIssued(expiresInSeconds: unknown) {
  advanceAuthEpoch();
  recordSessionExpiry(expiresInSeconds);
}

/**
 * Marks this tab's session as ending: no further API request or refresh. It
 * fires SESSION_ENDING_EVENT once per end (the only place it is fired), so
 * views drop unsaved-work guards before the identity is cleared.
 */
export function markSessionEnding() {
  if (sessionEnding) return;
  sessionEnding = true;
  announceSessionEnding();
}

// The session and identity a /me response must still belong to when it
// arrives: one answered after a logout, a new login or an org switch is not
// applied.
function sessionScope(): string {
  return `${readSessionId()}|${currentPolicyIdentity()}`;
}

function delay(ms: number, signal?: AbortSignal) {
  return new Promise<void>((resolve) => {
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}

// Set once the lock manager refused a request (a sandboxed or opaque-origin
// page, a broken implementation): from then on this page has no Web Locks.
let locksUnusable = false;

function webLocks(): LockManager | null {
  if (locksUnusable) return null;
  const locks = typeof navigator !== "undefined" ? navigator.locks : undefined;
  return locks?.request ? locks : null;
}

/**
 * Whether this browser can keep sessions apart across tabs (Web Locks). Views
 * check it before starting a sign-in, which clears the stored identity first.
 */
export function sessionsSupported(): boolean {
  return webLocks() !== null;
}

// The load-time probe of the lock manager (a refusing one counts as none).
let lockProbe: Promise<void> = Promise.resolve();

/** sessionsSupported(), once the load-time probe of the lock manager settled. */
export function whenSessionsSupportKnown(): Promise<boolean> {
  return lockProbe.then(() => sessionsSupported());
}

// postRefresh rotates the cookies via POST /auth/refresh. It carries the CSRF
// double-submit header: whenever a fresh whm_access cookie rides along (path
// /api covers /api/auth/refresh), the CSRF middleware of older servers rejects
// a header-less refresh with 403.
function postRefresh(signal?: AbortSignal) {
  const headers: Record<string, string> = {};
  const csrfToken = getCookie("whm_csrf");
  if (csrfToken) {
    headers["X-CSRF-Token"] = csrfToken;
  }
  return axios.post(
    `${API_BASE_URL}/auth/refresh`,
    {},
    { withCredentials: true, timeout: REFRESH_REQUEST_TIMEOUT_MS, headers, signal },
  );
}

// sendRefresh sends one refresh attempt that another tab's session change can
// abort (an aborted request's late Set-Cookie is discarded).
async function sendRefresh() {
  const abort = new AbortController();
  refreshAbort = abort;
  try {
    return await postRefresh(abort.signal);
  } finally {
    if (refreshAbort === abort) refreshAbort = null;
  }
}

/**
 * Revokes the server-side refresh token. The whm_refresh cookie is only sent
 * to /auth/refresh, so /auth/logout alone cannot revoke it; logout calls this
 * first. Failures never block the logout. Every attempt stops once a newer
 * session began (the marker left `endedMarker`), or another identity signed
 * in, or `signal` aborted, which also aborts the request in flight.
 */
export async function revokeRefreshToken(
  endedMarker?: string,
  identity: string = storedIdentity(),
  signal?: AbortSignal,
): Promise<void> {
  for (let attempt = 0; attempt <= REVOKE_RETRY_DELAYS_MS.length; attempt++) {
    if (signal?.aborted) return;
    // A different session signed in from another tab while this one waited:
    // its token is not this logout's to revoke.
    const current = storedIdentity();
    if (current && current !== identity) return;
    if (endedMarker !== undefined && readSessionMarker() !== endedMarker) return;
    // Read the token on every attempt: a refresh in another tab rotates it.
    // Without Web Locks nothing orders this revoke against a sign-in in
    // another tab, so it presents the CSRF value recorded for the ended
    // session: a newer session's cookies that land meanwhile then fail the
    // server's check instead of being revoked.
    const pinned = !webLocks();
    const csrfToken = pinned ? recordedLogoutCsrf(endedMarker) : getCookie("whm_csrf");
    if (!csrfToken) return;
    let status: number | undefined;
    try {
      if (pinned) {
        // Without Web Locks: cookie-neutral (this page cannot order a cookie
        // change against other tabs, so it changes none; no /auth/logout
        // follows), and with keepalive: the page may leave at once (a full
        // navigation to the login page), and no other page finishes this
        // revoke for it.
        const response = await fetch(`${API_BASE_URL}/auth/refresh`, {
          method: "POST",
          credentials: "include",
          keepalive: true,
          headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
          body: JSON.stringify({ revoke: true, keep_cookies: true }),
          signal,
        });
        if (response.ok) return;
        status = response.status;
      } else {
        await axios.post(
          `${API_BASE_URL}/auth/refresh`,
          { revoke: true },
          {
            withCredentials: true,
            timeout: REVOKE_REQUEST_TIMEOUT_MS,
            headers: { "X-CSRF-Token": csrfToken },
            signal,
          },
        );
        return;
      }
    } catch (error) {
      if (signal?.aborted) return;
      status = axios.isAxiosError(error) ? error.response?.status : undefined;
    }
    // The CSRF cookie was rotated mid-flight: try again with the new one
    // (never without Web Locks: the new one may be a newer session's).
    if (status === 403 && !pinned && getCookie("whm_csrf") !== csrfToken) continue;
    // The revoke shares the per-IP refresh rate limit, which a busy clinic
    // behind one NAT address can exhaust: wait and retry a 429. Anything else,
    // or a last failure, still lets the logout go ahead.
    if (status !== 429 || attempt >= REVOKE_RETRY_DELAYS_MS.length) return;
    await delay(REVOKE_RETRY_DELAYS_MS[attempt], signal);
  }
}

// storedIdentity identifies the signed-in user and organization of this
// browser, so a logout can tell when another tab signed someone else in.
function storedIdentity(): string {
  try {
    const user = JSON.parse(localStorage.getItem("user") || "null");
    return user?.id ? `${user.id}|${user.organization_id ?? ""}` : "";
  } catch {
    return "";
  }
}

// refreshWithRetry refreshes once, retrying transient failures (network,
// timeout, 5xx, 429) with backoff. It throws the last error when the refresh
// is definitively rejected or every retry failed.
function sessionEndingError(config?: InternalAxiosRequestConfig) {
  return new AxiosError("The session is ending", AxiosError.ERR_CANCELED, config);
}

async function refreshWithRetry(
  requestEpoch: number,
  marker: string | null,
): Promise<void> {
  for (let attempt = 0; ; attempt++) {
    // The session changed (a logout, or a new sign-in), or an end started in
    // this tab, while this refresh was queued or backing off: it must not
    // touch the new state, and nothing may extend an ending session.
    if (sessionEnding || readSessionMarker() !== marker || isEndedMarker(marker)) {
      if (attempt > 0) showReconnectToast("dismiss");
      throw sessionEnding ? sessionEndingError() : sessionChangedError();
    }
    let response;
    try {
      response = await sendRefresh();
    } catch (refreshError) {
      // Stale work first: once the session changed, this outcome belongs to
      // no one (no expiry, no reconnect state, no retry).
      if (readSessionMarker() !== marker) {
        if (attempt > 0) showReconnectToast("dismiss");
        throw sessionChangedError();
      }
      // Without Web Locks (a session from before) another tab may have
      // rotated the cookie while this request was in flight; its success is
      // ours too.
      if (readAuthEpoch() > requestEpoch) {
        if (attempt > 0) showReconnectToast("dismiss");
        return;
      }
      if (isDefinitiveRefreshFailure(refreshError)) {
        if (attempt > 0) showReconnectToast("dismiss");
        throw refreshError;
      }
      if (attempt >= REFRESH_RETRY_DELAYS_MS.length) {
        showReconnectToast("unavailable");
        emitAuthSessionState("unavailable");
        throw refreshError;
      }
      if (attempt === 0) {
        showReconnectToast("reconnecting");
        emitAuthSessionState("reconnecting");
      }
      await delay(REFRESH_RETRY_DELAYS_MS[attempt]);
      continue;
    }
    // A session change that started while this request was in flight waits
    // for it (shared lock) and ends what it issued; leave no bookkeeping.
    if (readSessionMarker() !== marker) {
      if (attempt > 0) showReconnectToast("dismiss");
      const now = readSessionMarker();
      // This refresh rotated the session that has just ended: its pending
      // logout must now go out with the new CSRF value.
      if (now === ENDED_MARKER_PREFIX + (marker ?? "")) recordPendingCsrf(now);
      throw sessionChangedError();
    }
    onSessionIssued(response?.data?.data?.expires_in);
    noteBegunCsrf(marker);
    if (attempt > 0) showReconnectToast("dismiss");
    emitAuthSessionState(attempt > 0 ? "reconnected" : "refreshed");
    return;
  }
}

// refreshAcrossTabs serializes the refresh across all same-origin tabs. Each
// tab re-checks the shared epoch once it holds the lock, so a tab whose 401
// predates another tab's refresh just retries with the rotated cookie instead
// of spending (and losing) the single-use refresh token again. Without Web
// Locks it refuses (SESSION_UNSUPPORTED_CODE): a session that already existed
// then ends (endUnsupportedSession).
async function refreshAcrossTabs(
  requestEpoch: number,
  marker: string | null,
): Promise<void> {
  const run = async () => {
    if (sessionEnding) throw sessionEndingError();
    if (readSessionMarker() !== marker) throw sessionChangedError();
    if (readAuthEpoch() > requestEpoch) return;
    await refreshWithRetry(requestEpoch, marker);
  };
  const locks = webLocks();
  if (locks) {
    let entered = false;
    try {
      await locks.request(REFRESH_LOCK_NAME, () => {
        entered = true;
        return run();
      });
      return;
    } catch (error) {
      if (entered) throw error;
      // The lock manager refused: as without Web Locks (below).
      locksUnusable = true;
    }
  }
  // Without Web Locks no refresh runs at all: its answer could land on a
  // session another tab began. The caller ends the session instead.
  throw sessionUnsupportedError();
}

// ensureFreshSession resolves once the session is newer than requestEpoch. A
// single refresh per tab is in flight at a time; concurrent callers share it.
// Work for a request sent under another session, or for an ended session, is
// refused (sessionChangedError): it is never refreshed for or replayed.
function ensureFreshSession(
  requestEpoch: number,
  requestMarker: string | null = readSessionMarker(),
): Promise<void> {
  // An ending session (this tab) is never extended either.
  if (sessionEnding) return Promise.reject(sessionEndingError());
  const marker = readSessionMarker();
  if (marker !== requestMarker || isEndedMarker(marker)) {
    return Promise.reject(sessionChangedError());
  }
  if (readAuthEpoch() > requestEpoch) return Promise.resolve();
  if (!refreshInFlight) {
    refreshInFlight = refreshAcrossTabs(requestEpoch, marker).finally(() => {
      refreshInFlight = null;
    });
  }
  return refreshInFlight;
}

function isSessionChange(error: unknown): boolean {
  return axios.isAxiosError(error) && error.message === "The session changed";
}

/**
 * Whether a sign-in or registration was answered after the browser's session
 * changed elsewhere, so what it issued was undone here: the server has acted
 * on it (an account created, an invitation used). The user should sign in.
 */
export function sentBeforeSessionChange(error: unknown): boolean {
  if (!isSessionChange(error)) return false;
  const config = (error as AxiosError).config as AuthAwareRequestConfig | undefined;
  return config?._issueMarker !== undefined;
}

// onSessionChanged is called when refresh work is refused because the
// session changed or ended (never an expiry or a replay), and whenever
// another tab changes the session marker. A tab still showing a session that
// has ended, or that another sign-in has replaced, leaves it, on a public page
// too (the SSO callback page after its exchange, say). This does not depend
// on the shared stored identity, which the ending tab has cleared.
function onSessionChanged() {
  if (tabSession === null) return;
  const marker = readSessionMarker();
  if (marker === ENDED_MARKER_PREFIX + tabSession) {
    leaveEndedSession();
  } else if (sessionIdOf(marker) !== tabSession) {
    leaveReplacedSession();
  }
}

// leaveEndedSession signs this tab out of its session, which ended in another
// tab: no request, and no new reason for the login page.
function leaveEndedSession() {
  const showed = tabSession;
  tabSession = null;
  markSessionEnding();
  clearSessionTiming();
  showReconnectToast("dismiss");
  clearStoredIdentity();
  leaveForLogin(showed);
}

// leaveReplacedSession takes this tab off a session that a newer sign-in has
// replaced. The browser-wide state now belongs to that newer session, so this
// writes nothing shared and announces nothing: it only stops this tab's
// requests and leaves for the login page.
function leaveReplacedSession() {
  const showed = tabSession;
  tabSession = null;
  markSessionEnding();
  leaveForLogin(showed);
}

// leaveForLogin sends a tab that has just left a session to the login page.
// Only a public page that showed no session (signing in, an invitation, the
// SSO callback before its exchange) stays where it is.
function leaveForLogin(showed: string | null) {
  if (showed !== null || !isOnPublicPage()) {
    window.location.href = basePath + "/login";
  }
}

// holdRefreshLock waits until no other work holds the shared refresh lock
// (Web Locks), for at most SESSION_CHANGE_LOCK_WAIT_MS, then keeps refreshes
// and other session changes out until release() is called. It resolves null
// when the wait ran out: the caller must not go ahead as if it held the lock.
// Ownership never lapses on a timer: only release() (or the page going away)
// ends it, and the sign-in backstop calls release() for its own grant only.
// Without Web Locks nothing else mutates cookies (no sign-in, exchange or
// refresh runs there): the logout of a session that already existed goes
// ahead at once.
function holdRefreshLock(): Promise<(() => void) | null> {
  const locks = webLocks();
  if (!locks) return Promise.resolve(() => undefined);
  return new Promise((resolveHold) => {
    let timedOut = false;
    let granted = false;
    const controller =
      typeof AbortController !== "undefined" ? new AbortController() : null;
    const timer = setTimeout(() => {
      timedOut = true;
      controller?.abort();
      resolveHold(null);
    }, SESSION_CHANGE_LOCK_WAIT_MS);
    // The lock manager refused (it threw, or rejected before granting): this
    // page has no usable Web Locks. Settle now, as without them, never at the
    // 20 s cap; a session change then refuses as unsupported.
    const unusable = () => {
      if (timedOut || granted) return;
      locksUnusable = true;
      clearTimeout(timer);
      resolveHold(holdRefreshLock());
    };
    try {
      locks
        .request(REFRESH_LOCK_NAME, controller ? { signal: controller.signal } : {}, () => {
          if (timedOut) return undefined;
          granted = true;
          clearTimeout(timer);
          return new Promise<void>((release) => resolveHold(() => release()));
        })
        .catch(unusable);
    } catch {
      unusable();
    }
  });
}

// finishLogout ends the server session of `ended` once the work ahead of it
// on the shared lock has settled: a refresh or sign-in response applied after
// the logout would otherwise put cookies back. The user has already left; this
// only orders the network requests, within bounded waits.
async function finishLogout(
  ended: string,
  identity?: string,
  recorded = false,
): Promise<void> {
  const release = await holdRefreshLock();
  try {
    // The lock stayed busy: defer, never run unlocked. The pending record is
    // finished by the next sign-in or page load (or a tab taking over).
    if (!release) return;
    try {
      // A sign-in (in any tab) that got the lock first has already finished it.
      if (recorded && readPendingLogout()?.marker !== ended) return;
      await sendLogoutRequests(ended, identity);
    } finally {
      release();
    }
  } finally {
    if (pendingLogoutMarker === ended) pendingLogoutMarker = null;
  }
}

// sendLogoutRequests revokes the refresh token (aborted after
// LOGOUT_REVOKE_BUDGET_MS) and then calls /auth/logout, for the session that
// `ended` ended, unless a newer session began. The caller holds the lock.
// A page that cannot hold the shared lock only revokes, cookie-neutral: the
// cookie clearing stays pending for a page that can order it.
async function sendLogoutRequests(ended: string, identity?: string): Promise<void> {
  // A newer sign-in has begun: its session is not this logout's to end.
  if (readSessionMarker() !== ended) return;
  recordPendingCsrf(ended);
  const revoke = new AbortController();
  logoutRequests = revoke;
  const budget = setTimeout(() => revoke.abort(), LOGOUT_REVOKE_BUDGET_MS);
  try {
    await revokeRefreshToken(ended, identity, revoke.signal);
  } finally {
    clearTimeout(budget);
    // Nothing of the revoke may land after this point.
    revoke.abort();
  }
  if (!webLocks()) return;
  if (readSessionMarker() !== ended) return;
  // Someone else signed in meanwhile: the cookies are theirs now.
  const current = storedIdentity();
  if (identity !== undefined && current && current !== identity) return;
  const logout = new AbortController();
  logoutRequests = logout;
  const csrfToken = getCookie("whm_csrf");
  try {
    await axios.post(
      `${API_BASE_URL}/auth/logout`,
      {},
      {
        withCredentials: true,
        timeout: LOGOUT_REQUEST_TIMEOUT_MS,
        headers: csrfToken ? { "X-CSRF-Token": csrfToken } : {},
        signal: logout.signal,
      },
    );
    if (readPendingLogout()?.marker === ended) writeStoredValue(LOGOUT_PENDING_KEY, null);
  } catch {
    // The pending record stays: the next sign-in or page load retries it.
  } finally {
    if (logoutRequests === logout) logoutRequests = null;
  }
}

// finishPendingLogout completes, under the lock the caller holds, a logout
// whose tab went away before it got through (and that no sign-in has
// superseded since).
// The queued logout of the same record then finds nothing left to do.
async function finishPendingLogout(): Promise<void> {
  const pending = pendingLogoutApplies();
  if (pending) await sendLogoutRequests(pending.marker);
}


// endIssuedSession revokes and clears what a sign-in or org switch response
// just set, because the session ended while it was in flight. The caller holds
// the lock.
// A newer session begun meanwhile (in another tab) aborts it and stops it
// before its /auth/logout.
async function endIssuedSession(): Promise<void> {
  const marker = readSessionMarker();
  const controller = new AbortController();
  logoutRequests = controller;
  try {
    const csrfToken = getCookie("whm_csrf");
    if (csrfToken) {
      await axios
        .post(
          `${API_BASE_URL}/auth/refresh`,
          { revoke: true },
          {
            withCredentials: true,
            timeout: REVOKE_REQUEST_TIMEOUT_MS,
            headers: { "X-CSRF-Token": csrfToken },
            signal: controller.signal,
          },
        )
        .catch(() => undefined);
    }
    if (controller.signal.aborted || readSessionMarker() !== marker) return;
    const current = getCookie("whm_csrf");
    const pending = pendingLogoutApplies();
    await axios
      .post(
        `${API_BASE_URL}/auth/logout`,
        {},
        {
          withCredentials: true,
          timeout: LOGOUT_REQUEST_TIMEOUT_MS,
          headers: current ? { "X-CSRF-Token": current } : {},
          signal: controller.signal,
        },
      )
      .then(
        () => {
          // This also completed a logout that was still pending.
          if (pending && readPendingLogout()?.marker === pending.marker) {
            writeStoredValue(LOGOUT_PENDING_KEY, null);
          }
        },
        () => undefined,
      );
  } finally {
    if (logoutRequests === controller) logoutRequests = null;
  }
}

// leftCookiesWithoutAnswer tells whether a sign-in or org switch that got no
// answer left cookies of its own in the jar (to be undone under its lock).
function leftCookiesWithoutAnswer(config: AuthAwareRequestConfig): boolean {
  const marker = readSessionMarker();
  const csrf = getCookie("whm_csrf");
  if (marker !== config._issueMarker) {
    // Another session began meanwhile (defensive: every session begins under
    // the lock). Its cookies are intact while the jar holds the CSRF value it
    // began with; otherwise this request's late cookies replaced them.
    const begun = readBegunSession();
    return !(begun && begun.marker === marker && begun.csrf === csrf);
  }
  // Nothing else can change the cookies under this lock: a changed CSRF
  // cookie is this request's.
  return csrf !== config._issueCsrf;
}

function switchNeedsSessionRecovery(error: AxiosError, config?: AuthAwareRequestConfig): boolean {
  if (!config?._releaseSessionLock || !config._switchDispatched ||
      !isOrganizationSwitch(requestPath(config.url))) return false;
  if (!error.response) return true;
  const body = error.response.data as { status?: unknown; error_type?: unknown } | null;
  if (body?.status === "error") {
    if (body.error_type === "switch_not_applied") return false;
    if (body.error_type === "switch_outcome_unknown" ||
        body.error_type === "switch_session_recovery_required") return true;
  }
  // A proxy/server failure after dispatch cannot prove that Redis did not
  // commit. Only the explicit application not-applied result preserves it.
  return error.response.status >= 500;
}

function endUncertainSwitchSession(config: AuthAwareRequestConfig) {
  const marker = config._issueMarker;
  if (marker === undefined || readSessionMarker() !== marker || isEndedMarker(marker) ||
      storedIdentity() !== config._switchIdentity) {
    onSessionChanged();
    return;
  }
  const showed = tabSession;
  markSessionEnding();
  // Record the existing scoped pending logout while still holding the issuing
  // lock, but never await its second lock acquisition here. A following login
  // must finish this cleanup before issuing a new logical session.
  if (!startServerLogout(config._switchIdentity ?? "", sessionIdOf(marker))) return;
  clearLastActivity();
  showReconnectToast("dismiss");
  emitAuthSessionState("expired");
  clearStoredIdentity();
  leaveForLogin(showed);
}

/**
 * Called before the browser leaves for the identity provider. The SSO
 * callback sets no cookie: the callback page exchanges a one-time code for
 * the session (exchangeSsoCode), ordered under the lock like a password
 * sign-in, so nothing needs ordering here. Without Web Locks it refuses: the
 * exchange could not be ordered, and would be refused anyway.
 */
export async function beginSsoSignIn(): Promise<void> {
  if (!webLocks()) throw sessionUnsupportedError();
}

/**
 * Exchanges the one-time code an SSO callback put in the page URL for the
 * session: POST /auth/refresh {"sso_exchange": code}, a session-issuing
 * request with everything a password sign-in has (the lock, an earlier logout
 * first, the new session only on success, the undo of a late or unanswered
 * answer, the timeout). The code is single-use and bound to this browser.
 * A received application 503 requires a fresh sign-in, never a replay of this
 * single-use code (a store error does not prove it remains unspent). This
 * failure path neither issues a new auth identity nor expires the previously
 * settled session. Attempt code/binding/note bookkeeping may change; an earlier
 * pending logout still settles before the exchange, under its existing rules.
 */
export function exchangeSsoCode(code: string) {
  return api.post("/auth/refresh", { sso_exchange: code }, {
    _ssoExchange: true,
  } as AxiosRequestConfig);
}

// The SSO code a callback page took off its URL, kept in this tab until its
// exchange is sent (a reload while the exchange waits for the lock retries
// it). The server honours a code for 60 s.
const SSO_CODE_KEY = "whm_sso_code";
const SSO_CODE_LIFETIME_MS = 60_000;

/** Keeps the callback's code in this tab until its exchange is sent. */
export function keepSsoCode(code: string) {
  try {
    sessionStorage.setItem(SSO_CODE_KEY, JSON.stringify({ code, at: Date.now() }));
  } catch {
    // Without storage a reload just cannot retry.
  }
}

/** The kept SSO code whose exchange was never sent, if it is still valid. */
export function unsentSsoCode(): string | null {
  try {
    const kept = JSON.parse(sessionStorage.getItem(SSO_CODE_KEY) ?? "null");
    if (typeof kept?.code === "string" && Date.now() - Number(kept.at) < SSO_CODE_LIFETIME_MS) {
      return kept.code;
    }
    sessionStorage.removeItem(SSO_CODE_KEY);
  } catch {
    // Nothing kept.
  }
  return null;
}

function dropSsoCode() {
  try {
    sessionStorage.removeItem(SSO_CODE_KEY);
  } catch {
    // Nothing kept.
  }
}

// Set (in this tab) when an SSO exchange is sent, with the session marker of
// that moment, and cleared once its outcome is seen here. A tab that finds it
// after a reload never saw the answer: the exchange's cookies may have landed
// unseen, so the stored identity may not be the jar's session any more. It
// stays (every reload, Back) until a newer session consistent with the jar
// settles it: one another tab began, or a sign-in in this tab.
const SSO_SENT_KEY = "whm_sso_sent";

function noteSsoExchangeSent() {
  try {
    sessionStorage.setItem(SSO_SENT_KEY, JSON.stringify({ marker: readSessionMarker() }));
  } catch {
    // Without storage a reload cannot tell.
  }
}

function clearSsoExchangeSent() {
  try {
    sessionStorage.removeItem(SSO_SENT_KEY);
  } catch {
    // Nothing noted.
  }
}

/**
 * Whether an SSO exchange this tab sent is still unsettled: its answer was
 * never seen here (a reload cut it off). Until it is settled the tab must not
 * show the stored identity, which may not be the jar's session. A newer
 * session, consistent with the jar (its own CSRF value), that began after the
 * exchange was sent settles it, and can be shown.
 */
export function ssoExchangeInterrupted(): boolean {
  let sent: { marker: string | null } | null = null;
  try {
    sent = JSON.parse(sessionStorage.getItem(SSO_SENT_KEY) ?? "null");
  } catch {
    return false;
  }
  if (!sent) return false;
  const marker = readSessionMarker();
  const begun = readBegunSession();
  const newerLive =
    marker !== null &&
    !isEndedMarker(marker) &&
    marker !== sent.marker &&
    begun?.marker === marker &&
    begun.csrf === getCookie("whm_csrf");
  if (newerLive) clearSsoExchangeSent();
  return !newerLive;
}

/** Resolves once a logout started in this tab has finished in the background. */
export function whenLogoutSettled(): Promise<void> {
  return pendingLogout ?? Promise.resolve();
}

// Other same-origin tabs this tab has heard from. Only one whose liveness
// stamp is fresh counts as able to take a logout over: a frozen, discarded or
// crashed tab's stamp goes stale.
const tabId = Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
const otherTabs = new Set<string>();
let tabChannel: BroadcastChannel | null = null;
let tabHeartbeat: ReturnType<typeof setInterval> | null = null;

function beatTab() {
  writeStoredValue(TAB_ALIVE_PREFIX + tabId, String(Date.now()));
}

function startTabHeartbeat() {
  if (!tabChannel) return;
  beatTab();
  if (tabHeartbeat === null) tabHeartbeat = setInterval(beatTab, TAB_HEARTBEAT_MS);
}

function stopTabHeartbeat() {
  if (tabHeartbeat !== null) {
    clearInterval(tabHeartbeat);
    tabHeartbeat = null;
  }
  writeStoredValue(TAB_ALIVE_PREFIX + tabId, null);
}

function anotherTabAlive(): boolean {
  for (const id of otherTabs) {
    const at = readStoredNumber(TAB_ALIVE_PREFIX + id);
    if (at > 0 && Date.now() - at < TAB_ALIVE_WINDOW_MS) return true;
  }
  return false;
}

function announceTab(type: "hello" | "here" | "bye") {
  try {
    tabChannel?.postMessage({ type, id: tabId });
  } catch {
    // Presence is best effort.
  }
}

function startTabPresence() {
  if (typeof BroadcastChannel === "undefined") return;
  try {
    tabChannel = new BroadcastChannel(TAB_CHANNEL_NAME);
  } catch {
    tabChannel = null;
    return;
  }
  tabChannel.onmessage = (event: MessageEvent) => {
    const { type, id } = (event.data ?? {}) as { type?: string; id?: unknown };
    if (typeof id !== "string" || id === tabId) return;
    if (type === "hello") {
      otherTabs.add(id);
      announceTab("here");
    } else if (type === "here") {
      otherTabs.add(id);
    } else if (type === "bye") {
      otherTabs.delete(id);
      takeOverPendingLogout();
    }
  };
  announceTab("hello");
  startTabHeartbeat();
}

// A tab that closed with its logout still pending handed it over: finish it
// here, ordered like any other.
function takeOverPendingLogout() {
  const pending = pendingLogoutApplies();
  // A page without Web Locks takes nothing over: it could only revoke again.
  if (!pending || pendingLogoutMarker === pending.marker || !webLocks()) return;
  ownedLogout = pending.marker;
  pendingLogoutMarker = pending.marker;
  pendingLogout = finishLogout(pending.marker, undefined, true);
}

// The tab closes while a logout is still pending. If another live tab is open
// it takes the logout over and finishes it under the lock. Otherwise this tab
// revokes the session's refresh chain with keepalive, cookie-neutral
// (keep_cookies): its answer may arrive after a newer session set its own
// cookies, and must not clear them. The pending record stays, so the next page
// load clears the old cookies, ordered. (Tabs that all close at the same moment
// each see the others still live and leave it all to the next page load.)
function sendLogoutOnPageHide() {
  announceTab("bye");
  stopTabHeartbeat();
  // Without Web Locks the logout already revoked, cookie-neutral; the
  // clearing is left to a page that holds the lock.
  if (!webLocks()) return;
  const pending = pendingLogoutApplies();
  if (!pending || pending.marker !== ownedLogout || anotherTabAlive()) return;
  const csrfToken = getCookie("whm_csrf");
  if (!csrfToken) return;
  try {
    void fetch(`${API_BASE_URL}/auth/refresh`, {
      method: "POST",
      credentials: "include",
      keepalive: true,
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken },
      body: JSON.stringify({ revoke: true, keep_cookies: true }),
    }).catch(() => undefined);
  } catch {
    // Best effort.
  }
}

function releaseSessionLock(config: unknown) {
  const held = config as AuthAwareRequestConfig | undefined;
  const release = held?._releaseSessionLock;
  if (release) {
    held._releaseSessionLock = undefined;
    release();
  }
}

function isUnsupported(error: unknown): boolean {
  return axios.isAxiosError(error) && error.code === SESSION_UNSUPPORTED_CODE;
}

// endUnsupportedSession ends a session that existed in a browser without Web
// Locks once it would need a refresh (which may not run there): logged out
// cookie-neutral (the refresh chain revoked; the cookie clearing stays pending
// for a page that holds the lock), and the user is sent to the login page,
// which says why a sign-in is refused. Once: a second refresh need finds this
// tab showing no session (startServerLogout then ends nothing) and only leaves.
function endUnsupportedSession() {
  const shown = tabSession;
  if (startServerLogout(storedIdentity(), shown)) clearLastActivity();
  // Then this tab leaves, as on any end: its own session, just ended, or a
  // newer one that replaced it (whose shared state it leaves alone).
  tabSession = shown;
  onSessionChanged();
}

let routeMatcher: RouterMatcher | null = null;

// A page that needs no session: one the router marks requiresAuth: false
// (signing in, registering from an invitation, the SSO callback, the legal
// pages, ...). It is not bound to the stored session at load, so it is never
// sent away when that one ends; once it shows a session of its own (a sign-in
// or exchange done there), it leaves it like any page.
function isOnPublicPage(): boolean {
  try {
    const pathname = window.location.pathname;
    const path =
      (basePath && pathname.startsWith(basePath) ? pathname.slice(basePath.length) : pathname) || "/";
    routeMatcher ??= createRouterMatcher(routes, {});
    return routeMatcher.resolve({ path }, START_LOCATION).meta.requiresAuth === false;
  } catch {
    return false;
  }
}

// expireSession ends the session after a definitive refresh rejection: clear
// the browser identity and send the user to the login page.
function expireSession(expectedMarker: string | null) {
  // Only the session the failing refresh belonged to ends here. If the
  // browser has moved on (or it already ended), this tab just leaves.
  if (readSessionMarker() !== expectedMarker || isEndedMarker(expectedMarker)) {
    onSessionChanged();
    return;
  }
  tabSession = null;
  markSessionEnding();
  endSessionMarker(sessionIdOf(expectedMarker));
  if (hasStoredUser()) {
    // Explained on the login page ("Your session expired").
    recordSignOutReason({ reason: "expired" });
  }
  clearSessionTiming();
  clearLastActivity();
  showReconnectToast("dismiss");
  emitAuthSessionState("expired");

  clearStoredIdentity();
  if (!isOnPublicPage()) {
    window.location.href = basePath + "/login";
  }
}

// clearStoredIdentity removes the signed-in identity from browser storage.
function clearStoredIdentity() {
  clearBrowserOrganizationIdentity();
  for (const key of [
    "user",
    "auth_token",
    "refresh_token",
    "break_started_at",
  ]) {
    let removed = false;
    try {
      localStorage.removeItem(key);
      removed = localStorage.getItem(key) === null;
    } catch {
      // Fall through to a non-sensitive tombstone where writes work.
    }
    if (!removed) {
      try {
        localStorage.setItem(key, "");
      } catch {
        // A denied storage backend must not skip redirect/isolation.
      }
    }
  }
}

// startServerLogout ends the logical session now. Its server side follows in
// the background (finishLogout: revoke, then /auth/logout, ordered on the
// lock), and a pending record keeps it until it has got through.
function startServerLogout(identity: string, expected: string | null): boolean {
  tabSession = null;
  const ended = expected === null ? null : endSessionMarker(expected);
  if (ended === null) {
    // This tab shows no session, or the browser has moved on to another one:
    // nothing of ours to end, and the shared state is not ours to clear.
    if (proactiveRefreshTimer !== null) {
      clearTimeout(proactiveRefreshTimer);
      proactiveRefreshTimer = null;
    }
    return false;
  }
  clearSessionTiming();
  // (A key earlier builds kept; nothing reads it any more.)
  writeStoredValue(SESSION_CSRF_KEY, null);
  ownedLogout = ended;
  writeStoredValue(
    LOGOUT_PENDING_KEY,
    JSON.stringify({ marker: ended, csrf: getCookie("whm_csrf") }),
  );
  pendingLogoutMarker = ended;
  pendingLogout = finishLogout(ended, identity, readPendingLogout()?.marker === ended);
  return true;
}

/**
 * Ends the server session without waiting for it, ordered like a logout. The
 * caller clears the identity and leaves for the login page at once, so a slow
 * or hung backend never keeps patient data on screen. If the page goes away
 * first, an open tab takes the logout over (or the last tab sends it), and
 * the next sign-in or page load finishes whatever is still pending.
 */
export function endSessionInBackground() {
  sessionEnding = true;
  startServerLogout(storedIdentity(), tabSession);
}

// endIdleSession ends a session left idle past the organization's policy:
// the server session in the background, the identity at once. It runs
// before any refresh could extend such a session, and before the app renders
// on load (leave=false: the router then opens the login page).
function endIdleSession(leave: boolean) {
  // A tab that no longer shows the current session (another sign-in replaced
  // it) only leaves: the clock, the identity and the session are not its own.
  if (!isTabSessionCurrent()) {
    leaveReplacedSession();
    return;
  }
  markSessionEnding();
  recordSignOutReason({ reason: "idle", minutes: readSessionIdlePolicy() });
  clearLastActivity();
  endSessionInBackground();
  clearStoredIdentity();
  if (leave && !isOnPublicPage()) {
    window.location.href = basePath + "/login";
  }
}

// idleSessionEnded ends the session if it is idle past the policy (say, a
// laptop that slept through the deadline) and reports whether it did.
function idleSessionEnded(): boolean {
  if (!hasStoredUser() || !isIdleSessionStale()) return false;
  endIdleSession(true);
  return true;
}

/**
 * Re-reads the organization's inactivity policy (it rides on GET /me). A
 * failure keeps the last-known policy and never refreshes or ends the session.
 */
export async function checkSessionIdlePolicy(): Promise<void> {
  const config: AxiosRequestConfig & { skipAuthRefresh: boolean } = {
    skipAuthRefresh: true,
  };
  try {
    await api.get("/me", config);
  } catch {
    // Keep the last-known policy.
  }
}

/**
 * Refreshes the session now, e.g. for "Stay signed in". It shares the
 * cross-tab lock and retry policy of the 401 path, and a definitive rejection
 * ends the session.
 */
export async function keepSessionAlive(): Promise<void> {
  // "Stay signed in" or a delayed check never extends an ending session, or
  // one that ended in another tab (a tab still showing it leaves it).
  const marker = readSessionMarker();
  try {
    await ensureFreshSession(readAuthEpoch(), marker);
  } catch (refreshError) {
    if (isSessionChange(refreshError)) {
      onSessionChanged();
    } else if (isDefinitiveRefreshFailure(refreshError)) {
      expireSession(marker);
    }
    throw refreshError;
  }
}

async function runProactiveRefresh() {
  proactiveRefreshTimer = null;
  // Hidden tabs wait for visibilitychange; a visible tab keeps the session warm.
  if (document.visibilityState === "hidden") return;
  // No identity stored any more (an end in another tab clears it): a tab
  // still showing that session leaves it.
  if (!hasStoredUser()) {
    onSessionChanged();
    return;
  }
  // Never extend a session that is ending, or already idle past the policy.
  if (sessionEnding || idleSessionEnded()) return;
  const expiresAt = readStoredNumber(AUTH_EXPIRES_AT_KEY);
  if (!expiresAt) return;
  if (expiresAt - Date.now() > PROACTIVE_REFRESH_LEAD_MS) {
    // Another tab already extended the session.
    scheduleProactiveRefresh();
    return;
  }
  const marker = readSessionMarker();
  try {
    await ensureFreshSession(readAuthEpoch(), marker);
  } catch (refreshError) {
    if (isSessionChange(refreshError)) {
      onSessionChanged();
    } else if (isDefinitiveRefreshFailure(refreshError)) {
      expireSession(marker);
    } else if (isUnsupported(refreshError)) {
      endUnsupportedSession();
    }
    // Transient failures keep the user signed in; the next 401 retries.
  }
}

function scheduleProactiveRefresh() {
  if (proactiveRefreshTimer !== null) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }
  const expiresAt = readStoredNumber(AUTH_EXPIRES_AT_KEY);
  if (!expiresAt) return;
  const wait = Math.min(
    Math.max(0, expiresAt - PROACTIVE_REFRESH_LEAD_MS - Date.now()),
    MAX_TIMER_DELAY_MS,
  );
  proactiveRefreshTimer = setTimeout(() => {
    void runProactiveRefresh();
  }, wait);
}

// The window and document listeners this module instance added.
const detachers: Array<() => void> = [];

function onWindow<K extends keyof WindowEventMap>(
  type: K,
  listener: (event: WindowEventMap[K]) => void,
) {
  window.addEventListener(type, listener);
  detachers.push(() => window.removeEventListener(type, listener));
}

function onDocument<K extends keyof DocumentEventMap>(
  type: K,
  listener: (event: DocumentEventMap[K]) => void,
) {
  document.addEventListener(type, listener);
  detachers.push(() => document.removeEventListener(type, listener));
}

/**
 * Test-only: detaches this module instance (its window and document
 * listeners, tab channel, heartbeat and timers), so that only an instance
 * loaded later reacts to page events.
 */
export function disposeForTests() {
  for (const detach of detachers.splice(0)) detach();
  tabChannel?.close();
  tabChannel = null;
  if (tabHeartbeat !== null) {
    clearInterval(tabHeartbeat);
    tabHeartbeat = null;
  }
  if (proactiveRefreshTimer !== null) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }
}

if (typeof window !== "undefined" && typeof document !== "undefined") {
  onDocument("visibilitychange", () => {
    if (document.visibilityState !== "visible") return;
    const expiresAt = readStoredNumber(AUTH_EXPIRES_AT_KEY);
    if (expiresAt && expiresAt - Date.now() <= PROACTIVE_REFRESH_LEAD_MS) {
      void runProactiveRefresh();
    } else {
      scheduleProactiveRefresh();
    }
  });
  onWindow("storage", (event) => {
    if (event.key === AUTH_EXPIRES_AT_KEY) scheduleProactiveRefresh();
    // Another tab began a session: this tab's logout requests still in flight
    // must not clear its cookies.
    if (event.key === SESSION_MARKER_KEY && event.newValue && !isEndedMarker(event.newValue)) {
      logoutRequests?.abort();
    }
    // Another tab's session change: this tab's refresh in flight is stale, and
    // its late Set-Cookie must not land after that change.
    if (event.key === SESSION_MARKER_KEY) refreshAbort?.abort();
    // Another tab ended this tab's session (say, a menu sign-out) or began a
    // new one: leave at once, without waiting for a 401.
    if (event.key === SESSION_MARKER_KEY) onSessionChanged();
  });
  onWindow("pagehide", sendLogoutOnPageHide);
  onWindow("pageshow", (event) => {
    // Back from the back/forward cache: say hello again.
    if (event.persisted) {
      announceTab("hello");
      startTabHeartbeat();
      // The restored page may still show a session that ended or was replaced
      // meanwhile, or one it already left (every leave sets sessionEnding, and
      // a reload starts clear): reload it, so it opens on what the browser
      // holds now. A public page shows no session: it stays (say, a form).
      if (
        !isOnPublicPage() &&
        (sessionEnding || (tabSession !== null && !isTabSessionCurrent()))
      ) {
        window.location.reload();
      }
    }
  });
  startTabPresence();
  scheduleProactiveRefresh();
  // Probe the lock manager once at load: one that refuses (a sandboxed or
  // opaque-origin page) counts as none before a first sign-in clears anything.
  try {
    const locks = webLocks();
    if (locks && typeof locks.query === "function") {
      lockProbe = locks.query().then(
        () => undefined,
        () => {
          locksUnusable = true;
        },
      );
    }
  } catch {
    locksUnusable = true;
  }

  const pendingAtLoad = pendingLogoutApplies();
  if (pendingAtLoad) {
    // A logout that never got through (the tab closed first, or it failed):
    // finish it now, ordered like any other, while the jar is still the
    // ended session's (its CSRF value, or none: then only /auth/logout goes
    // out). Anything else in the jar is not that session's: drop the record.
    // A page without Web Locks leaves it to one that holds the lock (it
    // could only revoke again).
    const csrf = getCookie("whm_csrf");
    if (csrf !== null && csrf !== pendingAtLoad.csrf) {
      writeStoredValue(LOGOUT_PENDING_KEY, null);
    } else if (webLocks()) {
      ownedLogout = pendingAtLoad.marker;
      pendingLogoutMarker = pendingAtLoad.marker;
      pendingLogout = finishLogout(pendingAtLoad.marker, undefined, true);
    }
  }

  // An identity stored under an ended session belongs to that session (its
  // tab never got to clear it): drop it, so the app opens on the login page.
  if (isEndedMarker(readSessionMarker()) && hasStoredUser()) {
    clearLastActivity();
    clearStoredIdentity();
  }
  // A restored identity shows the current session; a public page shows none
  // (say, an invitation being accepted while another account is stored).
  if (hasStoredUser() && !isEndedMarker(readSessionMarker()) && !isOnPublicPage()) {
    tabSession = readSessionId();
  }

  // A session left idle past the organization's policy (a tab reopened the
  // next morning, a reload or restored session on a shared PC) ends here,
  // before the router or any view renders: with the identity gone, the app
  // opens on the login page, which explains why.
  if (hasStoredUser() && isIdleSessionStale()) {
    // A public page (an SSO callback page too: its exchange then goes after
    // this logout) ends it as the browser's, and stays where it is.
    if (isOnPublicPage()) tabSession = readSessionId();
    endIdleSession(false);
  }
}

// Request interceptor to add CSRF token and organization header
api.interceptors.request.use(
  (config: InternalAxiosRequestConfig) => {
    // Stamp the auth epoch and logical session the request is sent under.
    (config as AuthAwareRequestConfig)._authEpoch = readAuthEpoch();
    (config as AuthAwareRequestConfig)._sessionMarker = readSessionMarker();
    // A tab still showing a session that a newer sign-in replaced never
    // sends that session's work with the newer session's cookies.
    if (
      tabSession !== null &&
      readSessionId() !== tabSession &&
      !isSessionlessAuthEndpoint(config.url)
    ) {
      leaveReplacedSession();
      return Promise.reject(sessionChangedError(config));
    }
    // While a session ends nothing may use its cookies, except signing in.
    if (sessionEnding && !isSessionlessAuthEndpoint(config.url)) {
      return Promise.reject(sessionEndingError(config));
    }
    const path = requestPath(config.url);
    if (path === "/auth/logout") {
      // The user leaves at once: this request answers immediately. The
      // server-side logout (revoke, then /auth/logout) runs in the background,
      // ordered after any refresh in flight, and every refresh queued or
      // backing off for this session stops (the marker changed).
      if (startServerLogout(storedIdentity(), tabSession)) clearLastActivity();
      config.adapter = async (requestConfig) => ({
        data: { data: { status: "logged_out" } },
        status: 200,
        statusText: "OK",
        headers: {},
        config: requestConfig,
      });
      return config;
    }
    // Add CSRF token on mutating requests (cookie-based auth sends cookies automatically)
    const method = (config.method || "").toUpperCase();
    if (
      method === "POST" ||
      method === "PUT" ||
      method === "DELETE" ||
      method === "PATCH"
    ) {
      const csrfToken = getCookie("whm_csrf");
      if (csrfToken) {
        config.headers["X-CSRF-Token"] = csrfToken;
      }
    }
    // Add organization override header for org switching
    const selectedOrgId = readSelectedOrganizationId();
    const organizationId = resolveOrganizationHeader(
      config.headers.get("X-Organization-ID"),
      selectedOrgId,
    );
    if (organizationId) {
      config.headers.set("X-Organization-ID", organizationId);
    }
    if (path === "/me") {
      (config as AuthAwareRequestConfig)._sessionScope = sessionScope();
    }
    if (isSessionIssuing(config as AuthAwareRequestConfig, path)) {
      // A sign-in or org switch waits (bounded; the caller shows its normal
      // busy state) until refresh work and an earlier logout have settled,
      // and keeps refreshes and logouts out until its own response (at most
      // SESSION_ISSUING_TIMEOUT_MS). A successful sign-in starts a new logical
      // session; an org switch keeps it (the auth epoch already retires
      // refreshes issued before the switch).
      const held = config as AuthAwareRequestConfig;
      // A retried request (401, refresh, replay) starts afresh.
      held._issueMarker = undefined;
      held._issueCsrf = undefined;
      held._switchDispatched = false;
      // Without Web Locks nothing keeps this tab's new session apart from
      // another tab's refresh or logout: refuse (outdated browser, or plain
      // HTTP off localhost).
      if (!webLocks()) return Promise.reject(sessionUnsupportedError(config));
      const startMarker = readSessionMarker();
      return holdRefreshLock().then(async (release) => {
        // The lock stayed busy: refuse ("please try again"), never send unlocked.
        if (!release) throw sessionBusyError(config);
        // The lock manager refused meanwhile: as without Web Locks.
        if (!webLocks()) {
          release();
          throw sessionUnsupportedError(config);
        }
        // The backstop, if no answer ever comes: abort the request and let go
        // of the lock only once it has settled (its own error path releases
        // it, after undoing whatever it set), never with it still in flight.
        const transport = new AbortController();
        const outer = config.signal;
        if (outer?.aborted) transport.abort();
        else outer?.addEventListener?.("abort", () => transport.abort());
        config.signal = transport.signal;
        const backstop = setTimeout(() => transport.abort(), SESSION_LOCK_BACKSTOP_MS);
        held._releaseSessionLock = () => {
          clearTimeout(backstop);
          release();
        };
        if (isOrganizationSwitch(path)) {
          // An org switch belongs to the session it was asked in: never after
          // that session ended or another began.
          const marker = readSessionMarker();
          if (marker !== startMarker || isEndedMarker(marker)) {
            releaseSessionLock(held);
            throw sessionChangedError(config);
          }
        } else {
          // A logout whose tab went away is completed first. If it still does
          // not get through, no new session starts over the old one's
          // cookies: refuse, and keep it pending.
          await finishPendingLogout();
          if (pendingLogoutApplies()) {
            releaseSessionLock(held);
            throw sessionBusyError(config);
          }
        }
        held._issueMarker = readSessionMarker();
        held._issueCsrf = getCookie("whm_csrf");
        if (isOrganizationSwitch(path)) {
          held._switchIdentity = storedIdentity();
          // Mark only entry into the real adapter, not cancellation while
          // waiting for the lock. Preserve the original adapter on a 401 retry.
          held._switchAdapter ??= axios.getAdapter(config.adapter ?? api.defaults.adapter);
          config.adapter = async (requestConfig) => {
            if (requestConfig.signal?.aborted) {
              throw new AxiosError("canceled", AxiosError.ERR_CANCELED, requestConfig);
            }
            (requestConfig as AuthAwareRequestConfig)._switchDispatched = true;
            return held._switchAdapter!(requestConfig);
          };
        }
        if (!config.timeout || config.timeout > SESSION_ISSUING_TIMEOUT_MS) {
          config.timeout = SESSION_ISSUING_TIMEOUT_MS;
        }
        if (method !== "GET") {
          const csrfToken = getCookie("whm_csrf");
          if (csrfToken) {
            config.headers.set("X-CSRF-Token", csrfToken);
          } else {
            config.headers.delete("X-CSRF-Token");
          }
        }
        // The exchange goes out now: its code is spent, never to be resent,
        // and until its outcome is seen here a reload must not guess it.
        if (held._ssoExchange) {
          dropSsoCode();
          noteSsoExchangeSent();
        }
        return config;
      });
    }
    return config;
  },
  (error: AxiosError) => {
    return Promise.reject(error);
  },
);

// Response interceptor: session bookkeeping and 401 handling
api.interceptors.response.use(
  async (response: AxiosResponse) => {
    const config = response.config as AuthAwareRequestConfig | undefined;
    const path = requestPath(config?.url);
    const issued = isSessionIssuing(config, path);
    if (issued) {
      // The exchange's answer is seen here: a reload from now on need not guess.
      if (config?._ssoExchange) clearSsoExchangeSent();
      try {
        if (readSessionMarker() !== config?._issueMarker) {
          // The session ended while this was in flight: what it just issued
          // must not outlive that end. Revoke it and clear the jar (the lock
          // is still held), and report no sign-in.
          await endIssuedSession();
          throw sessionChangedError(config);
        }
        if (isOrganizationSwitch(path)) {
          // Same session, new cookies: keep its begun record current.
          noteBegunCsrf(readSessionMarker());
        } else {
          beginSessionMarker();
          if (config?._ssoExchange) {
            // The SSO account's identity and organization are not the ones a
            // previous session stored (a password sign-in clears them before
            // it is sent; the callback page has nothing to clear until now).
            // The callback page applies the answer's user next, as a sign-in
            // does, with no request in between.
            clearBrowserOrganizationIdentity();
            writeStoredValue("user", null);
          }
        }
        sessionEnding = false;
        onSessionIssued(response.data?.data?.expires_in);
      } finally {
        releaseSessionLock(config);
      }
    }
    // The inactivity policy rides on GET /me. It applies only if the session
    // and identity the request was sent under are still current, and it is
    // cached for the user and organization the response names.
    if (path === "/me") {
      const me = response.data?.data;
      const scope = config?._sessionScope;
      if (me?.id && scope === sessionScope()) {
        rememberSessionIdlePolicy(
          response.headers?.["x-session-idle-minutes"],
          `${me.id}|${me.organization_id ?? ""}`,
        );
      }
    }
    // A sign-in (password, registration, or an SSO code exchange) starts the
    // inactivity clock and retires the "why you were signed out" banner.
    if (issued && !isOrganizationSwitch(path)) {
      clearSignOutReason();
      recordActivity();
    }
    return response;
  },
  async (error: AxiosError) => {
    // A sign-in or org switch that got no answer (timeout, network, abort) may
    // still have had its cookies stored: if the CSRF cookie changed, undo what
    // it issued before letting go of the lock.
    const issuing = error.config as AuthAwareRequestConfig | undefined;
    try {
      if (
        issuing?._releaseSessionLock &&
        issuing._issueMarker !== undefined &&
        !error.response &&
        leftCookiesWithoutAnswer(issuing)
      ) {
        await endIssuedSession();
      }
      // Its outcome is seen (refused, or undone just now).
      if (issuing?._ssoExchange) clearSsoExchangeSent();
    } finally {
      // First, so a 401 on an org switch can refresh without waiting on
      // itself; and even if the undo threw.
      try {
        if (switchNeedsSessionRecovery(error, issuing)) {
          endUncertainSwitchSession(issuing!);
        }
      } finally {
        releaseSessionLock(error.config);
      }
    }
    const originalRequest = error.config as AuthAwareRequestConfig | undefined;

    if (
      error.response?.status !== 401 ||
      !originalRequest ||
      originalRequest._retry ||
      originalRequest.skipAuthRefresh ||
      isSessionlessAuthEndpoint(originalRequest.url)
    ) {
      return Promise.reject(error);
    }

    // No refresh may race the revoke of a session that is ending.
    if (sessionEnding) {
      return Promise.reject(error);
    }

    // A session idle past the policy ends rather than refreshing.
    if (idleSessionEnded()) {
      return Promise.reject(error);
    }

    // One retry per request: either after a refresh, or straight away when
    // the cookie was already rotated after this request was sent. Never for
    // a request sent under another (or an ended) session.
    originalRequest._retry = true;
    const requestMarker = originalRequest._sessionMarker ?? null;
    try {
      await ensureFreshSession(originalRequest._authEpoch ?? 0, requestMarker);
    } catch (refreshError) {
      if (isSessionChange(refreshError)) {
        onSessionChanged();
      } else if (isDefinitiveRefreshFailure(refreshError)) {
        expireSession(requestMarker);
      } else if (isUnsupported(refreshError)) {
        endUnsupportedSession();
      }
      // Transient failure: keep the user signed in and surface the 401.
      return Promise.reject(error);
    }
    if (readSessionMarker() !== requestMarker) return Promise.reject(error);
    return api(originalRequest);
  },
);

// API service methods
export const authService = {
  getWSToken: () => api.get("/auth/ws-token"),
};

export const usersService = {
  list: (params?: {
    search?: string;
    page?: number;
    limit?: number;
    role_id?: string;
    online_only?: boolean;
  }, signal?: AbortSignal) => api.get("/users", { params, signal }),
  get: (id: string) => api.get(`/users/${id}`),
  create: (data: {
    email: string;
    password: string;
    full_name: string;
    role_id?: string;
  }) => api.post("/users", data),
  update: (
    id: string,
    data: {
      email?: string;
      password?: string;
      full_name?: string;
      role_id?: string;
      is_active?: boolean;
    },
  ) => api.put(`/users/${id}`, data),
  delete: (id: string) => api.delete(`/users/${id}`),
  me: () => api.get("/me"),
  updateSettings: (data: {
    email_notifications: boolean;
    new_message_alerts: boolean;
    campaign_updates: boolean;
  }) => api.put("/me/settings", data),
  changePassword: (data: { current_password: string; new_password: string }) =>
    api.put("/me/password", data),
  updateAvailability: (isAvailable: boolean) =>
    api.put("/me/availability", { is_available: isAvailable }),
  listMyOrganizations: (signal?: AbortSignal) => api.get("/me/organizations", { signal }),
};

export const apiKeysService = {
  list: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{ api_keys: any[]; total?: number }>("/api-keys", { params }),
  get: (id: string) => api.get(`/api-keys/${id}`),
  create: (data: { name: string; expires_at?: string }) =>
    api.post("/api-keys", data),
  update: (id: string, data: { is_active?: boolean }) =>
    api.put(`/api-keys/${id}`, data),
  delete: (id: string) => api.delete(`/api-keys/${id}`),
};

export const accountsService = {
  list: () => api.get("/accounts"),
};

// This projection is deliberately smaller than the protected review preview.
// Contact and channel list responses may expose it without disclosing another
// candidate's identity or any authenticated selector values.
export interface ContactIdentityReviewEffectiveState {
  known: boolean;
  ai_allowed: boolean;
  blocked: boolean;
  open_hold_count: number;
  latest_hold_id?: string;
  latest_generation?: number;
  reason: string;
}

export function effectiveAIIsAllowed(
  state: ContactIdentityReviewEffectiveState | null | undefined,
): boolean {
  return state?.known === true
    && state.ai_allowed === true
    && state.blocked === false;
}

export interface ContactIdentityReviewCandidate {
  contact_id: string;
  selector_reasons: number;
}

export interface ContactIdentityReviewSnapshot {
  hold_id: string;
  whatsapp_account_id: string;
  onboarding_cycle: number;
  protocol_version: number;
  supported: boolean;
  principal_generation: number;
  disposition: string;
  version: number;
  member_count: number;
  member_digest: string;
  candidates: ContactIdentityReviewCandidate[];
  decision_target_contact_id?: string;
  decision_request_id?: string;
  decision_chain_digest?: string;
}

export interface ContactIdentityReviewPreview {
  snapshot: ContactIdentityReviewSnapshot;
  chain_digest: string;
  union_candidates: ContactIdentityReviewCandidate[];
  open_generations: number[];
  // Current members a decision may target. A member that already belongs to
  // a different WhatsApp user is listed as a candidate but never here.
  routable_contact_ids?: string[];
  // Every open generation a decision on this hold closes.
  open_hold_ids?: string[];
}

export interface ContactIdentityReviewDecisionRequest {
  request_id: string;
  hold_id: string;
  target_contact_id: string;
  expected_version: number;
  expected_member_digest: string;
  expected_chain_digest: string;
  request_digest: string;
}

export interface ContactIdentityReviewDecisionResult {
  snapshot: ContactIdentityReviewSnapshot;
  superseded_generations: number[];
  idempotent_replay: boolean;
}

export interface StagedIdentityReviewItem {
  id: string;
  hold_id: string;
  protocol_version: number;
  revision?: string;
  status: string;
  message_type: string;
  media_status?: string;
  received_at: string;
  // True when no decision can resolve the receipt (its hold has no unique
  // direct-BSUID owner); it closes only when the account is onboarded again.
  read_only?: boolean;
}

export interface StagedIdentityReviewDetail extends StagedIdentityReviewItem {
  content: string;
  media_mime_type?: string;
  media_filename?: string;
  media_available: boolean;
}

export const contactsService = {
  list: (params?: {
    search?: string;
    page?: number;
    limit?: number;
    tags?: string;
    // Inbox filters. unread is team-shared read state (incoming messages not
    // yet read, or a manual "Mark as unread"); assigned=me also counts an
    // active transfer to the current user.
    unread?: "true";
    assigned?: "me" | "unassigned";
  }, signal?: AbortSignal) => api.get("/contacts", { params, signal }),
  get: (id: string) => api.get(`/contacts/${id}`),
  create: (data: any) => api.post("/contacts", data),
  update: (id: string, data: any) => api.put(`/contacts/${id}`, data),
  delete: (id: string) => api.delete(`/contacts/${id}`),
  assign: (id: string, userId: string | null) =>
    api.put(`/contacts/${id}/assign`, { user_id: userId }),
  updateTags: (id: string, tags: string[]) =>
    api.put(`/contacts/${id}/tags`, { tags }),
  getSessionData: (id: string) => api.get(`/contacts/${id}/session-data`),
  getIdentityReviewState: (id: string, signal?: AbortSignal) =>
    api.get<ContactIdentityReviewEffectiveState>(
      `/contacts/${encodeURIComponent(id)}/identity-review`,
      { signal },
    ),
  previewIdentityReview: (id: string, signal?: AbortSignal) =>
    api.get<ContactIdentityReviewPreview>(
      `/contacts/${encodeURIComponent(id)}/identity-review/preview`,
      { signal },
    ),
  decideIdentityReview: (id: string, data: ContactIdentityReviewDecisionRequest) =>
    api.post<ContactIdentityReviewDecisionResult>(
      `/contacts/${encodeURIComponent(id)}/identity-review/decisions`,
      data,
    ),
  listStagedIdentityReviews: (
    params?: { page?: number; limit?: number },
    signal?: AbortSignal,
  ) => api.get<{ reviews: StagedIdentityReviewItem[]; total: number; read_only_total?: number }>(
    "/identity-reviews/staged",
    { params, signal },
  ),
  // Held copies of the given holds, oldest first. hold_id repeats once per hold.
  listStagedIdentityReviewsForHolds: (holdIds: string[], limit: number, signal?: AbortSignal, page = 1) => {
    const params = new URLSearchParams();
    for (const holdId of holdIds) params.append("hold_id", holdId);
    params.set("order", "oldest");
    params.set("page", String(page));
    params.set("limit", String(limit));
    return api.get<{ reviews: StagedIdentityReviewItem[]; total: number; read_only_total?: number }>(
      "/identity-reviews/staged",
      { params, signal },
    );
  },
  getStagedIdentityReview: (id: string, signal?: AbortSignal) =>
    api.get<StagedIdentityReviewDetail>(
      `/identity-reviews/staged/${encodeURIComponent(id)}`,
      { signal },
    ),
  getStagedIdentityReviewMedia: (id: string, revision: string, signal?: AbortSignal) =>
    api.get<Blob>(
      `/identity-reviews/staged/${encodeURIComponent(id)}/media/${encodeURIComponent(String(revision))}`,
      { responseType: "blob", signal },
    ),
  markRead: (id: string, lastVisibleMessageId: string, organizationId: string) => {
    const explicitOrganizationId = organizationId.trim();
    if (!explicitOrganizationId) {
      throw new Error("Organization is required to mark a chat conversation read");
    }
    return api.post(
      `/contacts/${encodeURIComponent(id)}/mark-read`,
      { last_visible_message_id: lastVisibleMessageId },
      { headers: { "X-Organization-ID": explicitOrganizationId } },
    );
  },
  // Manual inbox actions share the mark-read endpoint. "read" resolves the
  // latest incoming message on the server; "unread" sets a team-shared flag
  // and never sends anything to WhatsApp. The automatic markRead body above is
  // deliberately left unchanged.
  // People a contact can be assigned to, for callers without users:read:
  // the organization's members (organizations:read), membership-aware like
  // the server's assignee check.
  assignableMembers: (signal?: AbortSignal) =>
    api.get<{
      members: Array<{
        user_id: string;
        full_name?: string | null;
        email?: string | null;
        is_active?: boolean | null;
        role_name?: string | null;
      }>;
      total: number;
    }>("/organizations/members", { params: { page: 1, limit: 100 }, signal }),
  markManualReadState: (id: string, mark: "read" | "unread") =>
    api.post<{ status: string; cursor_synced: boolean; manual_unread: boolean }>(
      `/contacts/${encodeURIComponent(id)}/mark-read`,
      { mark },
    ),
};

// Generic Import/Export Service
export interface ExportColumn {
  key: string;
  label: string;
}

export interface ExportConfig {
  table: string;
  columns: ExportColumn[];
  default_columns: string[];
}

export interface ImportConfig {
  table: string;
  required_columns: ExportColumn[];
  optional_columns: ExportColumn[];
  unique_column: string;
}

export interface ImportResult {
  created: number;
  updated: number;
  skipped: number;
  errors: number;
  messages: string[];
}

export const dataService = {
  // Get export configuration for a table
  getExportConfig: (table: string) =>
    api.get<ExportConfig>(`/export/${table}/config`),

  // Get import configuration for a table
  getImportConfig: (table: string) =>
    api.get<ImportConfig>(`/import/${table}/config`),

  // Export data - returns CSV blob
  exportData: async (
    table: string,
    columns?: string[],
    filters?: Record<string, string>,
  ) => {
    const response = await api.post(
      "/export",
      { table, columns, filters },
      {
        responseType: "blob",
      },
    );
    return response;
  },

  // Import data from CSV file
  importData: (
    table: string,
    file: File,
    updateOnDuplicate?: boolean,
    columnMapping?: Record<string, string>,
  ) => {
    const formData = new FormData();
    formData.append("file", file);
    formData.append("table", table);
    if (updateOnDuplicate) {
      formData.append("update_on_duplicate", "true");
    }
    if (columnMapping) {
      formData.append("column_mapping", JSON.stringify(columnMapping));
    }
    return api.post<ImportResult>("/import", formData, {
      headers: { "Content-Type": "multipart/form-data" },
    });
  },
};

// A chat media download that receives no bytes for this long is abandoned.
export const MEDIA_STALL_TIMEOUT_MS = 30000;

export const messagesService = {
  list: (
    contactId: string,
    params?: {
      page?: number;
      limit?: number;
      before_id?: string;
      account?: string;
    },
    signal?: AbortSignal,
  ) => api.get(`/contacts/${contactId}/messages`, {
    // The legacy endpoint historically marked the whole contact read on GET.
    // The current Chat UI owns an exact visible-message cursor instead, so
    // every transcript fetch must remain side-effect free.
    params: { ...params, acknowledge: false },
    signal,
  }),
  send: (
    contactId: string,
    data: {
      type: string;
      content: any;
      reply_to_message_id?: string;
      whatsapp_account?: string;
      // Interactive payload. Mirrors backend InteractiveContent.
      interactive?: {
        type: "button" | "cta_url" | "list" | "voice_call" | "flow";
        body: string;
        buttons?: Array<{ id: string; title: string }>;
        button_text?: string;
        url?: string;
        // voice_call only
        display_text?: string;
        ttl_minutes?: number;
        // flow only
        flow_id?: string;
        first_screen?: string;
        header?: string;
      };
      // Re-send a stored message of this organization to contactId. The
      // server derives type and content; type/content are ignored.
      forward_of_message_id?: string;
    },
  ) => api.post(`/contacts/${contactId}/messages`, data),
  sendTemplate: (
    contactId: string,
    data: {
      template_name: string;
      template_params?: Record<string, string>;
      header_params?: Record<string, string>;
      button_params?: Record<string, string>;
      account_name?: string;
    },
    headerFile?: File,
  ) => {
    if (headerFile) {
      const formData = new FormData();
      formData.append("contact_id", contactId);
      formData.append("template_name", data.template_name);
      if (data.template_params) {
        formData.append(
          "template_params",
          JSON.stringify(data.template_params),
        );
      }
      if (data.header_params) {
        formData.append("header_params", JSON.stringify(data.header_params));
      }
      if (data.button_params) {
        formData.append("button_params", JSON.stringify(data.button_params));
      }
      if (data.account_name) {
        formData.append("account_name", data.account_name);
      }
      formData.append("header_file", headerFile);
      return api.post("/messages/template", formData, {
        headers: { "Content-Type": "multipart/form-data" },
      });
    }
    return api.post("/messages/template", { contact_id: contactId, ...data });
  },
  sendReaction: (contactId: string, messageId: string, emoji: string) =>
    api.post(`/contacts/${contactId}/messages/${messageId}/reaction`, {
      emoji,
    }),
  // One file per request: the WhatsApp Cloud API carries a single media
  // object per message. Goes through the shared client so CSRF, workspace and
  // token-refresh handling match every other call; uploads of up to 15 MB on
  // a slow link take minutes, so there is no fixed timeout.
  sendMedia: (
    data: {
      file: File;
      contact_id: string;
      type: "image" | "video" | "audio" | "document";
      caption?: string;
      whatsapp_account?: string;
      reply_to_message_id?: string;
    },
    options: {
      signal?: AbortSignal;
      onUploadProgress?: (loaded: number, total: number | undefined) => void;
    } = {},
  ) => {
    const formData = new FormData();
    formData.append("file", data.file);
    formData.append("contact_id", data.contact_id);
    formData.append("type", data.type);
    if (data.caption) formData.append("caption", data.caption);
    if (data.whatsapp_account) {
      formData.append("whatsapp_account", data.whatsapp_account);
    }
    if (data.reply_to_message_id) {
      formData.append("reply_to_message_id", data.reply_to_message_id);
    }
    return api.post("/messages/media", formData, {
      headers: { "Content-Type": "multipart/form-data" },
      timeout: 0,
      signal: options.signal,
      onUploadProgress: (event) =>
        options.onUploadProgress?.(event.loaded, event.total),
    });
  },
  // Native <img>/<video>/<audio>/<a> requests carry the auth cookie but not the
  // X-Organization-ID header, so the server would resolve them against the
  // login (default) workspace. Chat media is fetched through this client
  // instead, pinned to the workspace its transcript was loaded from.
  getMedia: (
    messageId: string,
    organizationId: string,
    options: {
      signal?: AbortSignal;
      onProgress?: (loaded: number, total: number | undefined) => void;
    } = {},
  ) => {
    const explicitOrganizationId = organizationId.trim();
    if (!explicitOrganizationId) {
      throw new Error("Organization is required to load chat media");
    }
    const { signal, onProgress } = options;
    // WhatsApp media can be up to 16 MB, which takes minutes on a slow link,
    // so a download fails only when it stops making progress rather than
    // after a fixed total time.
    const controller = new AbortController();
    const forwardAbort = () => controller.abort(signal?.reason);
    if (signal?.aborted) forwardAbort();
    else signal?.addEventListener("abort", forwardAbort, { once: true });
    let stalled = false;
    let watchdog: ReturnType<typeof setTimeout> | undefined;
    const restartWatchdog = () => {
      clearTimeout(watchdog);
      watchdog = setTimeout(() => {
        stalled = true;
        controller.abort();
      }, MEDIA_STALL_TIMEOUT_MS);
    };
    restartWatchdog();
    return api
      .get<Blob>(`/media/${encodeURIComponent(messageId)}`, {
        responseType: "blob",
        timeout: 0,
        headers: { "X-Organization-ID": explicitOrganizationId },
        signal: controller.signal,
        onDownloadProgress: (event) => {
          restartWatchdog();
          onProgress?.(event.loaded, event.total);
        },
      })
      .catch((error: unknown) => {
        if (stalled) throw new Error("Chat media download stalled");
        throw error;
      })
      .finally(() => {
        clearTimeout(watchdog);
        signal?.removeEventListener("abort", forwardAbort);
      });
  },
};

export const templatesService = {
  list: (params?: {
    status?: string;
    category?: string;
    account?: string;
    search?: string;
    page?: number;
    limit?: number;
  }) => api.get<{ templates: any[]; total?: number }>("/templates", { params }),
  get: (id: string) => api.get(`/templates/${id}`),
  uploadMedia: (accountName: string, file: File) => {
    const formData = new FormData();
    formData.append("file", file);
    formData.append("account", accountName);
    return api.post("/templates/upload-media", formData, {
      headers: { "Content-Type": "multipart/form-data" },
    });
  },
};

export const flowsService = {
  list: (params?: {
    account?: string;
    search?: string;
    page?: number;
    limit?: number;
  }) => api.get<{ flows: any[]; total?: number }>("/flows", { params }),
  create: (data: any) => api.post("/flows", data),
  update: (id: string, data: any) => api.put(`/flows/${id}`, data),
  delete: (id: string) => api.delete(`/flows/${id}`),
  saveToMeta: (id: string) => api.post(`/flows/${id}/save-to-meta`),
  publish: (id: string) => api.post(`/flows/${id}/publish`),
  duplicate: (id: string) => api.post(`/flows/${id}/duplicate`),
  sync: (whatsappAccount: string) =>
    api.post("/flows/sync", { whatsapp_account: whatsappAccount }),
};

export const campaignsService = {
  list: (params?: {
    status?: string;
    from?: string;
    to?: string;
    search?: string;
    page?: number;
    limit?: number;
  }) => api.get("/campaigns", { params }),
  get: (id: string) => api.get(`/campaigns/${id}`),
  create: (data: any) => api.post("/campaigns", data),
  update: (id: string, data: any) => api.put(`/campaigns/${id}`, data),
  delete: (id: string) => api.delete(`/campaigns/${id}`),
  start: (id: string) => api.post(`/campaigns/${id}/start`),
  pause: (id: string) => api.post(`/campaigns/${id}/pause`),
  cancel: (id: string) => api.post(`/campaigns/${id}/cancel`),
  retryFailed: (id: string) => api.post(`/campaigns/${id}/retry-failed`),
  // Recipients
  getRecipients: (id: string) => api.get(`/campaigns/${id}/recipients`),
  addRecipients: (
    id: string,
    recipients: Array<{
      phone_number: string;
      recipient_name?: string;
      template_params?: Record<string, any>;
    }>,
  ) => api.post(`/campaigns/${id}/recipients/import`, { recipients }),
  deleteRecipient: (campaignId: string, recipientId: string) =>
    api.delete(`/campaigns/${campaignId}/recipients/${recipientId}`),
  // Media
  uploadMedia: (campaignId: string, file: File) => {
    const formData = new FormData();
    formData.append("file", file);
    return api.post(`/campaigns/${campaignId}/media`, formData, {
      headers: { "Content-Type": "multipart/form-data" },
    });
  },
  getMedia: (campaignId: string) =>
    api.get(`/campaigns/${campaignId}/media`, { responseType: "arraybuffer" }),
};

export const chatbotService = {
  // Settings
  getSettings: () => api.get("/chatbot/settings"),
  updateSettings: (data: any) => api.put("/chatbot/settings", data),

  // Keywords
  listKeywords: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{ rules: any[]; total?: number }>("/chatbot/keywords", { params }),
  getKeyword: (id: string) => api.get(`/chatbot/keywords/${id}`),
  createKeyword: (data: any) => api.post("/chatbot/keywords", data),
  updateKeyword: (id: string, data: any) =>
    api.put(`/chatbot/keywords/${id}`, data),
  deleteKeyword: (id: string) => api.delete(`/chatbot/keywords/${id}`),

  // Flows
  listFlows: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{ flows: any[]; total?: number }>("/chatbot/flows", { params }),
  getFlow: (id: string) => api.get(`/chatbot/flows/${id}`),
  createFlow: (data: any) => api.post("/chatbot/flows", data),
  updateFlow: (id: string, data: any) => api.put(`/chatbot/flows/${id}`, data),
  deleteFlow: (id: string) => api.delete(`/chatbot/flows/${id}`),

  // AI Contexts
  listAIContexts: (params?: {
    search?: string;
    page?: number;
    limit?: number;
  }) =>
    api.get<{ contexts: any[]; total?: number }>("/chatbot/ai-contexts", {
      params,
    }),
  getAIContext: (id: string) => api.get(`/chatbot/ai-contexts/${id}`),
  createAIContext: (data: any) => api.post("/chatbot/ai-contexts", data),
  updateAIContext: (id: string, data: any) =>
    api.put(`/chatbot/ai-contexts/${id}`, data),
  deleteAIContext: (id: string) => api.delete(`/chatbot/ai-contexts/${id}`),

  // Agent Transfers
  listTransfers: (params?: {
    status?: string;
    agent_id?: string;
    team_id?: string;
    contact_id?: string;
    limit?: number;
    offset?: number;
    include?: string; // 'all' | 'contact,agent,team' etc.
  }) => api.get("/chatbot/transfers", { params }),
  createTransfer: (data: {
    contact_id: string;
    whatsapp_account: string;
    agent_id?: string;
    notes?: string;
    source?: string;
  }) => api.post("/chatbot/transfers", data),
  pickNextTransfer: () => api.post("/chatbot/transfers/pick"),
  resumeTransfer: (id: string) => api.put(`/chatbot/transfers/${id}/resume`),
  assignTransfer: (
    id: string,
    agentId: string | null,
    teamId?: string | null,
  ) =>
    api.put(`/chatbot/transfers/${id}/assign`, {
      agent_id: agentId,
      team_id: teamId,
    }),
};

export interface CannedResponseButton {
  id: string;
  title: string;
  type?: "reply" | "url" | "phone" | "voice_call" | "flow";
  url?: string;
  phone_number?: string;
  ttl_minutes?: number;
  flow_id?: string;
  screen?: string;
}

export interface CannedResponse {
  id: string;
  name: string;
  shortcut: string;
  content: string;
  category: string;
  is_active: boolean;
  usage_count: number;
  buttons?: CannedResponseButton[];
  created_at: string;
  updated_at: string;
}

interface CannedResponseUpsertPayload {
  name?: string;
  shortcut?: string;
  content?: string;
  category?: string;
  is_active?: boolean;
  buttons?: CannedResponseButton[];
}

export const cannedResponsesService = {
  list: (params?: {
    category?: string;
    search?: string;
    active_only?: string;
    page?: number;
    limit?: number;
  }) =>
    api.get<{ canned_responses: CannedResponse[]; total?: number }>(
      "/canned-responses",
      { params },
    ),
  get: (id: string) => api.get<CannedResponse>(`/canned-responses/${id}`),
  create: (
    data: CannedResponseUpsertPayload & { name: string; content: string },
  ) => api.post("/canned-responses", data),
  update: (id: string, data: CannedResponseUpsertPayload) =>
    api.put(`/canned-responses/${id}`, data),
  delete: (id: string) => api.delete(`/canned-responses/${id}`),
  use: (id: string) => api.post(`/canned-responses/${id}/use`),
};

export const agentAnalyticsService = {
  getSummary: (params?: { from?: string; to?: string; agent_id?: string }) =>
    api.get("/analytics/agents", { params }),
};

// Meta WhatsApp Analytics Types
export type MetaAnalyticsType =
  | "analytics"
  | "pricing_analytics"
  | "template_analytics"
  | "call_analytics";

export type MetaGranularity = "HALF_HOUR" | "DAY" | "MONTH";

export interface MetaAnalyticsAccount {
  id: string;
  name: string;
  phone_id: string;
  is_mock?: boolean;
}

export interface MetaMessagingDataPoint {
  start: number;
  end: number;
  sent: number;
  delivered: number;
}

interface MetaConversationDataPoint {
  start: number;
  end: number;
  conversation: number;
  conversation_type: string;
  conversation_direction: string;
  conversation_category: string;
  cost: number;
}

export interface MetaPricingDataPoint {
  start: number;
  end: number;
  volume: number;
  cost: number;
  country?: string; // Country code (IN, US, etc.)
  pricing_type?: string; // FREE_CUSTOMER_SERVICE, FREE_ENTRY_POINT, REGULAR
  pricing_category?: string; // MARKETING, UTILITY, AUTHENTICATION, SERVICE, etc.
  tier?: string; // Pricing tier
}

interface MetaTemplateCostItem {
  type: string; // amount_spent, cost_per_delivered, cost_per_url_button_click
  value?: number; // The cost value
}

interface MetaTemplateClickItem {
  type: string; // quick_reply_button, unique_url_button
  button_content: string; // The button text
  count: number; // Number of clicks
}

export interface MetaTemplateDataPoint {
  start: number;
  end: number;
  template_id: string;
  sent: number;
  delivered: number;
  read: number;
  replied?: number;
  clicked?: MetaTemplateClickItem[]; // Array of button click details
  cost?: MetaTemplateCostItem[];
}

export interface MetaCallDataPoint {
  start: number;
  end: number;
  count: number;
  cost: number;
  average_duration: number;
  direction?: string; // USER_INITIATED or BUSINESS_INITIATED
}

interface MetaAnalyticsData {
  id: string;
  analytics?: {
    granularity: string;
    data_points: MetaMessagingDataPoint[];
  };
  conversation_analytics?: {
    granularity: string;
    data_points: MetaConversationDataPoint[];
  };
  pricing_analytics?: {
    granularity: string;
    data_points: MetaPricingDataPoint[];
  };
  template_analytics?: {
    granularity: string;
    data_points: MetaTemplateDataPoint[];
  };
  call_analytics?: {
    granularity: string;
    data_points: MetaCallDataPoint[];
  };
}

export interface MetaAnalyticsResponse {
  account_id: string;
  account_name: string;
  data: MetaAnalyticsData | null;
  template_names?: Record<string, string>; // meta_template_id -> template name
  is_mock?: boolean;
}

export const metaAnalyticsService = {
  get: (params: {
    account_id?: string;
    analytics_type: MetaAnalyticsType;
    start: string;
    end: string;
    granularity?: MetaGranularity;
    template_ids?: string;
  }) =>
    api.get<{
      accounts: MetaAnalyticsResponse[];
      cached: boolean;
      demo_data?: boolean;
    }>("/analytics/meta", { params }),

  getAccounts: () =>
    api.get<{ accounts: MetaAnalyticsAccount[]; demo_data?: boolean }>(
      "/analytics/meta/accounts",
    ),

  refresh: () => api.post("/analytics/meta/refresh"),
};

// Dashboard Widgets (customizable analytics)
export interface DashboardWidget {
  id: string;
  name: string;
  description: string;
  data_source: string;
  metric: string;
  field: string;
  filters: Array<{ field: string; operator: string; value: string }>;
  display_type: string;
  chart_type: string;
  group_by_field: string;
  show_change: boolean;
  color: string;
  size: string;
  display_order: number;
  grid_x: number;
  grid_y: number;
  grid_w: number;
  grid_h: number;
  config: Record<string, any>;
  is_shared: boolean;
  is_default: boolean;
  is_owner: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface WidgetData {
  widget_id: string;
  value: number;
  change: number;
  prev_value: number;
  chart_data: Array<{ label: string; value: number }>;
  data_points: Array<{ label: string; value: number; color?: string }>;
  grouped_series?: {
    labels: string[];
    datasets: Array<{ label: string; data: number[] }>;
  };
  table_rows?: Array<{
    id: string;
    label: string;
    sub_label: string;
    status: string;
    direction?: string;
    created_at: string;
  }>;
}

interface DataSourceInfo {
  name: string;
  label: string;
  fields: string[];
  aggregate_fields: string[];
}

export interface LayoutItem {
  id: string;
  grid_x: number;
  grid_y: number;
  grid_w: number;
  grid_h: number;
}

export const widgetsService = {
  list: () => api.get<{ widgets: DashboardWidget[] }>("/widgets"),
  create: (data: {
    name: string;
    description?: string;
    data_source: string;
    metric: string;
    field?: string;
    filters?: Array<{ field: string; operator: string; value: string }>;
    display_type?: string;
    chart_type?: string;
    group_by_field?: string;
    show_change?: boolean;
    color?: string;
    size?: string;
    config?: Record<string, any>;
    is_shared?: boolean;
  }) => api.post<DashboardWidget>("/widgets", data),
  update: (
    id: string,
    data: Partial<{
      name: string;
      description: string;
      data_source: string;
      metric: string;
      field: string;
      filters: Array<{ field: string; operator: string; value: string }>;
      display_type: string;
      chart_type: string;
      group_by_field: string;
      show_change: boolean;
      color: string;
      size: string;
      config: Record<string, any>;
      is_shared: boolean;
    }>,
  ) => api.put<DashboardWidget>(`/widgets/${id}`, data),
  delete: (id: string) => api.delete(`/widgets/${id}`),
  getAllData: (params?: { from?: string; to?: string }) =>
    api.get<{
      data: Record<string, WidgetData>;
      errors: Record<string, { status: number; message: string }>;
    }>("/widgets/data", { params }),
  getDataSources: () =>
    api.get<{
      data_sources: DataSourceInfo[];
      metrics: string[];
      display_types: string[];
      operators: Array<{ value: string; label: string }>;
    }>("/widgets/data-sources"),
  saveLayout: (layout: LayoutItem[]) => api.post("/widgets/layout", { layout }),
};

export const organizationService = {
  getSettings: () => api.get("/org/settings"),
  updateSettings: (data: {
    mask_phone_numbers?: boolean;
    timezone?: string;
    date_format?: string;
    name?: string;
    calling_enabled?: boolean;
    max_call_duration?: number;
    transfer_timeout_secs?: number;
    hold_music_file?: string;
    ringback_file?: string;
  }) => api.put("/org/settings", data),
  uploadOrgAudio: (file: File, type: "hold_music" | "ringback") => {
    const formData = new FormData();
    formData.append("file", file);
    return api.post(`/org/audio?type=${type}`, formData, {
      headers: { "Content-Type": "multipart/form-data" },
    });
  },
};

// Organizations
export interface Organization {
  id: string;
  reseller_id?: string;
  reseller_name?: string;
  name: string;
  slug?: string;
  created_at: string;
}

export const organizationsService = {
  list: (signal?: AbortSignal) =>
    api.get<{ organizations: Organization[] }>("/organizations", { signal }),
  create: (data: { name: string; reseller_id?: string }) =>
    api.post("/organizations", data),
  delete: (id: string) =>
    api.delete<{
      message: string;
      organization_id: string;
      recoverable: boolean;
    }>(`/organizations/${id}`),
  createInvitation: (data: { role_id?: string } = {}) =>
    api.post<{ data: { token: string; expires_at: string } }>(
      "/organizations/invitations",
      data,
    ),
  // Members
  addMember: (data: { user_id?: string; email?: string; role_id?: string }) =>
    api.post("/organizations/members", data),
};

export interface Reseller {
  id: string;
  name: string;
  slug: string;
  status: "active" | "suspended";
  plan: "starter" | "growth" | "enterprise";
  max_organizations: number;
  brand_name: string;
  logo_url: string;
  primary_color: string;
  accent_color: string;
  support_email: string;
  custom_domain: string;
  organization_count: number;
  member_count: number;
  created_at: string;
}

export interface ResellerMember {
  id: string;
  user_id: string;
  email: string;
  full_name: string;
  role: "owner" | "admin";
  is_active: boolean;
  created_at: string;
}

export interface ResellerOrganizationSubscription {
  id?: string
  plan_id?: string
  plan_price_id?: string
  plan_code: string
  plan_name?: string
  status: string
  provider: string
  trial_ends_at?: string
  current_period_start?: string
  current_period_end?: string
  grace_until?: string
  cancel_at_period_end?: boolean
  cancel_at?: string
}

export interface ResellerUsageOrganization extends Organization {
  subscription: ResellerOrganizationSubscription
}

export interface ResellerUsage {
  reseller_id: string;
  plan: string;
  max_organizations: number;
  organizations: ResellerUsageOrganization[];
  page: number;
  limit: number;
  total: number;
  organization_count: number;
  user_count: number;
  whatsapp_accounts: number;
  contacts: number;
  messages: number;
}

export const resellersService = {
  list: () => api.get<{ resellers: Reseller[] }>("/resellers"),
  get: (id: string) => api.get<Reseller>(`/resellers/${id}`),
  create: (data: {
    name: string;
    brand_name?: string;
    plan?: Reseller["plan"];
    max_organizations?: number;
    workspace_name?: string;
    support_email?: string;
  }) => api.post("/resellers", data),
  update: (
    id: string,
    data: Partial<
      Pick<
        Reseller,
        | "name"
        | "brand_name"
        | "logo_url"
        | "primary_color"
        | "accent_color"
        | "support_email"
        | "custom_domain"
        | "status"
        | "plan"
        | "max_organizations"
      >
    >,
  ) => api.put<Reseller>(`/resellers/${id}`, data),
  usage: (id: string, params?: { page?: number; limit?: number }) =>
    api.get<ResellerUsage>(`/resellers/${id}/usage`, { params }),
  members: (id: string) =>
    api.get<{ members: ResellerMember[] }>(`/resellers/${id}/members`),
  addMember: (
    id: string,
    data: { email: string; role: ResellerMember["role"] },
  ) => api.post(`/resellers/${id}/members`, data),
  removeMember: (id: string, memberId: string) =>
    api.delete(`/resellers/${id}/members/${memberId}`),
};

export interface Webhook {
  id: string;
  name: string;
  url: string;
  events: string[];
  headers: Record<string, string>;
  is_active: boolean;
  has_secret: boolean;
  created_at: string;
  updated_at: string;
}

export interface WebhookEvent {
  value: string;
  label: string;
  description: string;
}

export interface Team {
  id: string;
  name: string;
  description: string;
  assignment_strategy: "round_robin" | "load_balanced" | "manual";
  per_agent_timeout_secs: number;
  is_active: boolean;
  member_count: number;
  members?: TeamMember[];
  created_by_id?: string;
  created_by_name?: string;
  updated_by_id?: string;
  updated_by_name?: string;
  created_at: string;
  updated_at: string;
}

export interface TeamMember {
  id: string;
  team_id?: string;
  user_id: string;
  role: "manager" | "agent";
  last_assigned_at: string | null;
  // Flat structure from API
  full_name: string;
  email: string;
  is_available: boolean;
  // Optional nested user for local additions
  user?: {
    id: string;
    full_name: string;
    email: string;
    is_available: boolean;
  };
}

export const teamsService = {
  list: (
    params?: { search?: string; page?: number; limit?: number },
    signal?: AbortSignal,
  ) => api.get<{ teams: Team[] }>("/teams", { params, signal }),
  get: (id: string) => api.get<{ team: Team }>(`/teams/${id}`),
  create: (data: {
    name: string;
    description?: string;
    assignment_strategy?: "round_robin" | "load_balanced" | "manual";
    per_agent_timeout_secs?: number;
  }) => api.post<{ team: Team }>("/teams", data),
  update: (
    id: string,
    data: {
      name?: string;
      description?: string;
      assignment_strategy?: "round_robin" | "load_balanced" | "manual";
      per_agent_timeout_secs?: number;
      is_active?: boolean;
    },
  ) => api.put<{ team: Team }>(`/teams/${id}`, data),
  delete: (id: string) => api.delete(`/teams/${id}`),
  // Members
  listMembers: (teamId: string) =>
    api.get<{ members: TeamMember[] }>(`/teams/${teamId}/members`),
  addMember: (
    teamId: string,
    data: { user_id: string; role?: "manager" | "agent" },
  ) => api.post<{ member: TeamMember }>(`/teams/${teamId}/members`, data),
  removeMember: (teamId: string, userId: string) =>
    api.delete(`/teams/${teamId}/members/${userId}`),
};

// Audit Logs
export interface AuditLogChange {
  field: string;
  old_value: any;
  new_value: any;
}

export interface AuditLogEntry {
  id: string;
  resource_type: string;
  resource_id: string;
  user_id: string;
  user_name: string;
  action: "created" | "updated" | "deleted";
  changes: AuditLogChange[];
  created_at: string;
}

export const auditLogsService = {
  get: (id: string) => api.get<AuditLogEntry>(`/audit-logs/${id}`),
  list: (params?: {
    resource_type?: string;
    resource_id?: string;
    user_id?: string;
    action?: string;
    from?: string;
    to?: string;
    page?: number;
    limit?: number;
  }) =>
    api.get<{ audit_logs: AuditLogEntry[]; total: number }>("/audit-logs", {
      params,
    }),
};

export const webhooksService = {
  list: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{
      webhooks: Webhook[];
      available_events: WebhookEvent[];
      total?: number;
    }>("/webhooks", { params }),
  get: (id: string) => api.get<Webhook>(`/webhooks/${id}`),
  create: (data: {
    name: string;
    url: string;
    events: string[];
    headers?: Record<string, string>;
    secret?: string;
    is_active?: boolean;
  }) => api.post<Webhook>("/webhooks", data),
  update: (
    id: string,
    data: {
      name?: string;
      url?: string;
      events?: string[];
      headers?: Record<string, string>;
      secret?: string;
      clear_secret?: boolean;
      is_active?: boolean;
    },
  ) => api.put<Webhook>(`/webhooks/${id}`, data),
  delete: (id: string) => api.delete(`/webhooks/${id}`),
  test: (id: string) => api.post(`/webhooks/${id}/test`),
};

export interface CustomAction {
  id: string;
  name: string;
  icon: string;
  action_type: "webhook" | "url" | "javascript";
  config: {
    url?: string;
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    open_in_new_tab?: boolean;
    code?: string;
  };
  is_active: boolean;
  display_order: number;
  created_at: string;
  updated_at: string;
}

export interface ActionResult {
  success: boolean;
  message?: string;
  redirect_url?: string;
  clipboard?: string;
  toast?: {
    message: string;
    type: "success" | "error" | "info" | "warning";
  };
  data?: Record<string, any>;
}

export const customActionsService = {
  list: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{ custom_actions: CustomAction[]; total?: number }>(
      "/custom-actions",
      { params },
    ),
  get: (id: string) => api.get<CustomAction>(`/custom-actions/${id}`),
  create: (data: {
    name: string;
    icon?: string;
    action_type: "webhook" | "url" | "javascript";
    config: Record<string, any>;
    is_active?: boolean;
    display_order?: number;
  }) => api.post<CustomAction>("/custom-actions", data),
  update: (
    id: string,
    data: {
      name?: string;
      icon?: string;
      action_type?: "webhook" | "url" | "javascript";
      config?: Record<string, any>;
      is_active?: boolean;
      display_order?: number;
    },
  ) => api.put<CustomAction>(`/custom-actions/${id}`, data),
  delete: (id: string) => api.delete(`/custom-actions/${id}`),
  execute: (id: string, contactId: string) =>
    api.post<ActionResult>(`/custom-actions/${id}/execute`, {
      contact_id: contactId,
    }),
};

// Roles and Permissions
export interface Permission {
  id: string;
  resource: string;
  action: string;
  description: string;
  key: string; // "resource:action"
}

export interface Role {
  id: string;
  name: string;
  description: string;
  is_system: boolean;
  is_default: boolean;
  permissions: string[]; // ["resource:action", ...]
  user_count: number;
  created_at: string;
  updated_at: string;
}

export const rolesService = {
  list: (
    params?: { search?: string; page?: number; limit?: number },
    signal?: AbortSignal,
  ) => api.get<{ roles: Role[] }>("/roles", { params, signal }),
  get: (id: string) => api.get<Role>(`/roles/${id}`),
  create: (data: {
    name: string;
    description?: string;
    is_default?: boolean;
    permissions: string[];
  }) => api.post<Role>("/roles", data),
  update: (
    id: string,
    data: {
      name?: string;
      description?: string;
      is_default?: boolean;
      permissions?: string[];
    },
  ) => api.put<Role>(`/roles/${id}`, data),
  delete: (id: string) => api.delete(`/roles/${id}`),
};

export const permissionsService = {
  list: (signal?: AbortSignal) =>
    api.get<{ permissions: Permission[] }>("/permissions", { signal }),
};

// Tags
export interface Tag {
  name: string;
  color: string;
  created_at: string;
  updated_at: string;
}

export const tagsService = {
  list: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{ tags: Tag[]; total?: number; page?: number; limit?: number }>(
      "/tags",
      { params },
    ),
  create: (data: { name: string; color?: string }) =>
    api.post<Tag>("/tags", data),
  update: (name: string, data: { name?: string; color?: string }) =>
    api.put<Tag>(`/tags/${encodeURIComponent(name)}`, data),
  delete: (name: string) => api.delete(`/tags/${encodeURIComponent(name)}`),
};

// Conversation Notes
export interface ConversationNote {
  id: string;
  contact_id: string;
  created_by_id: string;
  created_by_name: string;
  content: string;
  created_at: string;
  updated_at: string;
}

export const notesService = {
  list: (contactId: string, params?: { limit?: number; before?: string }) =>
    api.get<{ notes: ConversationNote[]; total: number; has_more: boolean }>(
      `/contacts/${contactId}/notes`,
      { params },
    ),
  create: (contactId: string, data: { content: string }) =>
    api.post<ConversationNote>(`/contacts/${contactId}/notes`, data),
  update: (contactId: string, noteId: string, data: { content: string }) =>
    api.put<ConversationNote>(`/contacts/${contactId}/notes/${noteId}`, data),
  delete: (contactId: string, noteId: string) =>
    api.delete(`/contacts/${contactId}/notes/${noteId}`),
};

// Calling - Call Logs & IVR Flows
export interface CallLog {
  id: string;
  organization_id: string;
  whatsapp_account: string;
  contact_id: string;
  whatsapp_call_id: string;
  caller_phone: string;
  direction: "incoming" | "outgoing";
  status:
    | "ringing"
    | "answered"
    | "completed"
    | "missed"
    | "rejected"
    | "failed"
    | "initiating"
    | "accepted"
    | "transferring";
  duration: number;
  ivr_flow_id?: string;
  ivr_path?: Record<string, any>;
  agent_id?: string;
  started_at?: string;
  answered_at?: string;
  ended_at?: string;
  disconnected_by?: "client" | "agent" | "system";
  error_message?: string;
  recording_s3_key?: string;
  recording_duration?: number;
  contact?: {
    id: string;
    phone_number: string;
    profile_name: string;
  };
  agent?: {
    id: string;
    full_name: string;
    email: string;
  };
  ivr_flow?: IVRFlow;
  created_at: string;
  updated_at: string;
}

// v2 Node-based IVR Flow types
export type IVRNodeType =
  | "greeting"
  | "menu"
  | "gather"
  | "http_callback"
  | "transfer"
  | "goto_flow"
  | "timing"
  | "hangup";

export interface IVRNodePosition {
  x: number;
  y: number;
}

export interface IVRNode {
  id: string;
  type: IVRNodeType;
  label: string;
  position: IVRNodePosition;
  config: Record<string, any>;
}

export interface IVREdge {
  from: string;
  to: string;
  condition: string;
}

export interface IVRFlowData {
  version: 2;
  nodes: IVRNode[];
  edges: IVREdge[];
  entry_node: string;
}

// v2 Node-based Chatbot Flow types. Mirrors IVR's graph shape with a
// chat-specific node-type union. Only types listed in the union are
// implemented today; others land in Phase 3.
export type ChatNodeType =
  | "start"
  | "message"
  | "buttons"
  | "end"
  | "prompt"
  | "api_call"
  | "condition"
  | "timing"
  | "set_variable"
  | "ai_response"
  | "transfer"
  | "webhook"
  | "goto_flow"
  | "whatsapp_flow";

export interface ChatNode {
  id: string;
  type: ChatNodeType;
  label: string;
  position: IVRNodePosition;
  config: Record<string, any>;
}

export interface ChatEdge {
  from: string;
  to: string;
  condition: string;
}

export interface ChatFlowGraph {
  version: 2;
  nodes: ChatNode[];
  edges: ChatEdge[];
  entry_node: string;
}

export interface IVRFlow {
  id: string;
  organization_id: string;
  whatsapp_account: string;
  name: string;
  description: string;
  is_active: boolean;
  is_call_start: boolean;
  is_outgoing_end: boolean;
  menu: IVRFlowData;
  welcome_audio_url: string;
  created_at: string;
  updated_at: string;
}

export interface CallTransfer {
  id: string;
  organization_id: string;
  call_log_id: string;
  whatsapp_call_id: string;
  caller_phone: string;
  contact_id: string;
  whatsapp_account: string;
  status: "waiting" | "connected" | "completed" | "abandoned" | "no_answer";
  team_id?: string;
  agent_id?: string;
  initiating_agent_id?: string;
  transferred_at: string;
  connected_at?: string;
  completed_at?: string;
  hold_duration: number;
  talk_duration: number;
  ivr_path?: Record<string, any>;
  contact?: {
    id: string;
    phone_number: string;
    profile_name: string;
  };
  agent?: {
    id: string;
    full_name: string;
    email: string;
  };
  initiating_agent?: {
    id: string;
    full_name: string;
    email: string;
  };
  team?: {
    id: string;
    name: string;
  };
  call_log?: CallLog;
  created_at: string;
  updated_at: string;
}

// Outgoing Calls
export interface CallPermission {
  id: string;
  contact_id: string;
  whatsapp_account: string;
  status: "pending" | "accepted" | "declined" | "expired";
  message_id?: string;
  requested_at: string;
  responded_at?: string;
  expires_at?: string;
}

export const outgoingCallsService = {
  initiate: (data: {
    contact_id: string;
    whatsapp_account: string;
    sdp_offer: string;
  }) =>
    api.post<{ call_log_id: string; sdp_answer: string }>(
      "/calls/outgoing",
      data,
    ),
  hangup: (callLogId: string) =>
    api.post(`/calls/outgoing/${callLogId}/hangup`),
  requestPermission: (data: { contact_id: string; whatsapp_account: string }) =>
    api.post<{ permission_id: string }>("/calls/permission-request", data),
  getPermission: (contactId: string, whatsappAccount: string) =>
    api.get<CallPermission>(`/calls/permission/${contactId}`, {
      params: { whatsapp_account: whatsappAccount },
    }),
  getICEServers: () =>
    api.get<{
      expires_at?: string;
      ice_transport_policy?: "all" | "relay";
      ice_servers: Array<{
        urls: string[];
        username?: string;
        credential?: string;
      }>;
    }>("/calls/ice-servers"),
};

export const callLogsService = {
  list: (params?: {
    status?: string;
    account?: string;
    contact_id?: string;
    direction?: string;
    ivr_flow_id?: string;
    phone?: string;
    from?: string;
    to?: string;
    page?: number;
    limit?: number;
  }) =>
    api.get<{ call_logs: CallLog[]; total: number }>("/call-logs", { params }),
  get: (id: string) => api.get<CallLog>(`/call-logs/${id}`),
  getRecordingURL: (id: string) =>
    api.get<{ url: string; duration: number }>(`/call-logs/${id}/recording`),
  hold: (id: string) => api.post<{ status: string }>(`/call-logs/${id}/hold`),
  resume: (id: string) =>
    api.post<{ status: string }>(`/call-logs/${id}/resume`),
};

export const callTransfersService = {
  list: (params?: { status?: string; page?: number; limit?: number }) =>
    api.get<{ call_transfers: CallTransfer[]; total: number }>(
      "/call-transfers",
      { params },
    ),
  get: (id: string) => api.get<CallTransfer>(`/call-transfers/${id}`),
  connect: (id: string, sdpOffer: string) =>
    api.post<{ sdp_answer: string }>(`/call-transfers/${id}/connect`, {
      sdp_offer: sdpOffer,
    }),
  hangup: (id: string) => api.post(`/call-transfers/${id}/hangup`),
  initiate: (data: {
    call_log_id: string;
    team_id: string;
    agent_id?: string;
  }) => api.post<{ status: string }>("/call-transfers/initiate", data),
};

export const ivrFlowsService = {
  list: (params?: { search?: string; page?: number; limit?: number }) =>
    api.get<{ ivr_flows: IVRFlow[]; total: number }>("/ivr-flows", { params }),
  get: (id: string) => api.get<IVRFlow>(`/ivr-flows/${id}`),
  create: (data: {
    whatsapp_account: string;
    name: string;
    description?: string;
    is_call_start?: boolean;
    menu: IVRFlowData;
    welcome_audio_url?: string;
  }) => api.post<IVRFlow>("/ivr-flows", data),
  update: (
    id: string,
    data: {
      name?: string;
      description?: string;
      is_active?: boolean;
      is_call_start?: boolean;
      is_outgoing_end?: boolean;
      menu?: IVRFlowData;
      welcome_audio_url?: string;
    },
  ) => api.put<IVRFlow>(`/ivr-flows/${id}`, data),
  delete: (id: string) => api.delete(`/ivr-flows/${id}`),
  uploadAudio: (file: File) => {
    const formData = new FormData();
    formData.append("file", file);
    return api.post("/ivr-flows/audio", formData, {
      headers: { "Content-Type": "multipart/form-data" },
    });
  },
  getAudioUrl: (filename: string) =>
    `${api.defaults.baseURL}/ivr-flows/audio/${encodeURIComponent(filename)}`,
};

export default api;
