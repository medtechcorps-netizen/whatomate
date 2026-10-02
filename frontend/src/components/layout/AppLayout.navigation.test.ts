/** @vitest-environment happy-dom */

import { mount, type VueWrapper } from '@vue/test-utils'
import { nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AppLayout from './AppLayout.vue'
import {
  MORE_TOOLS_SECTION_ID,
  MORE_TOOLS_STORAGE_KEY,
  navigationSections,
} from './navigation'

const mocks = vi.hoisted(() => ({
  authStore: null as any,
  organizationsStore: null as any,
  unreadStore: null as any,
  route: null as any,
}))

const translations: Record<string, string> = {
  'nav.sectionPartner': 'Partner',
  'nav.sectionDailyWork': 'Daily work',
  'nav.sectionReports': 'Reports',
  'nav.sectionMoreTools': 'More tools',
  'nav.today': 'Today',
  'nav.omnichannel': 'Omnichannel Inbox',
  'nav.omnichannelUnreadOne': '{count} conversation with unread messages',
  'nav.omnichannelUnreadMany': '{count} conversations with unread messages',
  'nav.dashboard': 'Dashboard',
  'nav.analytics': 'Analytics',
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
    template: '<a v-bind="$attrs" :href="typeof to === \'string\' ? to : to.path"><slot /></a>',
  },
  RouterView: { template: '<div />' },
  useRoute: () => mocks.route,
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
  isUnreadRelevantInboxActivity: () => false,
  wsService: {
    connect: vi.fn(),
    disconnect: vi.fn(),
    getConnectionState: () => 'connected',
    onInboxActivity: vi.fn(() => vi.fn()),
    onConnectionStateChange: vi.fn(() => vi.fn()),
  },
}))

vi.mock('@/services/api', () => ({
  authService: { getWSToken: vi.fn() },
}))

vi.mock('@/components/brand/ReReplyLogo.vue', () => ({
  default: { template: '<div />' },
}))

const ALL_ENTITLEMENTS = [
  'omnichannel.enabled',
  'crm.enabled',
  'bookings.enabled',
  'commerce.enabled',
  'copilot.enabled',
]

