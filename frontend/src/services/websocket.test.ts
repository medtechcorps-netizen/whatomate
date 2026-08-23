/** @vitest-environment happy-dom */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  contactsStore: {
    currentContact: null as { id: string } | null,
    addMessage: vi.fn(),
    fetchContacts: vi.fn().mockResolvedValue(undefined),
    fetchMessages: vi.fn().mockResolvedValue(undefined),
    refreshCurrentMessages: vi.fn().mockResolvedValue(undefined),
    resetForIdentityChange: vi.fn(),
    updateMessageStatus: vi.fn(),
    updateMessageReactions: vi.fn(),
  },
  transfersStore: {
    addTransfer: vi.fn(),
    updateTransfer: vi.fn(),
    fetchTransfers: vi.fn().mockResolvedValue(undefined),
    resetForIdentityChange: vi.fn(),
  },
  callingStore: {
    handleCallEvent: vi.fn(),
    resetForIdentityChange: vi.fn(),
  },
  tagsStore: { resetForIdentityChange: vi.fn() },
  usersStore: { resetForIdentityChange: vi.fn() },
  rolesStore: { resetForIdentityChange: vi.fn() },
  teamsStore: { resetForIdentityChange: vi.fn() },
  authStore: {
    user: null as { id: string } | null,
    userSettings: {} as Record<string, unknown>,
    refreshUserData: vi.fn().mockResolvedValue(true),
  },
  notesStore: {
    addNote: vi.fn(),
    onNoteUpdated: vi.fn(),
    onNoteDeleted: vi.fn(),
    clearNotes: vi.fn(),
  },
  markRead: vi.fn().mockResolvedValue(undefined),
  toastInfo: vi.fn(),
  toastWarning: vi.fn(),
  routerPush: vi.fn(),
}))

vi.mock('@/stores/contacts', () => ({
  useContactsStore: () => mocks.contactsStore,
}))
vi.mock('@/stores/transfers', () => ({
  useTransfersStore: () => mocks.transfersStore,
}))
vi.mock('@/stores/calling', () => ({
  useCallingStore: () => mocks.callingStore,
}))
vi.mock('@/stores/tags', () => ({
  useTagsStore: () => mocks.tagsStore,
}))
vi.mock('@/stores/users', () => ({
  useUsersStore: () => mocks.usersStore,
}))
vi.mock('@/stores/roles', () => ({
  useRolesStore: () => mocks.rolesStore,
}))
vi.mock('@/stores/teams', () => ({
  useTeamsStore: () => mocks.teamsStore,
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => mocks.authStore,
}))
vi.mock('@/stores/notes', () => ({
  useNotesStore: () => mocks.notesStore,
}))
vi.mock('@/services/api', () => ({
  contactsService: { markRead: mocks.markRead },
}))
vi.mock('vue-sonner', () => ({
  toast: {
    info: mocks.toastInfo,
    warning: mocks.toastWarning,
  },
}))
vi.mock('@/router', () => ({
  default: { push: mocks.routerPush },
}))

type SocketHandler<T = Event> = ((event: T) => void) | null

class FakeWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  static instances: FakeWebSocket[] = []

  readonly url: string
  readyState = FakeWebSocket.CONNECTING
  sent: string[] = []
  onopen: SocketHandler = null
  onmessage: SocketHandler<MessageEvent> = null
  onclose: SocketHandler<CloseEvent> = null
  onerror: SocketHandler = null

  constructor(url: string | URL) {
    this.url = String(url)
    FakeWebSocket.instances.push(this)
  }

  send(data: string) {
    this.sent.push(data)
  }

  open() {
    this.readyState = FakeWebSocket.OPEN
    this.onopen?.(new Event('open'))
  }

  receive(message: unknown) {
    this.onmessage?.(
      new MessageEvent('message', { data: JSON.stringify(message) }),
    )
  }

  receiveRaw(data: string) {
    this.onmessage?.(new MessageEvent('message', { data }))
  }

  serverClose() {
    if (this.readyState === FakeWebSocket.CLOSED) return
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.(new CloseEvent('close'))
  }

  close() {
    this.serverClose()
  }
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

