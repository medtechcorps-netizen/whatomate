/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  exchange: vi.fn(),
  get: vi.fn(),
  setAuth: vi.fn(),
  push: vi.fn(),
  replace: vi.fn(),
  keep: vi.fn(),
  interrupted: vi.fn(),
  unsent: vi.fn(),
  restoreSession: vi.fn(),
  store: { isAuthenticated: false, user: null as null | { role?: { name: string } } },
  query: {} as Record<string, string>,
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push, replace: mocks.replace }),
  useRoute: () => ({ query: mocks.query }),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-sonner', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    setAuth: mocks.setAuth,
    restoreSession: mocks.restoreSession,
    get isAuthenticated() {
      return mocks.store.isAuthenticated
    },
    get user() {
      return mocks.store.user
    },
  }),
}))

vi.mock('@/services/api', () => ({
  api: { get: mocks.get },
  exchangeSsoCode: mocks.exchange,
  keepSsoCode: mocks.keep,
  ssoExchangeInterrupted: mocks.interrupted,
  unsentSsoCode: mocks.unsent,
  SESSION_BUSY_CODE: 'ERR_SESSION_BUSY',
  SESSION_UNSUPPORTED_CODE: 'ERR_SESSION_UNSUPPORTED',
}))

import SSOCallbackView from './SSOCallbackView.vue'

// The exchange answers like a sign-in: {expires_in, user}.
const USER_B = { id: 'user-b', role: { name: 'admin' } }
const answer = { data: { data: { expires_in: 900, user: USER_B } } }

function mountCallback() {
  return mount(SSOCallbackView, {
    global: {
      mocks: { $t: (key: string) => key },
      stubs: { RouterLink: { template: '<a><slot /></a>' } },
    },
  })
}

describe('SSOCallbackView', () => {
  beforeEach(() => {
    mocks.exchange.mockReset()
    mocks.get.mockReset()
    mocks.setAuth.mockReset()
    mocks.push.mockReset()
    mocks.replace.mockReset().mockResolvedValue(undefined)
    mocks.keep.mockReset()
    mocks.interrupted.mockReset().mockReturnValue(false)
    mocks.unsent.mockReset().mockReturnValue(null)
    mocks.restoreSession.mockReset().mockReturnValue(false)
    mocks.store = { isAuthenticated: false, user: null }
    mocks.query = {}
  })

  it('exchanges the one-time code (taking it off the URL) and applies the user it answers, with no /me', async () => {
    mocks.query = { code: 'code-b' }
    mocks.exchange.mockResolvedValueOnce(answer)

    mountCallback()
    await flushPromises()

    // Kept (this tab, until sent), then off the URL, then exchanged, then /me.
    expect(mocks.keep).toHaveBeenCalledWith('code-b')
    expect(mocks.replace).toHaveBeenCalledWith({ query: { code: undefined } })
    expect(mocks.exchange).toHaveBeenCalledWith('code-b')
    expect(mocks.keep.mock.invocationCallOrder[0]).toBeLessThan(mocks.replace.mock.invocationCallOrder[0])
    expect(mocks.replace.mock.invocationCallOrder[0]).toBeLessThan(mocks.exchange.mock.invocationCallOrder[0])
    expect(mocks.get).not.toHaveBeenCalled()
    expect(mocks.setAuth).toHaveBeenCalledWith({ user: { id: 'user-b', role: { name: 'admin' } } })
  })

  it('sends the exchange only once the code is off the URL', async () => {
    mocks.query = { code: 'code-b' }
    let replaced!: () => void
    mocks.replace.mockReturnValueOnce(new Promise<void>((resolve) => (replaced = resolve)))
    mocks.exchange.mockResolvedValueOnce(answer)

    mountCallback()
    await flushPromises()
    expect(mocks.exchange).not.toHaveBeenCalled()

    replaced()
    await flushPromises()
    expect(mocks.exchange).toHaveBeenCalledWith('code-b')
  })

  it('without a code (and none kept, no session) says the link is spent, and sends nothing', async () => {
    const wrapper = mountCallback()
    await flushPromises()

    expect(mocks.exchange).not.toHaveBeenCalled()
    expect(mocks.get).not.toHaveBeenCalled()
    expect(mocks.replace).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('auth.ssoLinkUsed')
  })

  it('without a code, an exchange a reload interrupted says so (and enters no session)', async () => {
    mocks.interrupted.mockReturnValue(true)
    // Even with a session stored from before (the one the SSO was replacing).
    mocks.store = { isAuthenticated: false, user: { role: { name: 'admin' } } }
    mocks.restoreSession.mockReturnValue(true)
    const wrapper = mountCallback()
    await flushPromises()

    expect(wrapper.text()).toContain('auth.ssoInterrupted')
    expect(mocks.replace).not.toHaveBeenCalled()
    expect(mocks.restoreSession).not.toHaveBeenCalled()
    expect(mocks.exchange).not.toHaveBeenCalled()
  })

  it('a reload while the exchange waited retries the kept, unsent code', async () => {
    mocks.unsent.mockReturnValue('code-b')
    mocks.exchange.mockResolvedValueOnce(answer)

    mountCallback()
    await flushPromises()

    expect(mocks.exchange).toHaveBeenCalledWith('code-b')
    expect(mocks.keep).not.toHaveBeenCalled()
    expect(mocks.setAuth).toHaveBeenCalledWith({ user: { id: 'user-b', role: { name: 'admin' } } })
  })

  it.each([
    ['in this page (Back after the sign-in)', true, false],
    ['stored (a reload after the sign-in)', false, true],
  ])('without a code, a live session %s goes on into the app', async (_label, inStore, restored) => {
    mocks.store = { isAuthenticated: inStore, user: { role: { name: 'agent' } } }
    mocks.restoreSession.mockReturnValue(restored)
    const wrapper = mountCallback()
    await flushPromises()

    expect(mocks.exchange).not.toHaveBeenCalled()
    expect(mocks.get).not.toHaveBeenCalled()
    expect(mocks.replace).toHaveBeenCalledWith('/analytics/agents')
    expect(wrapper.text()).not.toContain('auth.ssoFailed')
  })

  it.each([
    [{ code: 'ERR_SESSION_UNSUPPORTED' }, 'errors.unsupportedBrowser'],
    [{ code: 'ERR_SESSION_BUSY' }, 'errors.timeout'],
    [{ response: { data: { message: 'Invalid or expired sign-in, please try again' } } }, 'Invalid or expired sign-in, please try again'],
  ])('a refused exchange (%o) shows %s and loads no user', async (error, message) => {
    mocks.query = { code: 'code-b' }
    mocks.exchange.mockRejectedValueOnce(Object.assign(new Error('refused'), error))

    const wrapper = mountCallback()
    await flushPromises()

    expect(mocks.get).not.toHaveBeenCalled()
    expect(mocks.setAuth).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain(message)
  })
})
