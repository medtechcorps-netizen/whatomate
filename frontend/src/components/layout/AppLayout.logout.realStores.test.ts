/** @vitest-environment happy-dom */

// The menu sign-out through the real layout, auth and organizations stores and
// the real API client: a tab whose session a newer sign-in replaced leaves
// locally and leaves the newer session's identity and organization stored.

import { flushPromises, shallowMount, type VueWrapper } from '@vue/test-utils'
import axios from 'axios'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  routerPush: vi.fn(),
}))

vi.mock('vue-router', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-router')>()),
  RouterLink: { template: '<a><slot /></a>' },
  RouterView: { template: '<div />' },
  useRoute: () => ({ path: '/', name: 'dashboard' }),
  useRouter: () => ({ push: mocks.routerPush }),
}))

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), { loading: vi.fn(), error: vi.fn(), dismiss: vi.fn(), success: vi.fn() }),
}))

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => ({
    unreadConversationCount: null,
    hasUnread: false,
    resetForIdentityChange: vi.fn(),
    setIdentity: vi.fn(),
    refresh: vi.fn(),
  }),
}))

vi.mock('@/services/websocket', () => ({
  isUnreadRelevantInboxActivity: () => true,
  wsService: {
    connect: vi.fn(),
    disconnect: vi.fn(),
    getConnectionState: () => 'disconnected',
    onInboxActivity: vi.fn(() => vi.fn()),
    onConnectionStateChange: vi.fn(() => vi.fn()),
  },
}))

vi.mock('@/components/brand/ReReplyLogo.vue', () => ({
  default: { template: '<div />' },
}))

const U1 = { id: 'user-1', email: 'u1@example.com', organization_id: 'org-1' }
const U2 = { id: 'user-2', email: 'u2@example.com', organization_id: 'org-2' }
const originalLocation = window.location

