/** @vitest-environment happy-dom */

// The SSO callback page after a reload cut its exchange off, with the real API
// client and auth store: the identity stored from before (A) is never resumed
// while the jar's session is unknown (the exchange's cookies, B's, may have
// landed unseen), on any number of reloads; the spent code is never sent
// again, and nothing is revoked or cleared to get there.

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

const USER_A = { id: 'user-a', email: 'a@example.com', organization_id: 'org-a', role: { name: 'admin' } }

// Every target browser has Web Locks: a FIFO stand-in. A page that goes away
// takes its locks with it: a reload installs a fresh one.
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

const originalLocation = window.location
let assignedHref: string | null = null
const wrappers: VueWrapper[] = []
const apis: Array<{ disposeForTests: () => void }> = []

describe('SSOCallbackView after a reload cut its exchange off', () => {
  beforeAll(async () => {
    await import('./SSOCallbackView.vue')
    await import('@/stores/auth')
    ;(await import('@/services/api')).disposeForTests()
  }, 60_000)

  let revokesAndLogouts: string[]

  beforeEach(() => {
    localStorage.clear()
    sessionStorage.clear()
    // Signed in as A (the session from before the SSO sign-in).
    localStorage.setItem('user', JSON.stringify(USER_A))
    localStorage.setItem('whm_session_id', 'session-a')
    localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-a', csrf: 'csrf-a' }))
    document.cookie = 'whm_csrf=csrf-a; path=/'
    installWebLocks()
    vi.stubGlobal('BroadcastChannel', undefined)
    revokesAndLogouts = []
    vi.spyOn(axios, 'post').mockImplementation(async (url: string) => {
      revokesAndLogouts.push(url)
      return { data: { data: {} }, status: 200 }
    })
    assignedHref = null
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
        assign: vi.fn(),
      },
    })
  })

  afterEach(() => {
    while (wrappers.length) wrappers.pop()!.unmount()
    while (apis.length) apis.pop()!.disposeForTests()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined })
    document.cookie = 'whm_csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'
  })

  // Loads the callback page at `url` as a page load does (fresh modules);
  // `answer` serves the page's own API requests.
  async function loadCallbackPage(url: string, answer: (config: InternalAxiosRequestConfig) => Promise<unknown>) {
    vi.resetModules()
    const pinia = await import('pinia')
    const piniaInstance = pinia.createPinia()
    pinia.setActivePinia(piniaInstance)
    const api = await import('@/services/api')
    apis.push(api)
    const { default: SSOCallbackView } = await import('./SSOCallbackView.vue')
    const { useAuthStore } = await import('@/stores/auth')
    const Blank = defineComponent({ render: () => h('div') })
    const router: Router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/auth/sso/callback', component: SSOCallbackView },
        { path: '/', component: Blank },
        { path: '/analytics/agents', component: Blank },
        { path: '/login', component: Blank },
      ],
    })
    api.api.defaults.adapter = (async (config: InternalAxiosRequestConfig) => ({
      data: { data: await answer(config) },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    })) as unknown as typeof api.api.defaults.adapter
    await router.push(url)
    await router.isReady()
    const App = defineComponent({ render: () => h(RouterView) })
    const wrapper = mount(App, {
      global: {
        plugins: [piniaInstance, router],
        mocks: { $t: (key: string) => key },
        stubs: { RouterLink: { template: '<a><slot /></a>' } },
      },
    })
    wrappers.push(wrapper)
    await flushPromises()
    return { api, router, store: useAuthStore(), wrapper }
  }

  it('neither of two reloads resumes the session from before, and the spent code is never sent again', async () => {
    const sent: string[] = []
    // The page sends its exchange; its answer never comes here (the page
    // reloads while it is out).
    await loadCallbackPage('/auth/sso/callback?code=code-b', (config) => {
      sent.push(config.url ?? '')
      return new Promise(() => undefined)
    })
    expect(sent).toEqual(['/auth/refresh'])
    expect(sessionStorage.getItem('whm_sso_code')).toBeNull()
    installWebLocks()

    for (const landed of [false, true]) {
      // The second time, the unseen answer's cookies (B's) are in the jar.
      if (landed) document.cookie = 'whm_csrf=csrf-of-b; path=/'
      const { api, router, store, wrapper } = await loadCallbackPage('/auth/sso/callback', async (config) => {
        sent.push(config.url ?? '')
        return {}
      })

      expect(wrapper.text()).toContain('auth.ssoInterrupted')
      expect(store.user).toBeNull()
      expect(router.currentRoute.value.path).toBe('/auth/sso/callback')
      expect(api.ssoExchangeInterrupted()).toBe(true)
    }

    // The code was sent once; nothing was revoked or cleared to get here.
    expect(sent).toEqual(['/auth/refresh'])
    expect(revokesAndLogouts).toEqual([])
    expect(localStorage.getItem('whm_session_id')).toBe('session-a')
    expect(JSON.parse(localStorage.getItem('user')!).id).toBe('user-a')
  })

  it('a newer session that began elsewhere, consistent with the jar, settles it: the page goes on into that session', async () => {
    await loadCallbackPage('/auth/sso/callback?code=code-b', () => new Promise(() => undefined))
    installWebLocks()
    // Another tab signs in as C: its marker, its cookies, its stored identity.
    localStorage.setItem('whm_session_id', 'session-c')
    localStorage.setItem('whm_session_begun', JSON.stringify({ marker: 'session-c', csrf: 'csrf-of-c' }))
    localStorage.setItem('user', JSON.stringify({ ...USER_A, id: 'user-c', email: 'c@example.com' }))
    document.cookie = 'whm_csrf=csrf-of-c; path=/'

    const { router, store } = await loadCallbackPage('/auth/sso/callback', async () => ({}))

    expect(store.user?.id).toBe('user-c')
    expect(router.currentRoute.value.path).toBe('/')
    expect(sessionStorage.getItem('whm_sso_sent')).toBeNull()
  })
})
