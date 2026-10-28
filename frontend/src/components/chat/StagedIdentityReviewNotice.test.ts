/** @vitest-environment happy-dom */
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import StagedIdentityReviewNotice from './StagedIdentityReviewNotice.vue'

const mocks = vi.hoisted(() => ({ list: vi.fn() }))
const auth = reactive({ user: { id: 'user-a' } as { id: string } | null, organizationId: 'org-a', allowed: true, hasPermission: (): boolean => auth.allowed })
const organizations = reactive({ selectedOrgId: 'org-a' })
let session: string | null = 'session-a'
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => organizations }))
vi.mock('@/lib/tabSession', async original => ({ ...(await original<typeof import('@/lib/tabSession')>()), currentTabSession: () => session, isTabSessionCurrent: () => session !== null }))
vi.mock('@/lib/browserIdentity', async original => ({ ...(await original<typeof import('@/lib/browserIdentity')>()), readSelectedOrganizationId: () => null }))
vi.mock('@/services/api', async original => ({ ...(await original<typeof import('@/services/api')>()), contactsService: { listStagedIdentityReviews: mocks.list } }))
vi.mock('./ContactIdentityReviewDialog.vue', () => ({ default: {
  name: 'ContactIdentityReviewDialog', props: ['open', 'contactId', 'canReview', 'canViewStaged', 'initialSection'],
  emits: ['update:open', 'queue-invalidated'], template: '<div data-testid="dialog-stub" />',
} }))

const item = { id: '11111111-1111-4111-8111-111111111111', hold_id: '22222222-2222-4222-8222-222222222222', protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-28T08:00:00Z', read_only: false, eligibility_known: true, decision_available: true, eligibility_reason: 'actionable' }
function response(total: number, actionable = total, chains = actionable) {
  const row = actionable ? item : { ...item, read_only: true, decision_available: false, eligibility_reason: 'no_eligible_member' }
  return { data: { data: { reviews: total ? [row] : [], total, actionable_total: actionable, actionable_chain_total: chains, read_only_total: total - actionable, eligibility_known: true, page: 1, limit: 1 } } }
}
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done }); return { promise, resolve } }

