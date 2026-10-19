/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  login: vi.fn(),
  beginSsoSignIn: vi.fn(),
  get: vi.fn(),
  supported: true,
  // What the load-time probe of the lock manager finds (default: supported).
  known: undefined as boolean | undefined,
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  push: vi.fn(),
  query: {} as Record<string, string>,
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push, replace: vi.fn() }),
  useRoute: () => ({ query: mocks.query }),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-sonner', () => ({
  toast: { error: mocks.toastError, success: mocks.toastSuccess },
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ login: mocks.login }),
}))

vi.mock('@/services/api', () => ({
  api: { get: mocks.get },
  beginSsoSignIn: mocks.beginSsoSignIn,
  sessionsSupported: () => mocks.supported,
  whenSessionsSupportKnown: () => Promise.resolve(mocks.known ?? mocks.supported),
  SESSION_BUSY_CODE: 'ERR_SESSION_BUSY',
  SESSION_UNSUPPORTED_CODE: 'ERR_SESSION_UNSUPPORTED',
}))

vi.mock('@/components/brand/ReReplyLogo.vue', () => ({
  default: { template: '<div />' },
}))

// The page's decorative image (a public asset the template imports).
vi.mock('/brand/rereply-mark.png', () => ({ default: '' }))

import LoginView from './LoginView.vue'

const originalLocation = window.location
let assignedHref: string | null = null

function mountLogin() {
  return mount(LoginView, {
    global: {
      mocks: { $t: (key: string) => key },
      stubs: { RouterLink: { template: '<a><slot /></a>' } },
    },
  })
}

describe('LoginView without Web Locks', () => {
  beforeEach(() => {
    mocks.supported = false
    mocks.known = undefined
    mocks.login.mockReset()
    mocks.beginSsoSignIn.mockReset()
    mocks.get.mockReset().mockResolvedValue({ data: { data: [{ provider: 'google', name: 'Google' }] } })
    mocks.toastError.mockReset()
    mocks.push.mockReset()
    mocks.query = {}
    assignedHref = null
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: {
        pathname: '/login',
        get href() {
          return assignedHref ?? 'http://localhost:3000/login'
        },
        set href(value: string) {
          assignedHref = value
        },
      },
    })
  })

  afterEach(() => {
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
  })

  const notice = '[data-testid="login-unsupported-notice"]'

  it('says why on arrival (a session from before ends here at its first refresh need)', async () => {
    const wrapper = mountLogin()

    expect(wrapper.find(notice).exists()).toBe(true)
    expect(wrapper.get(notice).text()).toBe('errors.unsupportedBrowser')
    expect(wrapper.get(notice).attributes('role')).toBe('alert')
  })

  it('says why once the load probe finds a lock manager that refuses', async () => {
    mocks.supported = true
    mocks.known = false
    const wrapper = mountLogin()
    expect(wrapper.find(notice).exists()).toBe(false)

    await flushPromises()

    expect(wrapper.find(notice).exists()).toBe(true)
  })

  it('shows no notice where Web Locks work', async () => {
    mocks.supported = true
    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.find(notice).exists()).toBe(false)
  })

  it('says why and starts no password sign-in (so nothing stored is cleared)', async () => {
    const wrapper = mountLogin()
    await flushPromises()
    await wrapper.get('#email').setValue('b@example.com')
    await wrapper.get('#password').setValue('a-password')

    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(mocks.login).not.toHaveBeenCalled()
    expect(mocks.toastError).toHaveBeenCalledWith('errors.unsupportedBrowser')
    expect(mocks.push).not.toHaveBeenCalled()
  })

  it('fills in the email handed over (after a registration that needs a sign-in)', async () => {
    mocks.query = { email: 'b@example.com' }
    const wrapper = mountLogin()
    await flushPromises()

    expect((wrapper.get('#email').element as HTMLInputElement).value).toBe('b@example.com')
  })

  it('says why and starts no SSO sign-in', async () => {
    const wrapper = mountLogin()
    await flushPromises()
    await wrapper.get('[data-testid="sso-workspace-code"]').setValue('klinik-relive-a1b2c3d4')
    await wrapper.get('[data-testid="sso-workspace-discovery"]').trigger('submit')
    await flushPromises()

    await wrapper.get('[data-testid="sso-provider-google"]').trigger('click')
    await flushPromises()

    expect(mocks.beginSsoSignIn).not.toHaveBeenCalled()
    expect(mocks.toastError).toHaveBeenCalledWith('errors.unsupportedBrowser')
    expect(assignedHref).toBeNull()
  })

  it.each([
    ['ERR_SESSION_UNSUPPORTED', 'errors.unsupportedBrowser'],
    ['ERR_SESSION_BUSY', 'errors.timeout'],
  ])('maps a sign-in refused with %s (say, by a lock manager that refused) to %s', async (code, message) => {
    mocks.supported = true
    mocks.login.mockRejectedValueOnce(Object.assign(new Error('refused'), { code }))
    const wrapper = mountLogin()
    await flushPromises()
    await wrapper.get('#email').setValue('b@example.com')
    await wrapper.get('#password').setValue('a-password')

    await wrapper.get('form').trigger('submit')
    await flushPromises()

    expect(mocks.login).toHaveBeenCalledTimes(1)
    expect(mocks.toastError).toHaveBeenCalledWith(message)
    expect(mocks.push).not.toHaveBeenCalled()
    // Only an unsupported refusal leaves the standing notice.
    expect(wrapper.find(notice).exists()).toBe(code === 'ERR_SESSION_UNSUPPORTED')
  })

  it('maps an SSO start refused as unsupported to that message', async () => {
    mocks.supported = true
    mocks.beginSsoSignIn.mockRejectedValueOnce(Object.assign(new Error('refused'), { code: 'ERR_SESSION_UNSUPPORTED' }))
    const wrapper = mountLogin()
    await flushPromises()
    await wrapper.get('[data-testid="sso-workspace-code"]').setValue('klinik-relive-a1b2c3d4')
    await wrapper.get('[data-testid="sso-workspace-discovery"]').trigger('submit')
    await flushPromises()

    await wrapper.get('[data-testid="sso-provider-google"]').trigger('click')
    await flushPromises()

    expect(mocks.beginSsoSignIn).toHaveBeenCalledTimes(1)
    expect(mocks.toastError).toHaveBeenCalledWith('errors.unsupportedBrowser')
    expect(assignedHref).toBeNull()
    expect(wrapper.find(notice).exists()).toBe(true)
  })
})
