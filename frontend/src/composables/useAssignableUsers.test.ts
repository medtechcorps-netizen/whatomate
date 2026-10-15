import { effectScope, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  permissions: new Set<string>(),
  fetchUsers: vi.fn(),
  assignableMembers: vi.fn(),
  users: [] as Array<Record<string, any>>,
  organization: { selectedOrgId: 'org-1' as string | null },
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    organizationId: 'org-1',
    hasPermission: (resource: string, action: string) => mocks.permissions.has(`${resource}:${action}`),
  }),
}))
vi.mock('@/stores/users', () => ({
  useUsersStore: () => ({
    get users() {
      return mocks.users
    },
    fetchUsers: mocks.fetchUsers,
  }),
}))
vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => mocks.organization,
}))
vi.mock('@/services/api', () => ({
  contactsService: { assignableMembers: mocks.assignableMembers },
}))

async function setup() {
  const { useAssignableUsers } = await import('./useAssignableUsers')
  const scope = effectScope()
  const api = scope.run(() => useAssignableUsers())!
  return { scope, api }
}

describe('useAssignableUsers', () => {
  let stop: (() => void) | null = null

  beforeEach(() => {
    vi.clearAllMocks()
    mocks.permissions = new Set()
    mocks.users = []
    mocks.organization = reactive({ selectedOrgId: 'org-1' })
    mocks.fetchUsers.mockResolvedValue(undefined)
  })

  afterEach(() => {
    stop?.()
    stop = null
  })

  it('uses the users list for callers with users:read', async () => {
    mocks.permissions = new Set(['users:read', 'organizations:read'])
    mocks.users = [
      { id: 'u-1', full_name: 'Aisyah Rahman', email: 'a@example.test', is_active: true, role: { name: 'admin' } },
      { id: 'u-2', full_name: 'Gone', email: 'g@example.test', is_active: false },
    ]
    const { scope, api } = await setup()
    stop = () => scope.stop()
    await api.load()
    expect(mocks.fetchUsers).toHaveBeenCalledTimes(1)
    expect(mocks.assignableMembers).not.toHaveBeenCalled()
    expect(api.users.value).toEqual([
      { id: 'u-1', full_name: 'Aisyah Rahman', email: 'a@example.test', role_name: 'admin' },
    ])
    expect(api.nameOf('u-1')).toBe('Aisyah Rahman')
  })

  it('falls back to active organization members without users:read', async () => {
    mocks.permissions = new Set(['organizations:read'])
    mocks.assignableMembers.mockResolvedValue({
      data: {
        data: {
          members: [
            { user_id: 'u-1', full_name: 'Ben Lim', email: 'b@example.test', is_active: true, role_name: 'manager' },
            { user_id: 'u-2', full_name: 'Inactive', is_active: false },
            { user_id: 'u-3', full_name: null, is_active: true },
          ],
          total: 3,
        },
      },
    })
    const { scope, api } = await setup()
    stop = () => scope.stop()
    await api.load()
    expect(mocks.fetchUsers).not.toHaveBeenCalled()
    expect(api.users.value).toEqual([
      { id: 'u-1', full_name: 'Ben Lim', email: 'b@example.test', role_name: 'manager' },
    ])
    expect(api.nameOf('u-1')).toBe('Ben Lim')
    expect(api.nameOf(null)).toBeUndefined()
  })

  it('offers nobody and calls nothing without either permission', async () => {
    const { scope, api } = await setup()
    stop = () => scope.stop()
    await api.load()
    expect(mocks.fetchUsers).not.toHaveBeenCalled()
    expect(mocks.assignableMembers).not.toHaveBeenCalled()
    expect(api.users.value).toEqual([])
  })

  it('reloads for a new workspace instead of offering the previous members', async () => {
    mocks.permissions = new Set(['organizations:read'])
    mocks.assignableMembers.mockResolvedValueOnce({
      data: { data: { members: [{ user_id: 'old', full_name: 'Old Org', is_active: true }], total: 1 } },
    })
    const { scope, api } = await setup()
    stop = () => scope.stop()
    await api.load()
    expect(api.nameOf('old')).toBe('Old Org')

    mocks.assignableMembers.mockResolvedValueOnce({
      data: { data: { members: [{ user_id: 'new', full_name: 'New Org', is_active: true }], total: 1 } },
    })
    mocks.organization.selectedOrgId = 'org-2'
    await nextTick()
    await vi.waitFor(() => expect(api.nameOf('new')).toBe('New Org'))
    expect(api.nameOf('old')).toBeUndefined()
    expect(mocks.assignableMembers).toHaveBeenCalledTimes(2)
  })
})
