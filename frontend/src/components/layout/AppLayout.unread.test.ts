/** @vitest-environment happy-dom */

import { mount, type VueWrapper } from '@vue/test-utils'
import { nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AppLayout from './AppLayout.vue'

const mocks = vi.hoisted(() => ({
  authStore: null as any,
  organizationsStore: null as any,
  unreadStore: null as any,
  connect: vi.fn(),
  disconnect: vi.fn(),
  onInboxActivity: vi.fn((_callback: (event: { type: string; payload?: any }) => void) => vi.fn()),
  onConnectionStateChange: vi.fn((callback: (state: string) => void) => {
    callback('connected')
    return vi.fn()
  }),
}))

const translations: Record<string, string> = {
  'nav.omnichannel': 'Omnichannel Inbox',
  'nav.omnichannelUnreadOne': '{count} conversation with unread messages',
  'nav.omnichannelUnreadMany': '{count} conversations with unread messages',
  'nav.openMobileWorkspaceMenu': 'Open mobile workspace menu',
  'nav.openMobileWorkspaceMenuWithUnread': 'Open mobile workspace menu. {unread}',
  'nav.expandSidebar': 'Expand sidebar',
  'nav.collapseSidebar': 'Collapse sidebar',
}

function translate(key: string, params?: Record<string, unknown>) {
  const template = translations[key] ?? key
  return template.replace(/\{(\w+)\}/g, (_match, name: string) => String(params?.[name] ?? ''))
}

vi.mock('vue-router', () => ({
  RouterLink: {
    props: ['to'],
    inheritAttrs: false,
    template: '<a v-bind="$attrs"><slot /></a>',
  },
  RouterView: { template: '<div />' },
  useRoute: () => ({ path: '/', name: 'dashboard' }),
  useRouter: () => ({ push: vi.fn() }),
}))

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: translate }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => mocks.authStore,
}))

vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => mocks.organizationsStore,
}))

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => mocks.unreadStore,
}))

vi.mock('@/services/websocket', () => ({
  isUnreadRelevantInboxActivity: (event: { type: string; payload?: any }) => {
    if (event.type === 'status_update') return false
    if (event.type === 'new_message') return event.payload?.direction !== 'outgoing'
    if (event.type === 'realtime_sync') {
      return event.payload?.kind !== 'message_status_changed'
    }
    return event.type === 'channel_sync'
  },
  wsService: {
    connect: mocks.connect,
    disconnect: mocks.disconnect,
    getConnectionState: () => 'connected',
    onInboxActivity: mocks.onInboxActivity,
    onConnectionStateChange: mocks.onConnectionStateChange,
  },
}))

vi.mock('@/services/api', () => ({
  authService: { getWSToken: vi.fn() },
}))

vi.mock('@/components/brand/ReReplyLogo.vue', () => ({
  default: { template: '<div />' },
}))

function unreadStore(count: number | null) {
  return reactive({
    unreadConversationCount: count,
    get hasUnread() {
      return (this.unreadConversationCount ?? 0) > 0
    },
    resetForIdentityChange: vi.fn(),
    setIdentity: vi.fn(),
    refresh: vi.fn().mockResolvedValue(true),
  })
}

function mountLayout() {
  return mount(AppLayout, {
    global: {
      mocks: { $t: translate },
      stubs: {
        ActiveCallPanel: true,
        OrganizationSwitcher: true,
        ScrollArea: { template: '<div><slot /></div>' },
        ScrollToTop: true,
        Transition: { template: '<div><slot /></div>' },
        UserMenu: true,
        Button: {
          inheritAttrs: false,
          template: '<button v-bind="$attrs"><slot /></button>',
        },
      },
    },
  })
}

function desktopBadge(view: VueWrapper) {
  return view.find('[data-testid="omnichannel-desktop-nav-unread-badge"]')
}

function mobileBadge(view: VueWrapper) {
  return view.find('[data-testid="omnichannel-mobile-nav-unread-badge"]')
}

