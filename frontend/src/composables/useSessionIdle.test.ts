/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, reactive } from 'vue'

const mocks = vi.hoisted(() => ({
  checkSessionIdlePolicy: vi.fn(),
  markSessionEnding: vi.fn(),
  endSessionInBackground: vi.fn(),
  keepSessionAlive: vi.fn(),
  // The logical session id the API client reports (services/api.ts), and
  // the session this tab shows.
  sessionId: 'session-1',
  tabSession: 'session-1' as string | null,
  currentStore: null as any,
  calling: null as any,
}))

vi.mock('@/services/api', () => ({
  checkSessionIdlePolicy: mocks.checkSessionIdlePolicy,
  endSessionInBackground: mocks.endSessionInBackground,
  keepSessionAlive: mocks.keepSessionAlive,
  markSessionEnding: mocks.markSessionEnding,
  readSessionId: () => mocks.sessionId,
  readTabSession: () => mocks.tabSession,
  isTabSessionCurrent: () => mocks.tabSession !== null && mocks.tabSession === mocks.sessionId,
}))

vi.mock('@/lib/tabSession', () => ({
  currentTabSession: () => mocks.tabSession !== null && mocks.tabSession === mocks.sessionId ? mocks.tabSession : null,
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => mocks.currentStore,
}))

vi.mock('@/stores/calling', () => ({
  useCallingStore: () => mocks.calling,
}))

import { SESSION_IDLE_WARNING_MS, useSessionIdle } from './useSessionIdle'
import {
  LAST_ACTIVITY_KEY,
  clearLastActivity,
  SESSION_ENDING_EVENT,
  SESSION_IDLE_POLICY_KEY,
  SESSION_POLICY_EVENT,
  SIGNED_OUT_BROADCAST_KEY,
} from '@/lib/sessionActivity'
import { SIGN_OUT_REASON_KEY } from '@/lib/sessionSignout'

const MINUTE = 60_000

// In-memory BroadcastChannel shared by every "tab" in a test.
class FakeBroadcastChannel {
  static channels = new Set<FakeBroadcastChannel>()
  onmessage: ((event: MessageEvent) => void) | null = null
  constructor(public name: string) {
    FakeBroadcastChannel.channels.add(this)
  }
  postMessage(data: unknown) {
    for (const channel of FakeBroadcastChannel.channels) {
      if (channel !== this && channel.name === this.name) {
        channel.onmessage?.({ data } as MessageEvent)
      }
    }
  }
  close() {
    FakeBroadcastChannel.channels.delete(this)
  }
}

function authStore() {
  const store = reactive({
    isAuthenticated: true,
    organizationId: 'org-1',
    clearAuth: vi.fn(() => {
      store.isAuthenticated = false
    }),
  })
  return store
}

type Idle = ReturnType<typeof useSessionIdle>

let assignedHref: string | null = null
const originalLocation = window.location
const wrappers: VueWrapper[] = []

// The signed-in user and organization the cached policies are keyed by.
const IDENTITY = 'user-1|org-1'

function policy(minutes: number | null, identity = IDENTITY) {
  if (minutes === null) localStorage.removeItem(SESSION_IDLE_POLICY_KEY)
  else localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ [identity]: minutes }))
}

function signedOut(reason: string, session = 'session-1') {
  return { type: 'signed-out', reason, session }
}

async function open(store = authStore()) {
  mocks.currentStore = store
  let idle!: Idle
  const Harness = defineComponent({
    setup() {
      idle = useSessionIdle()
      return () => null
    },
  })
  const wrapper = mount(Harness)
  wrappers.push(wrapper)
  await flushPromises()
  return { idle, store }
}

