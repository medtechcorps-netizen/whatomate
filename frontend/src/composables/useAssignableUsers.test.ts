import { effectScope, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  permissions: new Set<string>(),
  listUsers: vi.fn(),
  assignableMembers: vi.fn(),
  organization: { selectedOrgId: 'org-1' as string | null },
  auth: { user: { id: 'caller-1' } as { id: string } | null, organizationId: 'org-1' },
  sharedUsers: [{ id: 'stale-other-org', full_name: 'Wrong workspace', is_active: true }],
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get user() { return mocks.auth.user },
    get organizationId() { return mocks.auth.organizationId },
    hasPermission: (resource: string, action: string) => mocks.permissions.has(`${resource}:${action}`),
  }),
}))
vi.mock('@/stores/users', () => ({
  useUsersStore: () => ({ users: mocks.sharedUsers, fetchUsers: vi.fn() }),
}))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => mocks.organization }))
vi.mock('@/services/api', () => ({
  contactsService: { assignableMembers: mocks.assignableMembers },
  usersService: { list: mocks.listUsers },
}))

type Source = 'members' | 'users'
const service = (source: Source) => source === 'members' ? mocks.assignableMembers : mocks.listUsers
function person(source: Source, id: string, extra: Record<string, unknown> = {}) {
  return {
    [source === 'members' ? 'user_id' : 'id']: id,
    full_name: `Person ${id}`, email: `${id}@example.test`, is_active: true,
    ...(source === 'members' ? { role_name: 'manager' } : { role: { name: 'manager' } }),
    ...extra,
  }
}
const rows = (source: Source, count: number, start = 1) =>
  Array.from({ length: count }, (_, index) => person(source, String(start + index)))
const response = (source: Source, entries: unknown[], total = entries.length, page = 1) =>
  ({ data: { data: { [source]: entries, total, page, limit: 100 } } })
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const settle = async () => { await Promise.resolve(); await Promise.resolve() }

