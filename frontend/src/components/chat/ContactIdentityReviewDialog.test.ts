/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactIdentityReviewDialog from './ContactIdentityReviewDialog.vue'

const mocks = vi.hoisted(() => ({
  getState: vi.fn(),
  preview: vi.fn(),
  decide: vi.fn(),
  listStaged: vi.fn(),
  getStaged: vi.fn(),
  getMedia: vi.fn(),
}))

vi.mock('@/services/api', async importOriginal => ({
  ...(await importOriginal<typeof import('@/services/api')>()),
  contactsService: {
    getIdentityReviewState: mocks.getState,
    previewIdentityReview: mocks.preview,
    decideIdentityReview: mocks.decide,
    listStagedIdentityReviews: mocks.listStaged,
    getStagedIdentityReview: mocks.getStaged,
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
  latest_hold_id: 'hold-1',
  latest_generation: 3,
}

const preview = {
  snapshot: {
    hold_id: 'hold-1',
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
    vi.clearAllMocks()
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('00000000-0000-4000-8000-000000000123')
    mocks.getState.mockResolvedValue({ data: { data: blocked } })
    mocks.preview.mockResolvedValue({ data: { data: preview } })
    mocks.listStaged.mockResolvedValue({ data: { data: { reviews: [], total: 0 } } })
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
    await flushPromises()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await flushPromises()

    expect(mocks.decide).toHaveBeenCalledTimes(2)
    const first = mocks.decide.mock.calls[0][1]
    const second = mocks.decide.mock.calls[1][1]
    expect(second).toEqual(first)
    expect(first).toMatchObject({
      request_id: '00000000-0000-4000-8000-000000000123',
      target_contact_id: 'contact-b',
      hold_id: 'hold-1',
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
    expect(mocks.getState).toHaveBeenNthCalledWith(2, 'contact-b', expect.any(AbortSignal))
  })

  it('loads staged detail only through the protected review service', async () => {
    mocks.listStaged.mockResolvedValueOnce({
      data: {
        data: {
          reviews: [{
            id: 'staged-1',
            hold_id: 'hold-1',
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
          id: 'staged-1',
          hold_id: 'hold-1',
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

    expect(mocks.listStaged).toHaveBeenCalledWith({ page: 1, limit: 100 }, expect.any(AbortSignal))
    expect(mocks.getStaged).toHaveBeenCalledWith('staged-1', expect.any(AbortSignal))
    expect(wrapper.text()).toContain('Sanitized caption')
  })

  it('pages through every protected staged review beyond the API limit', async () => {
    mocks.listStaged
      .mockResolvedValueOnce({
        data: {
          data: {
            reviews: [{
              id: 'staged-page-one',
              hold_id: 'hold-1',
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
              id: 'staged-page-two',
              hold_id: 'hold-2',
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

    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 1–1 of 101 · Page 1 of 2')
    expect(wrapper.text()).toContain('page-one-message')
    await wrapper.get('[data-testid="staged-next-page"]').trigger('click')
    await flushPromises()

    expect(mocks.listStaged).toHaveBeenNthCalledWith(2, { page: 2, limit: 100 }, expect.any(AbortSignal))
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 101–101 of 101 · Page 2 of 2')
    expect(wrapper.text()).toContain('page-two-message')
    expect(wrapper.text()).not.toContain('page-one-message')
  })

  it('supports previous-page navigation and resets pagination when the dialog context resets', async () => {
    mocks.listStaged.mockImplementation(({ page }: { page: number }) => Promise.resolve({
      data: {
        data: {
          reviews: [{
            id: `staged-page-${page}`,
            hold_id: `hold-${page}`,
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

    expect(mocks.listStaged).toHaveBeenNthCalledWith(3, { page: 1, limit: 100 }, expect.any(AbortSignal))
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Page 1 of 2')

    await wrapper.get('[data-testid="staged-next-page"]').trigger('click')
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Page 2 of 2')
    await wrapper.setProps({ open: false })
    await wrapper.setProps({ open: true })
    await flushPromises()
    await openQueue().trigger('click')
    await flushPromises()

    expect(mocks.listStaged).toHaveBeenLastCalledWith({ page: 1, limit: 100 }, expect.any(AbortSignal))
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
              id: 'initial-page-one',
              hold_id: 'hold-1',
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
              id: 'fresh-page-one',
              hold_id: 'hold-fresh',
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
            id: 'stale-page-two',
            hold_id: 'hold-stale',
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
