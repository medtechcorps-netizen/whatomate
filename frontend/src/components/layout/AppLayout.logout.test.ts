/** @vitest-environment happy-dom */

import { flushPromises, shallowMount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import AppLayout from './AppLayout.vue'

const mocks = vi.hoisted(() => ({
  disconnect: vi.fn(),
  connect: vi.fn(),
  logout: vi.fn(),
  resetOrganizations: vi.fn(),
  resetOmnichannelUnread: vi.fn(),
  setOmnichannelIdentity: vi.fn(),
  refreshOmnichannelUnread: vi.fn(),
  routerPush: vi.fn(),
}))

vi.mock('vue-router', () => ({
  RouterLink: { template: '<a><slot /></a>' },
  RouterView: { template: '<div />' },
  useRoute: () => ({ path: '/', name: 'dashboard' }),
  useRouter: () => ({ push: mocks.routerPush }),
}))

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isAuthenticated: false,
    user: { id: 'agent-1', is_super_admin: false },
    organizationId: 'previous-tenant',
    hasPermission: (resource: string, action: string) =>
      action === 'read' && (resource === 'conversations' || resource === 'channel_accounts'),
    hasProductEntitlement: (entitlement: string) => entitlement === 'omnichannel.enabled',
    refreshUserData: vi.fn(),
    ensureProductEntitlements: vi.fn(),
    logout: mocks.logout,
  }),
}))

vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => ({
    selectedOrgId: 'previous-tenant',
    resetForIdentityChange: mocks.resetOrganizations,
  }),
}))

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => ({
    unreadConversationCount: null,
    hasUnread: false,
    resetForIdentityChange: mocks.resetOmnichannelUnread,
    setIdentity: mocks.setOmnichannelIdentity,
    refresh: mocks.refreshOmnichannelUnread,
  }),
}))

vi.mock('@/services/websocket', () => ({
  isUnreadRelevantInboxActivity: () => true,
  wsService: {
    connect: mocks.connect,
    disconnect: mocks.disconnect,
    getConnectionState: () => 'disconnected',
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

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(res => {
    resolve = res
  })
  return { promise, resolve }
}

describe('AppLayout logout identity isolation', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    vi.clearAllMocks()
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
  })

  it('disconnects first and clears the selected organization only after logout completes', async () => {
    const logoutRequest = deferred()
    mocks.logout.mockReturnValueOnce(logoutRequest.promise)
    wrapper = shallowMount(AppLayout, {
      global: {
        mocks: { $t: (key: string) => key },
        stubs: {
          UserMenu: {
            emits: ['logout'],
            template: '<button data-testid="logout" @click="$emit(\'logout\')">Logout</button>',
          },
          Transition: { template: '<div><slot /></div>' },
        },
      },
    })

    await wrapper.get('[data-testid="logout"]').trigger('click')
    expect(mocks.disconnect).toHaveBeenCalledTimes(1)
    expect(mocks.logout).toHaveBeenCalledTimes(1)
    expect(mocks.resetOrganizations).not.toHaveBeenCalled()
    expect(mocks.routerPush).not.toHaveBeenCalled()

    logoutRequest.resolve()
    await flushPromises()

    expect(mocks.resetOrganizations).toHaveBeenCalledTimes(1)
    expect(mocks.resetOmnichannelUnread).toHaveBeenCalledTimes(1)
    expect(mocks.routerPush).toHaveBeenCalledWith('/login')
  })
})
