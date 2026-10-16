/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import ChatListFilters from './ChatListFilters.vue'
import ChatRowActionsMenu from './ChatRowActionsMenu.vue'
import ContactAssigneeBadge from './ContactAssigneeBadge.vue'

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({
    t: (key: string, params?: Record<string, string>) =>
      params?.name ? `${key}:${params.name}` : key,
  }),
}))

const passThrough = { template: '<div><slot /></div>' }
const tooltipStubs = {
  Tooltip: passThrough,
  TooltipTrigger: passThrough,
  TooltipContent: { template: '<div data-testid="tooltip-content"><slot /></div>' },
}
// The real menu is portaled and opens on pointer events; these stubs render
// the items inline so selection can be asserted directly.
const menuStubs = {
  DropdownMenu: passThrough,
  DropdownMenuTrigger: passThrough,
  DropdownMenuContent: passThrough,
  DropdownMenuItem: defineComponent({
    emits: ['select'],
    setup(_, { slots, emit, attrs }) {
      return () => h('button', { ...attrs, type: 'button', onClick: () => emit('select') }, slots.default?.())
    },
  }),
}

function laneDClassTokens(html: string) {
  return Array.from(html.matchAll(/class="([^"]*)"/g)).flatMap(match => match[1].split(/\s+/))
}

describe('lane D chat components', () => {
  it('use no Tailwind group-/peer- variants, which the production CSS drops', () => {
    const tokens = [
      ...laneDClassTokens(mount(ChatListFilters, { global: { stubs: tooltipStubs } }).html()),
      ...laneDClassTokens(mount(ContactAssigneeBadge, {
        props: { contact: { assigned_user_id: 'u-1', assigned_user_name: 'Ben Lim' } },
      }).html()),
      ...laneDClassTokens(mount(ChatRowActionsMenu, {
        props: { contact: { id: 'c-1', unread_count: 1 } },
        global: { stubs: menuStubs },
      }).html()),
    ]
    expect(tokens.length).toBeGreaterThan(0)
    expect(tokens.filter(token => token.includes('group-') || token.includes('peer-'))).toEqual([])
  })
})