describe('useAssignableUsers', () => {
  const scopes: ReturnType<typeof effectScope>[] = []
  async function setup(source?: Source) {
    if (source) mocks.permissions.add(source === 'members' ? 'organizations:read' : 'users:read')
    const { useAssignableUsers } = await import('./useAssignableUsers')
    const scope = effectScope()
    scopes.push(scope)
    return { scope, api: scope.run(() => useAssignableUsers())! }
  }

  beforeEach(() => {
    vi.resetAllMocks()
    vi.useFakeTimers()
    mocks.permissions = reactive(new Set(['contacts:write']))
    mocks.organization = reactive({ selectedOrgId: 'org-1' })
    mocks.auth = reactive({ user: { id: 'caller-1' }, organizationId: 'org-1' })
  })
  afterEach(() => {
    scopes.splice(0).forEach(scope => scope.stop())
    vi.useRealTimers()
  })

  it('does not expose the shared settings store or call a directory without either read permission', async () => {
    const { api } = await setup()
    expect(api.users.value).toEqual([])
    await api.load()
    expect(api.users.value).toEqual([])
    expect(mocks.listUsers).not.toHaveBeenCalled()
    expect(mocks.assignableMembers).not.toHaveBeenCalled()
  })

  for (const source of ['members', 'users'] as const) {
    describe(source, () => {
      it('loads beyond 100 and stops exactly after the last page', async () => {
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 201))
          .mockResolvedValueOnce(response(source, rows(source, 100, 101), 201, 2))
          .mockResolvedValueOnce(response(source, rows(source, 1, 201), 201, 3))
        const { api } = await setup(source)
        expect(api.users.value).toEqual([])
        await api.load()
        expect(api.users.value).toHaveLength(100)
        expect(api.hasMore.value).toBe(true)
        await api.loadMore()
        expect(api.users.value).toHaveLength(200)
        await api.loadMore()
        expect(api.users.value).toHaveLength(201)
        expect(api.nameOf('201')).toBe('Person 201')
        expect(api.hasMore.value).toBe(false)
        await api.loadMore()
        expect(service(source)).toHaveBeenCalledTimes(3)
        expect(service(source).mock.calls.map(call => call[0])).toEqual([1, 2, 3].map(page => ({ page, limit: 100, search: undefined })))
        expect(service(source).mock.calls.every(call => call[2] === 'org-1' && call[1] instanceof AbortSignal)).toBe(true)
      })

      it('rejects overlapping pages instead of hiding a missing person through deduplication', async () => {
        const repeated = person(source, '100', source === 'members' ? { role_name: 'agent' } : { role: { name: 'agent' } })
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 102))
          .mockResolvedValueOnce(response(source, [repeated, person(source, '101')], 102, 2))
        const { api } = await setup(source)
        await api.load()
        await api.loadMore()
        expect(api.users.value).toEqual([])
        expect(api.failed.value).toBe(true)
        expect(api.hasMore.value).toBe(false)
      })

      it('does not mistake an inactive first page for the end of the directory', async () => {
        service(source).mockResolvedValueOnce(response(source, rows(source, 100).map(row => ({ ...row, is_active: false })), 101))
          .mockResolvedValueOnce(response(source, [person(source, '101')], 101, 2))
        const { api } = await setup(source)
        await api.load()
        expect(api.users.value).toEqual([])
        expect(api.hasMore.value).toBe(true)
        await api.loadMore()
        expect(api.nameOf('101')).toBe('Person 101')
      })

      it('rejects a changed total between pages rather than accepting a shifted directory', async () => {
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 200))
          .mockResolvedValueOnce(response(source, rows(source, 100, 101), 201, 2))
        const { api } = await setup(source)
        await api.load()
        await api.loadMore()
        expect(api.users.value).toEqual([])
        expect(api.failed.value).toBe(true)
        expect(api.hasMore.value).toBe(false)
      })

      it('searches on the server rather than filtering only the first page', async () => {
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 401))
          .mockResolvedValueOnce(response(source, [person(source, '401', { full_name: 'Target Person' })]))
        const { api } = await setup(source)
        await api.load()
        api.search.value = '  Target  '
        expect(api.users.value).toEqual([])
        expect(api.loading.value).toBe(true)
        await vi.advanceTimersByTimeAsync(249)
        expect(service(source)).toHaveBeenCalledTimes(1)
        await vi.advanceTimersByTimeAsync(1)
        expect(service(source).mock.calls[1][0]).toEqual({ page: 1, limit: 100, search: 'Target' })
        expect(api.nameOf('401')).toBe('Target Person')
        expect(api.hasMore.value).toBe(false)
      })

      it('aborts and discards a late page after an organization switch', async () => {
        const old = deferred<ReturnType<typeof response>>()
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 101))
          .mockReturnValueOnce(old.promise)
          .mockResolvedValueOnce(response(source, [person(source, 'new')]))
        const { api } = await setup(source)
        await api.load()
        const loadMore = api.loadMore()
        const oldSignal = service(source).mock.calls[1][1] as AbortSignal
        mocks.organization.selectedOrgId = 'org-2'
        expect(oldSignal.aborted).toBe(true)
        expect(api.users.value).toEqual([])
        await settle()
        expect(service(source).mock.calls[2][2]).toBe('org-2')
        expect(api.nameOf('new')).toBe('Person new')
        old.resolve(response(source, [person(source, 'old-late')], 101, 2))
        await loadMore
        expect(api.users.value.map(user => user.id)).toEqual(['new'])
      })

      it('shows failure instead of a truncated directory and retries from page one', async () => {
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 101))
          .mockRejectedValueOnce(new Error('network'))
          .mockResolvedValueOnce(response(source, [person(source, 'retry')]))
        const { api } = await setup(source)
        await api.load()
        await api.loadMore()
        expect(api.users.value).toEqual([])
        expect(api.failed.value).toBe(true)
        expect(api.loading.value).toBe(false)
        await api.loadMore()
        expect(service(source)).toHaveBeenCalledTimes(2)
        await api.load()
        expect(service(source).mock.calls[2][0].page).toBe(1)
        expect(api.failed.value).toBe(false)
        expect(api.nameOf('retry')).toBe('Person retry')
      })

      it('does not send a second load-more request on a double click', async () => {
        const page = deferred<ReturnType<typeof response>>()
        service(source).mockResolvedValueOnce(response(source, rows(source, 100), 101)).mockReturnValueOnce(page.promise)
        const { api } = await setup(source)
        await api.load()
        const first = api.loadMore()
        await api.loadMore()
        expect(service(source)).toHaveBeenCalledTimes(2)
        page.resolve(response(source, [person(source, '101')], 101, 2))
        await first
        expect(api.users.value).toHaveLength(101)
      })
    })
  }

  it('prefers users:read without broadening the member endpoint permission', async () => {
    mocks.permissions.add('organizations:read')
    mocks.listUsers.mockResolvedValue(response('users', [person('users', '1')]))
    const { api } = await setup('users')
    await api.load()
    expect(mocks.assignableMembers).not.toHaveBeenCalled()
    expect(api.users.value[0].role_name).toBe('manager')
  })

  it('invalidates on caller replacement, logout and loss of assign permission', async () => {
    mocks.listUsers.mockResolvedValue(response('users', [person('users', '1')]))
    const { api } = await setup('users')
    await api.load()
    mocks.auth.user = { id: 'caller-2' }
    expect(api.users.value).toEqual([])
    await settle()
    expect(mocks.listUsers).toHaveBeenCalledTimes(2)
    mocks.permissions.delete('contacts:write')
    expect(api.users.value).toEqual([])
    expect(api.loading.value).toBe(false)
    mocks.auth.user = null
    await api.load()
    expect(mocks.listUsers).toHaveBeenCalledTimes(2)
  })

  it('reloads through the member permission when users:read is revoked', async () => {
    mocks.permissions.add('organizations:read')
    mocks.listUsers.mockResolvedValue(response('users', [person('users', '1')]))
    mocks.assignableMembers.mockResolvedValue(response('members', [person('members', '2')]))
    const { api } = await setup('users')
    await api.load()
    mocks.permissions.delete('users:read')
    expect(api.users.value).toEqual([])
    await settle()
    expect(api.users.value.map(user => user.id)).toEqual(['2'])
  })

  it('also invalidates when the same user receives a replacement auth session', async () => {
    mocks.listUsers.mockResolvedValue(response('users', [person('users', '1')]))
    const { api } = await setup('users')
    await api.load()
    mocks.auth.user = { id: 'caller-1' }
    expect(api.users.value).toEqual([])
    await settle()
    expect(mocks.listUsers).toHaveBeenCalledTimes(2)
  })

  it('aborts old searches immediately, debounces typing and ignores out-of-order responses', async () => {
    const old = deferred<ReturnType<typeof response>>()
    mocks.assignableMembers.mockReturnValueOnce(old.promise).mockResolvedValueOnce(response('members', [person('members', 'new')]))
    const { api } = await setup('members')
    const first = api.load()
    api.search.value = 'n'
    expect(mocks.assignableMembers.mock.calls[0][1].aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(200)
    api.search.value = 'new'
    await vi.advanceTimersByTimeAsync(250)
    expect(mocks.assignableMembers).toHaveBeenCalledTimes(2)
    old.resolve(response('members', [person('members', 'old')]))
    await first
    expect(api.users.value.map(user => user.id)).toEqual(['new'])
    expect(api.failed.value).toBe(false)
  })

  it('aborts in-flight work and cancels pending search on disposal', async () => {
    const pending = deferred<ReturnType<typeof response>>()
    mocks.assignableMembers.mockReturnValueOnce(pending.promise)
    const { api, scope } = await setup('members')
    const request = api.load()
    const signal = mocks.assignableMembers.mock.calls[0][1] as AbortSignal
    api.search.value = 'new'
    scope.stop()
    expect(signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(500)
    pending.resolve(response('members', [person('members', 'old')]))
    await request
    await api.load()
    expect(mocks.assignableMembers).toHaveBeenCalledTimes(1)
    expect(api.users.value).toEqual([])
  })

  it('disposal alone aborts an in-flight request and discards its late rejection', async () => {
    const pending = deferred<ReturnType<typeof response>>()
    mocks.listUsers.mockReturnValueOnce(pending.promise)
    const { api, scope } = await setup('users')
    const request = api.load()
    const signal = mocks.listUsers.mock.calls[0][1] as AbortSignal
    scope.stop()
    expect(signal.aborted).toBe(true)
    pending.reject(new Error('late failure'))
    await request
    expect(api.failed.value).toBe(false)
    expect(api.loading.value).toBe(false)
    expect(api.users.value).toEqual([])
  })

  it.each([
    { members: [], total: 101, page: 1, limit: 100 },
    { members: [], total: 0, page: 2, limit: 100 },
    { members: [], total: 0, page: 1, limit: 50 },
    { members: [], page: 1, limit: 100 },
    { members: [person('members', 'unexpected')], total: 0, page: 1, limit: 100 },
  ])('rejects malformed or incomplete page metadata: %j', async data => {
    mocks.assignableMembers.mockResolvedValue({ data: { data } })
    const { api } = await setup('members')
    await api.load()
    expect(api.failed.value).toBe(true)
    expect(api.users.value).toEqual([])
  })

  it('accepts the server null empty slice without showing a false failure', async () => {
    mocks.assignableMembers.mockResolvedValue({ data: { data: { members: null, total: 0, page: 1, limit: 100 } } })
    const { api } = await setup('members')
    await api.load()
    expect(api.failed.value).toBe(false)
    expect(api.hasMore.value).toBe(false)
    expect(api.nameOf(null)).toBeUndefined()
  })
})
