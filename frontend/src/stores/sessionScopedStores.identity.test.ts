/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  listTags: vi.fn(),
  createTag: vi.fn(),
  updateTag: vi.fn(),
  deleteTag: vi.fn(),
  listUsers: vi.fn(),
  getUser: vi.fn(),
  createUser: vi.fn(),
  updateUser: vi.fn(),
  deleteUser: vi.fn(),
  listRoles: vi.fn(),
  getRole: vi.fn(),
  createRole: vi.fn(),
  updateRole: vi.fn(),
  deleteRole: vi.fn(),
  listPermissions: vi.fn(),
  listTeams: vi.fn(),
  createTeam: vi.fn(),
  updateTeam: vi.fn(),
  deleteTeam: vi.fn(),
  listTeamMembers: vi.fn(),
  addTeamMember: vi.fn(),
  removeTeamMember: vi.fn(),
  listTransfers: vi.fn(),
  listCallLogs: vi.fn(),
  getCallLog: vi.fn(),
  listIVRFlows: vi.fn(),
  getIVRFlow: vi.fn(),
  createIVRFlow: vi.fn(),
  updateIVRFlow: vi.fn(),
  deleteIVRFlow: vi.fn(),
  listCallTransfers: vi.fn(),
  connectCallTransfer: vi.fn(),
  initiateCallTransfer: vi.fn(),
  hangupCallTransfer: vi.fn(),
  getICEServers: vi.fn(),
  initiateOutgoingCall: vi.fn(),
  hangupOutgoingCall: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  tagsService: {
    list: mocks.listTags,
    create: mocks.createTag,
    update: mocks.updateTag,
    delete: mocks.deleteTag,
  },
  usersService: {
    list: mocks.listUsers,
    get: mocks.getUser,
    create: mocks.createUser,
    update: mocks.updateUser,
    delete: mocks.deleteUser,
  },
  rolesService: {
    list: mocks.listRoles,
    get: mocks.getRole,
    create: mocks.createRole,
    update: mocks.updateRole,
    delete: mocks.deleteRole,
  },
  permissionsService: { list: mocks.listPermissions },
  teamsService: {
    list: mocks.listTeams,
    create: mocks.createTeam,
    update: mocks.updateTeam,
    delete: mocks.deleteTeam,
    listMembers: mocks.listTeamMembers,
    addMember: mocks.addTeamMember,
    removeMember: mocks.removeTeamMember,
  },
  chatbotService: { listTransfers: mocks.listTransfers },
  callLogsService: {
    list: mocks.listCallLogs,
    get: mocks.getCallLog,
    hold: vi.fn(),
    resume: vi.fn(),
  },
  ivrFlowsService: {
    list: mocks.listIVRFlows,
    get: mocks.getIVRFlow,
    create: mocks.createIVRFlow,
    update: mocks.updateIVRFlow,
    delete: mocks.deleteIVRFlow,
  },
  callTransfersService: {
    list: mocks.listCallTransfers,
    connect: mocks.connectCallTransfer,
    initiate: mocks.initiateCallTransfer,
    hangup: mocks.hangupCallTransfer,
  },
  outgoingCallsService: {
    getICEServers: mocks.getICEServers,
    initiate: mocks.initiateOutgoingCall,
    hangup: mocks.hangupOutgoingCall,
  },
}))

vi.mock('vue-sonner', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}))

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

