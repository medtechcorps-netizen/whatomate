/** @vitest-environment happy-dom */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  contactsStore: {
    currentContact: null as { id: string } | null,
    fetchContacts: vi.fn().mockResolvedValue(undefined),
    scheduleContactsRefresh: vi.fn(),
    resetForIdentityChange: vi.fn(),
  },
  resettable: { resetForIdentityChange: vi.fn() },
  transfersStore: {
    fetchTransfers: vi.fn().mockResolvedValue(undefined),
    resetForIdentityChange: vi.fn(),
  },
  authStore: { user: null, userSettings: {}, refreshUserData: vi.fn().mockResolvedValue(true) },
}))

vi.mock('@/stores/contacts', () => ({ useContactsStore: () => mocks.contactsStore }))
vi.mock('@/stores/transfers', () => ({ useTransfersStore: () => mocks.transfersStore }))
vi.mock('@/stores/calling', () => ({ useCallingStore: () => ({ ...mocks.resettable, handleCallEvent: vi.fn() }) }))
vi.mock('@/stores/tags', () => ({ useTagsStore: () => mocks.resettable }))
vi.mock('@/stores/users', () => ({ useUsersStore: () => mocks.resettable }))
vi.mock('@/stores/roles', () => ({ useRolesStore: () => mocks.resettable }))
vi.mock('@/stores/teams', () => ({ useTeamsStore: () => mocks.resettable }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => mocks.authStore }))
vi.mock('@/stores/notes', () => ({ useNotesStore: () => ({ clearNotes: vi.fn() }) }))
vi.mock('@/stores/omnichannelUnread', () => ({ useOmnichannelUnreadStore: () => mocks.resettable }))
vi.mock('@/services/api', () => ({ contactsService: { markRead: vi.fn() } }))
vi.mock('vue-sonner', () => ({ toast: { info: vi.fn(), warning: vi.fn() } }))
vi.mock('@/router', () => ({ default: { push: vi.fn() } }))

class LaneDFakeWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3
  static instances: LaneDFakeWebSocket[] = []
  readyState = LaneDFakeWebSocket.CONNECTING
  onopen: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null

  constructor() {
    LaneDFakeWebSocket.instances.push(this)
  }

  send() {}

  open() {
    this.readyState = LaneDFakeWebSocket.OPEN
    this.onopen?.(new Event('open'))
  }

  receive(message: unknown) {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) }))
  }

  close() {
    this.readyState = LaneDFakeWebSocket.CLOSED
    this.onclose?.(new CloseEvent('close'))
  }
}

describe('WebSocketService contact_update', () => {
  let service: Awaited<typeof import('./websocket')>['wsService'] | null = null

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    LaneDFakeWebSocket.instances = []
    vi.stubGlobal('WebSocket', LaneDFakeWebSocket)
  })

  afterEach(() => {
    service?.disconnect()
    service = null
    vi.runOnlyPendingTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  async function connected() {
    vi.resetModules()
    service = (await import('./websocket')).wsService
    await service.connect(async () => 'ws-token')
    const socket = LaneDFakeWebSocket.instances[0]
    socket.open()
    return socket
  }

  it('schedules a list refresh for the changed contact', async () => {
    const socket = await connected()
    socket.receive({ type: 'contact_update', payload: { contact_id: 'c-1', kind: 'assignment' } })
    expect(mocks.contactsStore.scheduleContactsRefresh).toHaveBeenCalledWith('c-1')
    expect(mocks.contactsStore.fetchContacts).not.toHaveBeenCalled()
  })

  it('ignores a malformed payload', async () => {
    const socket = await connected()
    socket.receive({ type: 'contact_update', payload: {} })
    socket.receive({ type: 'contact_update', payload: { contact_id: 42 } })
    socket.receive({ type: 'contact_update' })
    expect(mocks.contactsStore.scheduleContactsRefresh).not.toHaveBeenCalled()
  })

  it('is not inbox activity, so the navbar unread badge does not refetch on it', async () => {
    const socket = await connected()
    const activity = vi.fn()
    service!.onInboxActivity(activity)
    socket.receive({ type: 'contact_update', payload: { contact_id: 'c-1', kind: 'read_state' } })
    expect(activity).not.toHaveBeenCalled()
  })
})