describe('StagedIdentityReviewNotice current availability', () => {
  let wrapper: VueWrapper | undefined
  beforeEach(() => { mocks.list.mockReset(); auth.user = { id: 'user-a' }; auth.allowed = true; organizations.selectedOrgId = 'org-a'; session = 'session-a'; localStorage.clear() })
  afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.restoreAllMocks(); vi.useRealTimers() })
  function open(props = {}) {
    wrapper = mount(StagedIdentityReviewNotice, { props: { canView: true, refreshIntervalMs: 0, ...props }, global: { stubs: { Button: { template: '<button><slot /></button>' } } } })
    return wrapper
  }

  it('never requests or displays protected data without review authority', async () => {
    open({ canView: false }); await flushPromises()
    expect(mocks.list).not.toHaveBeenCalled()
    expect(wrapper!.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
  })
  it('hides only a successful complete known-zero result', async () => {
    mocks.list.mockResolvedValue(response(0)); open(); await flushPromises()
    expect(wrapper!.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
    expect(mocks.list).toHaveBeenCalledWith({ page: 1, limit: 1 }, { organizationId: 'org-a', signal: expect.any(AbortSignal) })
  })
  it.each(['failure', 'old-shape'])('shows unavailable instead of an empty queue on %s', async kind => {
    if (kind === 'failure') mocks.list.mockRejectedValue({ response: { status: 503 } })
    else mocks.list.mockResolvedValue({ data: { data: { reviews: [], total: 0 } } })
    open(); await flushPromises()
    expect(wrapper!.text()).toContain('could not be checked')
    expect(wrapper!.find('[data-testid="staged-identity-review-count"]').exists()).toBe(false)
    expect(wrapper!.find('[data-testid="staged-identity-review-retry"]').exists()).toBe(true)
  })
  it('shows chain and receipt counts separately and retains copies after all targets become owned elsewhere', async () => {
    mocks.list.mockResolvedValueOnce(response(3, 3, 1)).mockResolvedValue(response(3, 0, 0))
    open(); await flushPromises()
    expect(wrapper!.get('[data-testid="staged-identity-review-count"]').text()).toBe('1')
    expect(wrapper!.text()).toContain('3 held messages')
    window.dispatchEvent(new Event('focus')); await flushPromises()
    expect(wrapper!.get('[data-testid="staged-identity-review-count"]').text()).toBe('0')
    expect(wrapper!.text()).toContain('Held messages remain readable')
    expect(wrapper!.text()).not.toContain('onboarded again')
    await wrapper!.get('[data-testid="staged-identity-review-open"]').trigger('click')
    const dialog = wrapper!.findComponent({ name: 'ContactIdentityReviewDialog' })
    expect(dialog.props('contactId')).toBeNull()
    expect(dialog.props('initialSection')).toBe('staged')
  })
  it('removes a previous numeric verdict during a failed refresh and retries accessibly', async () => {
    mocks.list.mockResolvedValueOnce(response(4)).mockRejectedValueOnce(new Error('unavailable')).mockResolvedValue(response(2))
    open(); await flushPromises()
    window.dispatchEvent(new Event('focus')); await flushPromises()
    expect(wrapper!.find('[data-testid="staged-identity-review-count"]').exists()).toBe(false)
    await wrapper!.get('[data-testid="staged-identity-review-retry"]').trigger('click'); await flushPromises()
    expect(wrapper!.get('[data-testid="staged-identity-review-count"]').text()).toBe('2')
  })
  it.each(['org', 'permission', 'session'])('cannot republish a deferred result after %s replacement', async kind => {
    const pending = deferred<ReturnType<typeof response>>()
    mocks.list.mockReturnValueOnce(pending.promise).mockResolvedValue(response(0))
    open()
    const config = mocks.list.mock.calls[0]![1]
    if (kind === 'org') organizations.selectedOrgId = 'org-b'
    if (kind === 'permission') await wrapper!.setProps({ canView: false })
    if (kind === 'session') { session = 'session-b'; window.dispatchEvent(new Event('storage')) }
    expect(config.signal.aborted).toBe(true)
    pending.resolve(response(9)); await flushPromises()
    expect(wrapper!.text()).not.toContain('9 reviews')
    expect(wrapper!.find('[data-testid="staged-identity-review-count"]').exists()).toBe(false)
  })
  it('retires a same-user replaced auth object and coalesces only its current observation', async () => {
    const previous = deferred<ReturnType<typeof response>>()
    const current = deferred<ReturnType<typeof response>>()
    mocks.list.mockReturnValueOnce(previous.promise).mockReturnValueOnce(current.promise)
    open()
    expect(mocks.list).toHaveBeenCalledTimes(1)
    const oldRequest = mocks.list.mock.calls[0]![1]
    auth.user = { id: 'user-a' }
    expect(oldRequest.signal.aborted).toBe(true)
    expect(mocks.list).toHaveBeenCalledTimes(2)
    const newRequest = mocks.list.mock.calls[1]![1]
    expect(newRequest.organizationId).toBe('org-a')
    expect(newRequest.signal.aborted).toBe(false)
    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
    expect(mocks.list).toHaveBeenCalledTimes(2)
    previous.resolve(response(9)); await flushPromises()
    expect(wrapper!.find('[data-testid="staged-identity-review-count"]').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('9 reviews')
    window.dispatchEvent(new Event('focus')); await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(2)
    expect(newRequest.signal.aborted).toBe(false)
    current.resolve(response(2)); await flushPromises()
    expect(wrapper!.get('[data-testid="staged-identity-review-count"]').text()).toBe('2')
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('accepts only scoped local invalidations and never takes a filtered numeric total', async () => {
    mocks.list.mockResolvedValue(response(2)); open(); await flushPromises()
    const scope = { organizationId: 'org-a', userId: 'user-a', session: 'old-session', identity: auth.user, permissions: '[true,true,true]', generation: 1, key: 'other-query' }
    await wrapper!.setProps({ invalidation: scope }); await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(1)
    await wrapper!.setProps({ invalidation: { ...scope, session: 'session-a' } }); await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(2)
    expect(wrapper!.get('[data-testid="staged-identity-review-count"]').text()).toBe('2')
  })
  it('refuses new protected reads immediately when auth permissions change before parent props', async () => {
    const pending = deferred<ReturnType<typeof response>>()
    mocks.list.mockReturnValue(pending.promise)
    open()
    const request = mocks.list.mock.calls[0]![1]
    auth.allowed = false
    expect(request.signal.aborted).toBe(true)
    expect(mocks.list).toHaveBeenCalledTimes(1)
    pending.resolve(response(5)); await flushPromises()
    expect(wrapper!.find('[data-testid="staged-identity-review-count"]').exists()).toBe(false)
    expect(wrapper!.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
    expect(mocks.list).toHaveBeenCalledTimes(1)
  })
})
