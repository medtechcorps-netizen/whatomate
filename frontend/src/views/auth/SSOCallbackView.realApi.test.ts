/** @vitest-environment happy-dom */

// The SSO callback page with the real API client, auth store and router: the
// code leaves the URL before its exchange is sent, the user comes from the
// exchange's own answer, and a session the exchange began that another tab
// replaces is never entered (the tab goes to the login page instead).

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import axios, { type InternalAxiosRequestConfig } from 'axios'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

vi.mock('@/stores/calling', () => ({ useCallingStore: () => ({ isOnCall: false }) }))
vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), { loading: vi.fn(), error: vi.fn(), dismiss: vi.fn(), success: vi.fn() }),
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

const USER_B = { id: 'user-b', email: 'b@example.com', organization_id: 'org-b', role: { name: 'admin' } }

// Every target browser has Web Locks: a FIFO stand-in.
function installWebLocks() {
  const tails = new Map<string, Promise<void>>()
  const request = async (name: string, ...rest: unknown[]) => {
    const callback = rest[rest.length - 1] as () => Promise<unknown>
    const previous = tails.get(name) ?? Promise.resolve()
    let release!: () => void
    const held = new Promise<void>((resolve) => {
      release = resolve
    })
    tails.set(name, previous.then(() => held))
    await previous
    try {
      return await callback()
    } finally {
      release()
    }
  }
  Object.defineProperty(navigator, 'locks', {
    configurable: true,
    value: { request, query: async () => ({ held: [], pending: [] }) },
  })
}

// Another tab signs in: a new session marker, announced by a storage event.
function signInElsewhere(session: string) {
  localStorage.setItem('whm_session_id', session)
  window.dispatchEvent(new StorageEvent('storage', { key: 'whm_session_id', newValue: session }))
}

const originalLocation = window.location
let assignedHref: string | null = null
const assign = vi.fn()
const wrappers: VueWrapper[] = []
const apis: Array<{ disposeForTests: () => void }> = []

describe('SSOCallbackView with the real API client, store and router', () => {
  beforeAll(async () => {
    await import('./SSOCallbackView.vue')
    await import('@/stores/auth')
    ;(await import('@/services/api')).disposeForTests()
  }, 60_000)

  beforeEach(() => {
    vi.useFakeTimers({ now: new Date('2026-10-19T09:00:00Z') })
    localStorage.clear()
    sessionStorage.clear()
    document.cookie = 'whm_csrf=csrf-before; path=/'
    installWebLocks()
    vi.stubGlobal('BroadcastChannel', undefined)
    vi.spyOn(axios, 'post').mockResolvedValue({ data: { data: {} }, status: 200 })
    assignedHref = null
    assign.mockReset()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: {
        pathname: '/auth/sso/callback',
        get href() {
          return assignedHref ?? 'http://localhost:3000/auth/sso/callback'
        },
        set href(value: string) {
          assignedHref = value
        },
        assign,
      },
    })
  })

  afterEach(() => {
    while (wrappers.length) wrappers.pop()!.unmount()
    while (apis.length) apis.pop()!.disposeForTests()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    vi.useRealTimers()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    document.cookie = 'whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'
  })

  // Opens the callback page at `url` with fresh modules; `answer` serves the
  // page's own requests (the exchange, /me).
  async function openCallback(
    url: string,
    answer: (config: InternalAxiosRequestConfig, router: Router) => Promise<unknown> | unknown,
  ) {
    vi.resetModules()
    const pinia = await import('pinia')
    const piniaInstance = pinia.createPinia()
    pinia.setActivePinia(piniaInstance)
    const api = await import('@/services/api')
    apis.push(api)
    const { default: SSOCallbackView } = await import('./SSOCallbackView.vue')
    const { useAuthStore } = await import('@/stores/auth')
    const Blank = defineComponent({ render: () => h('div') })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/auth/sso/callback', component: SSOCallbackView },
        { path: '/', component: Blank },
        { path: '/analytics/agents', component: Blank },
        { path: '/login', component: Blank },
      ],
    })
    api.api.defaults.adapter = (async (config: InternalAxiosRequestConfig) => ({
      data: { data: await answer(config, router) },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    })) as unknown as typeof api.api.defaults.adapter
    await router.push(url)
    await router.isReady()
    const App = defineComponent({ render: () => h(RouterView) })
    wrappers.push(
      mount(App, {
        global: {
          plugins: [piniaInstance, router],
          mocks: { $t: (key: string) => key },
          stubs: { RouterLink: { template: '<a><slot /></a>' } },
        },
      }),
    )
    return { api, router, store: useAuthStore() }
  }

  it('takes the code off the URL before its exchange is sent, then goes on into the app as the new session', async () => {
    let codeInUrlAtExchange: unknown = 'not sent'
    const { api, router, store } = await openCallback('/auth/sso/callback?code=code-b', (config, r) => {
      if (config.url === '/auth/refresh') {
        codeInUrlAtExchange = r.currentRoute.value.query.code
        document.cookie = 'whm_csrf=csrf-of-b; path=/'
        return { expires_in: 900, user: USER_B }
      }
      return USER_B
    })
    await flushPromises()

    expect(codeInUrlAtExchange).toBeUndefined()
    expect(api.readTabSession()).toBe(localStorage.getItem('whm_session_id'))
    expect(store.user?.id).toBe('user-b')

    vi.advanceTimersByTime(1_000)
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/')
    expect(assign).not.toHaveBeenCalled()
    expect(assignedHref).toBeNull()
  })

  it('a session replaced during the success delay is never entered: the tab goes to the login page', async () => {
    const { api, router } = await openCallback('/auth/sso/callback?code=code-b', (config) => {
      if (config.url === '/auth/refresh') {
        document.cookie = 'whm_csrf=csrf-of-b; path=/'
        return { expires_in: 900, user: USER_B }
      }
      return USER_B
    })
    await flushPromises()
    expect(api.readTabSession()).not.toBeNull()

    signInElsewhere('session-c')
    // The page showed the session its exchange began: it leaves it.
    expect(api.readTabSession()).toBeNull()
    expect(assignedHref).toBe('/login')

    vi.advanceTimersByTime(1_000)
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/auth/sso/callback')
    expect(assign).toHaveBeenCalledWith('/login')
  })

  it("applies the user from the exchange's own answer: no /me is sent", async () => {
    const urls: string[] = []
    const { api, store } = await openCallback('/auth/sso/callback?code=code-b', (config) => {
      urls.push(config.url ?? '')
      document.cookie = 'whm_csrf=csrf-of-b; path=/'
      return { expires_in: 900, user: USER_B }
    })
    await flushPromises()

    expect(urls).toEqual(['/auth/refresh'])
    expect(store.user?.id).toBe('user-b')
    expect(api.readTabSession()).toBe(localStorage.getItem('whm_session_id'))
  })
})
