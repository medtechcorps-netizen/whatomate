/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useOrganizationsStore } from './organizations'

vi.mock('@/services/api', () => ({
  organizationsService: { list: vi.fn(), addMember: vi.fn(), createInvitation: vi.fn() },
  usersService: { listMyOrganizations: vi.fn() },
}))

describe('organization switch holds', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('stays blocked until every holder has released its own hold', () => {
    const store = useOrganizationsStore()
    store.blockOrganizationSwitch('chat-media-batch:1', 'Sending to A')
    store.blockOrganizationSwitch('embedded-signup', 'Finish signup first')
    store.unblockOrganizationSwitch('embedded-signup')
    expect(store.organizationSwitchBlocker).toEqual({ owner: 'chat-media-batch:1', message: 'Sending to A' })
    store.unblockOrganizationSwitch('chat-media-batch:1')
    expect(store.organizationSwitchBlocker).toBeNull()
  })

  it('never lets one holder release another holder\'s hold', () => {
    const store = useOrganizationsStore()
    store.blockOrganizationSwitch('chat-media-batch:2', 'Sending to B')
    store.unblockOrganizationSwitch('chat-media-batch:1')
    expect(store.organizationSwitchBlocker?.owner).toBe('chat-media-batch:2')
  })

  it('updates a holder\'s message in place and keeps the oldest hold first', () => {
    const store = useOrganizationsStore()
    store.blockOrganizationSwitch('a', 'first')
    store.blockOrganizationSwitch('b', 'second')
    store.blockOrganizationSwitch('a', 'first, updated')
    expect(store.organizationSwitchBlocker).toEqual({ owner: 'a', message: 'first, updated' })
  })

  it('clears every hold on an identity change', () => {
    const store = useOrganizationsStore()
    store.blockOrganizationSwitch('a', 'first')
    store.blockOrganizationSwitch('b', 'second')
    store.resetForIdentityChange()
    expect(store.organizationSwitchBlocker).toBeNull()
  })
})
