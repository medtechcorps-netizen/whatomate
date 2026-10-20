/** @vitest-environment happy-dom */

// Received responses through the real interceptor, callback and Pinia store.
// This fixture starts with a settled session, not a pending logout.
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import axios, { AxiosError, type InternalAxiosRequestConfig } from 'axios'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'

vi.mock('@/stores/calling', () => ({ useCallingStore: () => ({ isOnCall: false }) }))
vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), { loading: vi.fn(), error: vi.fn(), dismiss: vi.fn(), success: vi.fn() }),
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

const USER_A = { id: 'user-a', email: 'a@example.com', organization_id: 'org-a', role: { name: 'admin' } }
const USER_B = { id: 'user-b', email: 'b@example.com', organization_id: 'org-b', role: { name: 'admin' } }
const ATTEMPT_KEY = 'rereply:legacy-whatsapp-reply-attempt:v1:org-a:contact-a'
const originalLocation = window.location
let assignedHref: string | null = null
let wrapper: VueWrapper | undefined
let apiModule: typeof import('@/services/api') | undefined
const sessionEvents: string[] = []
const onSessionEvent = (event: Event) => sessionEvents.push((event as CustomEvent<{ state: string }>).detail.state)

function storageSnapshot(storage: Storage) {
  return Object.fromEntries(Array.from({ length: storage.length }, (_, index) => storage.key(index)!)
    .sort().map((key) => [key, storage.getItem(key)]))
}

describe('SSOCallbackView received exchange response with a settled identity', () => {
  beforeAll(async () => {
    await import('./SSOCallbackView.vue')
    await import('@/stores/auth')
    ;(await import('@/services/api')).disposeForTests()
  }, 60_000)

  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    document.cookie = 'whm_csrf=csrf-a; path=/'
    vi.stubGlobal('BroadcastChannel', undefined)
    Object.defineProperty(navigator, 'locks', {
      configurable: true,
      value: {
        request: async (_name: string, ...args: unknown[]) => (args.at(-1) as () => unknown)(),
        query: async () => ({ held: [], pending: [] }),
      },
    })
    assignedHref = null
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: {
        pathname: '/auth/sso/callback',
        get href() { return assignedHref ?? 'http://localhost:3000/auth/sso/callback' },
        set href(value: string) { assignedHref = value },
      },
    })
    sessionEvents.length = 0
    window.addEventListener('whm:auth-session', onSessionEvent)
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = undefined
    apiModule?.disposeForTests()
    apiModule = undefined
    window.removeEventListener('whm:auth-session', onSessionEvent)
    vi.clearAllTimers()
    vi.useRealTimers()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    document.cookie = 'whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'
  })

  it.each([503, 200])('only success replaces the real settled identity and caches (received %i)', async (status) => {
    vi.resetModules()
    const { createPinia, setActivePinia } = await import('pinia')
    const pinia = createPinia()
    setActivePinia(pinia)
    apiModule = await import('@/services/api')
    const { useAuthStore } = await import('@/stores/auth')
    const { default: SSOCallbackView } = await import('./SSOCallbackView.vue')
    const store = useAuthStore()
    localStorage.setItem('user', JSON.stringify(USER_A))
    localStorage.setItem('selected_organization_id', 'org-a')
    localStorage.setItem('whm_session_id', 'session-a')
    localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-a', csrf: 'csrf-a' }))
    localStorage.setItem('whm_auth_expires_at', String(Date.now() + 3_600_000))
    sessionStorage.setItem(ATTEMPT_KEY, JSON.stringify({ key: 'synthetic-existing-attempt' }))
    expect(store.restoreSession()).toBe(true)
    store.productEntitlements = { 'test-feature': true }
    store.productEntitlementMode = 'licensed'
    store.productEntitlementsLoaded = true
    const settledState = JSON.stringify(store.$state)
    const settledLocal = storageSnapshot(localStorage)
    const settledSession = storageSnapshot(sessionStorage)
    const csrfBefore = document.cookie
    const setAuth = vi.spyOn(store, 'setAuth')
    const clearAuth = vi.spyOn(store, 'clearAuth')
    const restoreSession = vi.spyOn(store, 'restoreSession')
    const rawPost = vi.spyOn(axios, 'post').mockRejectedValue(new Error('Unexpected standalone auth request'))
    const fetch = vi.fn().mockRejectedValue(new Error('Unexpected fetch'))
    vi.stubGlobal('fetch', fetch)
    const exchange = vi.fn(async (config: InternalAxiosRequestConfig) => {
      if (status === 503) {
        throw new AxiosError('Temporary exchange failure', AxiosError.ERR_BAD_RESPONSE, config, undefined, {
          data: { status: 'error', message: 'Start a fresh sign-in' },
          status, statusText: 'Service Unavailable', headers: {}, config,
        })
      }
      document.cookie = 'whm_csrf=csrf-b; path=/'
      return { data: { data: { expires_in: 900, user: USER_B } }, status, statusText: 'OK', headers: {}, config }
    })
    apiModule.api.defaults.adapter = exchange
    const Blank = defineComponent({ render: () => h('div') })
    const router = createRouter({ history: createMemoryHistory(), routes: [
      { path: '/auth/sso/callback', component: SSOCallbackView },
      { path: '/', component: Blank },
      { path: '/login', component: Blank },
    ] })
    await router.push('/auth/sso/callback?code=code-b')
    await router.isReady()
    vi.useFakeTimers()
    wrapper = mount(defineComponent({ render: () => h(RouterView) }), { global: {
      plugins: [pinia, router], mocks: { $t: (key: string) => key },
      stubs: { RouterLink: { template: '<a><slot /></a>' } },
    } })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(10_000)
    await flushPromises()

    expect(exchange).toHaveBeenCalledTimes(1)
    const [request] = exchange.mock.calls[0]!
    expect(request.url).toBe('/auth/refresh')
    expect(request.method).toBe('post')
    expect(JSON.parse(request.data)).toEqual({ sso_exchange: 'code-b' })
    expect(rawPost).not.toHaveBeenCalled()
    expect(fetch).not.toHaveBeenCalled()
    expect(clearAuth).not.toHaveBeenCalled()
    expect(restoreSession).not.toHaveBeenCalled()
    expect(sessionEvents).not.toContain('expired')
    expect(assignedHref).toBeNull()
    expect(apiModule.unsentSsoCode()).toBeNull()
    expect(sessionStorage.getItem('whm_sso_sent')).toBeNull()
    if (status === 503) {
      expect(wrapper.text()).toContain('Start a fresh sign-in')
      expect(router.currentRoute.value.path).toBe('/auth/sso/callback')
      expect(router.currentRoute.value.query.code).toBeUndefined()
      expect(setAuth).not.toHaveBeenCalled()
      expect(JSON.stringify(store.$state)).toBe(settledState)
      expect(storageSnapshot(localStorage)).toEqual(settledLocal)
      expect(storageSnapshot(sessionStorage)).toEqual(settledSession)
      expect(document.cookie).toBe(csrfBefore)
    } else {
      // Positive control: this same real component/store does apply success.
      expect(setAuth).toHaveBeenCalledExactlyOnceWith({ user: USER_B })
      expect(store.user).toEqual(USER_B)
      expect(store.productEntitlements).toEqual({})
      expect(store.productEntitlementsLoaded).toBe(false)
      expect(localStorage.getItem('selected_organization_id')).toBeNull()
      expect(sessionStorage.getItem(ATTEMPT_KEY)).toBeNull()
      expect(document.cookie).toContain('whm_csrf=csrf-b')
      expect(router.currentRoute.value.path).toBe('/')
    }
  })
})