describe('useSessionIdle', () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: new Date('2026-10-15T09:00:00Z') })
    clearLastActivity()
    localStorage.clear()
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-1' }))
    mocks.sessionId = 'session-1'
    mocks.tabSession = 'session-1'
    FakeBroadcastChannel.channels.clear()
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
    mocks.checkSessionIdlePolicy.mockReset().mockResolvedValue(undefined)
    // Like the API client: the one place SESSION_ENDING_EVENT is fired.
    mocks.markSessionEnding.mockReset().mockImplementation(() => {
      window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
    })
    mocks.endSessionInBackground.mockReset()
    mocks.keepSessionAlive.mockReset().mockResolvedValue(undefined)
    mocks.calling = reactive({ isOnCall: false })
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
      },
    })
  })

  afterEach(() => {
    while (wrappers.length) wrappers.pop()!.unmount()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    vi.useRealTimers()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
  })


  it('shares activity when storage writes fail without rebroadcasting incoming notices', async () => {
    policy(30)
    localStorage.setItem(LAST_ACTIVITY_KEY, String(Date.now()))
    const { idle, store } = await open()
    const received: unknown[] = []
    const other = new FakeBroadcastChannel('whm-session')
    other.onmessage = event => received.push(event.data)
    const storage = localStorage
    const deniedWrite = vi.fn(() => { throw new Error('storage denied') })
    vi.stubGlobal('localStorage', {
      getItem: storage.getItem.bind(storage), setItem: deniedWrite,
      removeItem: storage.removeItem.bind(storage), clear: storage.clear.bind(storage),
    })
    expect(() => localStorage.setItem('denial-probe', 'value')).toThrow('storage denied')
    deniedWrite.mockClear()

    await vi.advanceTimersByTimeAsync(20 * MINUTE)
    window.dispatchEvent(new Event('pointerdown'))
    expect(received).toEqual([{ type: 'activity', session: 'session-1', identity: IDENTITY, at: Date.now() }])
    const writes = deniedWrite
    writes.mockClear()
    await vi.advanceTimersByTimeAsync(20 * MINUTE)
    other.postMessage({ type: 'activity', session: 'session-1', identity: IDENTITY, at: Date.now() })
    expect(writes).not.toHaveBeenCalled()
    expect(received).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(20 * MINUTE)
    expect(idle.warningOpen.value).toBe(false)
    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(mocks.endSessionInBackground).not.toHaveBeenCalled()
  })

  it('bounds activity broadcasts even when lifecycle flushes happen repeatedly', async () => {
    policy(30)
    await open()
    const received: unknown[] = []
    const other = new FakeBroadcastChannel('whm-session')
    other.onmessage = event => received.push(event.data)
    await vi.advanceTimersByTimeAsync(5_000)
    for (let count = 0; count < 20; count++) {
      window.dispatchEvent(new Event('pointermove'))
      window.dispatchEvent(new Event('pagehide'))
      await vi.advanceTimersByTimeAsync(100)
    }
    expect(received).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(5_000)
    window.dispatchEvent(new Event('pointermove'))
    expect(received).toHaveLength(2)
  })

  it.each([
    { id: 'user-1' },
    { id: 'user-1', organization_id: ' ' },
    { id: 'user-1', organization_id: {} },
    { id: 42, organization_id: 'org-1' },
    { id: 'user|1', organization_id: 'org-1' },
    { id: 'user-1', organization_id: 'org|1' },
  ])('does not broadcast an incomplete or ambiguous scope or add a logout (%j)', async user => {
    policy(30)
    const { store } = await open()
    const received: unknown[] = []
    const other = new FakeBroadcastChannel('whm-session')
    other.onmessage = event => received.push(event.data)
    localStorage.setItem('user', JSON.stringify(user))
    await vi.advanceTimersByTimeAsync(5_000)
    window.dispatchEvent(new Event('pointerdown'))
    window.dispatchEvent(new Event('pagehide'))
    expect(received).toEqual([])
    expect(store.isAuthenticated).toBe(true)
    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(mocks.endSessionInBackground).not.toHaveBeenCalled()
    expect(assignedHref).toBeNull()
  })

  it('a replaced tab cannot persist or broadcast activity for the newer session', async () => {
    policy(30)
    await open()
    const received: unknown[] = []
    const other = new FakeBroadcastChannel('whm-session')
    other.onmessage = event => received.push(event.data)
    await vi.advanceTimersByTimeAsync(5_000)
    mocks.sessionId = 'session-2'
    const before = localStorage.getItem(LAST_ACTIVITY_KEY)
    window.dispatchEvent(new Event('pointerdown'))
    window.dispatchEvent(new Event('pagehide'))
    expect(localStorage.getItem(LAST_ACTIVITY_KEY)).toBe(before)
    expect(received).toEqual([])
  })

  it('warns two minutes before the timeout, then signs out at once', async () => {
    policy(30)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(27 * MINUTE)
    expect(idle.warningOpen.value).toBe(false)

    await vi.advanceTimersByTimeAsync(1 * MINUTE)
    expect(idle.warningOpen.value).toBe(true)
    expect(idle.countdown.value).toBe('2:00')

    await vi.advanceTimersByTimeAsync(SESSION_IDLE_WARNING_MS)
    // The identity goes and the login page loads straight away; the server
    // logout runs in the background.
    expect(store.clearAuth).toHaveBeenCalledTimes(1)
    expect(mocks.endSessionInBackground).toHaveBeenCalledTimes(1)
    expect(assignedHref).toBe('/login')
    expect(localStorage.getItem(LAST_ACTIVITY_KEY)).toBeNull()
    expect(JSON.parse(localStorage.getItem(SIGN_OUT_REASON_KEY)!)).toMatchObject({
      reason: 'idle',
      minutes: 30,
    })
  })

  it('counts pointer, keyboard and touch activity in this tab', async () => {
    policy(30)
    const { idle, store } = await open()

    for (const type of ['pointerdown', 'keydown', 'touchstart']) {
      await vi.advanceTimersByTimeAsync(20 * MINUTE)
      window.dispatchEvent(new Event(type))
    }
    await vi.advanceTimersByTimeAsync(20 * MINUTE)

    expect(idle.warningOpen.value).toBe(false)
    expect(store.clearAuth).not.toHaveBeenCalled()
  })

  it('counts activity in another tab through the shared timestamp', async () => {
    policy(30)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(29 * MINUTE)
    expect(idle.warningOpen.value).toBe(true)

    localStorage.setItem(LAST_ACTIVITY_KEY, String(Date.now()))
    await vi.advanceTimersByTimeAsync(1_000)
    expect(idle.warningOpen.value).toBe(false)

    await vi.advanceTimersByTimeAsync(20 * MINUTE)
    expect(store.clearAuth).not.toHaveBeenCalled()
  })

  it("shares this tab's activity and flushes it when the page is hidden", async () => {
    policy(30)
    await open()
    await vi.advanceTimersByTimeAsync(10 * MINUTE)
    window.dispatchEvent(new Event('pointerdown'))
    expect(Number(localStorage.getItem(LAST_ACTIVITY_KEY))).toBe(Date.now())

    // Throttled: a move 2 s later is not written yet...
    await vi.advanceTimersByTimeAsync(2_000)
    window.dispatchEvent(new Event('pointermove'))
    expect(Number(localStorage.getItem(LAST_ACTIVITY_KEY))).toBe(Date.now() - 2_000)
    // ...until the page goes away.
    window.dispatchEvent(new Event('pagehide'))
    expect(Number(localStorage.getItem(LAST_ACTIVITY_KEY))).toBe(Date.now())
  })

  it('needs "Stay signed in" once the warning is up', async () => {
    policy(30)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(28 * MINUTE + 1_000)
    expect(idle.warningOpen.value).toBe(true)
    window.dispatchEvent(new Event('pointermove'))
    window.dispatchEvent(new Event('keydown'))
    await vi.advanceTimersByTimeAsync(2 * MINUTE)

    expect(store.clearAuth).toHaveBeenCalledTimes(1)
  })

  it('"Stay signed in" refreshes the session and restarts the clock', async () => {
    policy(30)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(29 * MINUTE)
    await idle.staySignedIn()

    expect(mocks.keepSessionAlive).toHaveBeenCalledTimes(1)
    expect(idle.warningOpen.value).toBe(false)
    await vi.advanceTimersByTimeAsync(27 * MINUTE)
    expect(idle.warningOpen.value).toBe(false)
    expect(store.clearAuth).not.toHaveBeenCalled()
  })

  it('"Sign out" signs out without an inactivity banner', async () => {
    policy(30)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(29 * MINUTE)
    idle.signOutNow()

    expect(store.clearAuth).toHaveBeenCalledTimes(1)
    expect(mocks.endSessionInBackground).toHaveBeenCalledTimes(1)
    expect(assignedHref).toBe('/login')
    expect(localStorage.getItem(SIGN_OUT_REASON_KEY)).toBeNull()
  })

  it('signs the other tabs out without a second logout', async () => {
    policy(30)
    const tabA = await open()
    const tabB = await open()

    await vi.advanceTimersByTimeAsync(5 * MINUTE)
    tabA.idle.signOutNow()

    expect(mocks.endSessionInBackground).toHaveBeenCalledTimes(1)
    expect(tabB.store.clearAuth).toHaveBeenCalledTimes(1)
    // Each tab stops using the session's cookies at once.
    expect(mocks.markSessionEnding).toHaveBeenCalledTimes(2)
  })

  it('ignores an inactivity broadcast while the shared clock says active', async () => {
    policy(30)
    const { store } = await open()
    const other = new FakeBroadcastChannel('whm-session')

    await vi.advanceTimersByTimeAsync(5 * MINUTE)
    other.postMessage(signedOut('idle'))
    expect(store.clearAuth).not.toHaveBeenCalled()

    // Once this tab's view of the clock is also past the deadline, it agrees.
    vi.setSystemTime(Date.now() + 40 * MINUTE)
    other.postMessage(signedOut('idle'))
    expect(store.clearAuth).toHaveBeenCalledTimes(1)
  })

  it('falls back to the storage event where BroadcastChannel is missing', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    policy(30)
    const { store } = await open()

    window.dispatchEvent(new StorageEvent('storage', {
      key: SIGNED_OUT_BROADCAST_KEY,
      newValue: JSON.stringify({ reason: 'manual', session: 'session-1', at: Date.now() }),
    }))

    expect(store.clearAuth).toHaveBeenCalledTimes(1)
    expect(assignedHref).toBe('/login')
  })

  it('does not let activity after the deadline revive the session', async () => {
    policy(30)
    const { store } = await open()

    // The machine slept through the deadline: no timers ran.
    vi.setSystemTime(Date.now() + 45 * MINUTE)
    window.dispatchEvent(new Event('pointerdown'))
    await flushPromises()

    expect(store.clearAuth).toHaveBeenCalledTimes(1)
    expect(JSON.parse(localStorage.getItem(SIGN_OUT_REASON_KEY)!)).toMatchObject({ reason: 'idle' })
  })

  it('never signs out when the policy is off', async () => {
    policy(0)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(10 * 60 * MINUTE)

    expect(idle.warningOpen.value).toBe(false)
    expect(store.clearAuth).not.toHaveBeenCalled()
  })

  it('uses the 4 hour default until a policy is known', async () => {
    policy(null)
    const { idle, store } = await open()

    await vi.advanceTimersByTimeAsync(237 * MINUTE)
    expect(idle.warningOpen.value).toBe(false)
    await vi.advanceTimersByTimeAsync(1 * MINUTE)
    expect(idle.warningOpen.value).toBe(true)
    await vi.advanceTimersByTimeAsync(2 * MINUTE)
    expect(store.clearAuth).toHaveBeenCalledTimes(1)
  })

  it('does not treat a page load as activity: a stale clock signs out', async () => {
    policy(30)
    localStorage.setItem(LAST_ACTIVITY_KEY, String(Date.now() - 16 * 60 * MINUTE))
    const { store } = await open()

    expect(store.clearAuth).toHaveBeenCalledTimes(1)
    expect(assignedHref).toBe('/login')
    expect(JSON.parse(localStorage.getItem(SIGN_OUT_REASON_KEY)!)).toMatchObject({ reason: 'idle' })
  })

  it('keeps a recent clock across a reload', async () => {
    policy(30)
    const started = Date.now() - 20 * MINUTE
    localStorage.setItem(LAST_ACTIVITY_KEY, String(started))
    const { idle, store } = await open()

    expect(Number(localStorage.getItem(LAST_ACTIVITY_KEY))).toBe(started)
    await vi.advanceTimersByTimeAsync(8 * MINUTE)
    expect(idle.warningOpen.value).toBe(true)
    expect(store.clearAuth).not.toHaveBeenCalled()
  })

  it('starts the clock on a first sign-in', async () => {
    policy(30)
    await open()
    expect(Number(localStorage.getItem(LAST_ACTIVITY_KEY))).toBe(Date.now())
  })

  it('starts the clock when the policy is switched on', async () => {
    policy(0)
    localStorage.setItem(LAST_ACTIVITY_KEY, String(Date.now() - 10 * 60 * MINUTE))
    const { idle, store } = await open()

    window.dispatchEvent(new CustomEvent(SESSION_POLICY_EVENT, { detail: { minutes: 30, identity: IDENTITY } }))
    await vi.advanceTimersByTimeAsync(1_000)
    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(idle.timeoutMinutes.value).toBe(30)

    await vi.advanceTimersByTimeAsync(28 * MINUTE)
    expect(idle.warningOpen.value).toBe(true)
  })

  it('counts an active call as activity', async () => {
    policy(30)
    const { idle, store } = await open()

    mocks.calling.isOnCall = true
    await vi.advanceTimersByTimeAsync(45 * MINUTE)
    expect(idle.warningOpen.value).toBe(false)
    expect(store.clearAuth).not.toHaveBeenCalled()

    mocks.calling.isOnCall = false
    await vi.advanceTimersByTimeAsync(31 * MINUTE)
    expect(store.clearAuth).toHaveBeenCalledTimes(1)
  })

  it('announces the session is ending before clearing it', async () => {
    policy(30)
    const { idle, store } = await open()
    const seen: Array<{ authenticated: boolean; href: string | null }> = []
    const onEnding = () => seen.push({ authenticated: store.isAuthenticated, href: assignedHref })
    window.addEventListener('rereply:session-ending', onEnding)

    idle.signOutNow()

    window.removeEventListener('rereply:session-ending', onEnding)
    expect(seen).toEqual([{ authenticated: true, href: null }])
  })

  it('announces the session is ending on an inactivity sign-out', async () => {
    policy(30)
    const { store } = await open()
    const seen: boolean[] = []
    const onEnding = () => seen.push(store.isAuthenticated)
    window.addEventListener('rereply:session-ending', onEnding)

    await vi.advanceTimersByTimeAsync(30 * MINUTE)

    window.removeEventListener('rereply:session-ending', onEnding)
    expect(seen).toEqual([true])
    expect(store.clearAuth).toHaveBeenCalledTimes(1)
  })

  it('announces the session is ending when another tab signs out', async () => {
    policy(30)
    const { store } = await open()
    const seen: boolean[] = []
    const onEnding = () => seen.push(store.isAuthenticated)
    window.addEventListener('rereply:session-ending', onEnding)

    new FakeBroadcastChannel('whm-session').postMessage(signedOut('manual'))

    window.removeEventListener('rereply:session-ending', onEnding)
    expect(seen).toEqual([true])
  })

  it('checks the session after an inactivity broadcast it disagrees with', async () => {
    policy(30)
    const { store } = await open()

    await vi.advanceTimersByTimeAsync(5 * MINUTE)
    new FakeBroadcastChannel('whm-session').postMessage(signedOut('idle'))
    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(mocks.keepSessionAlive).not.toHaveBeenCalled()

    // After the other tab's revoke has had time to land, a refresh tells
    // whether the session is still alive (the API client ends a dead one).
    await vi.advanceTimersByTimeAsync(3_000)
    expect(mocks.keepSessionAlive).toHaveBeenCalledTimes(1)
  })

  it('keeps the clock when an active policy changes', async () => {
    policy(240)
    localStorage.setItem(LAST_ACTIVITY_KEY, String(Date.now() - 50 * MINUTE))
    const { store } = await open()
    expect(store.clearAuth).not.toHaveBeenCalled()

    // 50 minutes idle is past a new 30 minute policy: no fresh start.
    window.dispatchEvent(new CustomEvent(SESSION_POLICY_EVENT, { detail: { minutes: 30, identity: IDENTITY } }))
    await vi.advanceTimersByTimeAsync(1_000)

    expect(store.clearAuth).toHaveBeenCalledTimes(1)
  })

  it('writes no activity once the session is ending', async () => {
    policy(30)
    const { idle } = await open()
    window.dispatchEvent(new Event('pointerdown'))

    idle.signOutNow()
    window.dispatchEvent(new Event('pointermove'))
    window.dispatchEvent(new Event('pagehide'))

    expect(localStorage.getItem(LAST_ACTIVITY_KEY)).toBeNull()
  })

  it('fires the session-ending event once per sign-out', async () => {
    policy(30)
    const { idle } = await open()
    let events = 0
    const onEnding = () => events++
    window.addEventListener(SESSION_ENDING_EVENT, onEnding)

    idle.signOutNow()
    idle.signOutNow()

    window.removeEventListener(SESSION_ENDING_EVENT, onEnding)
    expect(events).toBe(1)
    expect(mocks.markSessionEnding).toHaveBeenCalledTimes(1)
  })

  it('names the ending session in both sign-out notices', async () => {
    policy(30)
    const { idle } = await open()
    const listener = new FakeBroadcastChannel('whm-session')
    const received: unknown[] = []
    listener.onmessage = (event) => received.push(event.data)

    idle.signOutNow()

    expect(received).toEqual([{ type: 'signed-out', reason: 'manual', session: 'session-1' }])
    expect(JSON.parse(localStorage.getItem(SIGNED_OUT_BROADCAST_KEY)!)).toMatchObject({
      reason: 'manual',
      session: 'session-1',
    })
  })

  it('ignores a late sign-out notice for an earlier session (BroadcastChannel)', async () => {
    policy(30)
    // A new login happened after the old session's notice was queued, and
    // this tab shows that newer session.
    mocks.sessionId = 'session-2'
    mocks.tabSession = 'session-2'
    const { store } = await open()

    new FakeBroadcastChannel('whm-session').postMessage(signedOut('manual', 'session-1'))
    new FakeBroadcastChannel('whm-session').postMessage({ type: 'signed-out', reason: 'manual' })

    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(mocks.markSessionEnding).not.toHaveBeenCalled()
    expect(assignedHref).toBeNull()
  })

  it('ignores a late sign-out notice for an earlier session (storage event)', async () => {
    vi.stubGlobal('BroadcastChannel', undefined)
    policy(30)
    mocks.sessionId = 'session-2'
    mocks.tabSession = 'session-2'
    const { store } = await open()

    for (const value of [
      { reason: 'manual', session: 'session-1', at: Date.now() },
      { reason: 'idle', session: 'session-1', at: Date.now() },
      { reason: 'manual', at: Date.now() },
    ]) {
      window.dispatchEvent(new StorageEvent('storage', {
        key: SIGNED_OUT_BROADCAST_KEY,
        newValue: JSON.stringify(value),
      }))
    }
    await vi.advanceTimersByTimeAsync(5_000)

    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(mocks.keepSessionAlive).not.toHaveBeenCalled()
  })

  it('signs out at once on a notice for its own session (both transports)', async () => {
    policy(30)
    const viaChannel = await open()
    new FakeBroadcastChannel('whm-session').postMessage(signedOut('manual', 'session-1'))
    expect(viaChannel.store.clearAuth).toHaveBeenCalledTimes(1)

    while (wrappers.length) wrappers.pop()!.unmount()
    vi.stubGlobal('BroadcastChannel', undefined)
    assignedHref = null
    const viaStorage = await open()
    window.dispatchEvent(new StorageEvent('storage', {
      key: SIGNED_OUT_BROADCAST_KEY,
      newValue: JSON.stringify({ reason: 'manual', session: 'session-1', at: Date.now() }),
    }))
    expect(viaStorage.store.clearAuth).toHaveBeenCalledTimes(1)
    expect(assignedHref).toBe('/login')
  })

  it('drops a delayed session check once a newer login replaced the session', async () => {
    policy(30)
    const { store } = await open()

    await vi.advanceTimersByTimeAsync(5 * MINUTE)
    new FakeBroadcastChannel('whm-session').postMessage(signedOut('idle', 'session-1'))
    // Before the check runs, a new login replaces the logical session.
    mocks.sessionId = 'session-2'
    await vi.advanceTimersByTimeAsync(5_000)

    expect(mocks.keepSessionAlive).not.toHaveBeenCalled()
    expect(store.clearAuth).not.toHaveBeenCalled()
  })

  it('applies a policy only for the identity it was learned for', async () => {
    policy(240)
    const { idle } = await open()

    // Another organization's policy, by event or through storage, is ignored.
    window.dispatchEvent(new CustomEvent(SESSION_POLICY_EVENT, { detail: { minutes: 30, identity: 'user-1|org-2' } }))
    localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ [IDENTITY]: 240, 'user-1|org-2': 30 }))
    window.dispatchEvent(new StorageEvent('storage', { key: SESSION_IDLE_POLICY_KEY }))
    expect(idle.timeoutMinutes.value).toBe(240)

    // Its own, propagated from another tab, is applied.
    localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ [IDENTITY]: 60, 'user-1|org-2': 30 }))
    window.dispatchEvent(new StorageEvent('storage', { key: SESSION_IDLE_POLICY_KEY }))
    expect(idle.timeoutMinutes.value).toBe(60)
  })

  it("switches to the new organization's own policy on an org switch", async () => {
    localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ [IDENTITY]: 0, 'user-1|org-2': 30 }))
    const { idle, store } = await open()
    expect(idle.timeoutMinutes.value).toBe(0)

    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-2' }))
    store.organizationId = 'org-2'
    await flushPromises()

    expect(idle.timeoutMinutes.value).toBe(30)
    expect(mocks.checkSessionIdlePolicy).toHaveBeenCalledTimes(1)
  })

  it('a frozen tab told its session ended leaves without touching the newer session', async () => {
    policy(30)
    // This tab still shows U1's session 1; meanwhile U1 signed out elsewhere
    // and U2 signed in (session 2) before this tab processed the notice.
    const { store } = await open()
    mocks.sessionId = 'session-2'
    localStorage.setItem('user', JSON.stringify({ id: 'user-2', organization_id: 'org-2' }))
    localStorage.setItem('selected_organization_id', 'org-2')

    new FakeBroadcastChannel('whm-session').postMessage(signedOut('manual', 'session-1'))

    expect(assignedHref).toBe('/login')
    expect(mocks.markSessionEnding).toHaveBeenCalledTimes(1)
    // Nothing shared is touched: it belongs to U2's session now.
    expect(store.clearAuth).not.toHaveBeenCalled()
    expect(mocks.endSessionInBackground).not.toHaveBeenCalled()
    expect(JSON.parse(localStorage.getItem('user')!).id).toBe('user-2')
    expect(localStorage.getItem('selected_organization_id')).toBe('org-2')
    expect(localStorage.getItem(SIGNED_OUT_BROADCAST_KEY)).toBeNull()
  })

  it("another tab's org switch brings this tab onto the new organization's policy", async () => {
    // Both organizations' policies are already cached (a repeat switcher).
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-a' }))
    localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ 'user-1|org-a': 30, 'user-1|org-b': 0 }))
    const storeA = authStore()
    storeA.organizationId = 'org-a'
    const switching = await open(storeA)
    const storeB = authStore()
    storeB.organizationId = 'org-a'
    const other = await open(storeB)
    expect(other.idle.timeoutMinutes.value).toBe(30)

    // Tab 1 switches to org B: it writes the user and the selected org.
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-b' }))
    localStorage.setItem('selected_organization_id', 'org-b')
    switching.store.organizationId = 'org-b'
    for (const key of ['user', 'selected_organization_id']) {
      window.dispatchEvent(new StorageEvent('storage', { key }))
    }
    await flushPromises()

    // Under B's Off, tab 1 records no activity; 31 minutes later tab 2 must
    // still be signed in (under A's 30 minutes it would sign everyone out).
    await vi.advanceTimersByTimeAsync(31 * MINUTE)
    expect(other.idle.timeoutMinutes.value).toBe(0)
    expect(other.store.clearAuth).not.toHaveBeenCalled()
    expect(switching.store.clearAuth).not.toHaveBeenCalled()
  })

  it('keeps its policy when a sign-in attempt in another tab clears the identity', async () => {
    policy(30)
    const { idle } = await open()
    expect(idle.timeoutMinutes.value).toBe(30)

    // Another tab starts a sign-in (it clears the stored identity first), and
    // the attempt may still fail.
    localStorage.removeItem('user')
    localStorage.removeItem('selected_organization_id')
    for (const key of ['user', 'selected_organization_id', SESSION_IDLE_POLICY_KEY]) {
      window.dispatchEvent(new StorageEvent('storage', { key }))
    }

    expect(idle.timeoutMinutes.value).toBe(30)
  })

  it("keys a super admin's policy by the organization its session's /me names", async () => {
    // Viewing another organization: the selected one differs from the token's.
    localStorage.setItem('selected_organization_id', 'org-viewed')
    policy(60)
    const { idle } = await open()

    expect(idle.timeoutMinutes.value).toBe(60)
  })

  it('re-checks the policy every 15 minutes while visible', async () => {
    policy(480)
    await open()
    expect(mocks.checkSessionIdlePolicy).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(15 * MINUTE)
    expect(mocks.checkSessionIdlePolicy).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(15 * MINUTE)
    expect(mocks.checkSessionIdlePolicy).toHaveBeenCalledTimes(2)
  })
})
