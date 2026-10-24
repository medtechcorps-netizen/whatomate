// Bridge between the auth store and the API client's per-tab session record
// (services/api.ts registers the implementations). Without them (say, with the
// API client mocked in a test) a tab counts as showing the current session.

type Handlers = {
  adopt: () => boolean
  isCurrent: () => boolean
  current: () => string | null
  owns: (session: string | null) => boolean
  ended: () => boolean
}

let handlers: Handlers | null = null

export function registerTabSession(implementation: Handlers) {
  handlers = implementation
}

/**
 * Records that this tab shows the browser's current session (the store calls
 * it whenever it restores or applies an identity). Returns false, adopting
 * nothing, when that session has ended: the identity must not be shown.
 */
export function adoptTabSession(): boolean {
  return handlers ? handlers.adopt() : true
}

/** Whether this tab shows the browser's current, live session. */
export function isTabSessionCurrent(): boolean {
  return handlers ? handlers.isCurrent() : true
}

/** The session this tab shows when it is the current one, else null. */
export function currentTabSession(): string | null {
  return handlers ? handlers.current() : ''
}

/**
 * Whether the shared state still belongs to `session` (taken from
 * currentTabSession() earlier): the browser is still in it or has just ended
 * it, and no newer session began since.
 */
export function sharedStateBelongsTo(session: string | null): boolean {
  return handlers ? handlers.owns(session) : session !== null
}

/** Whether the browser's session has ended (no newer one began since). */
export function browserSessionEnded(): boolean {
  return handlers ? handlers.ended() : false
}
