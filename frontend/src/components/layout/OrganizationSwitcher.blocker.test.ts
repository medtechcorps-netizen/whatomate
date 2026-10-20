/** @vitest-environment happy-dom */

import { flushPromises, shallowMount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import OrganizationSwitcher from './OrganizationSwitcher.vue'
import { useOrganizationsStore } from '@/stores/organizations'

const mocks = vi.hoisted(() => ({
  switchOrg: vi.fn(),
  listMyOrganizations: vi.fn(),
  toastError: vi.fn(),
  disconnect: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  organizationsService: { list: vi.fn(async () => ({ data: { data: { organizations: [] } } })), addMember: vi.fn(), createInvitation: vi.fn(), create: vi.fn() },
  usersService: { listMyOrganizations: mocks.listMyOrganizations },
  sessionsSupported: vi.fn(() => true),
  SESSION_BUSY_CODE: 'ERR_SESSION_BUSY',
  SESSION_UNSUPPORTED_CODE: 'ERR_SESSION_UNSUPPORTED',
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    user: { id: 'agent-1', organization_id: 'org-A', is_super_admin: false },
    hasPermission: () => false,
    switchOrg: mocks.switchOrg,
  }),
}))
vi.mock('@/services/websocket', () => ({ wsService: { disconnect: mocks.disconnect } }))
vi.mock('vue-router', () => ({
  useRoute: () => ({ name: 'chat', path: '/chat', query: {} }),
  useRouter: () => ({ replace: vi.fn() }),
}))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('vue-sonner', () => ({ toast: { error: mocks.toastError, success: vi.fn() } }))

describe('OrganizationSwitcher while switching is held', () => {
  let reload: ReturnType<typeof vi.fn>

  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    mocks.listMyOrganizations.mockResolvedValue({
      data: {
        data: {
          organizations: [
            { organization_id: 'org-A', name: 'Clinic A', role_name: 'agent', is_default: true },
            { organization_id: 'org-B', name: 'Clinic B', role_name: 'agent', is_default: false },
          ],
        },
      },
    })
    reload = vi.fn()
    vi.stubGlobal('location', { ...window.location, reload })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  async function mountSwitcher() {
    const wrapper = shallowMount(OrganizationSwitcher, { props: { collapsed: false } })
    await flushPromises()
    return wrapper
  }

  it('refuses the switch with the hold message: no selectOrganization, switchOrg or reload', async () => {
    const store = useOrganizationsStore()
    const select = vi.spyOn(store, 'selectOrganization')
    store.blockOrganizationSwitch('chat-media-batch:1', 'Files are still being sent to Aisyah.')
    const wrapper = await mountSwitcher()
    const picker = wrapper.findComponent({ name: 'Select' })
    expect(picker.exists()).toBe(true)
    picker.vm.$emit('update:modelValue', 'org-B')
    await flushPromises()
    expect(mocks.toastError).toHaveBeenCalledWith('Files are still being sent to Aisyah.')
    expect(mocks.switchOrg).not.toHaveBeenCalled()
    expect(select).not.toHaveBeenCalled()
    expect(reload).not.toHaveBeenCalled()
  })

  it('switches again once the last hold is released', async () => {
    const store = useOrganizationsStore()
    store.blockOrganizationSwitch('chat-media-batch:1', 'busy')
    store.unblockOrganizationSwitch('chat-media-batch:1')
    mocks.switchOrg.mockResolvedValue(undefined)
    const wrapper = await mountSwitcher()
    wrapper.findComponent({ name: 'Select' }).vm.$emit('update:modelValue', 'org-B')
    await flushPromises()
    expect(mocks.switchOrg).toHaveBeenCalledWith('org-B')
    expect(reload).toHaveBeenCalledTimes(1)
  })
})
