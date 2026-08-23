/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX } from '@/lib/legacyWhatsAppReplyAttempts'

const mocks = vi.hoisted(() => ({
  listOrganizations: vi.fn(),
  listMyOrganizations: vi.fn(),
  addMember: vi.fn(),
  createInvitation: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  organizationsService: {
    list: mocks.listOrganizations,
    addMember: mocks.addMember,
    createInvitation: mocks.createInvitation,
  },
  usersService: {
    listMyOrganizations: mocks.listMyOrganizations,
  },
}))

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function organization(id: string, name: string) {
  return { id, name, slug: id }
}

function membership(id: string, name: string) {
  return {
    organization_id: id,
    name,
    slug: id,
    role_name: 'agent',
    is_default: true,
  }
}

describe('organizations store identity isolation', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    localStorage.clear()
    sessionStorage.clear()
  })

  it('clears workspace names and ignores both old-account list responses after reset', async () => {
    const { useOrganizationsStore } = await import('./organizations')
    const store = useOrganizationsStore()
    const oldOrganizations = deferred<any>()
    const oldMemberships = deferred<any>()
    const newOrganizations = deferred<any>()
    const newMemberships = deferred<any>()
    let oldOrganizationsSignal: AbortSignal | undefined
    let oldMembershipsSignal: AbortSignal | undefined
    mocks.listOrganizations
      .mockImplementationOnce((signal?: AbortSignal) => {
        oldOrganizationsSignal = signal
        return oldOrganizations.promise
      })
      .mockImplementationOnce(() => newOrganizations.promise)
    mocks.listMyOrganizations
      .mockImplementationOnce((signal?: AbortSignal) => {
        oldMembershipsSignal = signal
        return oldMemberships.promise
      })
      .mockImplementationOnce(() => newMemberships.promise)

    store.organizations = [organization('old-org', 'Old Account Workspace') as any]
    store.myOrganizations = [membership('old-org', 'Old Account Workspace')]
    store.selectOrganization('old-org')
    store.blockOrganizationSwitch('old-flow', 'old blocker')
    store.error = 'old error'
    const oldOrganizationLoad = store.fetchOrganizations()
    const oldMembershipLoad = store.fetchMyOrganizations()

    store.resetForIdentityChange()
    expect(store.organizations).toEqual([])
    expect(store.myOrganizations).toEqual([])
    expect(store.selectedOrgId).toBeNull()
    expect(store.organizationSwitchBlocker).toBeNull()
    expect(store.error).toBeNull()
    expect(store.loading).toBe(false)
    expect(localStorage.getItem('selected_organization_id')).toBeNull()
    expect(oldOrganizationsSignal?.aborted).toBe(true)
    expect(oldMembershipsSignal?.aborted).toBe(true)

    const newOrganizationLoad = store.fetchOrganizations()
    const newMembershipLoad = store.fetchMyOrganizations()
    newOrganizations.resolve({
      data: { data: { organizations: [organization('new-org', 'New Account Workspace')] } },
    })
    newMemberships.resolve({
      data: { data: { organizations: [membership('new-org', 'New Account Workspace')] } },
    })
    await Promise.all([newOrganizationLoad, newMembershipLoad])

    oldOrganizations.resolve({
      data: { data: { organizations: [organization('old-org', 'Old Account Workspace')] } },
    })
    oldMemberships.resolve({
      data: { data: { organizations: [membership('old-org', 'Old Account Workspace')] } },
    })
    await Promise.all([oldOrganizationLoad, oldMembershipLoad])

    expect(store.organizations.map(item => item.name)).toEqual(['New Account Workspace'])
    expect(store.myOrganizations.map(item => item.name)).toEqual(['New Account Workspace'])
    expect(JSON.stringify(store.$state)).not.toContain('Old Account Workspace')
  })

  it('clears the pending-reply namespace on organization switch and logout reset', async () => {
    const { useOrganizationsStore } = await import('./organizations')
    const store = useOrganizationsStore()
    const firstKey = `${LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX}old-org:conversation-1`
    const secondKey = `${LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX}new-org:conversation-2`

    store.selectOrganization('old-org')
    sessionStorage.setItem(firstKey, '{"privacy":"minimal"}')
    store.selectOrganization('new-org')
    expect(sessionStorage.getItem(firstKey)).toBeNull()

    sessionStorage.setItem(secondKey, '{"privacy":"minimal"}')
    store.resetForIdentityChange()
    expect(sessionStorage.getItem(secondKey)).toBeNull()
  })
})
