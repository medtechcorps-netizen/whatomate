/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  exchange: vi.fn(),
  get: vi.fn(),
  setAuth: vi.fn(),
  push: vi.fn(),
  replace: vi.fn(),
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
  useAuthStore: () => ({ setAuth: mocks.setAuth }),
}))

vi.mock('@/services/api', () => ({
  api: { get: mocks.get },
  exchangeSsoCode: mocks.exchange,
  SESSION_BUSY_CODE: 'ERR_SESSION_BUSY',
  SESSION_UNSUPPORTED_CODE: 'ERR_SESSION_UNSUPPORTED',
}))

import SSOCallbackView from './SSOCallbackView.vue'

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
    mocks.replace.mockReset()
    mocks.query = {}
  })

  it('exchanges the one-time code (taking it off the URL), then loads the user', async () => {
    mocks.query = { code: 'code-b' }
    mocks.exchange.mockResolvedValueOnce({ data: { data: { expires_in: 900 } } })
    mocks.get.mockResolvedValueOnce({ data: { data: { id: 'user-b', role: { name: 'admin' } } } })

    mountCallback()
    await flushPromises()

    expect(mocks.replace).toHaveBeenCalledWith({ query: { code: undefined } })
    expect(mocks.exchange).toHaveBeenCalledWith('code-b')
    expect(mocks.exchange.mock.invocationCallOrder[0]).toBeLessThan(mocks.get.mock.invocationCallOrder[0])
    expect(mocks.get).toHaveBeenCalledWith('/me')
    expect(mocks.setAuth).toHaveBeenCalledWith({ user: { id: 'user-b', role: { name: 'admin' } } })
  })

  it('without a code it exchanges nothing and asks for nothing', async () => {
    const wrapper = mountCallback()
    await flushPromises()

    expect(mocks.exchange).not.toHaveBeenCalled()
    expect(mocks.get).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('auth.ssoLoginFailed')
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
