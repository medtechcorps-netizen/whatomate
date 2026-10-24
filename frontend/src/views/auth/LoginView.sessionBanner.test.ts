/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key,
  }),
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  useRoute: () => ({ query: {} }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ login: vi.fn() }),
}))

vi.mock('@/services/api', () => ({
  api: { get: vi.fn() },
  sessionsSupported: () => true,
  whenSessionsSupportKnown: () => Promise.resolve(true),
}))

vi.mock('@/components/brand/ReReplyLogo.vue', () => ({
  default: { template: '<div />' },
}))

// The template's decorative <img src="/brand/..."> compiles to an asset import.
vi.mock('/brand/rereply-mark.png', () => ({ default: '/brand/rereply-mark.png' }))

import LoginView from './LoginView.vue'
import { SIGN_OUT_REASON_KEY } from '@/lib/sessionSignout'

function mountLogin() {
  return mount(LoginView, {
    global: {
      mocks: { $t: (key: string) => key },
    },
  })
}

describe('LoginView sign-out banner', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
  })

  it('shows nothing after a normal sign-out', async () => {
    wrapper = mountLogin()
    await flushPromises()
    expect(wrapper.find('[data-testid="login-session-banner"]').exists()).toBe(false)
  })

  it('explains an inactivity sign-out', async () => {
    localStorage.setItem(SIGN_OUT_REASON_KEY, JSON.stringify({ reason: 'idle', minutes: 240, at: Date.now() }))
    wrapper = mountLogin()
    await flushPromises()

    const banner = wrapper.get('[data-testid="login-session-banner"]')
    expect(banner.attributes('role')).toBe('status')
    expect(banner.text()).toBe(
      'sessionSecurity.signedOutIdle:{"duration":"sessionSecurity.durationHours:{\\"count\\":4}"}',
    )
  })

  it('explains an expired session', async () => {
    localStorage.setItem(SIGN_OUT_REASON_KEY, JSON.stringify({ reason: 'expired', at: Date.now() }))
    wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.get('[data-testid="login-session-banner"]').text()).toBe('sessionSecurity.sessionExpired')
  })

  it('keeps the sign-in hooks the canary relies on', async () => {
    localStorage.setItem(SIGN_OUT_REASON_KEY, JSON.stringify({ reason: 'expired', at: Date.now() }))
    wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.find('#email').exists()).toBe(true)
    expect(wrapper.find('#password').exists()).toBe(true)
    const submit = wrapper.get('[data-testid="password-sign-in"]')
    expect(submit.attributes('type')).toBe('submit')
    // The banner adds no buttons to the sign-in form.
    expect(wrapper.get('[data-testid="login-session-banner"]').findAll('button')).toHaveLength(0)
  })
})
