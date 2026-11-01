/** @vitest-environment happy-dom */
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, reactive, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useIdentityReviewObservation } from './useIdentityReviewObservation'

const auth = reactive({ user: { id: 'user-a' } as { id: string } | null, organizationId: 'org-a', allowed: true,
  hasPermission: (): boolean => auth.allowed })
const organizations = reactive({ selectedOrgId: 'org-a' })
let session: string | null = 'session-a'
let storedOrg: string | null = null
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => organizations }))
vi.mock('@/services/api', () => ({ AUTH_SESSION_EVENT: 'auth-session-state' }))
vi.mock('@/lib/tabSession', () => ({ currentTabSession: () => session, isTabSessionCurrent: () => session !== null }))
vi.mock('@/lib/browserIdentity', () => ({ readSelectedOrganizationId: () => storedOrg }))

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(done => { resolve = done })
  return { promise, resolve }
}

describe('owned identity review observations', () => {
  let wrapper: VueWrapper | undefined
  beforeEach(() => { auth.user = { id: 'user-a' }; auth.organizationId = 'org-a'; auth.allowed = true; organizations.selectedOrgId = 'org-a'; session = 'session-a'; storedOrg = null })
  afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.restoreAllMocks(); vi.useRealTimers() })

  function harness(refresh: () => Promise<void>, clear = vi.fn()) {
    const query = ref('first')
    const enabled = ref(true)
    let observation!: ReturnType<typeof useIdentityReviewObservation>
    wrapper = mount(defineComponent({ setup() {
      observation = useIdentityReviewObservation({ enabled: () => enabled.value, query: () => query.value, clear, refresh })
      return () => null
    } }))
    return { observation, query, enabled, clear }
  }

  it.each(['org', 'user', 'permission', 'query', 'close'] as const)('clears synchronously and aborts the old %s scope', async change => {
    const fixture = harness(vi.fn(async () => {}))
    const old = fixture.observation.capture()!
    const request = fixture.observation.controller()
    const before = fixture.clear.mock.calls.length
    if (change === 'org') organizations.selectedOrgId = 'org-b'
    if (change === 'user') auth.user = { id: 'user-b' }
    if (change === 'permission') auth.allowed = false
    if (change === 'query') fixture.query.value = 'second'
    if (change === 'close') fixture.enabled.value = false
    expect(fixture.clear.mock.calls.length).toBeGreaterThan(before)
    expect(request.signal.aborted).toBe(true)
    expect(fixture.observation.current(old)).toBe(false)
    await flushPromises()
  })

  it('fences same-user new and ended logical sessions, including before their event arrives', async () => {
    const fixture = harness(vi.fn(async () => {}))
    const old = fixture.observation.capture()!
    session = 'session-b'
    expect(fixture.observation.current(old)).toBe(false)
    const request = fixture.observation.controller()
    window.dispatchEvent(new Event('storage'))
    expect(request.signal.aborted).toBe(true)
    const newer = fixture.observation.capture()!
    expect(newer.session).toBe('session-b')
    session = null
    expect(fixture.observation.current(newer)).toBe(false)
    expect(fixture.observation.capture()).toBeNull()
    await flushPromises()
  })

  it('refuses a different stored workspace without adopting it', () => {
    const fixture = harness(vi.fn(async () => {}))
    storedOrg = 'org-b'
    expect(fixture.observation.capture()).toBeNull()
    window.dispatchEvent(new Event('storage'))
    expect(organizations.selectedOrgId).toBe('org-a')
  })

  it('coalesces clock/focus/visibility and permits only one local invalidation follow-up', async () => {
    vi.useFakeTimers()
    const pending = deferred()
    const refresh = vi.fn().mockImplementationOnce(() => pending.promise).mockResolvedValue(undefined)
    const fixture = harness(refresh)
    expect(refresh).toHaveBeenCalledTimes(1)
    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(60_000)
    expect(refresh).toHaveBeenCalledTimes(1)
    void fixture.observation.refresh(true)
    void fixture.observation.refresh(true)
    pending.resolve()
    await flushPromises()
    expect(refresh).toHaveBeenCalledTimes(2)
  })

  it('pauses a forbidden observation until an explicit retry or scope change', async () => {
    const refresh = vi.fn(async () => {})
    const fixture = harness(refresh)
    await flushPromises()
    fixture.observation.pause()
    window.dispatchEvent(new Event('focus'))
    expect(refresh).toHaveBeenCalledTimes(1)
    await fixture.observation.refresh(true)
    expect(refresh).toHaveBeenCalledTimes(2)
  })

  it('cancels hidden/unmounted work and cannot revive queued follow-ups', async () => {
    vi.useFakeTimers()
    const pending = deferred()
    const refresh = vi.fn(() => pending.promise)
    const fixture = harness(refresh)
    const scope = fixture.observation.capture()!
    const request = fixture.observation.controller()
    void fixture.observation.refresh(true)
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden')
    document.dispatchEvent(new Event('visibilitychange'))
    expect(request.signal.aborted).toBe(true)
    expect(fixture.observation.current(scope)).toBe(false)
    wrapper!.unmount(); wrapper = undefined
    pending.resolve()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(120_000)
    window.dispatchEvent(new Event('focus'))
    expect(refresh).toHaveBeenCalledTimes(1)
  })
})
