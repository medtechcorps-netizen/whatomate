/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TooltipProvider } from '@/components/ui/tooltip'
import type { Message } from '@/stores/contacts'
import MessageActions from './MessageActions.vue'
import source from './MessageActions.vue?raw'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const QUICK = ['👍', '❤️'] as const

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: 'm-1',
    contact_id: 'c-1',
    direction: 'incoming',
    message_type: 'text',
    content: { body: 'Hello' },
    status: 'delivered',
    created_at: '2026-10-15T00:00:00Z',
    updated_at: '2026-10-15T00:00:00Z',
    ...overrides,
  }
}

const mounted: VueWrapper[] = []

function mountActions(props: Record<string, unknown> = {}) {
  // Tooltip needs its provider, as in App.vue.
  const Harness = defineComponent({
    setup(_, { attrs }) {
      return () => h(TooltipProvider, null, { default: () => h(MessageActions, attrs as never) })
    },
  })
  const wrapper = mount(Harness, {
    attrs: { message: message(), quickReactions: QUICK, reactionOpen: false, ...props },
    attachTo: document.body,
  })
  mounted.push(wrapper)
  return wrapper
}

afterEach(() => {
  while (mounted.length) mounted.pop()?.unmount()
  document.body.innerHTML = ''
})

describe('MessageActions', () => {
  it('keeps Reply a visible button with the lucide-reply icon and an accessible name', () => {
    const wrapper = mountActions()
    const reply = wrapper.get('[data-testid="chat-message-reply"]')
    expect(reply.element.tagName).toBe('BUTTON')
    expect(reply.find('[class*="lucide-reply"]').exists()).toBe(true)
    expect(reply.attributes('aria-label')).toBe('chat.reply')
    const react = wrapper.findAll('button').find(b => b.find('[class*="lucide-smile-plus"]').exists())
    expect(react?.attributes('aria-label')).toBe('chat.messageActions.react')
  })

  it('keeps the established strip footprint and marks an open picker', async () => {
    const wrapper = mountActions()
    const strip = wrapper.get('[data-testid="chat-message-actions"]')
    // Two rows tall; extra actions add a column, never height.
    for (const cls of ['grid', 'grid-rows-2', 'grid-flow-col', 'gap-0.5', 'self-center', 'ml-1']) {
      expect(strip.classes()).toContain(cls)
    }
    for (const button of strip.findAll('button')) {
      expect(button.classes()).toEqual(expect.arrayContaining(['h-6', 'w-6']))
    }
    // No Tailwind opacity utility may hide the strip at rest any more.
    expect(strip.classes().filter(c => c.includes('opacity-'))).toEqual([])
    expect(strip.classes()).not.toContain('is-open')

    await wrapper.setProps({ reactionOpen: true } as never)
    expect(wrapper.get('[data-testid="chat-message-actions"]').classes()).toContain('is-open')
  })

  it('stays visible but muted at rest, full on hover, focus or open picker', () => {
    // Tailwind currently emits no group-* rules (postcss-selector-parser 6.1.3),
    // so visibility is owned by these scoped rules.
    expect(source).toMatch(/\.message-actions\s*\{\s*opacity: 0\.6;/)
    expect(source).toMatch(/\.group:hover \.message-actions,\s*\.message-actions:focus-within,\s*\.message-actions\.is-open\s*\{\s*opacity: 1;/)
  })

  it('is at full strength on touch screens, which cannot hover', () => {
    expect(source).toMatch(/@media \(hover: none\)\s*\{\s*\.message-actions\s*\{\s*opacity: 1;/)
  })

  it('emits reply with the message', async () => {
    const wrapper = mountActions()
    await wrapper.get('[data-testid="chat-message-reply"]').trigger('click')
    const actions = wrapper.findComponent(MessageActions)
    expect(actions.emitted('reply')?.[0]?.[0]).toMatchObject({ id: 'm-1' })
  })

  it('emits a reaction from the open picker', async () => {
    const wrapper = mountActions({ reactionOpen: true })
    await flushPromises()
    const emoji = Array.from(document.body.querySelectorAll('button')).find(b => b.textContent?.trim() === '❤️')
    expect(emoji).toBeTruthy()
    emoji!.click()
    expect(wrapper.findComponent(MessageActions).emitted('react')?.[0]).toEqual(['m-1', '❤️'])
  })

  it('offers Retry only for failed non-template outgoing messages', async () => {
    expect(mountActions().findAll('[class*="lucide-rotate-cw"]')).toHaveLength(0)
    expect(
      mountActions({ message: message({ direction: 'outgoing', status: 'failed', message_type: 'template' }) })
        .findAll('[class*="lucide-rotate-cw"]'),
    ).toHaveLength(0)

    const wrapper = mountActions({ message: message({ direction: 'outgoing', status: 'failed' }) })
    const retry = wrapper.findAll('button').find(b => b.find('[class*="lucide-rotate-cw"]').exists())!
    expect(retry.attributes('aria-label')).toBe('chat.retry')
    await retry.trigger('click')
    expect(wrapper.findComponent(MessageActions).emitted('retry')?.[0]?.[0]).toMatchObject({ id: 'm-1' })

    await wrapper.setProps({ retrying: true } as never)
    const busy = wrapper.findAll('button').find(b => b.attributes('aria-label') === 'chat.retry')!
    expect(busy.attributes('disabled')).toBeDefined()
  })

  it('never renders composer icons that E2E specs locate globally', () => {
    const wrapper = mountActions({ message: message({ direction: 'outgoing', status: 'failed' }) })
    // chat.spec / chat-composer.spec match these by class substring or exact class.
    for (const icon of ['[class*="lucide-send"]', '[class*="lucide-paperclip"]', '.lucide-smile-icon', '[class*="lucide-x"]']) {
      expect(wrapper.find(icon).exists()).toBe(false)
    }
  })

  it('offers Forward only with permission and for forwardable messages', async () => {
    expect(mountActions().find('[data-testid="chat-message-forward"]').exists()).toBe(false)
    expect(
      mountActions({ canForward: true, message: message({ message_type: 'sticker', media_url: '/m' }) })
        .find('[data-testid="chat-message-forward"]').exists(),
    ).toBe(false)

    const wrapper = mountActions({ canForward: true })
    const forward = wrapper.get('[data-testid="chat-message-forward"]')
    expect(forward.element.tagName).toBe('BUTTON')
    expect(forward.attributes('aria-label')).toBe('chat.forward.action')
    expect(forward.find('[class*="lucide-forward"]').exists()).toBe(true)
    // chat.spec locates the composer send button by lucide-send.
    expect(forward.find('[class*="lucide-send"]').exists()).toBe(false)
    await forward.trigger('click')
    expect(wrapper.findComponent(MessageActions).emitted('forward')?.[0]?.[0]).toMatchObject({ id: 'm-1' })
  })

  it('never offers Retry on a failed forwarded bubble (it is forwarded again instead)', () => {
    const failedForward = { ...message({ direction: 'outgoing', status: 'failed' }), is_forwarded: true } as Message
    expect(mountActions({ message: failedForward }).findAll('[class*="lucide-rotate-cw"]')).toHaveLength(0)
  })
})
