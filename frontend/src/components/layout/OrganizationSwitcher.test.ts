/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  switchOrg: vi.fn(),
  selectOrganization: vi.fn(),
  supported: true,
  toastError: vi.fn(),
  reload: vi.fn(),
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({ name: 'dashboard', path: '/', query: {} }),
  useRouter: () => ({ replace: vi.fn() }),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key }),
}))

vi.mock('vue-sonner', () => ({
  toast: Object.assign(vi.fn(), { error: mocks.toastError, success: vi.fn() }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    user: { id: 'user-1', is_super_admin: false, organization_id: 'org-1' },
    hasPermission: () => false,
    switchOrg: mocks.switchOrg,
  }),
}))

vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => ({
    organizations: [],
    myOrganizations: [
      { organization_id: 'org-1', name: 'First', slug: 'first', role_name: 'admin', is_default: true },
      { organization_id: 'org-2', name: 'Second', slug: 'second', role_name: 'admin', is_default: false },
    ],
    isMultiOrg: true,
    selectedOrgId: null,
    selectedOrganization: null,
    organizationSwitchBlocker: null,
    loading: false,
    error: null,
    fetchMyOrganizations: vi.fn(async () => undefined),
    fetchOrganizations: vi.fn(async () => undefined),
    init: vi.fn(),
    selectOrganization: mocks.selectOrganization,
  }),
}))

vi.mock('@/services/websocket', () => ({
  wsService: { disconnect: vi.fn() },
}))

vi.mock('@/services/api', () => ({
  organizationsService: { create: vi.fn() },
  sessionsSupported: () => mocks.supported,
  SESSION_BUSY_CODE: 'ERR_SESSION_BUSY',
  SESSION_UNSUPPORTED_CODE: 'ERR_SESSION_UNSUPPORTED',
}))

import OrganizationSwitcher from './OrganizationSwitcher.vue'
import { Select } from '@/components/ui/select'

const originalLocation = window.location

async function chooseSecondOrganization() {
  const wrapper = mount(OrganizationSwitcher, { props: { collapsed: false } })
  await flushPromises()
  wrapper.findComponent(Select).vm.$emit('update:modelValue', 'org-2')
  await flushPromises()
  return wrapper
}

describe('OrganizationSwitcher refusals', () => {
  beforeEach(() => {
    mocks.supported = true
    mocks.switchOrg.mockReset()
    mocks.selectOrganization.mockReset()
    mocks.toastError.mockReset()
    mocks.reload.mockReset()
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { pathname: '/', href: 'http://localhost:3000/', reload: mocks.reload },
    })
  })

  afterEach(() => {
    Object.defineProperty(window, 'location', { configurable: true, value: originalLocation })
  })

  it('without Web Locks says why and sends no switch', async () => {
    mocks.supported = false

    await chooseSecondOrganization()

    expect(mocks.switchOrg).not.toHaveBeenCalled()
    expect(mocks.selectOrganization).not.toHaveBeenCalled()
    expect(mocks.toastError).toHaveBeenCalledWith('errors.unsupportedBrowser')
    expect(mocks.reload).not.toHaveBeenCalled()
  })

  it.each([
    ['ERR_SESSION_UNSUPPORTED', 'errors.unsupportedBrowser'],
    ['ERR_SESSION_BUSY', 'errors.timeout'],
    ['ERR_CANCELED', 'errors.timeout'],
  ])('maps a switch refused with %s to %s, without reloading', async (code, message) => {
    mocks.switchOrg.mockRejectedValueOnce(Object.assign(new Error('refused'), { code }))

    await chooseSecondOrganization()

    expect(mocks.switchOrg).toHaveBeenCalledWith('org-2')
    expect(mocks.toastError).toHaveBeenCalledWith(message)
    expect(mocks.selectOrganization).not.toHaveBeenCalled()
    expect(mocks.reload).not.toHaveBeenCalled()
  })

  it('reloads onto the new organization once the switch succeeds', async () => {
    mocks.switchOrg.mockResolvedValueOnce(undefined)

    await chooseSecondOrganization()

    expect(mocks.selectOrganization).toHaveBeenCalledWith('org-2')
    expect(mocks.reload).toHaveBeenCalled()
    expect(mocks.toastError).not.toHaveBeenCalled()
  })
})
