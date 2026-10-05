/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ContactIdentityReviewDialog from './ContactIdentityReviewDialog.vue'

const mocks = vi.hoisted(() => ({
  getState: vi.fn(),
  preview: vi.fn(),
  decide: vi.fn(),
  listStaged: vi.fn(),
  listStagedForHold: vi.fn(),
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
    listStagedIdentityReviewsForHolds: mocks.listStagedForHold,
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
    mocks.preview.mockResolvedValueOnce({ data: { data: { ...preview, open_hold_ids: ['hold-0', 'hold-1'] } } })
    const held = [
      { id: 'staged-hi', hold_id: 'hold-0', message_type: 'text', received_at: '2026-10-06T08:00:00Z', content: 'hi' },
      { id: 'staged-order', hold_id: 'hold-0', message_type: 'text', received_at: '2026-10-06T08:01:00Z', content: 'Order 42: two boxes, deliver Friday' },
      { id: 'staged-photo', hold_id: 'hold-1', message_type: 'image', received_at: '2026-10-06T08:02:00Z', content: '', media_mime_type: 'image/jpeg', media_filename: 'receipt.jpg' },
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

    expect(mocks.listStagedForHold).toHaveBeenCalledWith(['hold-0', 'hold-1'], 25, expect.any(AbortSignal))
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
    expect(mocks.getStaged).toHaveBeenLastCalledWith('staged-photo', expect.any(AbortSignal))
    expect(wrapper.text()).toContain('Load protected media')
  })

  it('says how many held copies remain beyond the ones shown', async () => {
    mocks.listStagedForHold.mockResolvedValueOnce({
      data: {
        data: {
          reviews: [{ id: 'staged-first', hold_id: 'hold-1', protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false }],
          total: 30,
        },
      },
    })
    mocks.getStaged.mockResolvedValueOnce({
      data: { data: { id: 'staged-first', hold_id: 'hold-1', protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-06T08:00:00Z', read_only: false, content: 'first', media_available: false } },
    })
    wrapper = mountDialog()
    await flushPromises()

    expect(wrapper.get('[data-testid="identity-review-held-count"]').text()).toContain('30 held messages')
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('read the remaining 29')
  })

  it('explains a review without a held copy and never reads the queue without access', async () => {
    wrapper = mountDialog()
    await flushPromises()
    expect(mocks.listStagedForHold).toHaveBeenCalledWith(['hold-1'], 25, expect.any(AbortSignal))
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('No held copy for this review')
    expect(mocks.getStaged).not.toHaveBeenCalled()
    wrapper.unmount()

    wrapper = mountDialog({ canViewStaged: false })
    await flushPromises()
    expect(mocks.listStagedForHold).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="identity-review-held-message"]').text()).toContain('needs access')
  })

  it.each([
    [422, 'This contact belongs to a different WhatsApp user and cannot receive this sender'],
    [409, 'Identity review state changed; reload and try again'],
  ])('drops a %s-refused request and re-reads the review instead of resending it', async (status, message) => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000001')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000002')
    mocks.decide
      .mockRejectedValueOnce({ isAxiosError: true, response: { status, data: { message } } })
      .mockResolvedValueOnce({ data: { data: { disposition: 'future_routing' } } })
    wrapper = mountDialog()
    await flushPromises()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await flushPromises()

    expect(mocks.decide).toHaveBeenCalledTimes(1)
    expect(mocks.preview).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[role="alert"]').text()).toContain(message)
    expect((wrapper.findAll('input[name="identity-review-target"]')[1].element as HTMLInputElement).checked).toBe(false)
    expect(wrapper.get('[data-testid="identity-review-decision"]').attributes('disabled')).toBeDefined()

    await wrapper.findAll('input[name="identity-review-target"]')[1].setValue()
    await wrapper.get('[data-testid="identity-review-decision"]').trigger('click')
    await flushPromises()
    expect(mocks.decide).toHaveBeenCalledTimes(2)
    expect(mocks.decide.mock.calls[0][1].request_id).toBe('00000000-0000-4000-8000-000000000001')
    expect(mocks.decide.mock.calls[1][1].request_id).toBe('00000000-0000-4000-8000-000000000002')
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
      id: 'staged-1',
      hold_id: 'hold-1',
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

      expect(mocks.getMedia).toHaveBeenCalledWith('staged-1', 'c'.repeat(64), expect.any(AbortSignal))
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

describe('ContactIdentityReviewDialog workspace staged entry point', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    for (const mock of Object.values(mocks)) mock.mockReset()
    mocks.listStaged.mockResolvedValue({
      data: {
        data: {
          reviews: [{
            id: 'staged-new-sender',
            hold_id: 'hold-new-sender',
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
          id: 'staged-new-sender',
          hold_id: 'hold-new-sender',
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
    expect(mocks.listStaged).toHaveBeenCalledWith({ page: 1, limit: 100 }, expect.any(AbortSignal))
    expect(wrapper.text()).toContain('Held WhatsApp messages')
    expect(wrapper.text()).not.toContain('Contact review')
    expect(wrapper.find('[data-testid="identity-review-effective-state"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="identity-review-decision"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="staged-pagination-status"]').text()).toContain('Showing 1–1 of 1')
    expect(wrapper.emitted('staged-total')).toEqual([[1]])

    const item = wrapper.findAll('button').find(button => button.text().includes('2026-01-15T08:30:00Z'))
    await item!.trigger('click')
    await flushPromises()

    expect(mocks.getStaged).toHaveBeenCalledWith('staged-new-sender', expect.any(AbortSignal))
    expect(wrapper.text()).toContain('Saw your ad, price?')
    const guidance = wrapper.get('[data-testid="staged-identity-review-guidance"]').text()
    expect(guidance).toContain('never moved into a conversation')
    expect(guidance).toContain('read-only for now')
    expect(guidance).toContain('later messages may be held too')
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
            { id: 'staged-conflict', hold_id: 'hold-conflict', protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-01-15T09:00:00Z', read_only: false },
            { id: 'staged-new-sender', hold_id: 'hold-new-sender', protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-01-15T08:30:00Z', read_only: true },
          ],
          total: 2,
          read_only_total: 1,
        },
      },
    })
    mocks.getStaged.mockResolvedValue({
      data: {
        data: {
          id: 'staged-conflict', hold_id: 'hold-conflict', protocol_version: 1, status: 'pending', message_type: 'text',
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
    expect(guidance).toContain('open the contact marked Review')
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
