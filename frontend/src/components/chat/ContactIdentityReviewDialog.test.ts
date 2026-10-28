/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactIdentityReviewDialog from './ContactIdentityReviewDialog.vue'
import { reactive } from 'vue'
import type { IdentityReviewRequestScope } from '@/services/api'

const mocks = vi.hoisted(() => ({
  getState: vi.fn(),
  preview: vi.fn(),
  decide: vi.fn(),
  listStaged: vi.fn(),
  listStagedForHold: vi.fn(),
  getStaged: vi.fn(),
  getMedia: vi.fn(),
}))

const auth = reactive({ user: { id: 'user-a' } as { id: string } | null, organizationId: 'org-a', allowed: true, hasPermission: (): boolean => auth.allowed })
const organizations = reactive({ selectedOrgId: 'org-a' })
let logicalSession: string | null = 'session-a'
vi.mock('@/stores/auth', () => ({ useAuthStore: () => auth }))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => organizations }))
vi.mock('@/lib/tabSession', async original => ({ ...(await original<typeof import('@/lib/tabSession')>()), currentTabSession: () => logicalSession, isTabSessionCurrent: () => logicalSession !== null }))
vi.mock('@/lib/browserIdentity', async original => ({ ...(await original<typeof import('@/lib/browserIdentity')>()), readSelectedOrganizationId: () => null }))

function fixtureID(label: string): string {
  let hash = 2166136261
  for (const char of label) hash = Math.imul(hash ^ char.charCodeAt(0), 16777619) >>> 0
  return `00000000-0000-4000-8000-${hash.toString(16).padStart(12, '0')}`
}

// These pre-existing behavioral cases used abbreviated pages and omitted the
// old optional flags. Materialize complete synthetic current-contract pages;
// explicit current-v1 fixtures below bypass this adapter, including refusals.
function fixtureRow(row: Record<string, unknown>) {
  if ('eligibility_known' in row) return row
  return { ...row, read_only: row.read_only === true, eligibility_known: true,
    decision_available: row.read_only !== true, eligibility_reason: row.read_only === true ? 'unsupported' : 'actionable',
    ...(row.media_available === true ? { revision: row.revision || 'a'.repeat(64), media_status: 'ready' } : {}),
  }
}
function fixtureList(result: { data: { data: Record<string, unknown> } }, page: number, limit: number) {
  const data = result.data.data
  if ('eligibility_known' in data) return result
  const rows = (data.reviews as Record<string, unknown>[]).map(fixtureRow)
  const total = data.total as number
  const size = Math.min(limit, Math.max(0, total - (page - 1) * limit))
  while (rows.length < size) rows.push(fixtureRow({ ...rows[0], id: fixtureID(`filler-${page}-${rows.length}`) }))
  const readOnly = (data.read_only_total as number | undefined) ?? rows.filter(row => row.read_only).length
  return { data: { data: { ...data, reviews: rows, page, limit, eligibility_known: true, read_only_total: readOnly,
    actionable_total: total - readOnly, actionable_chain_total: total > readOnly ? 1 : 0 } } }
}
function fixtureDetail(result: { data: { data: Record<string, unknown> } }) {
  return { data: { data: fixtureRow(result.data.data) } }
}

vi.mock('@/services/api', async importOriginal => ({
  ...(await importOriginal<typeof import('@/services/api')>()),
  contactsService: {
    getIdentityReviewState: mocks.getState,
    previewIdentityReview: mocks.preview,
    decideIdentityReview: mocks.decide,
    listStagedIdentityReviews: async (params: { page: number; limit: number }, scope: IdentityReviewRequestScope) => fixtureList(await mocks.listStaged(params, scope), params.page, params.limit),
    listStagedIdentityReviewsForHolds: async (ids: string[], limit: number, scope: IdentityReviewRequestScope, page = 1) => fixtureList(await mocks.listStagedForHold(ids, limit, scope, page), page, limit),
    getStagedIdentityReview: async (id: string, scope: IdentityReviewRequestScope) => fixtureDetail(await mocks.getStaged(id, scope)),
    getStagedIdentityReviewMedia: mocks.getMedia,
  },
}))

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

const allowed = {
  known: true,
  ai_allowed: true,
  blocked: false,
  open_hold_count: 0,
  reason: 'no_open_identity_review',
}

const blocked = {
  known: true,
  ai_allowed: false,
  blocked: true,
  open_hold_count: 1,
  reason: 'identity_review_open',
  latest_hold_id: fixtureID('hold-1'),
  latest_generation: 3,
}

const preview = {
  snapshot: {
    hold_id: fixtureID('hold-1'),
    whatsapp_account_id: 'account-1',
    onboarding_cycle: 2,
    protocol_version: 1,
    supported: true,
    principal_generation: 3,
    disposition: 'open',
    version: 4,
    member_count: 2,
    member_digest: 'a'.repeat(64),
    candidates: [
      { contact_id: 'contact-a', selector_reasons: 1 },
      { contact_id: 'contact-b', selector_reasons: 4 },
    ],
  },
  chain_digest: 'b'.repeat(64),
  union_candidates: [
    { contact_id: 'contact-a', selector_reasons: 1 },
    { contact_id: 'contact-b', selector_reasons: 4 },
  ],
  open_generations: [3],
  routable_contact_ids: ['contact-a', 'contact-b'],
}

function mountDialog(overrides: Record<string, unknown> = {}) {
  return mount(ContactIdentityReviewDialog, {
    props: {
      open: true,
      contactId: 'contact-a',
      contactLabel: 'Amina',
      effectiveState: blocked,
      canReview: true,
      canViewStaged: true,
      ...overrides,
    },
    global: {
      stubs: {
        Dialog: { template: '<div><slot /></div>' },
        DialogContent: { template: '<section><slot /></section>' },
        DialogHeader: { template: '<header><slot /></header>' },
        DialogTitle: { template: '<h2><slot /></h2>' },
        DialogDescription: { template: '<p><slot /></p>' },
        DialogFooter: { template: '<footer><slot /></footer>' },
        ScrollArea: { template: '<div><slot /></div>' },
        Badge: { template: '<span><slot /></span>' },
        Button: { template: '<button><slot /></button>' },
      },
    },
  })
}