async function loadService() {
  vi.resetModules()
  const module = await import('./websocket')
  return module.wsService
}

describe('WebSocketService realtime reliability', () => {
  let service: Awaited<ReturnType<typeof loadService>> | null = null

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    FakeWebSocket.instances = []
    vi.stubGlobal('WebSocket', FakeWebSocket)
    vi.stubGlobal(
      'Audio',
      class {
        currentTime = 0
        volume = 0
        play = vi.fn().mockResolvedValue(undefined)
      },
    )

    mocks.contactsStore.currentContact = null
    mocks.authStore.user = null
    mocks.authStore.userSettings = {}
  })

  afterEach(() => {
    service?.disconnect()
    service = null
    vi.runOnlyPendingTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('coalesces concurrent connect calls into one socket and authenticates first', async () => {
    service = await loadService()
    const token = deferred<string | null>()
    const getToken = vi.fn(() => token.promise)

    const first = service.connect(getToken)
    const second = service.connect(getToken)
    token.resolve('ws-token')
    await Promise.all([first, second])

    expect(getToken).toHaveBeenCalledTimes(1)
    expect(FakeWebSocket.instances).toHaveLength(1)

    const socket = FakeWebSocket.instances[0]
    socket.open()
    expect(JSON.parse(socket.sent[0])).toEqual({
      type: 'auth',
      payload: { token: 'ws-token' },
    })
    expect(service.getConnectionState()).toBe('connected')
  })

  it('allows a new login generation to connect while an old token request is unresolved', async () => {
    service = await loadService()
    const staleToken = deferred<string | null>()
    const staleAttempt = service.connect(() => staleToken.promise)

    service.disconnect()
    await service.connect(async () => 'new-tenant-token')

    expect(FakeWebSocket.instances).toHaveLength(1)
    const currentSocket = FakeWebSocket.instances[0]
    currentSocket.open()
    expect(JSON.parse(currentSocket.sent[0]).payload.token).toBe(
      'new-tenant-token',
    )

    staleToken.resolve('old-tenant-token')
    await staleAttempt
    expect(FakeWebSocket.instances).toHaveLength(1)
  })

  it('cancels a scheduled reconnect when disconnect is intentional', async () => {
    service = await loadService()
    await service.connect(async () => 'ws-token')
    const socket = FakeWebSocket.instances[0]
    socket.open()
    socket.serverClose()

    expect(service.getConnectionState()).toBe('reconnecting')
    service.disconnect()
    await vi.advanceTimersByTimeAsync(60_000)

    expect(FakeWebSocket.instances).toHaveLength(1)
    expect(service.getConnectionState()).toBe('disconnected')
  })

  it('abandons a WebSocket that remains stuck in CONNECTING', async () => {
    service = await loadService()
    await service.connect(async () => 'ws-token')
    const stuckSocket = FakeWebSocket.instances[0]

    await vi.advanceTimersByTimeAsync(14_999)
    expect(stuckSocket.readyState).toBe(FakeWebSocket.CONNECTING)

    await vi.advanceTimersByTimeAsync(1)
    expect(stuckSocket.readyState).toBe(FakeWebSocket.CLOSED)
    expect(service.getConnectionState()).toBe('reconnecting')

    await vi.advanceTimersByTimeAsync(1_000)
    expect(FakeWebSocket.instances).toHaveLength(2)
  })

  it('reconnects an OPEN socket when application pong frames stop', async () => {
    service = await loadService()
    await service.connect(async () => 'ws-token')
    const stuckSocket = FakeWebSocket.instances[0]
    stuckSocket.open()

    await vi.advanceTimersByTimeAsync(74_999)
    expect(stuckSocket.readyState).toBe(FakeWebSocket.OPEN)

    await vi.advanceTimersByTimeAsync(1)
    expect(stuckSocket.readyState).toBe(FakeWebSocket.CLOSED)
    expect(service.getConnectionState()).toBe('reconnecting')

    await vi.advanceTimersByTimeAsync(1_000)
    expect(FakeWebSocket.instances).toHaveLength(2)
  })

  it('keeps an OPEN socket alive while text pong frames arrive', async () => {
    service = await loadService()
    await service.connect(async () => 'ws-token')
    const socket = FakeWebSocket.instances[0]
    socket.open()

    await vi.advanceTimersByTimeAsync(60_000)
    socket.receiveRaw('pong')
    await vi.advanceTimersByTimeAsync(60_000)
    socket.receive({ type: 'pong' })
    await vi.advanceTimersByTimeAsync(60_000)

    expect(socket.readyState).toBe(FakeWebSocket.OPEN)
    expect(service.getConnectionState()).toBe('connected')
  })

  it.each(['null token', 'token error'] as const)(
    'retries when the initial WebSocket %s occurs and later opens a socket',
    async initialFailure => {
      service = await loadService()
      const getToken = vi.fn(async (): Promise<string | null> => 'retry-token')
      if (initialFailure === 'null token') {
        getToken.mockResolvedValueOnce(null)
      } else {
        getToken.mockRejectedValueOnce(new Error('token endpoint unavailable'))
      }

      await service.connect(getToken)
      expect(FakeWebSocket.instances).toHaveLength(0)
      expect(service.getConnectionState()).toBe('reconnecting')

      await vi.advanceTimersByTimeAsync(1_000)
      expect(getToken).toHaveBeenCalledTimes(2)
      expect(FakeWebSocket.instances).toHaveLength(1)
      const socket = FakeWebSocket.instances[0]
      socket.open()

      expect(JSON.parse(socket.sent[0])).toEqual({
        type: 'auth',
        payload: { token: 'retry-token' },
      })
      expect(service.getConnectionState()).toBe('connected')
    },
  )

  it('continues reconnecting beyond the former five-attempt limit', async () => {
    service = await loadService()
    await service.connect(async () => 'fresh-token')
    FakeWebSocket.instances[0].open()
    FakeWebSocket.instances[0].serverClose()

    for (let attempt = 0; attempt < 7; attempt += 1) {
      await vi.advanceTimersByTimeAsync(30_000)
      const newest = FakeWebSocket.instances.at(-1)
      expect(newest).toBeDefined()
      newest!.serverClose()
    }

    expect(FakeWebSocket.instances.length).toBeGreaterThan(6)
    expect(service.getConnectionState()).toBe('reconnecting')
  })

  it('ignores a frame from a stale socket after logout', async () => {
    service = await loadService()
    await service.connect(async () => 'old-tenant-token')
    const socket = FakeWebSocket.instances[0]
    socket.open()
    const staleHandler = socket.onmessage
    const activity = vi.fn()
    service.onInboxActivity(activity)

    service.disconnect()
    staleHandler?.(
      new MessageEvent('message', {
        data: JSON.stringify({
          type: 'new_message',
          payload: {
            id: 'old-message',
            contact_id: 'old-contact',
            direction: 'incoming',
          },
        }),
      }),
    )

    expect(activity).not.toHaveBeenCalled()
    expect(mocks.contactsStore.addMessage).not.toHaveBeenCalled()
    expect(mocks.contactsStore.fetchContacts).not.toHaveBeenCalled()
    expect(mocks.contactsStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
    expect(mocks.notesStore.clearNotes).toHaveBeenCalledTimes(1)
    expect(mocks.tagsStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
    expect(mocks.transfersStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
    expect(mocks.callingStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
    expect(mocks.usersStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
    expect(mocks.rolesStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
    expect(mocks.teamsStore.resetForIdentityChange).toHaveBeenCalledTimes(1)
  })

  it('catches up the open native chat after reconnecting', async () => {
    mocks.contactsStore.currentContact = { id: 'contact-1' }
    service = await loadService()
    await service.connect(async () => 'ws-token')
    FakeWebSocket.instances[0].open()
    FakeWebSocket.instances[0].serverClose()

    await vi.advanceTimersByTimeAsync(1_000)
    expect(FakeWebSocket.instances).toHaveLength(2)
    FakeWebSocket.instances[1].open()

    expect(mocks.contactsStore.fetchContacts).toHaveBeenCalledTimes(1)
    expect(mocks.contactsStore.refreshCurrentMessages).toHaveBeenCalledTimes(1)
    expect(mocks.transfersStore.fetchTransfers).toHaveBeenCalledTimes(1)
  })

  it('emits inbox activity for message, status, and canonical sync events', async () => {
    service = await loadService()
    await service.connect(async () => 'ws-token')
    const socket = FakeWebSocket.instances[0]
    socket.open()

    const activity = vi.fn()
    const unsubscribe = service.onInboxActivity(activity)
    socket.receive({
      type: 'new_message',
      payload: { id: 'm-1', contact_id: 'c-1', direction: 'incoming' },
    })
    socket.receive({
      type: 'status_update',
      payload: { message_id: 'm-1', status: 'delivered' },
    })
    socket.receive({
      type: 'channel_sync',
      payload: { event_id: 'event-1', organization_id: 'org-1' },
    })
    socket.receive({
      type: 'realtime_sync',
      payload: {
        event_id: 'event-2',
        organization_id: 'org-1',
        kind: 'message_status_changed',
      },
    })

    expect(activity.mock.calls.map(([event]) => event.type)).toEqual([
      'new_message',
      'status_update',
      'channel_sync',
      'realtime_sync',
    ])

    unsubscribe()
    socket.receive({ type: 'channel_sync', payload: { event_id: 'event-3' } })
    expect(activity).toHaveBeenCalledTimes(4)
  })

  it('lets a new identity refresh while an old generation refresh is still pending', async () => {
    service = await loadService()
    const oldTenantRefresh = deferred<void>()
    mocks.contactsStore.fetchContacts
      .mockImplementationOnce(() => oldTenantRefresh.promise)
      .mockResolvedValueOnce(undefined)

    await service.connect(async () => 'old-tenant-token')
    const oldSocket = FakeWebSocket.instances[0]
    oldSocket.open()
    oldSocket.receive({
      type: 'realtime_sync',
      payload: { organization_id: 'organization-a', event_id: 'event-a' },
    })
    await vi.advanceTimersByTimeAsync(150)
    expect(mocks.contactsStore.fetchContacts).toHaveBeenCalledTimes(1)

    service.disconnect()
    await service.connect(async () => 'new-tenant-token')
    const newSocket = FakeWebSocket.instances[1]
    newSocket.open()
    newSocket.receive({
      type: 'realtime_sync',
      payload: { organization_id: 'organization-b', event_id: 'event-b' },
    })
    await vi.advanceTimersByTimeAsync(150)

    expect(mocks.contactsStore.fetchContacts).toHaveBeenCalledTimes(2)
    expect(mocks.contactsStore.resetForIdentityChange).toHaveBeenCalledTimes(1)

    oldTenantRefresh.resolve()
    await Promise.resolve()
    expect(mocks.contactsStore.fetchContacts).toHaveBeenCalledTimes(2)
  })

  it('isolates failing inbox and connection-state observers', async () => {
    service = await loadService()
    const healthyActivity = vi.fn()
    const healthyState = vi.fn()
    service.onInboxActivity(() => {
      throw new Error('broken page callback')
    })
    service.onInboxActivity(healthyActivity)
    service.onConnectionStateChange(() => {
      throw new Error('broken state callback')
    })
    service.onConnectionStateChange(healthyState)

    await service.connect(async () => 'ws-token')
    const socket = FakeWebSocket.instances[0]
    socket.open()
    socket.receive({ type: 'channel_sync', payload: { reason: 'test' } })

    expect(healthyState).toHaveBeenCalledWith('connecting')
    expect(healthyState).toHaveBeenCalledWith('connected')
    expect(healthyActivity).toHaveBeenCalledWith({
      type: 'channel_sync',
      payload: { reason: 'test' },
    })
  })
})
