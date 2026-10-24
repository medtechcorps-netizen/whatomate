/** @vitest-environment happy-dom */

// useSessionIdle with the real API client (only the stores are stand-ins):
// the delayed session check after another tab's sign-out notice must take
// this tab off an ended session even when the ending tab has already cleared
// the shared identity.

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, reactive } from 'vue'

const mocks = vi.hoisted(() => ({ currentStore: null as any }))

vi.mock('@/stores/auth', () => ({ useAuthStore: () => mocks.currentStore }))
vi.mock('@/stores/calling', () => ({ useCallingStore: () => ({ isOnCall: false }) }))
vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), { loading: vi.fn(), error: vi.fn(), dismiss: vi.fn(), success: vi.fn() }),
}))

const MINUTE = 60_000

class FakeBroadcastChannel {
  static channels = new Set<FakeBroadcastChannel>()
  onmessage: ((event: MessageEvent) => void) | null = null
  constructor(public name: string) {
    FakeBroadcastChannel.channels.add(this)
  }
  postMessage(data: unknown) {
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
const apis: Array<{ disposeForTests: () => void }> = []

describe('useSessionIdle with the real API client', () => {
  // Transform the module graph (i18n included) once.
  beforeAll(async () => {
    await import('./useSessionIdle')
    ;(await import('@/services/api')).disposeForTests()
  }, 60_000)

  beforeEach(() => {
    vi.useFakeTimers({ now: new Date('2026-10-19T09:00:00Z') })
    localStorage.clear()
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-1' }))
    localStorage.setItem('whm_session_id', 'session-1')
    localStorage.setItem('whm_session_idle_minutes', JSON.stringify({ 'user-1|org-1': 30 }))
    localStorage.setItem('whm_last_activity', String(Date.now()))
    FakeBroadcastChannel.channels.clear()
    vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel)
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
    while (apis.length) apis.pop()!.disposeForTests()
    vi.unstubAllGlobals()
    vi.useRealTimers()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
  })

  it('the delayed session check leaves a session that another tab ended', async () => {
    vi.resetModules()
    const { useSessionIdle } = await import('./useSessionIdle')
    apis.push(await import('@/services/api'))
    const store = reactive({
      isAuthenticated: true,
      organizationId: 'org-1',
      clearAuth: vi.fn(),
    })
    mocks.currentStore = store
    const Harness = defineComponent({
      setup() {
        useSessionIdle()
        return () => null
      },
    })
    wrappers.push(mount(Harness))
    await flushPromises()
    await vi.advanceTimersByTimeAsync(5 * MINUTE)

    // Another tab (whose storage missed this tab's activity) signs out for
    // inactivity: it ends the session and clears the shared identity.
    localStorage.setItem('whm_session_id', 'ended:session-1')
    localStorage.removeItem('user')
    new FakeBroadcastChannel('whm-session').postMessage({
      type: 'signed-out',
      reason: 'idle',
      session: 'session-1',
    })
    // This tab's clock disagrees, so it checks the session a little later...
    expect(assignedHref).toBeNull()
    await vi.advanceTimersByTimeAsync(3_000)

    // ...and the real API client takes it off the ended session.
    expect(assignedHref).toBe('/login')
  })
})