function setAccess(permissions: string[] | 'all', entitlements: string[] = ALL_ENTITLEMENTS) {
  mocks.authStore.hasPermission = (resource: string, action: string) =>
    action === 'read' && (permissions === 'all' || permissions.includes(resource))
  mocks.authStore.hasProductEntitlement = (entitlement: string) => entitlements.includes(entitlement)
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

function desktopNav(view: VueWrapper) {
  return view.get('nav[role="menubar"]')
}

function section(view: VueWrapper, label: string) {
  return desktopNav(view).get(`[data-nav-section="${label}"]`)
}

function sectionPaths(view: VueWrapper, label: string) {
  return section(view, label)
    .findAll(':scope > a')
    .map(link => link.attributes('href'))
}

function moreToolsToggle(view: VueWrapper) {
  return desktopNav(view).find(`[data-testid="nav-section-toggle-${MORE_TOOLS_SECTION_ID}"]`)
}

function moreToolsGroup(view: VueWrapper) {
  return section(view, MORE_TOOLS_SECTION_ID)
}

function isShown(element: { attributes: (name: string) => string | undefined }) {
  return !(element.attributes('style') ?? '').includes('display: none')
}

const DAILY_WORK_PATHS = [
  '/today',
  '/inbox',
  '/chat',
  '/crm/pipeline',
  '/crm/tasks',
  '/calendar',
  '/commerce',
  '/settings/contacts',
]

const REPORT_PATHS = [
  '/',
  '/crm/insights',
  '/analytics/agents',
  '/analytics/meta-insights',
  '/analytics/search-visibility',
]

const MORE_TOOLS_PATHS = [
  '/launchpad',
  '/crm/automations',
  '/copilot',
  '/settings/accounts',
  '/chatbot',
  '/campaigns',
  '/templates',
  '/settings/canned-responses',
  '/flows',
  '/settings/tags',
  '/settings/teams',
  '/calling/logs',
  '/calling/ivr-flows',
  '/calling/transfers',
]

describe('AppLayout sidebar grouping', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    window.localStorage.clear()
    mocks.route = reactive({ path: '/', name: 'dashboard' })
    mocks.authStore = reactive({
      isAuthenticated: false,
      user: { id: 'agent-1', is_super_admin: false },
      organizationId: 'organization-1',
      hasPermission: () => true,
      hasProductEntitlement: () => true,
      refreshUserData: vi.fn(),
      ensureProductEntitlements: vi.fn(),
      logout: vi.fn(),
    })
    setAccess('all')
    mocks.organizationsStore = reactive({ selectedOrgId: null, resetForIdentityChange: vi.fn() })
    mocks.unreadStore = reactive({
      unreadConversationCount: 0,
      get hasUnread() {
        return (this.unreadConversationCount ?? 0) > 0
      },
      resetForIdentityChange: vi.fn(),
      setIdentity: vi.fn(),
      refresh: vi.fn().mockResolvedValue(true),
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.restoreAllMocks()
  })

  it('keeps every destination and only adds Today', () => {
    const groupedPaths = [...DAILY_WORK_PATHS, ...REPORT_PATHS, ...MORE_TOOLS_PATHS]
    const configuredPaths = navigationSections
      .filter(entry => !entry.pinBottom && entry.label !== 'nav.sectionPartner')
      .flatMap(entry => entry.items.map(item => item.path))
    expect(configuredPaths).toEqual(groupedPaths)
    expect(new Set(configuredPaths).size).toBe(configuredPaths.length)

    const partner = navigationSections.find(entry => entry.label === 'nav.sectionPartner')
    expect(partner?.items.map(item => item.path)).toEqual(['/resellers'])
    const pinned = navigationSections.find(entry => entry.pinBottom)
    expect(pinned?.items.map(item => item.path)).toEqual(['/settings'])
    expect(navigationSections.map(entry => entry.label)).toEqual([
      'nav.sectionPartner',
      'nav.sectionDailyWork',
      'nav.sectionReports',
      'nav.sectionMoreTools',
      '',
    ])
  })

  it('renders Daily work, Reports and More tools in order for a full-access user', () => {
    wrapper = mountLayout()

    const navText = desktopNav(wrapper).text()
    const order = ['Partner', 'Daily work', 'Reports', 'More tools'].map(label => navText.indexOf(label))
    expect(order.every(index => index >= 0)).toBe(true)
    expect([...order].sort((a, b) => a - b)).toEqual(order)

    expect(sectionPaths(wrapper, 'nav.sectionDailyWork')).toEqual(DAILY_WORK_PATHS)
    expect(sectionPaths(wrapper, 'nav.sectionReports')).toEqual(REPORT_PATHS)
    expect(sectionPaths(wrapper, MORE_TOOLS_SECTION_ID)).toEqual(MORE_TOOLS_PATHS)
    expect(isShown(section(wrapper, 'nav.sectionDailyWork'))).toBe(true)
    expect(isShown(section(wrapper, 'nav.sectionReports'))).toBe(true)
    // Only More tools has a toggle.
    expect(desktopNav(wrapper).findAll('button[aria-controls]')).toHaveLength(1)
  })

  it('keeps More tools closed by default with a labelled disclosure button', () => {
    wrapper = mountLayout()

    const toggle = moreToolsToggle(wrapper)
    expect(toggle.exists()).toBe(true)
    expect(toggle.element.tagName).toBe('BUTTON')
    expect(toggle.attributes('type')).toBe('button')
    expect(toggle.text()).toBe('More tools')
    expect(toggle.attributes('aria-expanded')).toBe('false')

    const group = moreToolsGroup(wrapper)
    expect(toggle.attributes('aria-controls')).toBe(group.attributes('id'))
    expect(isShown(group)).toBe(false)
  })

  it('remembers the open state in localStorage', async () => {
    wrapper = mountLayout()

    await moreToolsToggle(wrapper).trigger('click')
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')
    expect(isShown(moreToolsGroup(wrapper))).toBe(true)
    expect(window.localStorage.getItem(MORE_TOOLS_STORAGE_KEY)).toBe('open')

    wrapper.unmount()
    wrapper = mountLayout()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')
    expect(isShown(moreToolsGroup(wrapper))).toBe(true)

    await moreToolsToggle(wrapper).trigger('click')
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('false')
    expect(window.localStorage.getItem(MORE_TOOLS_STORAGE_KEY)).toBe('closed')

    wrapper.unmount()
    wrapper = mountLayout()
    expect(isShown(moreToolsGroup(wrapper))).toBe(false)
  })

  it('still works when browser storage is blocked', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked')
    })

    wrapper = mountLayout()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('false')

    await moreToolsToggle(wrapper).trigger('click')
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')
    expect(isShown(moreToolsGroup(wrapper))).toBe(true)
  })

  it('opens automatically on one of its pages and closes again when leaving', async () => {
    mocks.route.path = '/campaigns'
    mocks.route.name = 'campaigns'
    wrapper = mountLayout()

    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')
    expect(isShown(moreToolsGroup(wrapper))).toBe(true)
    expect(
      moreToolsGroup(wrapper).get('a[href="/campaigns"]').attributes('aria-current'),
    ).toBe('page')

    mocks.route.path = '/inbox'
    mocks.route.name = 'inbox'
    await nextTick()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('false')
    expect(isShown(moreToolsGroup(wrapper))).toBe(false)

    // Auto-opening never overwrites the remembered choice.
    expect(window.localStorage.getItem(MORE_TOOLS_STORAGE_KEY)).toBeNull()
  })

  it('opens automatically on a child page and shows that item’s sub-pages', async () => {
    mocks.route.path = '/chatbot/flows'
    mocks.route.name = 'chatbot-flows'
    wrapper = mountLayout()

    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')
    const group = moreToolsGroup(wrapper)
    expect(isShown(group)).toBe(true)
    expect(group.get('a[href="/chatbot/flows"]').attributes('aria-current')).toBe('page')
  })

  it('lets staff close an auto-opened group until they move to another page', async () => {
    mocks.route.path = '/settings/tags'
    mocks.route.name = 'tags'
    wrapper = mountLayout()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')

    await moreToolsToggle(wrapper).trigger('click')
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('false')
    expect(isShown(moreToolsGroup(wrapper))).toBe(false)

    mocks.route.path = '/settings/teams'
    mocks.route.name = 'teams'
    await nextTick()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('true')
  })

  it('opens automatically anywhere in Settings so manager tools stay visible there', async () => {
    mocks.route.path = '/settings'
    mocks.route.name = 'settings'
    wrapper = mountLayout()

    const toggle = moreToolsToggle(wrapper)
    expect(toggle.attributes('aria-expanded')).toBe('true')
    const group = moreToolsGroup(wrapper)
    expect(isShown(group)).toBe(true)
    expect(group.attributes('role')).toBe('group')
    expect(group.attributes('aria-labelledby')).toBe(toggle.attributes('id'))
    for (const path of ['/settings/accounts', '/settings/canned-responses', '/settings/tags', '/settings/teams']) {
      expect(group.find(`a[href="${path}"]`).exists()).toBe(true)
    }
    // Settings itself is not a More tools page.
    expect(group.find('a[aria-current="page"]').exists()).toBe(false)
    expect(window.localStorage.getItem(MORE_TOOLS_STORAGE_KEY)).toBeNull()

    // A path that only shares the prefix text does not count as Settings.
    mocks.route.path = '/settingsx'
    mocks.route.name = 'not-found'
    await nextTick()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('false')

    mocks.route.path = '/'
    mocks.route.name = 'dashboard'
    await nextTick()
    expect(moreToolsToggle(wrapper).attributes('aria-expanded')).toBe('false')
    expect(isShown(moreToolsGroup(wrapper))).toBe(false)
  })

  it('shows every More tools icon on the collapsed rail without a toggle', async () => {
    wrapper = mountLayout()
    expect(isShown(moreToolsGroup(wrapper))).toBe(false)

    await wrapper.get('button[aria-label="Collapse sidebar"]').trigger('click')
    await nextTick()

    expect(moreToolsToggle(wrapper).exists()).toBe(false)
    const group = moreToolsGroup(wrapper)
    expect(isShown(group)).toBe(true)
    expect(group.findAll(':scope > a')).toHaveLength(MORE_TOOLS_PATHS.length)
    expect(wrapper.get('button[aria-label="Expand sidebar"]').attributes('aria-expanded')).toBe('false')
  })

  it('gates Today exactly like CRM Insights', () => {
    const items = navigationSections.flatMap(entry => entry.items)
    const today = items.find(item => item.path === '/today')
    const insights = items.find(item => item.path === '/crm/insights')
    expect(today?.name).toBe('nav.today')
    expect(today?.entitlement).toBe('crm.enabled')
    expect(today?.entitlement).toBe(insights?.entitlement)
    expect(today?.anyPermissions).toEqual(insights?.anyPermissions)
    expect(today?.permission).toBeUndefined()
    expect(today?.requiredPermissions).toBeUndefined()

    const cases: Array<{ permissions: string[]; entitlements: string[]; visible: boolean }> = [
      { permissions: ['tasks'], entitlements: ['crm.enabled'], visible: true },
      { permissions: ['payments'], entitlements: ['crm.enabled'], visible: true },
      { permissions: ['tasks'], entitlements: [], visible: false },
      { permissions: ['conversations', 'chat'], entitlements: ['crm.enabled'], visible: false },
    ]
    for (const testCase of cases) {
      setAccess(testCase.permissions, testCase.entitlements)
      const view = mountLayout()
      const nav = desktopNav(view)
      expect(nav.find('a[href="/today"]').exists()).toBe(testCase.visible)
      expect(nav.find('a[href="/crm/insights"]').exists()).toBe(testCase.visible)
      view.unmount()
    }
  })

  it('keeps the Inbox item, label and unread badge unchanged inside Daily work', () => {
    mocks.unreadStore.unreadConversationCount = 5
    wrapper = mountLayout()

    const inboxItem = navigationSections
      .flatMap(entry => entry.items)
      .find(item => item.path === '/inbox')
    expect(inboxItem).toMatchObject({
      name: 'nav.omnichannel',
      permission: 'conversations',
      requiredPermissions: ['conversations', 'channel_accounts'],
      entitlement: 'omnichannel.enabled',
    })

    const badge = section(wrapper, 'nav.sectionDailyWork').get(
      '[data-testid="omnichannel-desktop-nav-unread-badge"]',
    )
    expect(badge.text()).toBe('5')
    const link = badge.element.closest('a')
    expect(link?.getAttribute('href')).toBe('/inbox')
    expect(link?.getAttribute('role')).toBe('menuitem')
    expect(link?.textContent).toContain('Omnichannel Inbox')
    expect(link?.getAttribute('aria-describedby')).toBe('omnichannel-nav-unread-description')
    expect(wrapper.findAll('[data-testid="omnichannel-desktop-nav-unread-badge"]')).toHaveLength(1)
    expect(wrapper.get('[data-testid="omnichannel-mobile-nav-unread-badge"]').text()).toBe('5')
  })

  it('keeps the three focused mobile destinations', () => {
    wrapper = mountLayout()

    const mobile = wrapper.get('[aria-label="Mobile workspace"]')
    const links = mobile.findAll('a[data-mobile-primary]')
    expect(links.map(link => link.attributes('href'))).toEqual(['/', '/analytics/agents', '/inbox'])
    expect(links.map(link => link.text().trim())).toEqual(['Dashboard', 'Analytics', 'Omnichannel Inbox'])
    expect(mobile.text()).not.toContain('More tools')
  })
})
