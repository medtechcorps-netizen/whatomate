/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { computed, ref } from 'vue'

const idle = vi.hoisted(() => ({
  state: null as any,
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${JSON.stringify(params)}` : key,
  }),
}))

vi.mock('@/composables/useSessionIdle', () => ({
  useSessionIdle: () => idle.state,
}))

import SessionIdleWarning from './SessionIdleWarning.vue'

function idleState(open: boolean) {
  const remainingMs = ref(95_000)
  return {
    warningOpen: ref(open),
    remainingMs,
    countdown: computed(() => '1:35'),
    staySignedIn: vi.fn(),
    signOutNow: vi.fn(),
  }
}

describe('SessionIdleWarning', () => {
  let wrapper: VueWrapper | null = null

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    document.body.innerHTML = ''
  })

  it('renders nothing while the session is active', async () => {
    idle.state = idleState(false)
    wrapper = mount(SessionIdleWarning, { attachTo: document.body })
    await flushPromises()
    expect(document.querySelector('[data-testid="session-idle-warning"]')).toBeNull()
  })

  it('shows the countdown with both choices', async () => {
    idle.state = idleState(true)
    wrapper = mount(SessionIdleWarning, { attachTo: document.body })
    await flushPromises()

    const dialog = document.querySelector('[data-testid="session-idle-warning"]')
    expect(dialog).not.toBeNull()
    expect(dialog!.textContent).toContain('sessionSecurity.idleWarningBody:{"time":"1:35"}')

    ;(document.querySelector('[data-testid="session-idle-stay"]') as HTMLButtonElement).click()
    expect(idle.state.staySignedIn).toHaveBeenCalledTimes(1)

    ;(document.querySelector('[data-testid="session-idle-sign-out"]') as HTMLButtonElement).click()
    expect(idle.state.signOutNow).toHaveBeenCalledTimes(1)
  })
})
