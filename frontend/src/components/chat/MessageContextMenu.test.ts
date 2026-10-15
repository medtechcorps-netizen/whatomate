/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Message } from '@/stores/contacts'
import MessageContextMenu from './MessageContextMenu.vue'

const mocks = vi.hoisted(() => ({
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) => (params ? `${key}:${JSON.stringify(params)}` : key),
  }),
}))
vi.mock('vue-sonner', () => ({ toast: { success: mocks.toastSuccess, error: mocks.toastError } }))

function message(id: string, body: string): Message {
  return {
    id,
    contact_id: 'c-1',
    direction: 'incoming',
    message_type: 'text',
    content: { body },
    status: 'delivered',
    created_at: '2026-10-15T00:00:00Z',
    updated_at: '2026-10-15T00:00:00Z',
  }
}

const MESSAGES = [message('m-1', 'First message'), message('m-2', '')]

let container: HTMLElement
const mounted: VueWrapper[] = []

function buildTranscript() {
  container = document.createElement('div')
  container.setAttribute('data-testid', 'chat-message-list')
  container.innerHTML = `
    <div id="message-m-1" data-testid="chat-message" data-message-id="m-1" class="flex group">
      <div class="chat-bubble"><span data-testid="text-1">First message</span><a href="https://example.test" data-testid="link-1">link</a></div>
      <div class="message-actions"><button data-testid="strip-1">strip</button></div>
    </div>
    <div id="message-m-2" data-testid="chat-message" data-message-id="m-2" class="flex group">
      <div class="chat-bubble"><span data-testid="text-2">[Image]</span></div>
    </div>
    <div data-testid="separator">Today</div>
  `
  document.body.appendChild(container)
}

function mountMenu() {
  const wrapper = mount(MessageContextMenu, {
    props: {
      container,
      messages: MESSAGES,
      quickReactions: ['👍', '🙏'],
      getText: (m: Message) => m.content?.body ?? '',
    },
    attachTo: document.body,
  })
  mounted.push(wrapper)
  return wrapper
}

function rightClick(selector: string) {
  const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 40, clientY: 60 })
  container.querySelector(selector)!.dispatchEvent(event)
  return event
}

function pointer(type: string, selector: string, init: PointerEventInit) {
  container.querySelector(selector)!.dispatchEvent(new PointerEvent(type, { bubbles: true, ...init }))
}

function menu() {
  return document.body.querySelector('[data-testid="chat-message-context-menu"]')
}

function menuItem(text: string) {
  return Array.from(document.body.querySelectorAll('[role="menuitem"]'))
    .find(item => item.textContent?.includes(text)) as HTMLElement | undefined
}

beforeEach(() => {
  vi.clearAllMocks()
  buildTranscript()
})

afterEach(async () => {
  vi.useRealTimers()
  // Close an open menu first so reka-ui's dismissable-layer stack is unwound.
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
  await flushPromises()
  while (mounted.length) mounted.pop()?.unmount()
  document.body.innerHTML = ''
})

describe('MessageContextMenu', () => {
  it('opens on right-click over a bubble with Reply, React and Copy text', async () => {
    mountMenu()
    const event = rightClick('[data-testid="text-1"]')
    await flushPromises()

    expect(event.defaultPrevented).toBe(true)
    expect(menu()).not.toBeNull()
    expect(menuItem('chat.reply')).toBeTruthy()
    expect(menuItem('chat.messageActions.copyText')).toBeTruthy()
    expect(document.body.querySelector('[aria-label^="chat.messageActions.reactWith"]')).not.toBeNull()
    // The menu is portaled: no lucide-reply/x/send button is added to the list.
    expect(container.querySelectorAll('button')).toHaveLength(1)
  })

  it('emits reply and react for the message under the pointer', async () => {
    const wrapper = mountMenu()
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    menuItem('chat.reply')!.click()
    await flushPromises()
    expect(wrapper.emitted('reply')?.[0]?.[0]).toMatchObject({ id: 'm-1' })

    rightClick('[data-testid="text-1"]')
    await flushPromises()
    const reaction = Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitem"]'))
      .find(item => item.getAttribute('aria-label') === 'chat.messageActions.reactWith:{"emoji":"🙏"}')
    reaction!.click()
    await flushPromises()
    expect(wrapper.emitted('react')?.[0]).toEqual(['m-1', '🙏'])
  })

  it('copies the message text', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    mountMenu()
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    menuItem('chat.messageActions.copyText')!.click()
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('First message')
    expect(mocks.toastSuccess).toHaveBeenCalledWith('chat.messageActions.copied')
  })

  it('disables Copy text for a message without text', async () => {
    mountMenu()
    rightClick('[data-testid="text-2"]')
    await flushPromises()
    expect(menuItem('chat.messageActions.copyText')?.hasAttribute('data-disabled')).toBe(true)
  })

  it('leaves the browser menu for links, the action strip, and non-message rows', async () => {
    mountMenu()
    for (const selector of ['[data-testid="link-1"]', '[data-testid="strip-1"]', '[data-testid="separator"]']) {
      expect(rightClick(selector).defaultPrevented).toBe(false)
    }
    await flushPromises()
    expect(menu()).toBeNull()
  })

  it('opens after a touch long-press and not after a moved press', async () => {
    vi.useFakeTimers()
    mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    pointer('pointermove', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 40 })
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()

    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    await vi.advanceTimersByTimeAsync(600)
    await flushPromises()
    expect(menu()).not.toBeNull()
  })

  it('ignores a mouse press and a released touch press', async () => {
    vi.useFakeTimers()
    mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'mouse', clientX: 10, clientY: 10 })
    pointer('pointerdown', '[data-testid="text-2"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    pointer('pointerup', '[data-testid="text-2"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()
  })

  it('stops listening when unmounted', async () => {
    const wrapper = mountMenu()
    wrapper.unmount()
    mounted.pop()
    expect(rightClick('[data-testid="text-1"]').defaultPrevented).toBe(false)
  })
})
