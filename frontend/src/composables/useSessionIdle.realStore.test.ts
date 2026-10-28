/** @vitest-environment happy-dom */

// The sign-out side of the per-tab session, with the real auth store and the
// real API client: a tab ends, and writes shared state for, only the session
// it shows; a tab that adopts an identity follows that identity's session.

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import axios from 'axios'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'

vi.mock('@/stores/calling', () => ({ useCallingStore: () => ({ isOnCall: false }) }))
vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), { loading: vi.fn(), error: vi.fn(), dismiss: vi.fn(), success: vi.fn() }),
}))

const MINUTE = 60_000
const U1 = { id: 'user-1', email: 'u1@example.com', organization_id: 'org-1' }
const U2 = { id: 'user-2', email: 'u2@example.com', organization_id: 'org-2' }

class FakeBroadcastChannel {
  static channels = new Set<FakeBroadcastChannel>()
  static deliverActivity = true
  onmessage: ((event: MessageEvent) => void) | null = null
  constructor(public name: string) {
    FakeBroadcastChannel.channels.add(this)
  }
  postMessage(data: unknown) {
    if (!FakeBroadcastChannel.deliverActivity && (data as { type?: unknown } | null)?.type === 'activity') return
    for (const channel of FakeBroadcastChannel.channels) {
      if (channel !== this && channel.name === this.name) channel.onmessage?.({ data } as MessageEvent)
    }
  }
  close() {
    FakeBroadcastChannel.channels.delete(this)
  }
}

const originalLocation = window.location
let assignedHref: string | null = null
const wrappers: VueWrapper[] = []
// API client instances loaded in a test; detached afterwards, so none reacts
// to a later test's page events.
const apis: Array<{ disposeForTests: () => void }> = []
let posts: string[] = []

const SHARED_KEYS = [
  'user',
  'selected_organization_id',
  'whm_session_id',
  'whm_auth_expires_at',
  'whm_last_activity',
  'whm_signout_reason',
  'whm_signed_out_at',
  'whm_logout_pending',
]

// Every target browser has Web Locks: a FIFO stand-in shared by the tabs of a
// test (without it a page's sign-out is cookie-neutral, the revoke only).
function installWebLocks() {
  const tails = new Map<string, Promise<void>>()
  const request = async (name: string, ...rest: unknown[]) => {
    const callback = rest[rest.length - 1] as () => Promise<unknown>
    const previous = tails.get(name) ?? Promise.resolve()
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    tails.set(name, previous.then(() => held))
    await previous
    try {
      return await callback()
    } finally {
      release()
    }
  }
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: { request, query: async () => ({ held: [], pending: [] }) },
  })
}

function sharedState() {
  return Object.fromEntries(SHARED_KEYS.map((key) => [key, localStorage.getItem(key)]))
}

// A tab that loads with U1 signed in (session s1), as after a page load.
async function openTab() {
  vi.resetModules()
  const pinia = await import('pinia')
  pinia.setActivePinia(pinia.createPinia())
  const api = await import('@/services/api')
  apis.push(api)
  api.api.defaults.adapter = vi.fn(async (config) => ({
    data: { data: {} }, status: 200, statusText: 'OK', headers: {}, config,
  })) as unknown as typeof api.api.defaults.adapter
  const { useAuthStore } = await import('@/stores/auth')
  const { useSessionIdle } = await import('./useSessionIdle')
  const store = useAuthStore()
  store.restoreSession()
  let idle!: ReturnType<typeof useSessionIdle>
  const Harness = defineComponent({
    setup() {
      idle = useSessionIdle()
      return () => null
    },
  })
  wrappers.push(mount(Harness))
  await flushPromises()
  const activity = await import('@/lib/sessionActivity')
  return { api, store, idle, activity }
}

// `user` signs in in another tab as a real sign-in does: a new marker (no
// pending logout left), its own identity and organization.
function signInElsewhere(user: typeof U1, session: string) {
  localStorage.setItem('whm_session_id', session)
  localStorage.removeItem('whm_logout_pending')
  localStorage.setItem('user', JSON.stringify(user))
  localStorage.setItem('selected_organization_id', user.organization_id)
  localStorage.setItem('whm_auth_expires_at', String(Date.now() + 15 * MINUTE))
  localStorage.setItem('whm_last_activity', String(Date.now()))
}