describe('the layout sign-out with the real stores and API client', { timeout: 30_000 }, () => {
  // Transform the module graph once, so a cold first import cannot eat into
  // the test's own time; that instance is detached at once.
  beforeAll(async () => {
    await import('./AppLayout.vue')
    ;(await import('@/services/api')).disposeForTests()
  }, 60_000)

  let wrapper: VueWrapper | null = null
  let posts: string[] = []
  let fetchRevoke: ReturnType<typeof vi.fn>
  let loaded: { disposeForTests: () => void } | null = null

  beforeEach(() => {
    localStorage.clear()
    localStorage.setItem('user', JSON.stringify(U1))
    localStorage.setItem('whm_session_id', 's1')
    localStorage.setItem('selected_organization_id', 'org-1')
    document.cookie = 'whm_csrf=csrf-1; path=/'
    posts = []
    vi.spyOn(axios, 'post').mockImplementation(async (url: string, body: unknown) => {
      posts.push((body as { revoke?: boolean })?.revoke ? 'revoke' : url.replace(/^.*\/api/, ''))
      return { data: { data: {} }, status: 200 }
    })
    fetchRevoke = vi.fn(async () => {
      posts.push('fetch-revoke')
      return new Response(null, { status: 200 })
    })
    vi.stubGlobal('fetch', fetchRevoke)
    vi.stubGlobal('BroadcastChannel', undefined)
    mocks.routerPush.mockReset()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { pathname: '/chat', href: 'http://localhost:3000/chat', reload: vi.fn() },
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    loaded?.disposeForTests()
    loaded = null
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    document.cookie = 'whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'
  })

  async function loadLayout(withLocks: boolean) {
    Object.defineProperty(navigator, 'locks', {
      configurable: true,
      value: withLocks ? {
        request: async (_name: string, ...args: unknown[]) => (args.at(-1) as () => unknown)(),
        query: async () => ({ held: [], pending: [] }),
      } : undefined,
    })
    vi.resetModules()
    const pinia = await import('pinia')
    pinia.setActivePinia(pinia.createPinia())
    const api = await import('@/services/api')
    loaded = api
    api.api.defaults.adapter = vi.fn(async (config) => ({
      data: { data: {} }, status: 200, statusText: 'OK', headers: {}, config,
    })) as unknown as typeof api.api.defaults.adapter
    const { readSelectedOrganizationId } = await import('@/lib/browserIdentity')
    const { useOrganizationsStore } = await import('@/stores/organizations')
    const { useAuthStore } = await import('@/stores/auth')
    const auth = useAuthStore()
    expect(auth.restoreSession()).toBe(true)
    expect(auth.user).toEqual(U1)
    expect(auth.isAuthenticated).toBe(true)
    const organizations = useOrganizationsStore()
    organizations.init()
    expect(organizations.selectedOrgId).toBe('org-1')
    expect(api.readTabSession()).toBe('s1')
    const { default: AppLayout } = await import('./AppLayout.vue')
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
    await flushPromises()
    return { api, auth, organizations, readSelectedOrganizationId }
  }

  it.each([true, false])("a replaced tab's menu sign-out leaves B stored with Web Locks=%s", async (withLocks) => {
    const { auth, organizations, readSelectedOrganizationId } = await loadLayout(withLocks)

    // U2 signs in in another tab: the shared state is the newer session's.
    localStorage.setItem('whm_session_id', 's2')
    localStorage.setItem('user', JSON.stringify(U2))
    localStorage.setItem('selected_organization_id', 'org-2')

    await wrapper!.get('[data-testid="logout"]').trigger('click')
    await flushPromises()

    expect(posts).toEqual([])
    expect(fetchRevoke).not.toHaveBeenCalled()
    expect(auth.user).toBeNull()
    expect(auth.isAuthenticated).toBe(false)
    expect(localStorage.getItem('whm_session_id')).toBe('s2')
    expect(JSON.parse(localStorage.getItem('user')!)).toEqual(U2)
    expect(localStorage.getItem('selected_organization_id')).toBe('org-2')
    // This tab dropped its own copy, and sends no organization header.
    expect(organizations.selectedOrgId).toBeNull()
    expect(readSelectedOrganizationId()).toBeNull()
    expect(mocks.routerPush).toHaveBeenCalledWith('/login')

    // Back (SPA) into U2's session: this tab adopts it, and with it U2's
    // selected organization (it does not rewrite it).
    expect(auth.restoreSession()).toBe(true)
    expect(auth.user).toEqual(U2)
    expect(readSelectedOrganizationId()).toBe('org-2')
    organizations.init()
    expect(organizations.selectedOrgId).toBe('org-2')
    expect(localStorage.getItem('selected_organization_id')).toBe('org-2')
  })

  it.each([true, false])('a current-session menu sign-out actually uses its transport with Web Locks=%s', async (withLocks) => {
    const { auth, organizations } = await loadLayout(withLocks)
    await wrapper!.get('[data-testid="logout"]').trigger('click')
    await flushPromises()

    expect(posts).toEqual(withLocks ? ['revoke', '/auth/logout'] : ['fetch-revoke'])
    if (withLocks) {
      expect(fetchRevoke).not.toHaveBeenCalled()
      expect(localStorage.getItem('whm_logout_pending')).toBeNull()
    } else {
      expect(fetchRevoke).toHaveBeenCalledTimes(1)
      const [url, request] = fetchRevoke.mock.calls[0] as unknown as [string, RequestInit]
      expect(url).toBe('/api/auth/refresh')
      expect(request).toMatchObject({ method: 'POST', keepalive: true, credentials: 'include' })
      expect(JSON.parse(String(request.body))).toEqual({ revoke: true, keep_cookies: true })
      expect(new Headers(request.headers).get('X-CSRF-Token')).toBe('csrf-1')
      expect(localStorage.getItem('whm_logout_pending')).not.toBeNull()
    }
    expect(auth.user).toBeNull()
    expect(organizations.selectedOrgId).toBeNull()
    expect(localStorage.getItem('user')).toBeNull()
    expect(localStorage.getItem('selected_organization_id')).toBeNull()
    expect(localStorage.getItem('whm_session_id')).not.toBe('s1')
    expect(mocks.routerPush).toHaveBeenCalledWith('/login')
  })
})