describe('ContactIdentityReviewDialog', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    auth.user = { id: 'user-a' }; auth.allowed = true; organizations.selectedOrgId = 'org-a'; logicalSession = 'session-a'; localStorage.clear()
    for (const mock of Object.values(mocks)) mock.mockReset()
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('00000000-0000-4000-8000-000000000123')
    mocks.getState.mockResolvedValue({ data: { data: blocked } })
    mocks.preview.mockResolvedValue({ data: { data: preview } })
    mocks.listStaged.mockResolvedValue({ data: { data: { reviews: [], total: 0 } } })
    mocks.listStagedForHold.mockResolvedValue({ data: { data: { reviews: [], total: 0 } } })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.restoreAllMocks()
  })

  it('shows only safe blocked status when the viewer lacks review authority', async () => {
    wrapper = mountDialog({ canReview: false, canViewStaged: false })
    await flushPromises()

    expect(wrapper.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI blocked')
    expect(wrapper.text()).toContain('No other contact details are visible here')
    expect(wrapper.text()).not.toContain('contact-b')
    expect(mocks.preview).not.toHaveBeenCalled()
  })

  it('rejects an incomplete authorized preview before a decision can be sent', async () => {
    mocks.preview.mockResolvedValueOnce({
      data: { data: { ...preview, snapshot: { ...preview.snapshot, member_count: 3 } } },
    })
    wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.text()).toContain('complete current candidate set could not be verified')
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()
    expect(mocks.decide).not.toHaveBeenCalled()
  })

  it.each([
    ['unknown protocol', { snapshot: { ...preview.snapshot, protocol_version: 2 } }],
    ['invalid member digest', { snapshot: { ...preview.snapshot, member_digest: 'not-a-digest' } }],
    ['invalid selector reason', {
      snapshot: {
        ...preview.snapshot,
        candidates: [
          { contact_id: 'contact-a', selector_reasons: 8 },
          { contact_id: 'contact-b', selector_reasons: 4 },
        ],
      },
    }],
    ['duplicate generation', { open_generations: [3, 3] }],
  ])('fails closed for a %s preview', async (_label, mutation) => {
    mocks.preview.mockResolvedValueOnce({
      data: {
        data: {
          ...preview,
          ...mutation,
        },
      },
    })
    wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.text()).toContain('complete current candidate set could not be verified')
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()
    expect(mocks.decide).not.toHaveBeenCalled()
  })

  it('fails closed when the preview does not list routable targets', async () => {
    const { routable_contact_ids: _omitted, ...withoutRoutable } = preview
    mocks.preview.mockResolvedValueOnce({ data: { data: withoutRoutable } })
    wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.text()).toContain('complete current candidate set could not be verified')
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()
  })

  it('pre-selects a single candidate only when it owns the sender WhatsApp ID', async () => {
    const single = (reasons: number) => ({
      ...preview,
      snapshot: { ...preview.snapshot, member_count: 1, candidates: [{ contact_id: 'contact-b', selector_reasons: reasons }] },
      union_candidates: [{ contact_id: 'contact-b', selector_reasons: reasons }],
      routable_contact_ids: ['contact-b'],
    })
    mocks.preview.mockResolvedValueOnce({ data: { data: single(1) } })
    wrapper = mountDialog()
    await flushPromises()
    expect((wrapper.get('input[name="identity-review-target"]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.find('[data-testid="identity-review-recycled-number-warning"]').exists()).toBe(false)
    wrapper.unmount()

    mocks.preview.mockResolvedValueOnce({ data: { data: single(4) } })
    wrapper = mountDialog()
    await flushPromises()
    const phoneOnly = wrapper.get('input[name="identity-review-target"]')
    expect((phoneOnly.element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="identity-review-recycled-number-warning"]').text()).toContain('can be reassigned')
    expect(wrapper.text()).toContain('Matches by phone')
    await phoneOnly.setValue()
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeUndefined()
  })

  it('never offers a member that belongs to another WhatsApp user', async () => {
    mocks.preview.mockResolvedValueOnce({ data: { data: { ...preview, routable_contact_ids: ['contact-a'] } } })
    wrapper = mountDialog()
    await flushPromises()

    const inputs = wrapper.findAll('input[name="identity-review-target"]')
    expect(inputs[0].attributes('disabled')).toBeUndefined()
    expect(inputs[1].attributes('disabled')).toBeDefined()
    expect(wrapper.findAll('[data-testid="identity-review-candidate-other-user"]')).toHaveLength(1)
    expect(wrapper.findAll('[data-testid="identity-review-candidate"]')[1].text()).toContain('Another WhatsApp user')
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()
  })

  it('shows every held copy of the open review chain, oldest first, with a count', async () => {
    mocks.preview.mockResolvedValueOnce({ data: { data: { ...preview, open_hold_ids: [fixtureID('hold-0'), fixtureID('hold-1')] } } })
    const held = [
      { id: fixtureID('staged-hi'), hold_id: fixtureID('hold-0'), message_type: 'text', received_at: '2026-10-06T08:00:00Z', content: 'hi' },
      { id: fixtureID('staged-order'), hold_id: fixtureID('hold-0'), message_type: 'text', received_at: '2026-10-06T08:01:00Z', content: 'Order 42: two boxes, deliver Friday' },
      { id: fixtureID('staged-photo'), hold_id: fixtureID('hold-1'), message_type: 'image', received_at: '2026-10-06T08:02:00Z', content: '', media_mime_type: 'image/jpeg', media_filename: 'receipt.jpg' },
    ]
    mocks.listStagedForHold.mockResolvedValueOnce({
      data: {
        data: {
          reviews: held.map(item => ({
            id: item.id, hold_id: item.hold_id, message_type: item.message_type, received_at: item.received_at,
            protocol_version: 1, status: 'pending', read_only: false,
          })),
          total: 3,
        },
      },
    })
    mocks.getStaged.mockImplementation(async (id: string) => {
      const copy = held.find(item => item.id === id)!
      return { data: { data: { ...copy, protocol_version: 1, status: 'pending', read_only: false, media_available: copy.message_type !== 'text' } } }
    })
    wrapper = mountDialog()
    await flushPromises()

    expect(mocks.listStagedForHold).toHaveBeenCalledWith([fixtureID('hold-0'), fixtureID('hold-1')], 25, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }), 1)
    expect(mocks.getStaged).toHaveBeenCalledTimes(3)
    const copies = wrapper.findAll('[data-testid="identity-review-held-copy"]')
    expect(copies).toHaveLength(3)
    expect(copies[0].text()).toContain('hi')
    expect(copies[1].text()).toContain('Order 42: two boxes, deliver Friday')
    expect(copies[2].get('[data-testid="identity-review-held-copy-media"]').text()).toContain('image/jpeg · receipt.jpg')
    expect(copies[2].text()).not.toContain('(no text)')
    expect(wrapper.get('[data-testid="identity-review-held-count"]').text()).toContain('3 held messages, oldest first')
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('Read these held copies before you confirm')

    await copies[2].get('button').trigger('click')
    await flushPromises()
    expect(mocks.getStaged).toHaveBeenLastCalledWith(fixtureID('staged-photo'), expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.text()).toContain('Load protected media')
  })

  it('says how many held copies remain beyond the ones shown', async () => {
    mocks.listStagedForHold.mockResolvedValueOnce({
      data: {
        data: {
          reviews: [{ id: fixtureID('staged-first'), hold_id: fixtureID('hold-1'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false }],
          total: 30,
        },
      },
    })
    mocks.getStaged.mockResolvedValueOnce({
      data: { data: { id: fixtureID('staged-first'), hold_id: fixtureID('hold-1'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false, content: 'first', media_available: false } },
    })
    wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.get('[data-testid="identity-review-held-count"]').text()).toContain('30 held messages')
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('5 more held for this review')
  })

  it('explains a review without a held copy and never reads the queue without access', async () => {
    wrapper = mountDialog()
    await flushPromises()
    expect(mocks.listStagedForHold).toHaveBeenCalledWith([fixtureID('hold-1')], 25, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }), 1)
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('No held copy for this review')
    expect(mocks.getStaged).not.toHaveBeenCalled()
    wrapper.unmount()

    wrapper = mountDialog({ canViewStaged: false })
    await flushPromises()
    expect(mocks.listStagedForHold).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('needs access')
  })

  it.each([
    [422, 'This contact belongs to a different WhatsApp user and cannot receive this sender',
      'This contact belongs to a different WhatsApp user and cannot receive this sender'],
    [409, 'Identity review state changed; reload and try again',
      'This review changed and has been reloaded; choose again.'],
  ])('drops a %s-refused request and re-reads the review instead of resending it', async (status, serverMessage, shown) => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000001')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000002')
    mocks.decide
      .mockRejectedValueOnce({ isAxiosError: true, response: { status, data: { message: serverMessage } } })
      .mockResolvedValueOnce({ data: { data: { disposition: 'future_routing' } } })
    wrapper = mountDialog()
    await flushPromises()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    // decide() awaits the real request digest before posting.
    await vi.waitFor(() => {
      expect(mocks.decide).toHaveBeenCalledTimes(1)
      expect(mocks.preview).toHaveBeenCalledTimes(2)
      expect(wrapper!.find('[role="alert"]').exists()).toBe(true)
    })
    expect(wrapper.get('[role="alert"]').text()).toContain(shown)
    expect(wrapper.emitted('resolved')).toBeUndefined()
    expect((wrapper.findAll('input[name="identity-review-target"]')[1].element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => expect(mocks.decide).toHaveBeenCalledTimes(2))
    expect(mocks.decide.mock.calls[0][1].request_id).toBe('00000000-0000-4000-8000-000000000001')
    expect(mocks.decide.mock.calls[1][1].request_id).toBe('00000000-0000-4000-8000-000000000002')
  })

  it('does not claim a reload when re-reading the refused review fails', async () => {
    mocks.getState
      .mockResolvedValueOnce({ data: { data: blocked } })
      .mockRejectedValueOnce(new Error('network down'))
    mocks.decide.mockRejectedValueOnce({
      isAxiosError: true,
      response: { status: 409, data: { message: 'Identity review state changed; reload and try again' } },
    })
    wrapper = mountDialog()
    await flushPromises()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => expect(mocks.getState).toHaveBeenCalledTimes(2))
    await flushPromises()
    const alert = wrapper.get('[role="alert"]').text()
    expect(alert).not.toContain('has been reloaded')
    expect(alert).toContain('network down')
    expect(wrapper.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI blocked')
  })

  it('reports a review that another reviewer resolved before this decision', async () => {
    mocks.getState
      .mockResolvedValueOnce({ data: { data: blocked } })
      .mockResolvedValueOnce({ data: { data: allowed } })
    mocks.decide.mockRejectedValueOnce({
      isAxiosError: true,
      response: { status: 409, data: { message: 'Identity review state changed; reload and try again' } },
    })
    wrapper = mountDialog()
    await flushPromises()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => expect(wrapper!.emitted('resolved')).toEqual([[allowed]]))
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI allowed')
  })

  it('sends at most 50 open holds and opens the review-filtered queue for the rest', async () => {
    const openHoldIDs = Array.from({ length: 60 }, (_, index) => fixtureID(`hold-${index}`))
    mocks.preview.mockResolvedValueOnce({ data: { data: { ...preview, open_hold_ids: openHoldIDs } } })
    mocks.listStagedForHold
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{ id: fixtureID('staged-first'), hold_id: fixtureID('hold-10'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false }],
            total: 30,
          },
        },
      })
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{ id: fixtureID('staged-first'), hold_id: fixtureID('hold-10'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false }],
            total: 30,
          },
        },
      })
    mocks.getStaged.mockResolvedValueOnce({
      data: { data: { id: fixtureID('staged-first'), hold_id: fixtureID('hold-10'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false, content: 'first', media_available: false } },
    })
    wrapper = mountDialog()
    await flushPromises()

    const sentHoldIDs = mocks.listStagedForHold.mock.calls[0][0]
    expect(sentHoldIDs).toHaveLength(50)
    expect(sentHoldIDs).toEqual(openHoldIDs.slice(-50))
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('5 more held for this review')
    expect(wrapper.get('[data-testid="identity-review-held-unlisted-generations"]').text())
      .toContain('10 older open review generations are not listed here')

    await wrapper.get('[data-testid="identity-review-held-open-queue"]').trigger('click')
    await flushPromises()
    expect(mocks.listStagedForHold).toHaveBeenLastCalledWith(openHoldIDs.slice(-50), 100, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }), 1)
    expect(wrapper.get('[data-testid="staged-review-filter"]').text()).toContain("this review's held messages")
    expect(wrapper.emitted('queue-invalidated')).toBeUndefined()

    await wrapper.get('[data-testid="staged-review-filter"] button').trigger('click')
    await flushPromises()
    expect(mocks.listStaged).toHaveBeenLastCalledWith({ page: 1, limit: 100 }, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.find('[data-testid="staged-review-filter"]').exists()).toBe(false)
  })

  it('explains a read-only review instead of asking for a reload', async () => {
    const readOnlyMessage = 'This identity review is read-only: no decision can resolve it until the WhatsApp number is onboarded again.'
    mocks.preview.mockRejectedValueOnce({
      isAxiosError: true,
      response: { status: 409, data: { message: readOnlyMessage, error_type: 'identity_review_read_only' } },
    })
    wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.get('[data-testid="identity-review-read-only"]').text()).toBe(readOnlyMessage)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI blocked')
    expect(wrapper.find('[data-testid="identity-review-decision"]').exists()).toBe(false)
  })

  it('reuses the exact idempotency request after an ambiguous decision failure', async () => {
    mocks.decide
      .mockRejectedValueOnce(new Error('connection closed before confirmation'))
      .mockResolvedValueOnce({ data: { data: { disposition: 'future_routing' } } })
    mocks.getState
      .mockResolvedValueOnce({ data: { data: blocked } })
      .mockResolvedValueOnce({ data: { data: allowed } })
    wrapper = mountDialog()
    await flushPromises()

    const candidates = wrapper.findAll('input[name="identity-review-target"]')
    await candidates[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => {
      expect(mocks.decide).toHaveBeenCalledTimes(1)
      expect(wrapper!.get('[role="alert"]').text()).toContain('connection closed before confirmation')
      expect(wrapper!.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeUndefined()
    })
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => {
      expect(mocks.decide).toHaveBeenCalledTimes(2)
      expect(wrapper!.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI allowed')
    })

    expect(mocks.decide).toHaveBeenCalledTimes(2)
    const first = mocks.decide.mock.calls[0][1]
    const second = mocks.decide.mock.calls[1][1]
    expect(second).toEqual(first)
    expect(first).toMatchObject({
      request_id: '00000000-0000-4000-8000-000000000123',
      target_contact_id: 'contact-b',
      hold_id: fixtureID('hold-1'),
      expected_version: 4,
      expected_member_digest: 'a'.repeat(64),
      expected_chain_digest: 'b'.repeat(64),
      request_digest: expect.stringMatching(/^[a-f0-9]{64}$/),
    })
    expect(wrapper.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI allowed')
  })

  it('creates and posts one decision when concurrent clicks race the request digest', async () => {
    const digest = deferred<ArrayBuffer>()
    vi.spyOn(crypto.subtle, 'digest').mockReturnValueOnce(digest.promise)
    mocks.decide.mockResolvedValueOnce({ data: { data: { disposition: 'future_routing' } } })
    mocks.getState
      .mockResolvedValueOnce({ data: { data: blocked } })
      .mockResolvedValueOnce({ data: { data: allowed } })
    wrapper = mountDialog()
    await flushPromises()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    const decision = wrapper.get('[data-testid="identity-review-decision"]')
    void decision.trigger('click')
    void decision.trigger('click')
    await Promise.resolve()

    expect(crypto.randomUUID).toHaveBeenCalledTimes(1)
    expect(mocks.decide).not.toHaveBeenCalled()

    digest.resolve(new Uint8Array(32).buffer)
    await flushPromises()

    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it('abandons a hashed decision without posting after the dialog closes', async () => {
    const digest = deferred<ArrayBuffer>()
    vi.spyOn(crypto.subtle, 'digest').mockReturnValueOnce(digest.promise)
    wrapper = mountDialog()
    await flushPromises()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    void wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await Promise.resolve()
    await wrapper.setProps({ open: false })

    digest.resolve(new Uint8Array(32).buffer)
    await flushPromises()

    expect(crypto.randomUUID).toHaveBeenCalledTimes(1)
    expect(mocks.decide).not.toHaveBeenCalled()
  })

  it('ignores a stale status response after the selected contact changes', async () => {
    const first = deferred<{ data: { data: typeof blocked } }>()
    mocks.getState
      .mockReturnValueOnce(first.promise)
      .mockResolvedValueOnce({ data: { data: allowed } })
    wrapper = mountDialog({ canReview: false, canViewStaged: false })
    await wrapper.setProps({ contactId: 'contact-b', effectiveState: allowed })
    await flushPromises()
    first.resolve({ data: { data: blocked } })
    await flushPromises()

    expect(wrapper.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI allowed')
    expect(mocks.getState).toHaveBeenNthCalledWith(2, 'contact-b', expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
  })

  it('loads staged detail only through the protected review service', async () => {
    mocks.listStaged.mockResolvedValueOnce({
      data: {
        data: {
          reviews: [{
            id: fixtureID('staged-1'),
            hold_id: fixtureID('hold-1'),
            protocol_version: 1,
            revision: 'c'.repeat(64),
            status: 'pending',
            message_type: 'image',
            received_at: '2026-09-06T00:00:00Z',
          }],
          total: 1,
        },
      },
    })
    mocks.getStaged.mockResolvedValueOnce({
      data: {
        data: {
          id: fixtureID('staged-1'),
          hold_id: fixtureID('hold-1'),
          protocol_version: 1,
          revision: 'c'.repeat(64),
          status: 'pending',
          message_type: 'image',
          received_at: '2026-09-06T00:00:00Z',
          content: 'Sanitized caption',
          media_available: true,
        },
      },
    })
    wrapper = mountDialog()
    await flushPromises()
    const stagedButton = wrapper.findAll('button').find(button => button.text().includes('Protected staged queue'))
    await stagedButton!.trigger('click')
    await flushPromises()
    const item = wrapper.findAll('button').find(button => button.text().includes('image'))
    await item!.trigger('click')
    await flushPromises()

    expect(mocks.listStaged).toHaveBeenCalledWith({ page: 1, limit: 100 }, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(mocks.getStaged).toHaveBeenCalledWith(fixtureID('staged-1'), expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.text()).toContain('Sanitized caption')
  })

  it.each([
    ['image/svg+xml', '<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>', undefined, 'staged-media'],
    ['image/png', 'png bytes', undefined, 'staged-media.png'],
    ['application/pdf', 'pdf bytes', 'lab-report.pdf', 'lab-report.pdf'],
  ])('hands staged %s media to the browser only as an opaque download', async (type, body, filename, downloadName) => {
    const createURL = vi.fn<(value: Blob | MediaSource) => string>().mockReturnValue('blob:staged-media')
    const revokeURL = vi.fn<(url: string) => void>()
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = createURL
      static revokeObjectURL = revokeURL
    })
    const item = {
      id: fixtureID('staged-1'),
      hold_id: fixtureID('hold-1'),
      protocol_version: 1,
      revision: 'c'.repeat(64),
      status: 'pending',
      message_type: 'image',
      received_at: '2026-09-06T00:00:00Z',
    }
    mocks.listStaged.mockResolvedValueOnce({ data: { data: { reviews: [item], total: 1 } } })
    mocks.getStaged.mockResolvedValueOnce({
      data: { data: { ...item, content: '', media_available: true, media_mime_type: type, media_filename: filename } },
    })
    mocks.getMedia.mockResolvedValueOnce({ data: new Blob([body], { type }) })

    try {
      wrapper = mountDialog()
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text().includes('Protected staged queue'))!.trigger('click')
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text().includes('image'))!.trigger('click')
      await flushPromises()
      await wrapper.findAll('button').find(button => button.text().includes('Load protected media'))!.trigger('click')
      await flushPromises()

      expect(mocks.getMedia).toHaveBeenCalledWith(fixtureID('staged-1'), 'c'.repeat(64), expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
      // The object URL shares this app's origin but none of the server's
      // headers: "Open link in new tab" must download the bytes, never
      // render a customer SVG as a page that runs script.
      const blob = createURL.mock.calls[0][0] as Blob
      expect(blob.type).toBe('application/octet-stream')
      expect(await blob.text()).toBe(body)
      const link = wrapper.get('a[download]')
      expect(link.attributes('href')).toBe('blob:staged-media')
      expect(link.attributes('download')).toBe(downloadName)

      wrapper.unmount()
      wrapper = null
      expect(revokeURL).toHaveBeenCalledWith('blob:staged-media')
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('pages through every protected staged review beyond the API limit', async () => {
    mocks.listStaged
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{
              id: fixtureID('staged-page-one'),
              hold_id: fixtureID('hold-1'),
              protocol_version: 1,
              status: 'pending',
              message_type: 'page-one-message',
              received_at: '2026-09-06T00:00:00Z',
            }],
            total: 101,
          },
        },
      })
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{
              id: fixtureID('staged-page-two'),
              hold_id: fixtureID('hold-2'),
              protocol_version: 1,
              status: 'pending',
              message_type: 'page-two-message',
              received_at: '2026-09-05T00:00:00Z',
            }],
            total: 101,
          },
        },
      })
    wrapper = mountDialog()
    await flushPromises()

    const stagedButton = wrapper.findAll('button').find(button => button.text().includes('Protected staged queue'))
    await stagedButton!.trigger('click')
    await flushPromises()

    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 1–100 of 101 · Page 1 of 2')
    expect(wrapper.text()).toContain('page-one-message')
    await wrapper.get('[data-testid="staged-next-page"]').trigger('click')
    await flushPromises()

    expect(mocks.listStaged).toHaveBeenNthCalledWith(2, { page: 2, limit: 100 }, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 101–101 of 101 · Page 2 of 2')
    expect(wrapper.text()).toContain('page-two-message')
    expect(wrapper.text()).not.toContain('page-one-message')
  })

  it('supports previous-page navigation and resets pagination when the dialog context resets', async () => {
    mocks.listStaged.mockImplementation(({ page }: { page: number }) => Promise.resolve({
      data: {
        data: {
          reviews: [{
            id: fixtureID(`staged-page-${page}`),
            hold_id: fixtureID(`hold-${page}`),
            protocol_version: 1,
            status: 'pending',
            message_type: `page-${page}-message`,
            received_at: '2026-09-06T00:00:00Z',
          }],
          total: 101,
        },
      },
    }))
    wrapper = mountDialog()
    await flushPromises()

    const openQueue = () => wrapper!.findAll('button').find(button => button.text().includes('Protected staged queue'))!
    await openQueue().trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="staged-next-page"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="staged-previous-page"]').trigger('click')
    await flushPromises()

    expect(mocks.listStaged).toHaveBeenNthCalledWith(3, { page: 1, limit: 100 }, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Page 1 of 2')

    await wrapper.get('[data-testid="staged-next-page"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Page 2 of 2')
    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    await flushPromises()
    await openQueue().trigger('click')
    await flushPromises()

    expect(mocks.listStaged).toHaveBeenLastCalledWith({ page: 1, limit: 100 }, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Page 1 of 2')
  })

  it('does not let a stale page response overwrite a reset dialog context', async () => {
    const stalePageTwo = deferred<{
      data: { data: { reviews: Array<Record<string, unknown>>; total: number } }
    }>()
    mocks.listStaged
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{
              id: fixtureID('initial-page-one'),
              hold_id: fixtureID('hold-1'),
              protocol_version: 1,
              status: 'pending',
              message_type: 'initial-page-one',
              received_at: '2026-09-06T00:00:00Z',
            }],
            total: 101,
          },
        },
      })
      .mockReturnValueOnce(stalePageTwo.promise)
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{
              id: fixtureID('fresh-page-one'),
              hold_id: fixtureID('hold-fresh'),
              protocol_version: 1,
              status: 'pending',
              message_type: 'fresh-page-one',
              received_at: '2026-09-07T00:00:00Z',
            }],
            total: 1,
          },
        },
      })
    wrapper = mountDialog()
    await flushPromises()

    let stagedButton = wrapper.findAll('button').find(button => button.text().includes('Protected staged queue'))
    await stagedButton!.trigger('click')
    await flushPromises()
    void wrapper.get('[data-testid="staged-next-page"]').trigger('click')
    await Promise.resolve()

    await wrapper.setProps({ contactId: 'contact-b' })
    await flushPromises()
    stagedButton = wrapper.findAll('button').find(button => button.text().includes('Protected staged queue'))
    await stagedButton!.trigger('click')
    await flushPromises()

    stalePageTwo.resolve({
      data: {
        data: {
          reviews: [{
            id: fixtureID('stale-page-two'),
            hold_id: fixtureID('hold-stale'),
            protocol_version: 1,
            status: 'pending',
            message_type: 'stale-page-two',
            received_at: '2026-09-05T00:00:00Z',
          }],
          total: 101,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('fresh-page-one')
    expect(wrapper.text()).not.toContain('stale-page-two')
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 1–1 of 1 · Page 1 of 1')
  })
})