// U2 signs in in another tab: a newer session owns the shared state now.
function newerSessionElsewhere() {
  localStorage.setItem('whm_session_id', 's2')
  localStorage.setItem('user', JSON.stringify(U2))
  localStorage.setItem('selected_organization_id', 'org-2')
  localStorage.setItem('whm_auth_expires_at', String(Date.now() + 15 * MINUTE))
  localStorage.setItem('whm_last_activity', String(Date.now()))
}

describe('the per-tab session with the real store and API client', () => {
  beforeAll(async () => {
    await import('@/stores/auth')
    await import('./useSessionIdle')
    ;(await import('@/services/api')).disposeForTests()
  }, 60_000)

  beforeEach(() => {
    vi.useFakeTimers({ now: new Date('2026-10-19T09:00:00Z') })
    localStorage.clear()
    localStorage.setItem('user', JSON.stringify(U1))
    localStorage.setItem('whm_session_id', 's1')
    localStorage.setItem('whm_last_activity', String(Date.now()))
    localStorage.setItem('whm_session_idle_minutes', JSON.stringify({ 'user-1|org-1': 30, 'user-2|org-2': 30 }))
    document.cookie = 'whm_csrf=csrf-1; path=/'
    FakeBroadcastChannel.channels.clear()
    FakeBroadcastChannel.deliverActivity = true
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    installWebLocks()
    posts = []
    vi.spyOn(axios, 'post').mockImplementation(async (url: string, body: unknown) => {
      posts.push((body as { revoke?: boolean })?.revoke ? 'revoke' : url.replace(/^.*\/api/, ''))
      return { data: { data: {} }, status: 200 }
    })
    assignedHref = null
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: {
        pathname: '/chat',
        get href() {
          return assignedHref ?? 'http://localhost:3000/chat'
        },
        set href(value: string) {
          assignedHref = value
        },
        reload: vi.fn(),
      },
    })
  })

  afterEach(() => {
    while (wrappers.length) wrappers.pop()!.unmount()
    while (apis.length) apis.pop()!.disposeForTests()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    vi.useRealTimers()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    document.cookie = 'whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'
  })


  it.each([true, false])('uses isolated tab clocks and activity delivery to prevent false idle revoke (delivery %s)', async (deliverActivity) => {
    // Both clients will reach their proactive-refresh idle guard after the
    // original persisted30minute deadline, so a view-only fix is insufficient.
    localStorage.setItem('whm_auth_expires_at', String(Date.now() + 31 * MINUTE))
    const listeners = vi.spyOn(window, 'addEventListener')
    const active = await openTab()
    const pointer = listeners.mock.calls.find(call => call[0] === 'pointerdown')?.[1] as EventListener
    expect(pointer).toBeTypeOf('function')
    const passive = await openTab()
    expect(active.activity).not.toBe(passive.activity)
    expect(active.activity.readLastActivity).not.toBe(passive.activity.readLastActivity)
    FakeBroadcastChannel.deliverActivity = deliverActivity
    const persisted = localStorage.getItem('whm_last_activity')
    const storage = localStorage
    const deniedWrite = vi.fn((key: string, value: string) => {
      if (key === 'whm_last_activity') throw new Error('storage denied')
      // Keep the independent session-ending marker writable so the negative
      // control reaches the real revoke path instead of its ownership refusal.
      storage.setItem(key, value)
    })
    vi.stubGlobal('localStorage', {
      getItem: storage.getItem.bind(storage), setItem: deniedWrite,
      removeItem: storage.removeItem.bind(storage), clear: storage.clear.bind(storage),
    })
    expect(() => localStorage.setItem('whm_last_activity', 'denial-probe')).toThrow('storage denied')
    deniedWrite.mockClear()

    await vi.advanceTimersByTimeAsync(20 * MINUTE)
    // Dispatch to this tab's own listener, not the shared test window's other
    // tab. Only the BroadcastChannel can carry this activity to the passive tab.
    pointer(new Event('pointerdown'))
    expect(active.activity.readLastActivity()).toBe(Date.now())
    expect(passive.activity.readLastActivity()).toBe(deliverActivity ? Date.now() : Number(persisted))
    await vi.advanceTimersByTimeAsync(11 * MINUTE)
    await flushPromises()

    if (deliverActivity) {
      expect(localStorage.getItem('whm_last_activity')).toBe(persisted)
      expect(active.store.isAuthenticated).toBe(true)
      expect(passive.store.isAuthenticated).toBe(true)
      expect(active.idle.warningOpen.value).toBe(false)
      expect(passive.idle.warningOpen.value).toBe(false)
      expect(posts).toContain('/auth/refresh')
      expect(posts).not.toContain('revoke')
      expect(posts).not.toContain('/auth/logout')
      expect(assignedHref).toBeNull()
    } else {
      // Negative control: removing only activity delivery leaves this tab on
      // the stale clock and it revokes the session at the original deadline.
      // The API idle guard can leave before the view's own timer; mocked
      // navigation does not unmount/clear its Pinia store. Observe the real
      // revoke request and navigation, not a simulated page destruction.
      expect(posts).toContain('revoke')
      expect(assignedHref).toBe('/login')
    }
  })

  it('"Sign out" in a tab whose session was replaced ends nothing and touches nothing shared', async () => {
    const { api, idle } = await openTab()
    expect(api.readTabSession()).toBe('s1')
    newerSessionElsewhere()
    const before = sharedState()

    idle.signOutNow()
    await flushPromises()

    expect(posts).toEqual([])
    expect(sharedState()).toEqual(before)
    expect(assignedHref).toBe('/login')
  })

  it('an inactivity sign-out in a tab whose session was replaced ends nothing and touches nothing shared', async () => {
    const { idle } = await openTab()
    newerSessionElsewhere()
    const lastActivity = localStorage.getItem('whm_last_activity')
    // Freeze the newer session's clock so this tab reaches its deadline.
    localStorage.setItem('whm_last_activity', String(Date.now() - 40 * MINUTE))
    const before = sharedState()

    // The tab's own clock runs out too (its proactive refresh and its idle
    // timer both get there): it must still only leave.
    await vi.advanceTimersByTimeAsync(31 * MINUTE)
    await flushPromises()

    expect(idle.timeoutMinutes.value).toBe(30)
    expect(posts).toEqual([])
    expect(sharedState()).toEqual(before)
    expect(lastActivity).not.toBeNull()
    expect(assignedHref).toBe('/login')
  })

  it('a menu sign-out in a tab whose session was replaced ends nothing and touches nothing shared', async () => {
    const { store } = await openTab()
    newerSessionElsewhere()
    const before = sharedState()

    await store.logout()
    await flushPromises()

    expect(posts).toEqual([])
    expect(sharedState()).toEqual(before)
    expect(store.isAuthenticated).toBe(false)
  })

  it("a replaced tab's own idle check (its proactive refresh) leaves without touching shared state", async () => {
    await openTab()
    newerSessionElsewhere()
    // The newer session's clock looks idle from here, and its expiry time
    // arrives in this (frozen) tab without the marker change.
    localStorage.setItem('whm_last_activity', String(Date.now() - 40 * MINUTE))
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_auth_expires_at' }))
    const before = sharedState()

    await vi.advanceTimersByTimeAsync(15 * MINUTE)
    await flushPromises()

    expect(posts).toEqual([])
    expect(sharedState()).toEqual(before)
    expect(assignedHref).toBe('/login')
  })

  for (const [who, newer] of [['the same user signs in again', U1], ['another user signs in', U2]] as const) {
    for (const when of ['before its logout request is handled', 'after its own session ended'] as const) {
      it(`a current tab's menu sign-out leaves the newer session alone when ${who} ${when}`, async () => {
        const { api, store } = await openTab()
        expect(api.readTabSession()).toBe('s1')
        let reached!: () => void
        const atGate = new Promise<void>((resolve) => {
          reached = resolve
        })
        let open!: () => void
        const gate = new Promise<void>((resolve) => {
          open = resolve
        })
        if (when === 'before its logout request is handled') {
          // Added last, so it runs before the client's own request handling.
          api.api.interceptors.request.use(async (config) => {
            reached()
            await gate
            return config
          })
        } else {
          // Runs after the client's own response handling.
          api.api.interceptors.response.use(async (response) => {
            reached()
            await gate
            return response
          })
        }

        const leaving = store.logout()
        await atGate
        signInElsewhere(newer, 's2')
        const before = sharedState()
        open()
        const own = await leaving
        await api.whenLogoutSettled()
        await flushPromises()

        // The newer session's state is byte for byte as it was; this tab
        // only leaves.
        expect(own).toBe(false)
        expect(sharedState()).toEqual(before)
        expect(store.isAuthenticated).toBe(false)
        expect(api.readTabSession()).toBeNull()
      })
    }
  }

  it("a current tab's menu sign-out, with no newer session, clears the shared identity", async () => {
    const { api, store } = await openTab()

    const own = await store.logout()
    await api.whenLogoutSettled()

    expect(own).toBe(true)
    expect(posts).toEqual(['revoke', '/auth/logout'])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:s1')
    expect(localStorage.getItem('user')).toBeNull()
    expect(localStorage.getItem('selected_organization_id')).toBeNull()
  })

  it("a tab whose session another tab's sign-in replaced restores nothing and keeps that identity", async () => {
    // The tab loaded showing U1's session (s1); its store has not restored
    // it yet (say, a frozen tab whose router runs only now).
    vi.resetModules()
    const pinia = await import('pinia')
    pinia.setActivePinia(pinia.createPinia())
    const api = await import('@/services/api')
    apis.push(api)
    const { useAuthStore } = await import('@/stores/auth')
    const store = useAuthStore()
    expect(api.readTabSession()).toBe('s1')
    signInElsewhere(U2, 's2')
    const before = sharedState()

    expect(store.restoreSession()).toBe(false)
    await flushPromises()

    expect(sharedState()).toEqual(before)
    expect(store.isAuthenticated).toBe(false)
    expect(api.readTabSession()).toBeNull()
    expect(assignedHref).toBe('/login')
  })

  it('a tab that shows no session ends nothing when it logs out again', async () => {
    const { api, store } = await openTab()
    await store.logout()
    await api.whenLogoutSettled()
    expect(posts).toEqual(['revoke', '/auth/logout'])
    expect(api.readTabSession()).toBeNull()
    // U2 signs in elsewhere; this tab, still on its login page, logs out again.
    newerSessionElsewhere()
    const before = sharedState()
    posts = []

    await store.logout()
    await api.whenLogoutSettled()

    expect(posts).toEqual([])
    expect(sharedState()).toEqual(before)
  })

  it('a background end from a tab that shows no session ends nothing', async () => {
    const { api, store } = await openTab()
    await store.logout()
    await api.whenLogoutSettled()
    expect(api.readTabSession()).toBeNull()
    newerSessionElsewhere()
    const before = sharedState()
    posts = []

    api.endSessionInBackground()
    await api.whenLogoutSettled()

    expect(posts).toEqual([])
    expect(localStorage.getItem('whm_session_id')).toBe('s2')
    expect(sharedState()).toEqual(before)
  })

  it("a current tab's sign-out names its own session and ends it", async () => {
    const { api, idle } = await openTab()
    const heard: unknown[] = []
    const listener = new FakeBroadcastChannel('whm-session')
    listener.onmessage = (event) => heard.push(event.data)

    idle.signOutNow()
    await api.whenLogoutSettled()

    expect(heard).toEqual([{ type: 'signed-out', reason: 'manual', session: 's1' }])
    expect(posts).toEqual(['revoke', '/auth/logout'])
    expect(localStorage.getItem('whm_session_id')).toBe('ended:s1')
    expect(localStorage.getItem('user')).toBeNull()
    expect(api.readTabSession()).toBeNull()
  })

  it('a tab that adopts an identity by restore follows that session, and leaves when it ends', async () => {
    const { api, store } = await openTab()
    // A menu sign-out in this tab (no reload)...
    await store.logout()
    await api.whenLogoutSettled()
    expect(api.readTabSession()).toBeNull()
    // ...U2 signs in elsewhere, and Back in this tab restores that identity.
    newerSessionElsewhere()
    expect(store.restoreSession()).toBe(true)
    expect(api.readTabSession()).toBe('s2')

    // U2's session ends in another tab: this tab leaves it.
    localStorage.setItem('whm_session_id', 'ended:s2')
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_session_id', newValue: 'ended:s2' }))
    expect(assignedHref).toBe('/login')
    expect(api.readTabSession()).toBeNull()
  })

  it('an identity stored under an ended session is not restored', async () => {
    const { api, store } = await openTab()
    localStorage.setItem('whm_session_id', 'ended:s1')
    localStorage.setItem('user', JSON.stringify(U1))

    expect(store.restoreSession()).toBe(false)
    expect(api.readTabSession()).toBeNull()
  })
})
