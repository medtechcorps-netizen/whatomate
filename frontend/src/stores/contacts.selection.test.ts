import { createPinia, setActivePinia } from 'pinia'
import { nextTick } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Contact, Message } from './contacts'

const mocks = vi.hoisted(() => ({
  getContact: vi.fn(),
  listContacts: vi.fn(),
  listMessages: vi.fn(),
  sendMessage: vi.fn(),
  sendTemplate: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  contactsService: {
    get: mocks.getContact,
    list: mocks.listContacts,
  },
  messagesService: {
    list: mocks.listMessages,
    send: mocks.sendMessage,
    sendTemplate: mocks.sendTemplate,
  },
}))

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

interface MessageListResponse {
  data: {
    data: {
      messages: Message[]
      has_more: boolean
    }
  }
}

interface ContactListResponse {
  data: {
    data: {
      contacts: Contact[]
      total: number
    }
  }
}

function contact(id: string): Contact {
  return {
    id,
    phone_number: `phone-${id}`,
    name: `Contact ${id}`,
    status: 'active',
    tags: [],
    metadata: {},
    unread_count: 0,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  }
}

function message(id: string, contactId: string): Message {
  return {
    id,
    contact_id: contactId,
    direction: 'incoming' as const,
    message_type: 'text',
    content: { body: id },
    status: 'delivered',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  }
}

