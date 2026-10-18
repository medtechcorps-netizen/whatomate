/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Contact, Message } from '@/stores/contacts'
import ForwardMessageDialog from './ForwardMessageDialog.vue'
import {
  MAX_FORWARD_TARGETS,
  canOfferForward,
  forwardSourceBlock,
  classifyForwardError,
  forwardDeliveryState,
  forwardTargetBlock,
  isForwardedMessage,
} from './messageForward'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  sendMessage: vi.fn(),
  activity: [] as Array<(event: { type: string; payload: any }) => void>,
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key),
  }),
}))
vi.mock('@/services/api', () => ({ contactsService: { list: mocks.list } }))
vi.mock('@/stores/contacts', () => ({ useContactsStore: () => ({ sendMessage: mocks.sendMessage }) }))
vi.mock('@/services/websocket', () => ({
  wsService: {
    onInboxActivity: (callback: (event: { type: string; payload: any }) => void) => {
      mocks.activity.push(callback)
      return () => {
        const index = mocks.activity.indexOf(callback)
        if (index > -1) mocks.activity.splice(index, 1)
      }
    },
  },
}))

function emitActivity(type: string, payload: Record<string, unknown>) {
  for (const callback of [...mocks.activity]) callback({ type, payload })
}

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
  mocks.sendMessage.mockResolvedValue({ id: 'new', status: 'pending' })
  mocks.activity.length = 0
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

  it('reads the CRM-only forwarded flag, also from stored metadata', () => {
    expect(isForwardedMessage(message())).toBe(false)
    expect(isForwardedMessage({ ...message(), is_forwarded: true } as Message)).toBe(true)
    expect(isForwardedMessage({ ...message(), metadata: { forwarded_from: { message_id: 'x' } } } as Message)).toBe(true)
  })

  it('does not offer a deleted-message tombstone for forwarding', () => {
    expect(forwardSourceBlock(message({ content: { body: '[Message deleted from WhatsApp Business App]' } }))).toBe('unsupported')
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
      .mockResolvedValueOnce({ id: 'a', status: 'pending' })
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

    const rows = () => Array.from(document.body.querySelectorAll('[data-testid="chat-forward-results"] li'))
    // Accepted is not delivered: the first target is only queued.
    expect(rows().map(row => row.getAttribute('data-state'))).toEqual(['queued', 'failed'])
    expect(rows()[0].textContent).toContain('chat.forward.queued')
    expect(rows()[1].textContent).toContain('The 24-hour service window for this contact is closed.')

    emitActivity('status_update', { message_id: 'a', status: 'sent' })
    await flushPromises()
    expect(rows().map(row => row.getAttribute('data-state'))).toEqual(['sent', 'failed'])
    expect(q('[data-testid="chat-forward-dialog"]')!.textContent).toContain('chat.forward.summary:{"sent":1,"total":2}')
    expect(mocks.activity).toHaveLength(0)
  })

  it('flips queued targets from live status events, including early and relayed ones', async () => {
    mocks.sendMessage.mockImplementation(async (contactId: string) => {
      // Target 2's failure is broadcast before its POST response arrives.
      if (contactId === '2') emitActivity('status_update', { message_id: 'm-2', status: 'failed', error_message: 'upload failed' })
      return { id: `m-${contactId}`, status: 'pending' }
    })
    await openDialog()
    await pick('1', '2', '5')
    await click('chat-forward-next')
    await click('chat-forward-confirm')
    await flushPromises()
    const states = () => Array.from(document.body.querySelectorAll('[data-testid="chat-forward-results"] li'))
      .map(row => row.getAttribute('data-state'))
    expect(states()).toEqual(['queued', 'failed', 'queued'])
    expect(q('[data-testid="chat-forward-results"]')!.textContent).toContain('upload failed')

    // Another replica relays only an identifier-level status change.
    emitActivity('realtime_sync', { kind: 'message_status_changed', message_id: 'm-5', status: 'delivered' })
    emitActivity('status_update', { message_id: 'unrelated', status: 'failed' })
    await flushPromises()
    expect(states()).toEqual(['queued', 'failed', 'sent'])
  })

  it('says delivery is not confirmed when no status arrives in time', async () => {
    vi.useFakeTimers()
    try {
      await openDialog()
      await pick('1')
      await click('chat-forward-next')
      await click('chat-forward-confirm')
      await vi.advanceTimersByTimeAsync(0)
      await flushPromises()
      expect(q('[data-testid="chat-forward-unconfirmed"]')).toBeNull()
      await vi.advanceTimersByTimeAsync(60_000)
      await flushPromises()
      expect(q('[data-testid="chat-forward-results"] li')!.getAttribute('data-state')).toBe('unconfirmed')
      expect(q('[data-testid="chat-forward-unconfirmed"]')!.textContent).toContain('chat.forward.unconfirmedNote')
      // A late status still lands.
      emitActivity('status_update', { message_id: 'new', status: 'read' })
      await flushPromises()
      expect(q('[data-testid="chat-forward-results"] li')!.getAttribute('data-state')).toBe('sent')
    } finally {
      vi.useRealTimers()
    }
  })

  it('stops listening for status events when closed', async () => {
    const wrapper = await openDialog()
    await pick('1')
    await click('chat-forward-next')
    await click('chat-forward-confirm')
    await flushPromises()
    expect(mocks.activity).toHaveLength(1)
    await wrapper.setProps({ open: false })
    await flushPromises()
    expect(mocks.activity).toHaveLength(0)
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

function httpError(status: number, data: Record<string, unknown> = {}) {
  return Object.assign(new Error(`Request failed with status code ${status}`), {
    isAxiosError: true,
    response: { status, data },
  })
}

describe('forward capacity contract (client)', () => {
  it('classifies a failed request by the server contract', () => {
    expect(classifyForwardError(httpError(503, { error_type: 'forward_temporarily_unavailable' }))).toBe('retryable')
    expect(classifyForwardError(httpError(503, {}))).toBe('uncertain')
    expect(classifyForwardError(httpError(500, { error_type: 'forward_outcome_unknown' }))).toBe('uncertain')
    expect(classifyForwardError(httpError(502))).toBe('uncertain')
    expect(classifyForwardError(Object.assign(new Error('timeout of 0ms exceeded'), { isAxiosError: true }))).toBe('uncertain')
    expect(classifyForwardError(httpError(409, { error_type: 'forward_window_closed' }))).toBe('failed')
    expect(classifyForwardError(httpError(429, { error_type: 'forward_rate_limited' }))).toBe('failed')
    // Definitely not sent, and retrying won't help: no Try again, no "may have been sent".
    expect(classifyForwardError(httpError(500, { error_type: 'forward_blocked' }))).toBe('failed')
    expect(classifyForwardError(httpError(400, { error_type: 'forward_account_unavailable' }))).toBe('failed')
  })

  it('reads the server-derived delivery state of a stalled forward', () => {
    expect(forwardDeliveryState({ ...message(), forward_delivery_state: 'delayed' } as Message)).toBe('delayed')
    expect(forwardDeliveryState({ ...message(), forward_delivery_state: 'uncertain' } as Message)).toBe('uncertain')
    expect(forwardDeliveryState({ ...message(), forward_delivery_state: 'interrupted' } as Message)).toBeNull()
    expect(forwardDeliveryState({ ...message(), forward_delivery_state: 'other' } as Message)).toBeNull()
    expect(forwardDeliveryState(message())).toBeNull()
  })

  async function confirmTargets(...ids: string[]) {
    await openDialog()
    await pick(...ids)
    await click('chat-forward-next')
    await click('chat-forward-confirm')
    await flushPromises()
  }

  const row = (id: string) => document.body.querySelector(`[data-testid="chat-forward-results"] li:nth-child(${id})`)!

  it('offers a manual retry, for that target only, when nothing was sent (503)', async () => {
    mocks.sendMessage.mockImplementation(async (contactId: string) => {
      if (contactId === '2') throw httpError(503, { error_type: 'forward_temporarily_unavailable', message: 'The server is busy, so this forward was not sent.' })
      return { id: `m-${contactId}`, status: 'pending' }
    })
    await confirmTargets('1', '2')
    expect(row('2').getAttribute('data-state')).toBe('retryable')
    expect(row('2').textContent).toContain('chat.forward.retryable')
    const calls = () => mocks.sendMessage.mock.calls.filter(call => call[0] === '2').length
    expect(calls()).toBe(1)
    await flushPromises()
    expect(calls()).toBe(1) // never retried by itself

    mocks.sendMessage.mockResolvedValue({ id: 'm-2b', status: 'pending' })
    await click('chat-forward-retry-2')
    await flushPromises()
    expect(calls()).toBe(2)
    expect(mocks.sendMessage.mock.calls.filter(call => call[0] === '1')).toHaveLength(1)
    expect(row('2').getAttribute('data-state')).toBe('queued')
    expect(document.body.querySelector('[data-testid="chat-forward-retry-2"]')).toBeNull()
  })

  it('says a forward may have been sent, and never retries, when the outcome is unknown', async () => {
    mocks.sendMessage.mockImplementation(async (contactId: string) => {
      if (contactId === '1') throw httpError(500, { error_type: 'forward_outcome_unknown' })
      throw Object.assign(new Error('Network Error'), { isAxiosError: true })
    })
    await confirmTargets('1', '2')
    expect(row('1').getAttribute('data-state')).toBe('uncertain')
    expect(row('2').getAttribute('data-state')).toBe('uncertain')
    expect(row('1').textContent).toContain('chat.forward.uncertain')
    expect(document.body.querySelector('[data-testid="chat-forward-uncertain"]')!.textContent).toContain('chat.forward.uncertainNote')
    expect(document.body.querySelector('[data-testid^="chat-forward-retry-"]')).toBeNull()
    expect(mocks.sendMessage).toHaveBeenCalledTimes(2)
  })

  it('shows a blocked forward as failed, with no Try again', async () => {
    mocks.sendMessage.mockRejectedValue(httpError(500, {
      error_type: 'forward_blocked',
      message: 'This forward was blocked by a server-side check and was not sent.',
    }))
    await confirmTargets('1')
    expect(row('1').getAttribute('data-state')).toBe('failed')
    expect(row('1').textContent).toContain('blocked by a server-side check')
    expect(document.body.querySelector('[data-testid^="chat-forward-retry-"]')).toBeNull()
    expect(document.body.querySelector('[data-testid="chat-forward-uncertain"]')).toBeNull()
  })

  it('gives each attempt its own confirmation window (a retry gets a fresh one)', async () => {
    vi.useFakeTimers()
    try {
      mocks.sendMessage.mockImplementation(async (contactId: string) => {
        if (contactId === '2') throw httpError(503, { error_type: 'forward_temporarily_unavailable' })
        return { id: `m-${contactId}`, status: 'pending' }
      })
      await confirmTargets('1', '2')
      await vi.advanceTimersByTimeAsync(40_000)
      mocks.sendMessage.mockResolvedValue({ id: 'm-2b', status: 'pending' })
      await click('chat-forward-retry-2')
      await vi.advanceTimersByTimeAsync(0)
      expect(row('2').getAttribute('data-state')).toBe('queued')

      await vi.advanceTimersByTimeAsync(25_000) // 65 s after the batch, 25 s after the retry
      expect(row('1').getAttribute('data-state')).toBe('unconfirmed')
      expect(row('2').getAttribute('data-state')).toBe('queued')

      await vi.advanceTimersByTimeAsync(40_000)
      expect(row('2').getAttribute('data-state')).toBe('unconfirmed')
      emitActivity('status_update', { message_id: 'm-2b', status: 'sent' })
      await flushPromises()
      expect(row('2').getAttribute('data-state')).toBe('sent')
    } finally {
      vi.useRealTimers()
    }
  })
})