describe('ChatListFilters', () => {
  it('renders the four chips with stable test ids and All selected by default', () => {
    const wrapper = mount(ChatListFilters, { global: { stubs: tooltipStubs } })
    const ids = wrapper.findAll('button').map(button => button.attributes('data-testid'))
    expect(ids).toEqual(['chat-filter-all', 'chat-filter-unread', 'chat-filter-mine', 'chat-filter-unassigned'])
    expect(wrapper.get('[data-testid="chat-filter-all"]').attributes('aria-pressed')).toBe('true')
    expect(wrapper.get('[data-testid="chat-filter-unread"]').attributes('aria-pressed')).toBe('false')
  })

  it('carries no numeric text or cursor-pointer class on any chip', () => {
    const wrapper = mount(ChatListFilters, { global: { stubs: tooltipStubs } })
    for (const chip of wrapper.findAll('button')) {
      expect(chip.text()).not.toMatch(/\d/)
      expect(chip.classes()).not.toContain('cursor-pointer')
    }
  })

  it('explains that chatbot-handled chats are stored as read', () => {
    const wrapper = mount(ChatListFilters, { global: { stubs: tooltipStubs } })
    expect(wrapper.get('[data-testid="tooltip-content"]').text()).toBe('chat.filters.unreadHint')
  })

  it('emits a changed filter only', async () => {
    const wrapper = mount(ChatListFilters, { props: { modelValue: 'mine' }, global: { stubs: tooltipStubs } })
    await wrapper.get('[data-testid="chat-filter-mine"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    await wrapper.get('[data-testid="chat-filter-unread"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([['unread']])
  })

  it('hides Unassigned for users limited to their own chats', () => {
    const wrapper = mount(ChatListFilters, { props: { showUnassigned: false }, global: { stubs: tooltipStubs } })
    expect(wrapper.find('[data-testid="chat-filter-unassigned"]').exists()).toBe(false)
  })
})

describe('ContactAssigneeBadge', () => {
  it('shows nothing for an unassigned chat', () => {
    const wrapper = mount(ContactAssigneeBadge, { props: { contact: { assigned_user_id: undefined } } })
    expect(wrapper.find('[data-testid="chat-assignee-badge"]').exists()).toBe(false)
  })

  it('names the assignee by first name and describes the assignment', () => {
    const wrapper = mount(ContactAssigneeBadge, {
      props: { contact: { assigned_user_id: 'u-2', assigned_user_name: 'Ben Lim' }, currentUserId: 'u-1' },
    })
    const badge = wrapper.get('[data-testid="chat-assignee-badge"]')
    expect(badge.text()).toBe('Ben')
    expect(badge.attributes('title')).toBe('chat.assignment.assignedTo:Ben Lim')
    expect(badge.attributes('data-assignee-kind')).toBe('other')
  })

  it('marks the current user and prefers the active transfer agent', () => {
    const wrapper = mount(ContactAssigneeBadge, {
      props: {
        contact: {
          assigned_user_id: 'u-2',
          assigned_user_name: 'Ben Lim',
          transfer_agent_id: 'u-1',
          transfer_agent_name: 'Aisyah Rahman',
        },
        currentUserId: 'u-1',
        variant: 'header',
      },
    })
    const badge = wrapper.get('[data-testid="chat-assignee-badge"]')
    expect(badge.text()).toBe('chat.assignment.you')
    expect(badge.attributes('data-assignee-kind')).toBe('you')
    expect(badge.attributes('title')).toBe('chat.assignment.assignedToYou')
  })

  it('falls back to a generic label when the name is unknown', () => {
    const wrapper = mount(ContactAssigneeBadge, {
      props: { contact: { transfer_agent_id: 'u-3' }, currentUserId: 'u-1' },
    })
    const badge = wrapper.get('[data-testid="chat-assignee-badge"]')
    expect(badge.text()).toBe('chat.assignment.teammate')
    expect(badge.attributes('title')).toBe('chat.assignment.handledBy:chat.assignment.teammate')
  })
})

describe('ChatRowActionsMenu', () => {
  function mountInRow(props: Record<string, unknown>) {
    const rowClick = vi.fn()
    const Row = defineComponent({
      components: { ChatRowActionsMenu },
      setup() {
        return { props, rowClick }
      },
      template: '<div data-testid="row" @click="rowClick"><ChatRowActionsMenu v-bind="props" /></div>',
    })
    const wrapper = mount(Row, { global: { stubs: menuStubs } })
    return { wrapper, rowClick }
  }

  it('offers Mark as read for an unread chat without selecting the row', async () => {
    const { wrapper, rowClick } = mountInRow({ contact: { id: 'c-1', unread_count: 2 } })
    const trigger = wrapper.get('[data-testid="chat-row-actions"]')
    expect(trigger.attributes('data-testid')).not.toBe('chat-contact')
    await trigger.trigger('click')
    expect(rowClick).not.toHaveBeenCalled()

    expect(wrapper.find('[data-testid="chat-row-mark-unread"]').exists()).toBe(false)
    await wrapper.get('[data-testid="chat-row-mark-read"]').trigger('click')
    const menu = wrapper.findComponent(ChatRowActionsMenu)
    expect(menu.emitted('mark-read')).toEqual([['c-1']])
  })

  it('treats a manual unread mark as unread', () => {
    const { wrapper } = mountInRow({ contact: { id: 'c-1', unread_count: 0, manual_unread: true } })
    expect(wrapper.find('[data-testid="chat-row-mark-read"]').exists()).toBe(true)
  })

  it('offers Mark as unread for a read chat', async () => {
    const { wrapper } = mountInRow({ contact: { id: 'c-1', unread_count: 0 } })
    await wrapper.get('[data-testid="chat-row-mark-unread"]').trigger('click')
    expect(wrapper.findComponent(ChatRowActionsMenu).emitted('mark-unread')).toEqual([['c-1']])
  })

  it('renders nothing for a user who cannot change read state', () => {
    for (const contact of [{ id: 'c-1', unread_count: 0 }, { id: 'c-2', unread_count: 3 }]) {
      const { wrapper } = mountInRow({ contact, canChangeReadState: false })
      expect(wrapper.find('[data-testid="chat-row-actions"]').exists()).toBe(false)
      expect(wrapper.find('[data-testid="chat-row-mark-read"]').exists()).toBe(false)
    }
  })

  it('keeps the trigger visible in the row flow, never invisible-but-clickable', () => {
    const { wrapper } = mountInRow({ contact: { id: 'c-1', unread_count: 1 } })
    const trigger = wrapper.get('[data-testid="chat-row-actions"]')
    const classes = trigger.classes()
    expect(classes).toContain('shrink-0')
    for (const hidden of ['absolute', 'opacity-0', 'invisible', 'hidden', 'sr-only']) {
      expect(classes).not.toContain(hidden)
    }
    // The production CSS build drops every Tailwind group-/peer- variant.
    expect(classes.filter(name => /(^|:)(group|peer)(-|\/|$)/.test(name) || name.includes('group-') || name.includes('peer-'))).toEqual([])
    const html = trigger.html()
    for (const reserved of ['lucide-send', 'lucide-paperclip', 'lucide-smile', 'lucide-reply', 'lucide-x']) {
      expect(html).not.toContain(`${reserved}"`)
      expect(html).not.toContain(`${reserved} `)
    }
  })
})
