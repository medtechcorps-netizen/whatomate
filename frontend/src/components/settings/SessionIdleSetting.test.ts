/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
  sessionId: 'session-1',
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key,
  }),
}))

vi.mock('@/services/api', () => ({
  api: { get: mocks.get, put: mocks.put },
  readSessionId: () => mocks.sessionId,
}))

vi.mock('vue-sonner', () => ({
  toast: { success: mocks.toastSuccess, error: mocks.toastError },
}))

import SessionIdleSetting from './SessionIdleSetting.vue'
import { Select } from '@/components/ui/select'
import {
  SESSION_IDLE_POLICY_KEY,
  SESSION_POLICY_EVENT,
  readSessionIdlePolicy,
} from '@/lib/sessionActivity'

function signIn(organizationId: string, userId = 'user-1') {
  localStorage.setItem('user', JSON.stringify({ id: userId, organization_id: organizationId }))
}

// A save whose response is held until release() is called.
function heldSave() {
  let release!: () => void
  mocks.put.mockImplementation(
    () => new Promise((resolve) => {
      release = () => resolve({ data: { data: {} } })
    }),
  )
  return () => release()
}

describe('SessionIdleSetting', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    localStorage.clear()
    signIn('org-1')
    mocks.sessionId = 'session-1'
    mocks.get.mockReset()
    mocks.put.mockReset().mockResolvedValue({ data: { data: {} } })
    mocks.toastSuccess.mockReset()
    mocks.toastError.mockReset()
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
  })

  it('uses the value the settings page loaded, without a request of its own', async () => {
    wrapper = mount(SessionIdleSetting, { props: { initialMinutes: null } })
    await flushPromises()
    const save = wrapper.get('[data-testid="session-idle-save"]')
    expect(save.attributes('disabled')).toBeDefined()

    await wrapper.setProps({ initialMinutes: 60 })
    expect(wrapper.findComponent(Select).props('modelValue')).toBe('60')
    expect(mocks.get).not.toHaveBeenCalled()
  })

  it('saves a new choice and applies it in this browser', async () => {
    wrapper = mount(SessionIdleSetting, { props: { initialMinutes: 60 } })
    await flushPromises()
    const select = wrapper.findComponent(Select)
    const save = wrapper.get('[data-testid="session-idle-save"]')
    expect(save.attributes('disabled')).toBeDefined()

    select.vm.$emit('update:modelValue', '0')
    await flushPromises()
    expect(save.attributes('disabled')).toBeUndefined()

    await save.trigger('click')
    await flushPromises()

    expect(mocks.put).toHaveBeenCalledWith('/org/settings', { session_idle_timeout_minutes: 0 })
    expect(readSessionIdlePolicy('user-1|org-1')).toBe(0)
    expect(mocks.toastSuccess).toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toHaveLength(1)
    expect(save.attributes('disabled')).toBeDefined()
  })

  for (const [viewing, cached] of [['another organization', 60], ['their own organization', 0]] as const) {
    it(`a super admin viewing ${viewing} caches ${cached === 0 ? 'the saved' : 'no'} policy for their own session`, async () => {
      // Signed in to org-1 (the organization /me names).
      localStorage.setItem('selected_organization_id', viewing === 'another organization' ? 'org-2' : 'org-1')
      localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ 'user-1|org-1': 60 }))
      wrapper = mount(SessionIdleSetting, { props: { initialMinutes: 60 } })
      await flushPromises()
      wrapper.findComponent(Select).vm.$emit('update:modelValue', '0')
      await flushPromises()
      await wrapper.get('[data-testid="session-idle-save"]').trigger('click')
      await flushPromises()

      expect(mocks.put).toHaveBeenCalledWith('/org/settings', { session_idle_timeout_minutes: 0 })
      expect(readSessionIdlePolicy('user-1|org-1')).toBe(cached)
      expect(mocks.toastSuccess).toHaveBeenCalled()
    })
  }

  it('reports a failed save', async () => {
    mocks.put.mockRejectedValue(new Error('403'))
    wrapper = mount(SessionIdleSetting, { props: { initialMinutes: 240 } })
    await flushPromises()

    wrapper.findComponent(Select).vm.$emit('update:modelValue', '30')
    await flushPromises()
    await wrapper.get('[data-testid="session-idle-save"]').trigger('click')
    await flushPromises()

    expect(mocks.toastError).toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(localStorage.getItem(SESSION_IDLE_POLICY_KEY)).toBeNull()
  })

  it("applies a save that completes after an org switch to neither organization's view", async () => {
    localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ 'user-1|org-2': 30 }))
    const events: unknown[] = []
    const onPolicy = (event: Event) => events.push((event as CustomEvent).detail)
    window.addEventListener(SESSION_POLICY_EVENT, onPolicy)
    wrapper = mount(SessionIdleSetting, { props: { initialMinutes: 240 } })
    await flushPromises()
    const release = heldSave()

    wrapper.findComponent(Select).vm.$emit('update:modelValue', '0')
    await flushPromises()
    await wrapper.get('[data-testid="session-idle-save"]').trigger('click')
    // Org A's save is in flight when the admin switches to org B.
    signIn('org-2')
    release()
    await flushPromises()

    window.removeEventListener(SESSION_POLICY_EVENT, onPolicy)
    expect(readSessionIdlePolicy('user-1|org-2')).toBe(30)
    expect(readSessionIdlePolicy()).toBe(30)
    expect(events).toEqual([])
    expect(mocks.toastSuccess).not.toHaveBeenCalled()
    expect(wrapper.emitted('saved')).toBeUndefined()
  })

  it('applies nothing from a save that completes after a new login', async () => {
    wrapper = mount(SessionIdleSetting, { props: { initialMinutes: 240 } })
    await flushPromises()
    const release = heldSave()

    wrapper.findComponent(Select).vm.$emit('update:modelValue', '30')
    await flushPromises()
    await wrapper.get('[data-testid="session-idle-save"]').trigger('click')
    // Same user and organization, but a new logical session.
    mocks.sessionId = 'session-2'
    release()
    await flushPromises()

    expect(localStorage.getItem(SESSION_IDLE_POLICY_KEY)).toBeNull()
    expect(mocks.toastSuccess).not.toHaveBeenCalled()
  })

  it('does not apply an old save when only the viewed workspace changes', async () => {
    localStorage.setItem('selected_organization_id', 'org-1')
    localStorage.setItem(SESSION_IDLE_POLICY_KEY, JSON.stringify({ 'user-1|org-1': 240 }))
    const events: unknown[] = []
    const onPolicy = (event: Event) => events.push((event as CustomEvent).detail)
    window.addEventListener(SESSION_POLICY_EVENT, onPolicy)
    try {
      wrapper = mount(SessionIdleSetting, { props: { initialMinutes: 240 } })
      await flushPromises()
      const release = heldSave()
      wrapper.findComponent(Select).vm.$emit('update:modelValue', '30')
      await flushPromises()
      await wrapper.get('[data-testid="session-idle-save"]').trigger('click')
      expect(mocks.put).toHaveBeenCalledExactlyOnceWith('/org/settings', { session_idle_timeout_minutes: 30 })

      // Super-admin workspace selection changes without a new login or /me identity.
      localStorage.setItem('selected_organization_id', 'org-2')
      await wrapper.setProps({ initialMinutes: 60 })
      release()
      await flushPromises()

      expect(readSessionIdlePolicy('user-1|org-1')).toBe(240)
      expect(events).toEqual([])
      expect(mocks.toastSuccess).not.toHaveBeenCalled()
      expect(wrapper.emitted('saved')).toBeUndefined()
      expect(wrapper.findComponent(Select).props('modelValue')).toBe('60')
    } finally {
      window.removeEventListener(SESSION_POLICY_EVENT, onPolicy)
    }
  })
})
