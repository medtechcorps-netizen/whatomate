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

const MESSAGES = [
  message('m-1', 'First message'),
  message('m-2', ''),
  { ...message('m-3', ''), message_type: 'image', media_url: '/m/3' },
  { ...message('m-4', '[Template: appointment_reminder]'), message_type: 'template' },
]

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
    <div id="message-m-3" data-testid="chat-message" data-message-id="m-3" class="flex group">
      <div class="chat-bubble"><img data-testid="img-3" alt="photo"><span data-testid="time-3">10:00</span></div>
    </div>
    <div id="message-m-4" data-testid="chat-message" data-message-id="m-4" class="flex group">
      <div class="chat-bubble"><span data-testid="text-4">[Template: appointment_reminder]</span></div>
    </div>
    <div data-testid="separator">Today</div>
  `
  document.body.appendChild(container)
}

function mountMenu(extra: Record<string, unknown> = {}) {
  const wrapper = mount(MessageContextMenu, {
    props: {
      container,
      messages: MESSAGES,
      quickReactions: ['👍', '🙏'],
      ...extra,
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
  container.querySelector(selector)!.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerId: 1, isPrimary: true, ...init }))
}

// Menu actions run after reka's post-close focus step (a 0 ms timer).
async function settle() {
  await flushPromises()
  await new Promise(resolve => setTimeout(resolve, 5))
  await flushPromises()
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
  vi.restoreAllMocks()
  // Close an open menu first so reka-ui's dismissable-layer stack is unwound.
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
  await flushPromises()
  while (mounted.length) mounted.pop()?.unmount()
  window.getSelection()?.removeAllRanges()
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
    await settle()
    expect(wrapper.emitted('reply')?.[0]?.[0]).toMatchObject({ id: 'm-1' })

    rightClick('[data-testid="text-1"]')
    await flushPromises()
    const reaction = Array.from(document.body.querySelectorAll<HTMLElement>('[role="menuitem"]'))
      .find(item => item.getAttribute('aria-label') === 'chat.messageActions.reactWith:{"emoji":"🙏"}')
    reaction!.click()
    await settle()
    expect(wrapper.emitted('react')?.[0]).toEqual(['m-1', '🙏'])
  })

  it('focuses the composer inside the Reply gesture and keeps it after the menu closes', async () => {
    const composer = document.createElement('textarea')
    document.body.appendChild(composer)
    const wrapper = mountMenu({ composer })
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    menuItem('chat.reply')!.click()
    // Synchronously, inside the click: iOS only raises the keyboard there.
    expect(document.activeElement).toBe(composer)
    // The reply bookkeeping still waits for the menu to close.
    expect(wrapper.emitted('reply')).toBeUndefined()
    await settle()
    expect(menu()).toBeNull()
    expect(wrapper.emitted('reply')).toHaveLength(1)
    expect(document.activeElement).toBe(composer)
  })

  it('copies the message text', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    mountMenu()
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    menuItem('chat.messageActions.copyText')!.click()
    // Synchronously, inside the click: WebKit refuses clipboard writes later.
    expect(writeText).toHaveBeenCalledWith('First message')
    await settle()
    expect(mocks.toastSuccess).toHaveBeenCalledWith('chat.messageActions.copied')
  })

  it('hides Copy text when there is no real message text to copy', async () => {
    mountMenu()
    // Empty text, an image without caption, and a server placeholder body.
    for (const selector of ['[data-testid="text-2"]', '[data-testid="time-3"]', '[data-testid="text-4"]']) {
      rightClick(selector)
      await flushPromises()
      expect(menu()).not.toBeNull()
      expect(menuItem('chat.reply')).toBeTruthy()
      expect(menuItem('chat.messageActions.copyText')).toBeUndefined()
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
      await settle()
    }
  })

  it('leaves the browser menu for links, images, the action strip, and non-message rows', async () => {
    mountMenu()
    for (const selector of ['[data-testid="link-1"]', '[data-testid="img-3"]', '[data-testid="strip-1"]', '[data-testid="separator"]']) {
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

  it('suppresses only the native callout on the pressed bubble, never text selection', async () => {
    vi.useFakeTimers()
    mountMenu()
    const bubble = container.querySelector<HTMLElement>('#message-m-1 .chat-bubble')!
    const setProperty = vi.spyOn(bubble.style, 'setProperty')
    const removeProperty = vi.spyOn(bubble.style, 'removeProperty')
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    expect(setProperty).toHaveBeenCalledWith('-webkit-touch-callout', 'none')
    // Partial selection (an IC or phone number) must stay possible on touch.
    expect(bubble.style.userSelect).toBe('')
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).not.toBeNull()
    pointer('pointerup', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    expect(removeProperty).toHaveBeenCalledWith('-webkit-touch-callout')
  })

  it('lets a selection that starts during the press win over the menu', async () => {
    vi.useFakeTimers()
    mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    const range = document.createRange()
    range.selectNodeContents(container.querySelector('[data-testid="text-1"]')!)
    window.getSelection()!.removeAllRanges()
    window.getSelection()!.addRange(range)
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()
    expect(window.getSelection()!.isCollapsed).toBe(false)
    window.getSelection()!.removeAllRanges()
  })

  it('respects an existing text selection on long-press, like right-click', async () => {
    vi.useFakeTimers()
    mountMenu()
    const text = container.querySelector('[data-testid="text-1"]')!
    const range = document.createRange()
    range.selectNodeContents(text)
    window.getSelection()!.removeAllRanges()
    window.getSelection()!.addRange(range)
    expect(rightClick('[data-testid="text-1"]').defaultPrevented).toBe(false)
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch', clientX: 10, clientY: 10 })
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()
    window.getSelection()!.removeAllRanges()
  })

  it('ignores mouse and pen presses and a released touch press', async () => {
    vi.useFakeTimers()
    mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'mouse', clientX: 10, clientY: 10 })
    await vi.advanceTimersByTimeAsync(600)
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'pen', clientX: 10, clientY: 10 })
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()
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

  it.each([false, true])('gives a late native selection priority after custom open (contextmenu=%s)', async contextMenu => {
    vi.useFakeTimers()
    const wrapper = mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch' })
    await vi.advanceTimersByTimeAsync(550)
    expect(menu()).not.toBeNull()
    const staleReaction = document.body.querySelector<HTMLElement>('[aria-label^="chat.messageActions.reactWith"]')!
    const range = document.createRange()
    range.selectNodeContents(container.querySelector('[data-testid="text-1"]')!)
    window.getSelection()!.addRange(range)
    if (contextMenu) expect(rightClick('[data-testid="text-1"]').defaultPrevented).toBe(false)
    else document.dispatchEvent(new Event('selectionchange'))
    await flushPromises()
    expect(menu()).toBeNull()
    expect(window.getSelection()!.toString()).toBe('First message')
    staleReaction.click()
    await vi.advanceTimersByTimeAsync(1200)
    expect(wrapper.emitted('react')).toBeUndefined()
    expect(container.querySelector<HTMLElement>('.chat-bubble')!.style.userSelect).toBe('')
  })

  it('preserves selection starting outside but intersecting the bubble', async () => {
    mountMenu()
    const range = document.createRange()
    range.setStart(container, 0)
    range.setEnd(container.querySelector('[data-testid="text-1"]')!.firstChild!, 5)
    window.getSelection()!.addRange(range)
    expect(rightClick('[data-testid="text-1"]').defaultPrevented).toBe(false)
    await flushPromises()
    expect(menu()).toBeNull()
    expect(window.getSelection()!.isCollapsed).toBe(false)
  })

  it.each([false, true])('dismisses on the non-bubbling ancestor viewport scroll (alreadyOpen=%s)', async alreadyOpen => {
    vi.useFakeTimers()
    const viewport = document.createElement('div')
    viewport.dataset.rekaScrollAreaViewport = ''
    container.before(viewport)
    viewport.appendChild(container)
    const wrapper = mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch' })
    if (alreadyOpen) await vi.advanceTimersByTimeAsync(550)
    const staleReply = menuItem('chat.reply')
    const scroll = new Event('scroll', { bubbles: false, cancelable: true })
    viewport.scrollTop = 60
    viewport.dispatchEvent(scroll)
    expect(scroll.defaultPrevented).toBe(false)
    expect(viewport.scrollTop).toBe(60)
    await vi.advanceTimersByTimeAsync(1200)
    expect(menu()).toBeNull()
    staleReply?.click()
    expect(wrapper.emitted('reply')).toBeUndefined()
  })

  it('ignores another panel scroll but closes on transcript or document scrolling', async () => {
    mountMenu()
    const unrelated = document.createElement('div')
    document.body.appendChild(unrelated)
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    unrelated.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(menu()).not.toBeNull()
    container.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(menu()).toBeNull()
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    document.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(menu()).toBeNull()
  })

  it('deduplicates only the same touch target and lets another desktop target replace it', async () => {
    vi.useFakeTimers()
    const wrapper = mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch' })
    await vi.advanceTimersByTimeAsync(550)
    const duplicate = new PointerEvent('contextmenu', { bubbles: true, cancelable: true, pointerType: 'touch', pointerId: 1 })
    container.querySelector('[data-testid="text-1"]')!.dispatchEvent(duplicate)
    expect(duplicate.defaultPrevented).toBe(true)
    const desktop = new PointerEvent('contextmenu', { bubbles: true, cancelable: true, pointerType: 'mouse', button: 2 })
    container.querySelector('[data-testid="text-2"]')!.dispatchEvent(desktop)
    await flushPromises()
    menuItem('chat.reply')!.click()
    await vi.advanceTimersByTimeAsync(1200)
    expect(wrapper.emitted('reply')).toHaveLength(1)
    expect(wrapper.emitted('reply')?.[0]?.[0]).toMatchObject({ id: 'm-2' })
  })

  it.each(['pointerup', 'pointercancel', 'second-touch'])('cancels a pending press on %s outside the transcript', async ending => {
    vi.useFakeTimers()
    mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch' })
    document.body.dispatchEvent(new PointerEvent(ending === 'second-touch' ? 'pointerdown' : ending, {
      bubbles: true, pointerType: 'touch', pointerId: ending === 'second-touch' ? 2 : 1, isPrimary: ending !== 'second-touch',
    }))
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()
    expect(container.querySelector<HTMLElement>('.chat-bubble')!.style.getPropertyValue('-webkit-touch-callout')).toBe('')
  })

  it.each(['removed-row', 'rebound-row', 'removed-message', 'scope'])('invalidates an open target on %s before any gesture effect', async change => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const composer = document.createElement('textarea')
    document.body.appendChild(composer)
    const focus = vi.spyOn(composer, 'focus')
    const wrapper = mountMenu({ composer, scopeKey: 'org-a/contact-a' })
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    const staleReply = menuItem('chat.reply')!
    const staleCopy = menuItem('chat.messageActions.copyText')!
    if (change === 'removed-row') container.querySelector('#message-m-1')!.remove()
    else if (change === 'rebound-row') container.querySelector('#message-m-1')!.setAttribute('data-message-id', 'm-2')
    else if (change === 'removed-message') await wrapper.setProps({ messages: MESSAGES.slice(1) })
    else await wrapper.setProps({ scopeKey: 'org-b/contact-b' })
    // DOM changes may precede the observer. The action fence still refuses.
    staleReply.click()
    staleCopy.click()
    await settle()
    expect(menu()).toBeNull()
    expect(focus).not.toHaveBeenCalled()
    expect(writeText).not.toHaveBeenCalled()
    expect(wrapper.emitted('reply')).toBeUndefined()
  })

  it('cancels accepted queued work when scope changes before the close callback', async () => {
    const wrapper = mountMenu({ scopeKey: 'first' })
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    menuItem('chat.reply')!.click()
    await wrapper.setProps({ scopeKey: 'second' })
    await settle()
    expect(wrapper.emitted('reply')).toBeUndefined()
  })

  it('cannot reopen from a queued trigger after scrolling or scope invalidation', async () => {
    const wrapper = mountMenu({ scopeKey: 'first' })
    rightClick('[data-testid="text-1"]')
    container.dispatchEvent(new Event('scroll'))
    await settle()
    expect(menu()).toBeNull()
    rightClick('[data-testid="text-1"]')
    await wrapper.setProps({ scopeKey: 'second' })
    await settle()
    expect(menu()).toBeNull()
  })

  it('invalidates a pending gesture when its original row is replaced with the same message ID', async () => {
    vi.useFakeTimers()
    mountMenu()
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch' })
    const row = container.querySelector('#message-m-1')!
    row.replaceWith(row.cloneNode(true))
    await vi.advanceTimersByTimeAsync(600)
    expect(menu()).toBeNull()
  })

  it('restores an existing callout declaration with its original priority after pointer cancellation', async () => {
    vi.useFakeTimers()
    mountMenu()
    const bubble = container.querySelector<HTMLElement>('.chat-bubble')!
    bubble.style.setProperty('-webkit-touch-callout', 'default', 'important')
    const original = bubble.style.getPropertyValue('-webkit-touch-callout')
    const priority = bubble.style.getPropertyPriority('-webkit-touch-callout')
    pointer('pointerdown', '[data-testid="text-1"]', { pointerType: 'touch' })
    await vi.advanceTimersByTimeAsync(550)
    pointer('pointercancel', '[data-testid="text-1"]', { pointerType: 'touch' })
    await flushPromises()
    expect(menu()).toBeNull()
    expect(bubble.style.getPropertyValue('-webkit-touch-callout')).toBe(original)
    expect(bubble.style.getPropertyPriority('-webkit-touch-callout')).toBe(priority)
  })

  it('resolves updated message content for Copy instead of the captured object', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    const wrapper = mountMenu()
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    await wrapper.setProps({ messages: [message('m-1', 'Updated text'), ...MESSAGES.slice(1)] })
    menuItem('chat.messageActions.copyText')!.click()
    expect(writeText).toHaveBeenCalledExactlyOnceWith('Updated text')
    await settle()
  })

  it('offers Forward only with permission and for forwardable messages', async () => {
    mountMenu()
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    expect(menuItem('chat.forward.action')).toBeUndefined()
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await settle()
    while (mounted.length) mounted.pop()?.unmount()

    const wrapper = mountMenu({ canForward: true })
    rightClick('[data-testid="text-1"]')
    await flushPromises()
    menuItem('chat.forward.action')!.click()
    // Forward opens a dialog, so it also waits until the menu has closed.
    expect(wrapper.emitted('forward')).toBeUndefined()
    await settle()
    expect(wrapper.emitted('forward')?.[0]?.[0]).toMatchObject({ id: 'm-1' })

    // m-2 is a text message without text: nothing to forward.
    rightClick('[data-testid="text-2"]')
    await flushPromises()
    expect(menuItem('chat.forward.action')).toBeUndefined()
  })
})