describe('session-scoped store identity reset', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('keeps a late old-identity tag response out of the cache', async () => {
    const response = deferred<any>()
    mocks.listTags.mockReturnValue(response.promise)
    const { useTagsStore } = await import('./tags')
    const store = useTagsStore()

    const load = store.fetchTags()
    store.resetForIdentityChange()
    response.resolve({
      data: {
        data: {
          tags: [{ name: 'old-private-tag', color: '#000000' }],
          total: 1,
        },
      },
    })
    await load

    expect(store.tags).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('aborts and ignores a late old-identity user list response', async () => {
    const response = deferred<any>()
    let requestSignal: AbortSignal | undefined
    mocks.listUsers.mockImplementation((_params, signal) => {
      requestSignal = signal
      return response.promise
    })
    const { useUsersStore } = await import('./users')
    const store = useUsersStore()

    const load = store.fetchUsers()
    store.resetForIdentityChange()
    expect(requestSignal?.aborted).toBe(true)
    response.resolve({
      data: {
        data: {
          users: [{ id: 'old-user', full_name: 'Old Tenant Agent' }],
          total: 1,
        },
      },
    })
    await load

    expect(store.users).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('clears role and permission caches and ignores late old-identity responses', async () => {
    const rolesResponse = deferred<any>()
    const permissionsResponse = deferred<any>()
    const signals: AbortSignal[] = []
    mocks.listRoles.mockImplementation((_params, signal) => {
      signals.push(signal)
      return rolesResponse.promise
    })
    mocks.listPermissions.mockImplementation(signal => {
      signals.push(signal)
      return permissionsResponse.promise
    })
    const { useRolesStore } = await import('./roles')
    const store = useRolesStore()

    const rolesLoad = store.fetchRoles()
    const permissionsLoad = store.fetchPermissions()
    store.resetForIdentityChange()
    expect(signals).toHaveLength(2)
    expect(signals.every(signal => signal.aborted)).toBe(true)
    rolesResponse.resolve({
      data: { data: { roles: [{ id: 'old-role', name: 'Old Role' }] } },
    })
    permissionsResponse.resolve({
      data: {
        data: {
          permissions: [{ id: 'old-permission', resource: 'users', action: 'read' }],
        },
      },
    })
    await Promise.all([rolesLoad, permissionsLoad])

    expect(store.roles).toEqual([])
    expect(store.permissions).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('aborts and ignores a late old-identity team list response', async () => {
    const response = deferred<any>()
    let requestSignal: AbortSignal | undefined
    mocks.listTeams.mockImplementation((_params, signal) => {
      requestSignal = signal
      return response.promise
    })
    const { useTeamsStore } = await import('./teams')
    const store = useTeamsStore()

    const load = store.fetchTeams()
    store.resetForIdentityChange()
    expect(requestSignal?.aborted).toBe(true)
    response.resolve({
      data: {
        data: {
          teams: [{ id: 'old-team', name: 'Old Tenant Team' }],
          total: 1,
        },
      },
    })
    await load

    expect(store.teams).toEqual([])
    expect(store.loading).toBe(false)
    expect(store.error).toBeNull()
  })

  it('keeps a late old-identity transfer response out of queues and counts', async () => {
    const response = deferred<any>()
    mocks.listTransfers.mockReturnValue(response.promise)
    const { useTransfersStore } = await import('./transfers')
    const store = useTransfersStore()

    const load = store.fetchTransfers({ status: 'active' })
    store.resetForIdentityChange()
    response.resolve({
      data: {
        data: {
          transfers: [{ id: 'old-transfer', status: 'active' }],
          general_queue_count: 4,
          team_queue_counts: { 'old-team': 3 },
          total_count: 1,
        },
      },
    })
    await load

    expect(store.transfers).toEqual([])
    expect(store.generalQueueCount).toBe(0)
    expect(store.teamQueueCounts).toEqual({})
    expect(store.totalCount).toBe(0)
    expect(store.isLoading).toBe(false)
  })

  it('clears calling caches and ignores a late call-log response', async () => {
    const response = deferred<any>()
    mocks.listCallLogs.mockReturnValue(response.promise)
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    store.handleCallEvent('call_transfer_waiting', {
      id: 'old-waiting-call',
      status: 'waiting',
    })
    store.setCallPermissionPending('old-contact')

    const load = store.fetchCallLogs()
    store.resetForIdentityChange()
    response.resolve({
      data: {
        data: {
          call_logs: [{ id: 'old-call-log' }],
          total: 1,
        },
      },
    })
    await load

    expect(store.callLogs).toEqual([])
    expect(store.callLogsTotal).toBe(0)
    expect(store.waitingTransfers).toEqual([])
    expect(store.getCallPermission('old-contact')).toBeNull()
    expect(store.callLogsLoading).toBe(false)
  })

  it('stops media acquired by an old identity while call setup is pending', async () => {
    const iceResponse = deferred<any>()
    mocks.getICEServers.mockReturnValue(iceResponse.promise)
    const stop = vi.fn()
    const stream = {
      getTracks: () => [{ stop }],
      getAudioTracks: () => [],
    } as unknown as MediaStream
    vi.stubGlobal('navigator', {
      ...navigator,
      mediaDevices: { getUserMedia: vi.fn().mockResolvedValue(stream) },
    })
    const { useCallingStore } = await import('./calling')
    const store = useCallingStore()
    store.handleCallEvent('call_transfer_waiting', {
      id: 'old-waiting-call',
      status: 'waiting',
    })

    const accept = store.acceptTransfer('old-waiting-call')
    await Promise.resolve()
    await Promise.resolve()
    store.resetForIdentityChange()
    iceResponse.resolve({ data: { data: { ice_servers: [] } } })

    await expect(accept).rejects.toThrow('Calling identity changed')
    expect(stop).toHaveBeenCalled()
    expect(store.isOnCall).toBe(false)
    expect(store.waitingTransfers).toEqual([])
  })
})