describe('contacts store conversation selection', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.clearAllMocks()
  })

  it('does not select a contact merely because its deep-link lookup finishes', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const selected = contact('selected')
    const lookedUp = contact('looked-up')
    store.setCurrentContact(selected)
    mocks.getContact.mockResolvedValue({ data: { data: lookedUp } })

    await expect(store.fetchContact(lookedUp.id)).resolves.toEqual(lookedUp)
    expect(store.currentContact?.id).toBe(selected.id)
  })

  it('ignores an older contact message response after a newer selection wins', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const firstResponse = deferred<MessageListResponse>()
    const secondResponse = deferred<MessageListResponse>()
    mocks.listMessages.mockImplementation((contactId: string) => {
      return contactId === 'first' ? firstResponse.promise : secondResponse.promise
    })

    store.setCurrentContact(contact('first'))
    const firstLoad = store.fetchMessages('first')
    store.setCurrentContact(contact('second'))
    const secondLoad = store.fetchMessages('second')

    secondResponse.resolve({
      data: { data: { messages: [message('second-message', 'second')], has_more: false } },
    })
    await secondLoad
    firstResponse.resolve({
      data: { data: { messages: [message('first-message', 'first')], has_more: true } },
    })
    await firstLoad

    expect(store.currentContact?.id).toBe('second')
    expect(store.messages.map(item => item.id)).toEqual(['second-message'])
    expect(store.hasMoreMessages).toBe(false)
    expect(store.isLoadingMessages).toBe(false)
  })

  it('refreshes the active chat atomically without clearing visible history', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const activeContact = contact('active')
    const olderLoadedMessage = {
      ...message('older-loaded-message', activeContact.id),
      created_at: '2026-01-01T00:00:00Z',
    }
    const currentMessage = {
      ...message('current-message', activeContact.id),
      status: 'sent',
      created_at: '2026-01-01T00:01:00Z',
    }
    const currentMessageUpdated = {
      ...currentMessage,
      status: 'read',
      updated_at: '2026-01-01T00:03:00Z',
    }
    const incomingMessage = message('incoming-message', activeContact.id)
    incomingMessage.created_at = '2026-01-01T00:02:00Z'
    const response = deferred<MessageListResponse>()
    mocks.listMessages.mockReturnValue(response.promise)

    store.setCurrentContact(activeContact)
    store.messages = [olderLoadedMessage, currentMessage]
    const refresh = store.refreshCurrentMessages()

    expect(store.messages.map(item => item.id)).toEqual([
      'older-loaded-message',
      'current-message',
    ])
    expect(store.isLoadingMessages).toBe(false)

    response.resolve({
      data: {
        data: {
          messages: [currentMessageUpdated, incomingMessage],
          has_more: false,
        },
      },
    })
    await refresh

    expect(store.messages.map(item => item.id)).toEqual([
      'older-loaded-message',
      'current-message',
      'incoming-message',
    ])
    expect(store.messages.find(item => item.id === 'current-message')?.status).toBe('read')
    expect(store.isLoadingMessages).toBe(false)
  })

  it('aborts and ignores an old tenant contact refresh after identity reset', async () => {
    const { useOrganizationsStore } = await import('./organizations')
    const organizationsStore = useOrganizationsStore()
    organizationsStore.selectedOrgId = 'organization-a'

    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const oldTenantResponse = deferred<ContactListResponse>()
    const newTenantResponse = deferred<ContactListResponse>()
    let oldTenantSignal: AbortSignal | undefined
    mocks.listContacts
      .mockImplementationOnce((_params: unknown, signal?: AbortSignal) => {
        oldTenantSignal = signal
        return oldTenantResponse.promise
      })
      .mockImplementationOnce(() => newTenantResponse.promise)

    const oldTenantLoad = store.fetchContacts()
    store.resetForIdentityChange()
    organizationsStore.selectedOrgId = 'organization-b'
    await nextTick()
    const newTenantLoad = store.fetchContacts()

    newTenantResponse.resolve({
      data: { data: { contacts: [contact('organization-b-contact')], total: 1 } },
    })
    await newTenantLoad
    oldTenantResponse.resolve({
      data: { data: { contacts: [contact('organization-a-contact')], total: 1 } },
    })
    await oldTenantLoad

    expect(oldTenantSignal?.aborted).toBe(true)
    expect(store.contacts.map(item => item.id)).toEqual(['organization-b-contact'])
    expect(store.contactsTotal).toBe(1)
    expect(store.isLoading).toBe(false)
  })

  it('preserves the normalized active search when callers use the default contact refresh', async () => {
    vi.useFakeTimers()
    try {
      const { useContactsStore } = await import('./contacts')
      const store = useContactsStore()
      mocks.listContacts.mockResolvedValue({
        data: { data: { contacts: [], total: 0 } },
      })
      store.searchQuery = '  +60 (12) 345-6789  '
      await nextTick()

      await store.fetchContacts()

      expect(mocks.listContacts).toHaveBeenCalledWith(
        expect.objectContaining({ search: '60123456789' }),
        expect.any(AbortSignal),
      )
      store.resetForIdentityChange()
    } finally {
      vi.useRealTimers()
    }
  })

  it('clears private search state and cancels both its debounce and active response on identity reset', async () => {
    vi.useFakeTimers()
    try {
      const { useContactsStore } = await import('./contacts')
      const store = useContactsStore()
      const activeResponse = deferred<ContactListResponse>()
      let activeSignal: AbortSignal | undefined
      mocks.listContacts.mockImplementationOnce((_params: unknown, signal?: AbortSignal) => {
        activeSignal = signal
        return activeResponse.promise
      })

      store.searchQuery = 'old patient phone'
      await nextTick()
      await vi.advanceTimersByTimeAsync(300)
      expect(mocks.listContacts).toHaveBeenCalledTimes(1)

      store.searchQuery = 'second pending secret'
      await nextTick()
      store.resetForIdentityChange()
      await nextTick()
      await vi.advanceTimersByTimeAsync(500)

      expect(store.searchQuery).toBe('')
      expect(activeSignal?.aborted).toBe(true)
      expect(mocks.listContacts).toHaveBeenCalledTimes(1)

      activeResponse.resolve({
        data: {
          data: {
            contacts: [contact('old-search-contact')],
            total: 1,
          },
        },
      })
      await Promise.resolve()
      await nextTick()
      expect(store.contacts).toEqual([])
      expect(store.contactsTotal).toBe(0)
      expect(store.isLoading).toBe(false)
    } finally {
      vi.useRealTimers()
    }
  })

  it('never appends a message for a contact other than the open transcript', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const first = contact('first')
    const second = contact('second')
    store.contacts = [first, second]
    store.setCurrentContact(second)
    store.messages = [message('second-message', second.id)]

    store.addMessage(message('first-message', first.id))

    expect(store.messages.map(item => item.id)).toEqual(['second-message'])
    expect(store.contacts.find(item => item.id === first.id)?.last_message_at).toBe(
      '2026-01-01T00:00:00Z',
    )
  })

  it('does not append a late send response after contact A changes to B', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const response = deferred<{ data: { data: Message } }>()
    mocks.sendMessage.mockReturnValue(response.promise)
    store.setCurrentContact(contact('first'))

    const send = store.sendMessage('first', 'text', { body: 'private A draft' })
    store.setCurrentContact(contact('second'))
    store.messages = [message('second-message', 'second')]
    response.resolve({ data: { data: message('first-outgoing', 'first') } })
    await send

    expect(store.currentContact?.id).toBe('second')
    expect(store.messages.map(item => item.id)).toEqual(['second-message'])
  })

  it('does not append a late template response after logout identity reset', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const response = deferred<{ data: { data: Message } }>()
    mocks.sendTemplate.mockReturnValue(response.promise)
    store.setCurrentContact(contact('first'))

    const send = store.sendTemplate('first', 'appointment_reminder')
    store.resetForIdentityChange()
    response.resolve({ data: { data: message('old-template', 'first') } })
    await send

    expect(store.currentContact).toBeNull()
    expect(store.messages).toEqual([])
  })
})