describe('AppLayout Omnichannel unread marker', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    vi.clearAllMocks()
    mocks.authStore = reactive({
      isAuthenticated: false,
      user: { id: 'agent-1', is_super_admin: false },
      organizationId: 'organization-1',
      hasPermission: (resource: string, action: string) =>
        action === 'read' && (resource === 'conversations' || resource === 'channel_accounts'),
      hasProductEntitlement: (entitlement: string) => entitlement === 'omnichannel.enabled',
      refreshUserData: vi.fn(),
      ensureProductEntitlements: vi.fn(),
      logout: vi.fn(),
    })
    mocks.organizationsStore = reactive({
      selectedOrgId: null,
      resetForIdentityChange: vi.fn(),
    })
    mocks.unreadStore = unreadStore(0)
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
  })

  it('hides every unread marker when the count is zero or unknown', async () => {
    wrapper = mountLayout()

    expect(desktopBadge(wrapper).exists()).toBe(false)
    expect(mobileBadge(wrapper).exists()).toBe(false)
    expect(wrapper.find('[data-testid="omnichannel-mobile-menu-unread-dot"]').exists()).toBe(false)
    expect(wrapper.find('#omnichannel-nav-unread-description').exists()).toBe(false)

    mocks.unreadStore.unreadConversationCount = null
    await nextTick()

    expect(desktopBadge(wrapper).exists()).toBe(false)
    expect(mobileBadge(wrapper).exists()).toBe(false)
    expect(wrapper.find('#omnichannel-nav-unread-description').exists()).toBe(false)
  })

  it('shows the exact count on desktop and mobile with a stable accessible nav name', async () => {
    mocks.unreadStore = unreadStore(7)
    wrapper = mountLayout()

    expect(desktopBadge(wrapper).text()).toBe('7')
    expect(mobileBadge(wrapper).text()).toBe('7')
    expect(desktopBadge(wrapper).attributes('aria-hidden')).toBe('true')
    expect(mobileBadge(wrapper).attributes('aria-hidden')).toBe('true')

    const description = wrapper.get('#omnichannel-nav-unread-description')
    expect(description.text()).toBe('7 conversations with unread messages')

    const desktopLink = desktopBadge(wrapper).element.closest('a')
    const mobileLink = mobileBadge(wrapper).element.closest('a')
    expect(desktopLink?.textContent).toContain('Omnichannel Inbox')
    expect(desktopLink?.getAttribute('aria-describedby')).toBe(description.attributes('id'))
    expect(desktopLink?.getAttribute('title')).toBe('7 conversations with unread messages')
    expect(mobileLink?.getAttribute('aria-describedby')).toBe(description.attributes('id'))
    expect(mobileLink?.getAttribute('title')).toBe('7 conversations with unread messages')

    const mobileMenu = wrapper.get('button[aria-label^="Open mobile workspace menu"]')
    expect(mobileMenu.attributes('aria-label')).toBe(
      'Open mobile workspace menu. 7 conversations with unread messages',
    )
    expect(wrapper.find('[data-testid="omnichannel-mobile-menu-unread-dot"]').exists()).toBe(true)
  })

  it('caps the visual count at 99+ while keeping the exact count accessible', async () => {
    mocks.unreadStore = unreadStore(128)
    wrapper = mountLayout()

    expect(desktopBadge(wrapper).text()).toBe('99+')
    expect(mobileBadge(wrapper).text()).toBe('99+')
    expect(wrapper.get('#omnichannel-nav-unread-description').text()).toBe(
      '128 conversations with unread messages',
    )
    expect(desktopBadge(wrapper).element.closest('a')?.getAttribute('title')).toBe(
      '128 conversations with unread messages',
    )
  })

  it('switches the desktop marker to a compact dot without losing its accessible description', async () => {
    mocks.unreadStore = unreadStore(4)
    wrapper = mountLayout()

    await wrapper.get('button[aria-label="Collapse sidebar"]').trigger('click')
    await nextTick()

    const badge = desktopBadge(wrapper)
    expect(badge.classes()).toContain('lg:absolute')
    expect(badge.classes()).toContain('lg:h-2')
    expect(badge.classes()).toContain('lg:text-[0px]')
    expect(badge.element.closest('a')?.getAttribute('aria-describedby')).toBe(
      'omnichannel-nav-unread-description',
    )
    expect(wrapper.get('#omnichannel-nav-unread-description').text()).toBe(
      '4 conversations with unread messages',
    )
  })

  it('keeps the mobile count in its nav item and hides the closed-menu dot after opening', async () => {
    mocks.unreadStore = unreadStore(3)
    wrapper = mountLayout()

    const mobileMenu = wrapper.get('button[aria-label^="Open mobile workspace menu"]')
    expect(wrapper.find('[data-testid="omnichannel-mobile-menu-unread-dot"]').exists()).toBe(true)

    await mobileMenu.trigger('click')
    await nextTick()

    expect(wrapper.find('[data-testid="omnichannel-mobile-menu-unread-dot"]').exists()).toBe(false)
    expect(mobileBadge(wrapper).text()).toBe('3')
    expect(mobileBadge(wrapper).element.closest('a')?.getAttribute('aria-describedby')).toBe(
      'omnichannel-nav-unread-description',
    )
  })

  it('refreshes on mount and debounced inbox activity, then removes its listener', async () => {
    vi.useFakeTimers()
    const visibility = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    const random = vi.spyOn(Math, 'random').mockReturnValue(0.5)

    try {
      mocks.authStore.isAuthenticated = true
      wrapper = mountLayout()

      await vi.advanceTimersByTimeAsync(0)
      expect(mocks.unreadStore.setIdentity).toHaveBeenCalledWith('organization-1', 'agent-1')
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(1)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledWith('organization-1', 'agent-1')

      const activity = mocks.onInboxActivity.mock.calls[0]?.[0]
      expect(activity).toBeTypeOf('function')
      activity?.({ type: 'new_message' })
      await vi.advanceTimersByTimeAsync(249)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(1)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(2)

      activity?.({ type: 'status_update', payload: { status: 'delivered' } })
      activity?.({ type: 'new_message', payload: { direction: 'outgoing' } })
      activity?.({
        type: 'realtime_sync',
        payload: { kind: 'message_status_changed' },
      })
      await vi.advanceTimersByTimeAsync(250)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(2)

      await vi.advanceTimersByTimeAsync(119501)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(3)

      const unsubscribe = mocks.onInboxActivity.mock.results[0]?.value
      const unsubscribeConnection = mocks.onConnectionStateChange.mock.results[0]?.value
      wrapper.unmount()
      wrapper = null
      expect(unsubscribe).toHaveBeenCalledTimes(1)
      expect(unsubscribeConnection).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(240000)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(3)
    } finally {
      visibility.mockRestore()
      random.mockRestore()
      vi.useRealTimers()
    }
  })

  it('uses the faster fallback poll while realtime is disconnected', async () => {
    vi.useFakeTimers()
    const visibility = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    const random = vi.spyOn(Math, 'random').mockReturnValue(0.5)
    mocks.onConnectionStateChange.mockImplementationOnce(callback => {
      callback('disconnected')
      return vi.fn()
    })

    try {
      mocks.authStore.isAuthenticated = true
      wrapper = mountLayout()

      await vi.advanceTimersByTimeAsync(0)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(29999)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(1)
      await vi.advanceTimersByTimeAsync(2)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(2)
    } finally {
      visibility.mockRestore()
      random.mockRestore()
      vi.useRealTimers()
    }
  })

  it('refreshes immediately when the browser window regains focus', async () => {
    vi.useFakeTimers()
    const visibility = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')

    try {
      mocks.authStore.isAuthenticated = true
      wrapper = mountLayout()
      await vi.advanceTimersByTimeAsync(0)
      mocks.unreadStore.refresh.mockClear()

      window.dispatchEvent(new Event('focus'))
      await vi.advanceTimersByTimeAsync(0)

      expect(mocks.unreadStore.refresh).toHaveBeenCalledTimes(1)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledWith('organization-1', 'agent-1')

      wrapper.unmount()
      wrapper = null
      mocks.unreadStore.refresh.mockClear()
      window.dispatchEvent(new Event('focus'))
      await vi.advanceTimersByTimeAsync(0)
      expect(mocks.unreadStore.refresh).not.toHaveBeenCalled()
    } finally {
      visibility.mockRestore()
      vi.useRealTimers()
    }
  })

  it('rekeys the badge when a different user signs into the same organization', async () => {
    vi.useFakeTimers()
    const visibility = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')

    try {
      mocks.authStore.isAuthenticated = true
      wrapper = mountLayout()
      await vi.advanceTimersByTimeAsync(0)
      mocks.unreadStore.setIdentity.mockClear()
      mocks.unreadStore.refresh.mockClear()

      mocks.authStore.user = { id: 'agent-2', is_super_admin: false }
      await nextTick()

      expect(mocks.unreadStore.setIdentity).toHaveBeenCalledWith('organization-1', 'agent-2')
      await vi.advanceTimersByTimeAsync(0)
      expect(mocks.unreadStore.refresh).toHaveBeenCalledWith('organization-1', 'agent-2')
    } finally {
      visibility.mockRestore()
      vi.useRealTimers()
    }
  })
})
