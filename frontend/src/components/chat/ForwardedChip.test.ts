/** @vitest-environment happy-dom */

import { mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Message } from '@/stores/contacts'
import ForwardedChip from './ForwardedChip.vue'
import { FORWARD_CLOCK_TICK_MS, FORWARD_STALLED_AFTER_MS } from './messageForward'

const NOW = Date.parse('2026-10-18T12:00:00Z')
const mounted: VueWrapper[] = []

function forward(extra: Record<string, unknown> = {}): Message {
  return {
    id: 'f-1',
    contact_id: 'c-1',
    direction: 'outgoing',
    message_type: 'text',
    content: { body: 'Lab result attached' },
    status: 'pending',
    created_at: new Date(NOW).toISOString(),
    updated_at: new Date(NOW).toISOString(),
    is_forwarded: true,
    forward_dispatch: 'queued',
    ...extra,
  } as unknown as Message
}

function mountChip(message: Message) {
  const wrapper = mount(ForwardedChip, {
    props: { message },
    global: { mocks: { $t: (key: string) => key } },
  })
  mounted.push(wrapper)
  return wrapper
}

afterEach(() => {
  for (const wrapper of mounted.splice(0)) wrapper.unmount()
  vi.useRealTimers()
})

describe('ForwardedChip', () => {
  it('marks a forwarded bubble for agents', () => {
    const wrapper = mountChip(forward({ status: 'sent' }))
    expect(wrapper.text()).toContain('chat.forward.chip')
    expect(wrapper.find('[data-forward-delivery-state]').exists()).toBe(false)
  })

  it('shows "delayed" in an open chat once a queued forward passes the stall age, without a reload', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    const wrapper = mountChip(forward())
    expect(wrapper.find('[data-forward-delivery-state]').exists()).toBe(false)

    await vi.advanceTimersByTimeAsync(FORWARD_STALLED_AFTER_MS + FORWARD_CLOCK_TICK_MS)
    const note = wrapper.get('[data-forward-delivery-state]')
    expect(note.attributes('data-forward-delivery-state')).toBe('delayed')
    expect(note.text()).toBe('chat.forward.delayedNote')

    // A late status event settles it: the note goes away.
    await wrapper.setProps({ message: forward({ status: 'sent' }) })
    expect(wrapper.find('[data-forward-delivery-state]').exists()).toBe(false)
  })

  it('says a forward whose send attempt has no recorded result may have been sent', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    const note = mountChip(forward({
      forward_dispatch: 'provider',
      forward_dispatch_at: new Date(NOW - FORWARD_STALLED_AFTER_MS - 1_000).toISOString(),
    })).get('[data-forward-delivery-state]')
    expect(note.attributes('data-forward-delivery-state')).toBe('uncertain')
    expect(note.text()).toBe('chat.forward.uncertainDeliveryNote')
  })

  it('runs one shared clock only while a chip is mounted', () => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
    const before = vi.getTimerCount()
    const first = mountChip(forward())
    const second = mountChip(forward({ id: 'f-2' }))
    expect(vi.getTimerCount()).toBe(before + 1)
    first.unmount()
    expect(vi.getTimerCount()).toBe(before + 1)
    second.unmount()
    expect(vi.getTimerCount()).toBe(before)
    mounted.splice(0)
  })

  it('uses the server state when this copy has no marker', () => {
    const note = mountChip(forward({ forward_dispatch: undefined, forward_delivery_state: 'delayed' }))
      .get('[data-forward-delivery-state]')
    expect(note.attributes('data-forward-delivery-state')).toBe('delayed')
  })
})
