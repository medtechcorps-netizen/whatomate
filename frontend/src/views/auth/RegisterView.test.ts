/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  register: vi.fn(),
  supported: true,
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  toastInfo: vi.fn(),
  push: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: mocks.push }),
  useRoute: () => ({ query: { invite: 'invite-token' } }),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-sonner', () => ({
  toast: { error: mocks.toastError, success: mocks.toastSuccess, info: mocks.toastInfo },
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ register: mocks.register }),
}))

vi.mock('@/services/api', () => ({
  sessionsSupported: () => mocks.supported,
  sentBeforeSessionChange: (error: { sentBeforeSessionChange?: boolean }) => error?.sentBeforeSessionChange === true,
  SESSION_BUSY_CODE: 'ERR_SESSION_BUSY',
  SESSION_UNSUPPORTED_CODE: 'ERR_SESSION_UNSUPPORTED',
}))

vi.mock('@/components/brand/ReReplyLogo.vue', () => ({
  default: { template: '<div />' },
}))

import RegisterView from './RegisterView.vue'

async function submitRegistration() {
  const wrapper = mount(RegisterView, {
    global: {
      mocks: { $t: (key: string) => key },
      stubs: { RouterLink: { template: '<a><slot /></a>' } },
    },
  })
  await wrapper.get('#fullName').setValue('B User')
  await wrapper.get('#email').setValue('b@example.com')
  await wrapper.get('#password').setValue('a-long-password-1')
  await wrapper.get('#confirmPassword').setValue('a-long-password-1')
  await wrapper.get('form').trigger('submit')
  await flushPromises()
  return wrapper
}

describe('RegisterView refusals', () => {
  beforeEach(() => {
    mocks.supported = true
    mocks.register.mockReset()
    mocks.toastError.mockReset()
    mocks.toastSuccess.mockReset()
    mocks.toastInfo.mockReset()
    mocks.push.mockReset()
  })

  it('a registration the server got, undone by a session change elsewhere, sends the user to sign in', async () => {
    mocks.register.mockRejectedValueOnce(
      Object.assign(new Error('The session changed'), { code: 'ERR_CANCELED', sentBeforeSessionChange: true }),
    )

    await submitRegistration()

    expect(mocks.toastInfo).toHaveBeenCalledWith('auth.accountMayExist')
    expect(mocks.toastError).not.toHaveBeenCalled()
    expect(mocks.push).toHaveBeenCalledWith({ path: '/login', query: { email: 'b@example.com' } })
  })

  it('without Web Locks says why and starts no registration (nothing is cleared)', async () => {
    mocks.supported = false

    await submitRegistration()

    expect(mocks.register).not.toHaveBeenCalled()
    expect(mocks.toastError).toHaveBeenCalledWith('errors.unsupportedBrowser')
    expect(mocks.push).not.toHaveBeenCalled()
  })

  it.each([
    ['ERR_SESSION_UNSUPPORTED', 'errors.unsupportedBrowser'],
    ['ERR_SESSION_BUSY', 'errors.timeout'],
    ['ECONNABORTED', 'errors.timeout'],
  ])('maps a registration refused with %s to %s', async (code, message) => {
    mocks.register.mockRejectedValueOnce(Object.assign(new Error('refused'), { code }))

    await submitRegistration()

    expect(mocks.register).toHaveBeenCalledTimes(1)
    expect(mocks.toastError).toHaveBeenCalledWith(message)
    expect(mocks.push).not.toHaveBeenCalled()
  })

  it("shows the server's message for a rejected registration", async () => {
    mocks.register.mockRejectedValueOnce(
      Object.assign(new Error('rejected'), { response: { data: { message: 'Invitation expired' } } }),
    )

    await submitRegistration()

    expect(mocks.toastError).toHaveBeenCalledWith('Invitation expired')
  })
})
