import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Contact } from './contacts'

const mocks = vi.hoisted(() => ({
  getContact: vi.fn(),
  listContacts: vi.fn(),
  markManualReadState: vi.fn(),
  readSelectedOrganizationId: vi.fn((): string | null => null),
}))

vi.mock('@/lib/browserIdentity', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/browserIdentity')>()),
  readSelectedOrganizationId: mocks.readSelectedOrganizationId,
}))

vi.mock('@/services/api', () => ({
  contactsService: {
    get: mocks.getContact,
    list: mocks.listContacts,
    markManualReadState: mocks.markManualReadState,
  },
  messagesService: {
    list: vi.fn(),
    send: vi.fn(),
    sendTemplate: vi.fn(),
  },
}))

function laneDContact(id: string, overrides: Partial<Contact> = {}): Contact {
  return {
    id,
    phone_number: `phone-${id}`,
    name: `Contact ${id}`,
    status: 'active',
    tags: [],
    metadata: {},
    unread_count: 0,
    last_message_at: '2026-01-01T00:00:00Z',
    identity_review_ai_state: {
      known: true,
      ai_allowed: true,
      blocked: false,
      open_hold_count: 0,
      reason: 'no_open_identity_review',
    },
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function listResponse(contacts: Contact[], total = contacts.length) {
  return { data: { data: { contacts, total } } }
}

async function loadStore() {
  const { useContactsStore } = await import('./contacts')
  return useContactsStore()
}

describe('contacts store inbox filters and read state', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
    mocks.readSelectedOrganizationId.mockReturnValue(null)
    mocks.listContacts.mockResolvedValue(listResponse([]))
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('keeps the default list request free of filter parameters', async () => {
    const store = await loadStore()
    expect(store.listFilter).toBe('all')
    await store.fetchContacts()
    const params = mocks.listContacts.mock.calls[0][0]
    expect(params).not.toHaveProperty('unread')
    expect(params).not.toHaveProperty('assigned')
  })

  it.each([
    ['unread', { unread: 'true' }],
    ['mine', { assigned: 'me' }],
    ['unassigned', { assigned: 'unassigned' }],
  ] as const)('sends the %s filter on every refresh and page load', async (filter, expected) => {
    const store = await loadStore()
    mocks.listContacts.mockResolvedValue(listResponse([laneDContact('a')], 2))

    store.setListFilter(filter)
    await vi.waitFor(() => expect(mocks.listContacts).toHaveBeenCalledTimes(1))
    expect(mocks.listContacts.mock.calls[0][0]).toMatchObject({ page: 1, ...expected })

    // Realtime, catch-up and search refreshes call fetchContacts without the
    // filter; it must still apply.
    await store.fetchContacts({ search: 'ali' })
    expect(mocks.listContacts.mock.calls[1][0]).toMatchObject({ search: 'ali', ...expected })

    mocks.listContacts.mockResolvedValue(listResponse([laneDContact('b')], 2))
    await store.loadMoreContacts()
    expect(mocks.listContacts.mock.calls[2][0]).toMatchObject({ page: 2, ...expected })
  })

  it('does not refetch when the same filter is chosen again', async () => {
    const store = await loadStore()
    store.setListFilter('all')
    expect(mocks.listContacts).not.toHaveBeenCalled()
  })

  it('keeps the open chat listed once after it drops out of a filter', async () => {
    const store = await loadStore()
    const open = laneDContact('open', { last_message_at: '2026-01-02T00:00:00Z' })
    const other = laneDContact('other')
    store.setCurrentContact(open)

    mocks.listContacts.mockResolvedValue(listResponse([other]))
    store.setListFilter('unread')
    await vi.waitFor(() => expect(store.contacts.map(c => c.id)).toEqual(['other']))
    expect(store.sortedContacts.map(c => c.id)).toEqual(['open', 'other'])
    expect(store.contactsTotal).toBe(1)

    mocks.listContacts.mockResolvedValue(listResponse([open, other]))
    await store.fetchContacts()
    expect(store.sortedContacts.map(c => c.id)).toEqual(['open', 'other'])

    // The default list is the server's list exactly.
    mocks.listContacts.mockResolvedValue(listResponse([other]))
    store.setListFilter('all')
    await vi.waitFor(() => expect(mocks.listContacts).toHaveBeenCalledTimes(3))
    await vi.waitFor(() => expect(store.sortedContacts.map(c => c.id)).toEqual(['other']))

    // A search shows search results only.
    store.setListFilter('unread')
    await vi.waitFor(() => expect(mocks.listContacts).toHaveBeenCalledTimes(4))
    store.searchQuery = 'zzz'
    expect(store.sortedContacts.map(c => c.id)).toEqual(['other'])
  })

  it('resets the filter on an identity change', async () => {
    const store = await loadStore()
    store.setListFilter('mine')
    store.resetForIdentityChange()
    expect(store.listFilter).toBe('all')
  })

  it('applies manual read state locally and refetches the list', async () => {
    const store = await loadStore()
    const row = laneDContact('row', { unread_count: 3 })
    mocks.listContacts.mockResolvedValue(listResponse([row]))
    await store.fetchContacts()
    mocks.listContacts.mockClear()

    mocks.markManualReadState.mockResolvedValue({
      data: { data: { status: 'ok', cursor_synced: true, manual_unread: false } },
    })
    await store.markContactReadState('row', 'read')
    expect(mocks.markManualReadState).toHaveBeenCalledWith('row', 'read')
    expect(store.contacts[0].unread_count).toBe(0)
    expect(store.contacts[0].manual_unread).toBe(false)
    expect(mocks.listContacts).toHaveBeenCalledTimes(1)

    mocks.markManualReadState.mockResolvedValue({
      data: { data: { status: 'ok', cursor_synced: false, manual_unread: true } },
    })
    await store.markContactReadState('row', 'unread')
    expect(mocks.markManualReadState).toHaveBeenLastCalledWith('row', 'unread')
    expect(store.contacts[0].manual_unread).toBe(true)
  })

  it('propagates a failed manual read-state request', async () => {
    const store = await loadStore()
    mocks.markManualReadState.mockRejectedValue(new Error('forbidden'))
    await expect(store.markContactReadState('row', 'unread')).rejects.toThrow('forbidden')
  })

  it('coalesces contact updates and re-reads a pinned open chat that changed', async () => {
    vi.useFakeTimers()
    const store = await loadStore()
    const open = laneDContact('open')
    store.setCurrentContact(open)
    mocks.listContacts.mockResolvedValue(listResponse([laneDContact('other')]))
    store.setListFilter('mine')
    await vi.runAllTimersAsync()
    mocks.listContacts.mockClear()
    mocks.getContact.mockResolvedValue({
      data: {
        data: laneDContact('open', {
          assigned_user_id: 'u-1',
          assigned_user_name: 'Aisyah Rahman',
          manual_unread: true,
        }),
      },
    })

    store.scheduleContactsRefresh('open')
    store.scheduleContactsRefresh('other')
    store.scheduleContactsRefresh()
    expect(mocks.listContacts).not.toHaveBeenCalled()
    await vi.runAllTimersAsync()

    expect(mocks.listContacts).toHaveBeenCalledTimes(1)
    expect(mocks.getContact).toHaveBeenCalledWith('open')
    expect(store.currentContact?.id).toBe('open')
    expect(store.currentContact?.assigned_user_name).toBe('Aisyah Rahman')
    expect(store.currentContact?.manual_unread).toBe(true)

    // An unrelated update refreshes the list but not the open chat.
    mocks.getContact.mockClear()
    store.scheduleContactsRefresh('other')
    await vi.runAllTimersAsync()
    expect(mocks.listContacts).toHaveBeenCalledTimes(2)
    expect(mocks.getContact).not.toHaveBeenCalled()
  })
})