describe('ContactIdentityReviewDialog workspace staged entry point', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    auth.user = { id: 'user-a' }; auth.allowed = true; organizations.selectedOrgId = 'org-a'; logicalSession = 'session-a'; localStorage.clear()
    for (const mock of Object.values(mocks)) mock.mockReset()
    mocks.listStaged.mockResolvedValue({
      data: {
        data: {
          reviews: [{
            id: fixtureID('staged-new-sender'),
            hold_id: fixtureID('hold-new-sender'),
            protocol_version: 1,
            status: 'pending',
            message_type: 'text',
            received_at: '2026-01-15T08:30:00Z',
            read_only: true,
          }],
          total: 1,
          read_only_total: 1,
        },
      },
    })
    mocks.getStaged.mockResolvedValue({
      data: {
        data: {
          id: fixtureID('staged-new-sender'),
          hold_id: fixtureID('hold-new-sender'),
          protocol_version: 1,
          status: 'pending',
          message_type: 'text',
          received_at: '2026-01-15T08:30:00Z',
          read_only: true,
          content: 'Saw your ad, price?',
          media_available: false,
        },
      },
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.restoreAllMocks()
  })

  it('opens straight into the protected queue without any contact request', async () => {
    wrapper = mountDialog({ contactId: null, effectiveState: null, initialSection: 'staged' })
    await flushPromises()

    expect(mocks.getState).not.toHaveBeenCalled()
    expect(mocks.preview).not.toHaveBeenCalled()
    expect(mocks.listStaged).toHaveBeenCalledWith({ page: 1, limit: 100 }, expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.text()).toContain('Held WhatsApp messages')
    expect(wrapper.text()).not.toContain('Contact review')
    expect(wrapper.find('[data-testid="identity-review-effective-state"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="identity-review-decision"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 1–1 of 1')
    expect(wrapper.emitted('queue-invalidated')).toEqual([[expect.objectContaining({ organizationId: 'org-a', userId: 'user-a', session: 'session-a' })]])

    const item = wrapper.findAll('button').find(button => button.text().includes('2026-01-15T08:30:00Z'))
    await item!.trigger('click')
    await flushPromises()

    expect(mocks.getStaged).toHaveBeenCalledWith(fixtureID('staged-new-sender'), expect.objectContaining({ organizationId: 'org-a', signal: expect.any(AbortSignal) }))
    expect(wrapper.text()).toContain('Saw your ad, price?')
    const guidance = wrapper.get('[data-testid="staged-identity-review-guidance"]').text()
    expect(guidance).toContain('held copy remains protected')
    expect(guidance).toContain('verified sender evidence')
    expect(guidance).not.toContain('onboarded again')
    // The sender may match a contact by phone, and an earlier held copy can
    // keep later messages held, so neither claim is made.
    expect(guidance).not.toContain('matches no single contact')
    expect(guidance).not.toContain('next message opens a normal conversation')
    expect(guidance).not.toContain('Identity review to choose')
  })

  it('marks read-only items in the list and points decidable ones to the flagged contact', async () => {
    mocks.listStaged.mockResolvedValue({
      data: {
        data: {
          reviews: [
            { id: fixtureID('staged-conflict'), hold_id: fixtureID('hold-conflict'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-01-15T09:00:00Z', read_only: false },
            { id: fixtureID('staged-new-sender'), hold_id: fixtureID('hold-new-sender'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-01-15T08:30:00Z', read_only: true },
          ],
          total: 2,
          read_only_total: 1,
        },
      },
    })
    mocks.getStaged.mockResolvedValue({
      data: {
        data: {
          id: fixtureID('staged-conflict'), hold_id: fixtureID('hold-conflict'), protocol_version: 1, status: 'pending', message_type: 'text',
          received_at: '2026-01-15T09:00:00Z', read_only: false, content: 'Conflict', media_available: false,
        },
      },
    })
    wrapper = mountDialog({ contactId: null, effectiveState: null, initialSection: 'staged' })
    await flushPromises()

    expect(wrapper.findAll('[data-testid="staged-read-only-badge"]')).toHaveLength(1)
    const conflict = wrapper.findAll('button').find(button => button.text().includes('2026-01-15T09:00:00Z'))
    expect(conflict!.text()).not.toContain('Read-only')
    await conflict!.trigger('click')
    await flushPromises()

    const guidance = wrapper.get('[data-testid="staged-identity-review-guidance"]').text()
    expect(guidance).toContain('complete current candidate set')
    expect(guidance).not.toContain('read-only for now')
  })

  it('stays on the contact review when the viewer cannot read the protected queue', async () => {
    mocks.getState.mockResolvedValue({ data: { data: blocked } })
    mocks.preview.mockResolvedValue({ data: { data: preview } })
    wrapper = mountDialog({ initialSection: 'staged', canViewStaged: false })
    await flushPromises()

    expect(mocks.listStaged).not.toHaveBeenCalled()
    expect(mocks.getState).toHaveBeenCalled()
  })
})

describe('ContactIdentityReviewDialog current observation fences', () => {
  let wrapper: VueWrapper | null = null
  const row = { id: fixtureID('current-event'), hold_id: fixtureID('current-hold'), protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-28T08:00:00Z', read_only: false, eligibility_known: true, decision_available: true, eligibility_reason: 'actionable' }
  function currentPage(total = 1, page = 1) {
    const size = Math.min(100, Math.max(0, total - (page - 1) * 100))
    return { data: { data: { reviews: Array.from({ length: size }, (_, index) => ({ ...row, id: index === 0 && page === 1 ? row.id : fixtureID(`current-${page}-${index}`) })), total, page, limit: 100, eligibility_known: true, actionable_total: total, actionable_chain_total: total ? 1 : 0, read_only_total: 0 } } }
  }
  const detail = { ...row, content: 'fresh protected copy', media_available: false }
  async function openCopy() {
    wrapper = mountDialog({ contactId: null, initialSection: 'staged' })
    await flushPromises()
    await wrapper.findAll('button').find(button => button.text().includes(row.received_at))!.trigger('click')
    await flushPromises()
  }
  beforeEach(() => {
    for (const mock of Object.values(mocks)) mock.mockReset()
    auth.user = { id: 'user-a' }; auth.allowed = true; organizations.selectedOrgId = 'org-a'; logicalSession = 'session-a'; localStorage.clear()
    mocks.listStaged.mockResolvedValue(currentPage())
    mocks.getStaged.mockResolvedValue({ data: { data: detail } })
    mocks.getState.mockResolvedValue({ data: { data: blocked } })
    mocks.preview.mockResolvedValue({ data: { data: preview } })
    mocks.listStagedForHold.mockResolvedValue({ data: { data: { reviews: [], total: 0 } } })
  })
  afterEach(() => { wrapper?.unmount(); wrapper = null; vi.restoreAllMocks(); vi.unstubAllGlobals() })

  it('keeps a freshly authorized unknown copy without showing decision guidance', async () => {
    mocks.getStaged.mockResolvedValue({ data: { data: { ...detail, eligibility_known: false, decision_available: false, eligibility_reason: 'unavailable', read_only: null } } })
    await openCopy()
    expect(wrapper!.text()).toContain('fresh protected copy')
    const guidance = wrapper!.get('[data-testid="staged-identity-review-guidance"]').text()
    expect(guidance).toContain('could not be checked')
    expect(guidance).not.toContain('choose')
    expect(wrapper!.find('[data-testid="identity-review-decision"]').exists()).toBe(false)
    expect(wrapper!.text()).toContain('Retry availability')
  })

  it.each([403, 404, 503])('removes the previous protected copy on a fresh detail %s', async status => {
    await openCopy()
    expect(wrapper!.text()).toContain('fresh protected copy')
    mocks.getStaged.mockRejectedValue({ response: { status } })
    window.dispatchEvent(new Event('focus')); await flushPromises()
    expect(wrapper!.text()).not.toContain('fresh protected copy')
    expect(wrapper!.text()).not.toContain('No staged reviews')
  })

  it('ignores late old-org pages and sends the new request to the captured new org', async () => {
    const old = deferred<ReturnType<typeof currentPage>>()
    mocks.listStaged.mockReturnValueOnce(old.promise).mockResolvedValue(currentPage(0))
    wrapper = mountDialog({ contactId: null, initialSection: 'staged' })
    const config = mocks.listStaged.mock.calls[0]![1]
    organizations.selectedOrgId = 'org-b'
    expect(config.signal.aborted).toBe(true)
    old.resolve(currentPage(10)); await flushPromises()
    expect(mocks.listStaged).toHaveBeenLastCalledWith({ page: 1, limit: 100 }, expect.objectContaining({ organizationId: 'org-b' }))
    expect(wrapper!.text()).not.toContain('of 10')
    const emitted = wrapper!.emitted('queue-invalidated') ?? []
    expect(emitted.every(([scope]) => (scope as { organizationId: string }).organizationId === 'org-b')).toBe(true)
  })

  it('never creates a blob URL after permission loss during protected-media retrieval', async () => {
    const media = deferred<{ data: Blob }>()
    const createURL = vi.fn(() => 'blob:never')
    vi.stubGlobal('URL', class extends URL { static createObjectURL = createURL; static revokeObjectURL = vi.fn() })
    mocks.getStaged.mockResolvedValue({ data: { data: { ...detail, message_type: 'image', media_available: true, revision: 'a'.repeat(64), media_status: 'ready' } } })
    mocks.getMedia.mockReturnValue(media.promise)
    await openCopy()
    await wrapper!.findAll('button').find(button => button.text().includes('Load protected media'))!.trigger('click')
    const config = mocks.getMedia.mock.calls[0]![2]
    await wrapper!.setProps({ canViewStaged: false, canReview: false })
    expect(config.signal.aborted).toBe(true)
    media.resolve({ data: new Blob(['private']) }); await flushPromises()
    expect(createURL).not.toHaveBeenCalled()
    expect(wrapper!.text()).not.toContain('fresh protected copy')
  })

  it('cancels queued held-copy fanout after four started reads when scope changes', async () => {
    const pending = deferred<{ data: { data: typeof detail } }>()
    const items = Array.from({ length: 10 }, (_, index) => ({ ...row, id: fixtureID(`fanout-${index}`), hold_id: preview.snapshot.hold_id }))
    mocks.listStagedForHold.mockResolvedValueOnce({ data: { data: { reviews: items, total: 10, page: 1, limit: 25, eligibility_known: true, actionable_total: 10, actionable_chain_total: 1, read_only_total: 0 } } })
    mocks.getStaged.mockReturnValue(pending.promise)
    wrapper = mountDialog(); await flushPromises()
    expect(mocks.getStaged).toHaveBeenCalledTimes(4)
    await wrapper.setProps({ canViewStaged: false })
    pending.resolve({ data: { data: detail } }); await flushPromises()
    expect(mocks.getStaged).toHaveBeenCalledTimes(4)
    expect(wrapper.findAll('[data-testid="identity-review-held-copy"]')).toHaveLength(0)
  })

  it('does not let pending held-copy reads delay or authorize the complete decision preview', async () => {
    const pending = deferred<{ data: { data: typeof detail } }>()
    const copy = { ...row, hold_id: preview.snapshot.hold_id }
    mocks.listStagedForHold.mockResolvedValueOnce({ data: { data: { reviews: [copy], total: 1, page: 1, limit: 25, eligibility_known: true, actionable_total: 1, actionable_chain_total: 1, read_only_total: 0 } } })
    mocks.getStaged.mockReturnValue(pending.promise)
    wrapper = mountDialog(); await flushPromises()
    expect(mocks.getStaged).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('Loading the held messages')
    expect(wrapper.findAll('[data-testid="identity-review-candidate"]')).toHaveLength(2)
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()
    await wrapper.findAll('input[name="identity-review-target"]')[1]!.setValue()
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeUndefined()
    const request = mocks.getStaged.mock.calls[0]![1]
    wrapper.unmount(); wrapper = null
    expect(request.signal.aborted).toBe(true)
    pending.resolve({ data: { data: detail } }); await flushPromises()
    expect(mocks.decide).not.toHaveBeenCalled()
  })

  it('fences a same-user replacement while hashing, without posting automatically', async () => {
    const hashing = deferred<ArrayBuffer>()
    vi.spyOn(crypto.subtle, 'digest').mockReturnValueOnce(hashing.promise)
    wrapper = mountDialog(); await flushPromises()
    await wrapper.findAll('input[name="identity-review-target"]')[1]!.setValue()
    void wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await Promise.resolve()
    logicalSession = 'new-session'
    window.dispatchEvent(new Event('storage'))
    hashing.resolve(new Uint8Array(32).buffer); await flushPromises()
    expect(mocks.decide).not.toHaveBeenCalled()
  })

  it('does not publish or replay an acknowledged mutation after its org was replaced', async () => {
    const result = deferred<{ data: object }>()
    mocks.decide.mockReturnValue(result.promise)
    wrapper = mountDialog(); await flushPromises()
    await wrapper.findAll('input[name="identity-review-target"]')[1]!.setValue()
    void wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => expect(mocks.decide).toHaveBeenCalledTimes(1))
    organizations.selectedOrgId = 'org-b'
    result.resolve({ data: {} }); await flushPromises()
    expect(wrapper.emitted('resolved')).toBeUndefined()
    expect(mocks.decide).toHaveBeenCalledTimes(1)
    expect(mocks.decide.mock.calls[0]![2]).toEqual({ organizationId: 'org-a' })
  })

  it('makes at most one clamp request when successive observations shrink the queue', async () => {
    mocks.listStaged.mockResolvedValueOnce(currentPage(201)).mockResolvedValueOnce(currentPage(201, 2))
      .mockResolvedValueOnce(currentPage(101, 3)).mockResolvedValueOnce(currentPage(1, 2))
    wrapper = mountDialog({ contactId: null, initialSection: 'staged' }); await flushPromises()
    await wrapper.get('[data-testid="staged-next-page"]').trigger('click'); await flushPromises()
    await wrapper.get('[data-testid="staged-next-page"]').trigger('click'); await flushPromises()
    expect(mocks.listStaged).toHaveBeenCalledTimes(4)
    expect(wrapper.text()).toContain('queue changed again')
    expect(wrapper.find('[data-testid="staged-pagination-status"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="staged-review-retry"]').exists()).toBe(true)
  })
})

describe('ContactIdentityReviewDialog owns the exact decision reread completion', () => {
  let wrapper: VueWrapper | null = null
  const releaseReads: Array<() => void> = []

  function pendingRead<T>(fallback: T) {
    let resolve!: (value: T) => void
    let reject!: (reason: unknown) => void
    const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail })
    releaseReads.push(() => resolve(fallback))
    return { promise, resolve, reject }
  }

  beforeEach(() => {
    for (const mock of Object.values(mocks)) mock.mockReset()
    auth.user = { id: 'user-a' }; auth.allowed = true; organizations.selectedOrgId = 'org-a'; logicalSession = 'session-a'; localStorage.clear()
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('00000000-0000-4000-8000-000000000123')
    mocks.getState.mockResolvedValue({ data: { data: blocked } })
    mocks.preview.mockResolvedValue({ data: { data: preview } })
    mocks.listStaged.mockResolvedValue({ data: { data: {
      reviews: [], total: 0, page: 1, limit: 100, eligibility_known: true,
      actionable_total: 0, actionable_chain_total: 0, read_only_total: 0,
    } } })
    mocks.listStagedForHold.mockResolvedValue({ data: { data: {
      reviews: [], total: 0, page: 1, limit: 25, eligibility_known: true,
      actionable_total: 0, actionable_chain_total: 0, read_only_total: 0,
    } } })
  })

  afterEach(async () => {
    wrapper?.unmount(); wrapper = null
    for (const release of releaseReads.splice(0)) release()
    await flushPromises()
    vi.restoreAllMocks()
  })

  async function startDecision(outcome: 'acknowledged' | 409 | 422 = 'acknowledged') {
    if (outcome === 'acknowledged') mocks.decide.mockResolvedValueOnce({ data: { data: { disposition: 'future_routing' } } })
    else mocks.decide.mockRejectedValueOnce({ isAxiosError: true, response: {
      status: outcome, data: { message: 'The selected routing decision was refused.' },
    } })
    wrapper = mountDialog()
    await flushPromises()
    await wrapper.findAll('input[name="identity-review-target"]')[1]!.setValue()
    void wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await vi.waitFor(() => {
      expect(mocks.decide).toHaveBeenCalledTimes(1)
      expect(mocks.getState).toHaveBeenCalledTimes(2)
    })
    expect(wrapper.emitted('queue-invalidated')?.[0]?.[0]).toMatchObject({ organizationId: 'org-a', userId: 'user-a', session: 'session-a' })
  }

  async function navigate(section: 'Protected staged queue' | 'Contact review') {
    await wrapper!.findAll('button').find(button => button.text() === section)!.trigger('click')
    await flushPromises()
  }

  it.each(['late-success', 'cancelled'] as const)('does not publish an acknowledged reread replaced by queue: %s', async completion => {
    const reread = pendingRead({ data: { data: allowed } })
    mocks.getState.mockResolvedValueOnce({ data: { data: blocked } }).mockReturnValueOnce(reread.promise)
    await startDecision()
    const signal = mocks.getState.mock.calls[1]![1].signal as AbortSignal
    await navigate('Protected staged queue')
    expect(signal.aborted).toBe(true)
    expect(wrapper!.text()).toContain('No staged reviews.')
    if (completion === 'late-success') reread.resolve({ data: { data: allowed } })
    else reread.reject(new DOMException('The read was aborted.', 'AbortError'))
    await flushPromises()
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(wrapper!.text()).toContain('No staged reviews.')
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
    expect(mocks.getState).toHaveBeenCalledTimes(2)
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it.each([409, 422] as const)('cannot write a stale %s refusal after a pending Preview is replaced by queue', async outcome => {
    const rereadPreview = pendingRead({ data: { data: preview } })
    mocks.preview.mockResolvedValueOnce({ data: { data: preview } }).mockReturnValueOnce(rereadPreview.promise)
    await startDecision(outcome)
    await vi.waitFor(() => expect(mocks.preview).toHaveBeenCalledTimes(2))
    const signal = mocks.preview.mock.calls[1]![1].signal as AbortSignal
    await navigate('Protected staged queue')
    expect(signal.aborted).toBe(true)
    rereadPreview.resolve({ data: { data: preview } })
    await flushPromises()
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper!.text()).not.toContain('has been reloaded')
    expect(wrapper!.text()).not.toContain('selected routing decision was refused')
    expect(wrapper!.text()).toContain('No staged reviews.')
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it.each(['acknowledged', 409, 422] as const)('does not publish a newer contact read after queue/contact ABA: %s', async outcome => {
    const oldRead = pendingRead({ data: { data: blocked } })
    const newer = { ...allowed, reason: 'newer_contact_read_after_navigation' }
    mocks.getState.mockResolvedValueOnce({ data: { data: blocked } }).mockReturnValueOnce(oldRead.promise)
      .mockResolvedValueOnce({ data: { data: newer } })
    await startDecision(outcome)
    const signal = mocks.getState.mock.calls[1]![1].signal as AbortSignal
    await navigate('Protected staged queue')
    await navigate('Contact review')
    expect(signal.aborted).toBe(true)
    expect(mocks.getState).toHaveBeenCalledTimes(3)
    expect(wrapper!.text()).toContain(newer.reason)
    expect(wrapper!.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI allowed')
    oldRead.resolve({ data: { data: blocked } })
    await flushPromises()
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(wrapper!.text()).toContain(newer.reason)
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it.each(['allowed', 'server-unknown'] as const)('publishes exactly the owned successful server state once: %s', async kind => {
    const serverState = kind === 'allowed' ? allowed : {
      known: false, ai_allowed: false, blocked: true, open_hold_count: 0, reason: 'server_policy_read_unavailable',
    }
    mocks.getState.mockResolvedValueOnce({ data: { data: blocked } }).mockResolvedValueOnce({ data: { data: serverState } })
    await startDecision()
    await vi.waitFor(() => expect(wrapper!.emitted('resolved')).toEqual([[serverState]]))
    await flushPromises()
    expect(wrapper!.emitted('resolved')).toHaveLength(1)
    expect(mocks.decide).toHaveBeenCalledTimes(1)
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
  })

  it('keeps a successful state reread when only the current Preview is explicitly read-only', async () => {
    mocks.preview.mockResolvedValueOnce({ data: { data: preview } }).mockRejectedValueOnce({ isAxiosError: true, response: {
      status: 409, data: { error_type: 'identity_review_read_only', message: 'No current decision is available.' },
    } })
    await startDecision()
    await vi.waitFor(() => expect(wrapper!.emitted('resolved')).toEqual([[blocked]]))
    expect(wrapper!.get('[data-testid="identity-review-read-only"]').text()).toContain('No current decision is available.')
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it.each(['state', 'preview'] as const)('does not publish local fallback when the acknowledged reread fails at %s', async failedRead => {
    const failure = { isAxiosError: true, response: { status: 503, data: { message: 'Current reread dependency unavailable.' } } }
    if (failedRead === 'state') mocks.getState.mockResolvedValueOnce({ data: { data: blocked } }).mockRejectedValueOnce(failure)
    else mocks.preview.mockResolvedValueOnce({ data: { data: preview } }).mockRejectedValueOnce(failure)
    await startDecision()
    await vi.waitFor(() => expect(wrapper!.get('[role="alert"]').text()).toContain('Current reread dependency unavailable.'))
    await flushPromises()
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(wrapper!.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI blocked')
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it('does not publish a replacement parent state as the result of its pending reread', async () => {
    const rereadPreview = pendingRead({ data: { data: preview } })
    mocks.preview.mockResolvedValueOnce({ data: { data: preview } }).mockReturnValueOnce(rereadPreview.promise)
    await startDecision()
    await vi.waitFor(() => expect(mocks.preview).toHaveBeenCalledTimes(2))
    const signal = mocks.preview.mock.calls[1]![1].signal as AbortSignal
    const heldReads = mocks.listStagedForHold.mock.calls.length
    const newer = { ...allowed, reason: 'newer_parent_projection' }
    await wrapper!.setProps({ effectiveState: newer })
    expect(wrapper!.text()).toContain(newer.reason)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(signal.aborted).toBe(true)
    expect(wrapper!.find('[data-testid="identity-review-decision"]').exists()).toBe(false)
    rereadPreview.resolve({ data: { data: preview } })
    await flushPromises()
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(wrapper!.text()).toContain(newer.reason)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(wrapper!.findAll('[data-testid="identity-review-candidate"]')).toHaveLength(0)
    expect(wrapper!.findAll('[data-testid="identity-review-held-copy"]')).toHaveLength(0)
    expect(mocks.listStagedForHold).toHaveBeenCalledTimes(heldReads)
    expect(mocks.decide).toHaveBeenCalledTimes(1)

    // A deliberate new read still obtains its own complete Preview; replacing
    // the old loading owner must not strand the contact or retry a mutation.
    await navigate('Contact review')
    expect(mocks.getState).toHaveBeenCalledTimes(3)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(wrapper!.findAll('[data-testid="identity-review-candidate"]')).toHaveLength(2)
    await wrapper!.findAll('input[name="identity-review-target"]')[1]!.setValue()
    expect(wrapper!.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeUndefined()
    expect(mocks.decide).toHaveBeenCalledTimes(1)
    expect(wrapper!.emitted('resolved')).toBeUndefined()
  })

  it.each([
    ['acknowledged', 'cancelled'],
    ['acknowledged', 'unavailable'],
    [409, 'late-success'],
    [409, 'cancelled'],
    [422, 'late-success'],
    [422, 'unavailable'],
  ] as const)('keeps a scoped parent projection after %s reread finishes with %s', async (outcome, completion) => {
    const rereadPreview = pendingRead({ data: { data: preview } })
    mocks.preview.mockResolvedValueOnce({ data: { data: preview } }).mockReturnValueOnce(rereadPreview.promise)
    await startDecision(outcome)
    await vi.waitFor(() => expect(mocks.preview).toHaveBeenCalledTimes(2))
    const signal = mocks.preview.mock.calls[1]![1].signal as AbortSignal
    const heldReads = mocks.listStagedForHold.mock.calls.length
    const newer = { ...blocked, reason: 'newer_parent_requires_fresh_preview' }
    await wrapper!.setProps({ effectiveState: newer })
    expect(wrapper!.text()).toContain(newer.reason)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(signal.aborted).toBe(true)
    expect(wrapper!.find('[data-testid="identity-review-decision"]').exists()).toBe(false)
    if (completion === 'late-success') rereadPreview.resolve({ data: { data: preview } })
    else if (completion === 'cancelled') rereadPreview.reject(new DOMException('The read was aborted.', 'AbortError'))
    else rereadPreview.reject({ isAxiosError: true, response: { status: 503, data: { message: 'Superseded preview unavailable.' } } })
    await flushPromises()
    expect(wrapper!.text()).toContain(newer.reason)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(wrapper!.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI blocked')
    expect(wrapper!.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper!.findAll('[data-testid="identity-review-candidate"]')).toHaveLength(0)
    expect(wrapper!.findAll('[data-testid="identity-review-held-copy"]')).toHaveLength(0)
    expect(mocks.listStagedForHold).toHaveBeenCalledTimes(heldReads)
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it('cannot clear a newer contact read loading owner when the superseded Preview finishes', async () => {
    const oldPreview = pendingRead({ data: { data: preview } })
    mocks.preview.mockResolvedValueOnce({ data: { data: preview } }).mockReturnValueOnce(oldPreview.promise)
    await startDecision()
    await vi.waitFor(() => expect(mocks.preview).toHaveBeenCalledTimes(2))
    await wrapper!.setProps({ effectiveState: { ...allowed, reason: 'newer_parent_projection' } })
    const fresh = { ...allowed, reason: 'explicit_new_contact_read' }
    const freshRead = pendingRead({ data: { data: fresh } })
    mocks.getState.mockReturnValueOnce(freshRead.promise)
    await navigate('Contact review')
    expect(mocks.getState).toHaveBeenCalledTimes(3)
    const freshSignal = mocks.getState.mock.calls[2]![1].signal as AbortSignal
    expect(wrapper!.text()).toContain('Checking durable state')
    oldPreview.resolve({ data: { data: preview } })
    await flushPromises()
    expect(freshSignal.aborted).toBe(false)
    expect(wrapper!.text()).toContain('Checking durable state')
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    freshRead.resolve({ data: { data: fresh } })
    await flushPromises()
    expect(wrapper!.text()).toContain(fresh.reason)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(wrapper!.emitted('resolved')).toBeUndefined()
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })

  it('does not accept a parent projection before the initial contact read establishes its scope', async () => {
    const initialRead = pendingRead({ data: { data: blocked } })
    mocks.getState.mockReturnValueOnce(initialRead.promise)
    wrapper = mountDialog()
    await vi.waitFor(() => expect(mocks.getState).toHaveBeenCalledTimes(1))
    const signal = mocks.getState.mock.calls[0]![1].signal as AbortSignal
    const unowned = { ...allowed, reason: 'parent_without_established_contact_scope' }
    await wrapper.setProps({ effectiveState: unowned })
    expect(wrapper.text()).not.toContain(unowned.reason)
    expect(wrapper.text()).toContain('Checking durable state')
    expect(signal.aborted).toBe(false)
    initialRead.resolve({ data: { data: blocked } })
    await flushPromises()
    expect(wrapper.text()).toContain(blocked.reason)
    expect(wrapper.text()).not.toContain('Checking durable state')
    expect(wrapper.findAll('[data-testid="identity-review-candidate"]')).toHaveLength(2)
    expect(wrapper.emitted('resolved')).toBeUndefined()
    expect(mocks.decide).not.toHaveBeenCalled()
  })

  it.each(['same-object', 'copied-object'] as const)('accepts feedback from its own successful resolved state without another request: %s', async feedback => {
    mocks.getState.mockResolvedValueOnce({ data: { data: blocked } }).mockResolvedValueOnce({ data: { data: allowed } })
    await startDecision()
    await vi.waitFor(() => expect(wrapper!.emitted('resolved')).toEqual([[allowed]]))
    const emitted = wrapper!.emitted('resolved')![0]![0] as typeof allowed
    await wrapper!.setProps({ effectiveState: feedback === 'same-object' ? emitted : { ...emitted } })
    await flushPromises()
    expect(wrapper!.text()).toContain(allowed.reason)
    expect(wrapper!.text()).not.toContain('Checking durable state')
    expect(wrapper!.get('[data-testid="identity-review-effective-state"]').text()).toBe('AI allowed')
    expect(wrapper!.emitted('resolved')).toEqual([[allowed]])
    expect(mocks.getState).toHaveBeenCalledTimes(2)
    expect(mocks.preview).toHaveBeenCalledTimes(1)
    expect(mocks.decide).toHaveBeenCalledTimes(1)
  })
})
