/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Contact, Message } from '@/stores/contacts'
import ForwardMessageDialog from './ForwardMessageDialog.vue'
import {
  MAX_FORWARD_TARGETS,
  canOfferForward,
  forwardSourceBlock,
  forwardTargetBlock,
  isForwardedMessage,
} from './messageForward'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  sendMessage: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key),
  }),
}))
vi.mock('@/services/api', () => ({ contactsService: { list: mocks.list } }))
vi.mock('@/stores/contacts', () => ({ useContactsStore: () => ({ sendMessage: mocks.sendMessage }) }))

const OPEN_REVIEW = { known: true, ai_allowed: true, blocked: false, open_hold_count: 0, reason: 'no_open_identity_review' }

function contact(id: string, overrides: Partial<Contact> = {}): Contact {
  return {
    id,
    phone_number: `6012000${id.padStart(4, '0')}`,
    name: `Patient ${id}`,
    profile_name: `Patient ${id}`,
    status: 'active',
    tags: [],
    metadata: {},
    unread_count: 0,
    service_window_open: true,
    identity_review_ai_state: OPEN_REVIEW,
    created_at: '2026-10-15T00:00:00Z',
    updated_at: '2026-10-15T00:00:00Z',
    ...overrides,
  }
}

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: 'src-1',
    contact_id: 'c-source',
    direction: 'incoming',
    message_type: 'text',
    content: { body: 'Lab result attached' },
    status: 'delivered',
    created_at: '2026-10-15T00:00:00Z',
    updated_at: '2026-10-15T00:00:00Z',
    ...overrides,
  }
}

const mounted: VueWrapper[] = []

async function openDialog(props: Record<string, unknown> = {}) {
  const wrapper = mount(ForwardMessageDialog, {
    props: { open: true, message: message(), preview: 'Lab result attached', ...props },
    attachTo: document.body,
    global: { mocks: { $t: (key: string) => key } },
  })
  mounted.push(wrapper)
  await flushPromises()
  return wrapper
}

function q<T extends Element = HTMLElement>(selector: string): T | null {
  return document.body.querySelector<T>(selector)
}

function checkbox(id: string): HTMLInputElement {
  return q<HTMLInputElement>(`[data-testid="chat-forward-target-${id}"] input[type="checkbox"]`)!
}

async function pick(...ids: string[]) {
  for (const id of ids) {
    checkbox(id).click()
    await flushPromises()
  }
}

async function click(testId: string) {
  q<HTMLButtonElement>(`[data-testid="${testId}"]`)!.click()
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  mocks.list.mockResolvedValue({
    data: {
      data: {
        contacts: [
          contact('1'),
          contact('2'),
          contact('3', { service_window_open: false }),
          contact('4', { identity_review_ai_state: { ...OPEN_REVIEW, blocked: true, reason: 'identity_review_open' } }),
          contact('5'),
          contact('6'),
          contact('7'),
          contact('8'),
        ],
      },
    },
  })
  mocks.sendMessage.mockResolvedValue({ id: 'new' })
})

afterEach(async () => {
  while (mounted.length) mounted.pop()?.unmount()
  await flushPromises()
  document.body.innerHTML = ''
})

describe('messageForward helpers', () => {
  it('mirrors the server forwardable types and media hydration rule', () => {
    expect(forwardSourceBlock(message())).toBeNull()
    expect(forwardSourceBlock(message({ content: { body: '  ' } }))).toBe('unsupported')
    expect(forwardSourceBlock(message({ message_type: 'image', media_url: '/api/media/x' }))).toBeNull()
    expect(forwardSourceBlock(message({ message_type: 'document' }))).toBe('media_pending')
    for (const type of ['sticker', 'location', 'contacts', 'interactive', 'template', 'button_reply']) {
      expect(forwardSourceBlock(message({ message_type: type, media_url: '/m' }))).toBe('unsupported')
      expect(canOfferForward(message({ message_type: type, media_url: '/m' }))).toBe(false)
    }
    expect(canOfferForward(message({ message_type: 'video' }))).toBe(true)
  })

  it('blocks closed windows and fails closed on identity review', () => {
    expect(forwardTargetBlock(contact('1'))).toBeNull()
    expect(forwardTargetBlock(contact('1', { service_window_open: false }))).toBe('window_closed')
    expect(forwardTargetBlock(contact('1', { service_window_open: undefined }))).toBe('window_closed')
    expect(forwardTargetBlock(contact('1', { identity_review_ai_state: undefined as never }))).toBe('identity_review')
    expect(forwardTargetBlock(contact('1', { identity_review_ai_state: { ...OPEN_REVIEW, blocked: true } }))).toBe('identity_review')
  })

  it('reads the CRM-only forwarded flag', () => {
    expect(isForwardedMessage(message())).toBe(false)
    expect(isForwardedMessage({ ...message(), is_forwarded: true } as Message)).toBe(true)
  })
})

describe('ForwardMessageDialog', () => {
  it('lists contacts with their 24h window state and disables closed ones', async () => {
    await openDialog()
    expect(mocks.list).toHaveBeenCalledWith(expect.objectContaining({ limit: 25 }), expect.any(AbortSignal))
    expect(checkbox('1').disabled).toBe(false)
    expect(checkbox('3').disabled).toBe(true)
    expect(q('[data-testid="chat-forward-target-3"]')!.textContent).toContain('chat.forward.windowClosed')
    expect(checkbox('4').disabled).toBe(true)
    expect(q('[data-testid="chat-forward-target-4"]')!.textContent).toContain('chat.forward.identityReview')
    expect(q('[data-testid="chat-forward-target-1"]')!.textContent).toContain('chat.forward.windowOpen')
  })

  it(`caps the selection at ${MAX_FORWARD_TARGETS}`, async () => {
    await openDialog()
    await pick('1', '2', '5', '6', '7')
    expect(checkbox('8').disabled).toBe(true)
    expect(q('[data-testid="chat-forward-dialog"]')!.textContent).toContain('chat.forward.limitReached')
    await pick('7')
    expect(checkbox('8').disabled).toBe(false)
  })

  it('confirms recipients by name, then forwards one request per target with per-target results', async () => {
    mocks.sendMessage
      .mockResolvedValueOnce({ id: 'a' })
      .mockRejectedValueOnce(Object.assign(new Error('Request failed with status code 409'), {
        isAxiosError: true,
        response: { status: 409, data: { message: 'The 24-hour service window for this contact is closed.' } },
      }))
    await openDialog()
    expect(q<HTMLButtonElement>('[data-testid="chat-forward-next"]')!.disabled).toBe(true)
    await pick('1', '2')
    await click('chat-forward-next')

    const recipients = q('[data-testid="chat-forward-recipients"]')!.textContent!
    expect(recipients).toContain('Patient 1')
    expect(recipients).toContain('Patient 2')
    expect(q('[data-testid="chat-forward-dialog"]')!.textContent).toContain('chat.forward.pdpaNotice')
    expect(mocks.sendMessage).not.toHaveBeenCalled()

    await click('chat-forward-confirm')
    await flushPromises()
    expect(mocks.sendMessage).toHaveBeenCalledTimes(2)
    expect(mocks.sendMessage).toHaveBeenNthCalledWith(1, '1', 'text', {}, undefined, undefined, { forwardOfMessageId: 'src-1' })
    expect(mocks.sendMessage).toHaveBeenNthCalledWith(2, '2', 'text', {}, undefined, undefined, { forwardOfMessageId: 'src-1' })

    const rows = Array.from(document.body.querySelectorAll('[data-testid="chat-forward-results"] li'))
    expect(rows.map(row => row.getAttribute('data-state'))).toEqual(['sent', 'failed'])
    expect(rows[1].textContent).toContain('The 24-hour service window for this contact is closed.')
    expect(q('[data-testid="chat-forward-dialog"]')!.textContent).toContain('chat.forward.summary:{"sent":1,"total":2}')
  })

  it('does not offer targets for an unsupported message and explains pending media', async () => {
    await openDialog({ message: message({ message_type: 'sticker', media_url: '/m' }) })
    expect(mocks.list).not.toHaveBeenCalled()
    expect(q('[data-testid="chat-forward-dialog"]')!.textContent).toContain('chat.forward.unsupportedSource')
    for (const wrapper of mounted.splice(0)) wrapper.unmount()
    await flushPromises()
    document.body.innerHTML = ''

    await openDialog({ message: message({ message_type: 'image' }) })
    expect(q('[data-testid="chat-forward-dialog"]')!.textContent).toContain('chat.forward.mediaPending')
    await pick('1')
    expect(q<HTMLButtonElement>('[data-testid="chat-forward-next"]')!.disabled).toBe(true)
  })

  it('searches with a debounce', async () => {
    vi.useFakeTimers()
    try {
      await openDialog()
      mocks.list.mockClear()
      const input = q<HTMLInputElement>('[data-testid="chat-forward-search"]')!
      input.value = 'ali'
      input.dispatchEvent(new Event('input'))
      await vi.advanceTimersByTimeAsync(100)
      expect(mocks.list).not.toHaveBeenCalled()
      await vi.advanceTimersByTimeAsync(200)
      expect(mocks.list).toHaveBeenCalledWith(expect.objectContaining({ search: 'ali' }), expect.any(AbortSignal))
    } finally {
      vi.useRealTimers()
    }
  })

  it('renders no composer icons that chat specs locate globally', async () => {
    await openDialog()
    for (const selector of ['[class*="lucide-send"]', '[class*="lucide-paperclip"]', '.lucide-smile-icon']) {
      expect(q(selector)).toBeNull()
    }
  })
})
