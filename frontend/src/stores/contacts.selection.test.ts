import { createPinia, setActivePinia } from 'pinia'
import { nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Contact, Message, ReplyPreview, useContactsStore } from './contacts'

const mocks = vi.hoisted(() => ({
  getContact: vi.fn(),
  listContacts: vi.fn(),
  listMessages: vi.fn(),
  quoteUpdates: vi.fn(),
  sendMessage: vi.fn(),
  sendTemplate: vi.fn(),
  readSelectedOrganizationId: vi.fn((): string | null => null),
  currentTabSession: vi.fn((): string | null => ''),
}))

vi.mock('@/lib/browserIdentity', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/browserIdentity')>()),
  readSelectedOrganizationId: mocks.readSelectedOrganizationId,
}))

vi.mock('@/lib/tabSession', async importOriginal => ({
  ...(await importOriginal<typeof import('@/lib/tabSession')>()),
  currentTabSession: mocks.currentTabSession,
}))

vi.mock('@/services/api', () => ({
  contactsService: {
    get: mocks.getContact,
    list: mocks.listContacts,
  },
  messagesService: {
    list: mocks.listMessages,
    quoteUpdates: mocks.quoteUpdates,
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
    identity_review_ai_state: {
      known: true,
      ai_allowed: true,
      blocked: false,
      open_hold_count: 0,
      reason: 'no_open_identity_review',
    },
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
    mocks.readSelectedOrganizationId.mockReturnValue(null)
    mocks.currentTabSession.mockReturnValue('')
  })

  describe('captured native text send', () => {
    async function scopedStore() {
      mocks.sendMessage.mockReset()
      const { useOrganizationsStore } = await import('./organizations')
      useOrganizationsStore().selectedOrgId = 'organization-a'
      const { useContactsStore } = await import('./contacts')
      const store = useContactsStore()
      store.setCurrentContact(contact('first'))
      store.messagesOrganizationScope = 'organization-a'
      return store
    }
    function accepted(id = 'accepted'): Message {
      return { ...message(id, 'first'), direction: 'outgoing', status: 'pending', whatsapp_account: 'account-a' }
    }

    it('pins the captured workspace through the service option before persistence changes', async () => {
      const store = await scopedStore()
      const response = deferred<{ data: { data: Message } }>()
      mocks.sendMessage.mockReturnValueOnce(response.promise)
      const send = store.sendMessage('first', 'text', { body: 'Private draft' }, 'quote', 'account-a', { organizationId: 'organization-a' })
      mocks.readSelectedOrganizationId.mockReturnValue('organization-b')
      expect(mocks.sendMessage).toHaveBeenCalledWith('first', {
        type: 'text', content: { body: 'Private draft' }, reply_to_message_id: 'quote', whatsapp_account: 'account-a',
      }, { organizationId: 'organization-a' })
      response.resolve({ data: { data: accepted() } })
      await send
      expect(mocks.sendMessage).toHaveBeenCalledTimes(1)
      expect(store.messages.map(item => item.id)).toEqual(['accepted'])
    })

    it.each(['', 'organization-b'])('refuses explicit workspace %s before a request', async organizationId => {
      const store = await scopedStore()
      await expect(store.sendMessage('first', 'text', { body: 'Private draft' }, undefined, undefined, { organizationId })).rejects.toThrow('workspace changed')
      expect(mocks.sendMessage).not.toHaveBeenCalled()
      expect(store.messages).toEqual([])
    })

    it('refuses a stale transcript or another contact before a request', async () => {
      const store = await scopedStore()
      store.messagesOrganizationScope = 'organization-b'
      await expect(store.sendMessage('first', 'text', { body: 'Private draft' }, undefined, undefined, { organizationId: 'organization-a' })).rejects.toThrow('workspace changed')
      store.messagesOrganizationScope = 'organization-a'
      await expect(store.sendMessage('second', 'text', { body: 'Private draft' }, undefined, undefined, { organizationId: 'organization-a' })).rejects.toThrow('workspace changed')
      expect(mocks.sendMessage).not.toHaveBeenCalled()
    })

    it('joins socket first by canonical id without downgrading status or losing the reply preview', async () => {
      const store = await scopedStore()
      const response = deferred<{ data: { data: Message } }>()
      mocks.sendMessage.mockReturnValueOnce(response.promise)
      const send = store.sendMessage('first', 'text', { body: 'Body' }, 'quote', 'account-a', { organizationId: 'organization-a' })
      store.addMessage({ ...accepted(), status: 'delivered' })
      const late = { ...accepted(), reply_to_message_id: 'quote', reply_to_message: { id: 'quote', content: { body: 'Quoted' }, message_type: 'text', direction: 'incoming' as const } }
      response.resolve({ data: { data: late } })
      await send
      expect(store.messages).toHaveLength(1)
      expect(store.messages[0]).toMatchObject({ id: 'accepted', status: 'delivered', reply_to_message_id: 'quote' })
      expect(store.messages[0].reply_to_message?.content.body).toBe('Quoted')
      expect(mocks.sendMessage).toHaveBeenCalledTimes(1)
    })

    it('joins response first with its socket echo without creating a local message', async () => {
      const store = await scopedStore()
      mocks.sendMessage.mockResolvedValueOnce({ data: { data: accepted() } })
      await store.sendMessage('first', 'text', { body: 'Body' }, undefined, 'account-a', { organizationId: 'organization-a' })
      store.addMessage(accepted())
      store.updateMessageStatus('accepted', 'sent')
      expect(store.messages).toHaveLength(1)
      expect(store.messages[0]).toMatchObject({ id: 'accepted', status: 'sent' })
    })

    it.each<[string, Partial<Message>]>([
      ['id', { id: '' }], ['contact', { contact_id: 'second' }], ['account', { whatsapp_account: 'account-b' }],
      ['direction', { direction: 'incoming' }], ['type', { message_type: 'image' }], ['status', { status: 'invented' }],
    ])('rejects a noncanonical %s response without appending or resending', async (_kind, fields) => {
      const store = await scopedStore()
      mocks.sendMessage.mockResolvedValueOnce({ data: { data: { ...accepted(), ...fields } } })
      await expect(store.sendMessage('first', 'text', { body: 'Body' }, undefined, 'account-a', { organizationId: 'organization-a' })).rejects.toThrow('unconfirmed')
      expect(store.messages).toEqual([])
      expect(mocks.sendMessage).toHaveBeenCalledTimes(1)
    })

    it('keeps late acceptance out of a replacement identity transcript', async () => {
      const store = await scopedStore()
      const response = deferred<{ data: { data: Message } }>()
      mocks.sendMessage.mockReturnValueOnce(response.promise)
      const send = store.sendMessage('first', 'text', { body: 'Body' }, undefined, 'account-a', { organizationId: 'organization-a' })
      store.resetForIdentityChange()
      store.setCurrentContact(contact('first'))
      store.messagesOrganizationScope = 'organization-a'
      store.messages = [message('replacement', 'first')]
      response.resolve({ data: { data: accepted() } })
      await send
      expect(store.messages.map(item => item.id)).toEqual(['replacement'])
      expect(mocks.sendMessage).toHaveBeenCalledTimes(1)
    })


    it('keeps captured acceptance out of a transcript whose workspace changed', async () => {
      const store = await scopedStore()
      const response = deferred<{ data: { data: Message } }>()
      mocks.sendMessage.mockReturnValueOnce(response.promise)
      const send = store.sendMessage('first', 'text', { body: 'Body' }, undefined, 'account-a', { organizationId: 'organization-a' })
      store.messagesOrganizationScope = 'organization-b'
      store.messages = [message('replacement', 'first')]
      response.resolve({ data: { data: accepted() } })
      await send
      expect(store.messages.map(item => item.id)).toEqual(['replacement'])
      expect(mocks.sendMessage).toHaveBeenCalledTimes(1)
    })

    it('preserves the account filter when another account acceptance arrives', async () => {
      const store = await scopedStore()
      const response = deferred<{ data: { data: Message } }>()
      mocks.sendMessage.mockReturnValueOnce(response.promise)
      const send = store.sendMessage('first', 'text', { body: 'Body' }, undefined, 'account-a', { organizationId: 'organization-a' })
      store.setAccountFilter('account-b')
      response.resolve({ data: { data: accepted() } })
      await send
      expect(store.messages).toEqual([])
    })
  })

  describe('late quote refresh', () => {
    const organizationId = 'organization-a'
    const contactId = 'quote-chat'
    const account = 'account-a'
    const preview: ReplyPreview = {
      id: 'original',
      content: { body: 'Late original' },
      message_type: 'text',
      direction: 'incoming',
    }

    function user(id: string) {
      const roleId = 'fixture-quote-reader'
      return {
        id, email: `${id}@example.test`, full_name: id, organization_id: organizationId,
        role_id: roleId,
        role: {
          id: roleId, name: 'agent', is_system: false,
          permissions: [{ id: 'chat-read', resource: 'chat', action: 'read' }],
        },
      }
    }

    function pendingQuote(): Message {
      return {
        ...message('older-loaded-reply', contactId),
        whatsapp_account: account,
        quote_pending: true,
        is_reply: false,
        reactions: [{ emoji: '👍' }],
      }
    }

    function socketMessage(id: string, whatsappAccount?: string): Message {
      return {
        ...message(id, contactId),
        wamid: `wamid.synthetic-${id}`,
        whatsapp_account: whatsappAccount,
        ingested_at: '2026-01-01T00:02:00Z',
      }
    }

    function quoteResponse(id: string, reply: ReplyPreview = preview) {
      return {
        data: {
          data: {
            quote_updates: [{
              id,
              reply_to_message_id: preview.id,
              reply_to_message: reply,
              // A quote lookup must never replace unrelated transcript state.
              content: { body: 'Must not be merged' },
              status: 'failed',
              whatsapp_account: 'account-b',
              reactions: [],
            }],
          },
        },
      }
    }

    async function loadTranscript(store: ReturnType<typeof useContactsStore>, rows: Message[], selectedAccount: string | null) {
      store.setCurrentContact(contact(contactId))
      store.setAccountFilter(selectedAccount)
      mocks.listMessages.mockResolvedValueOnce({ data: { data: { messages: rows, has_more: true } } })
      await store.fetchMessages(contactId, { account: selectedAccount ?? undefined })
    }

    async function openTranscript(rows: Message[], selectedAccount: string | null = account) {
      const { useAuthStore } = await import('./auth')
      const { useOrganizationsStore } = await import('./organizations')
      const { useContactsStore } = await import('./contacts')
      const auth = useAuthStore()
      auth.user = user('agent-a')
      useOrganizationsStore().selectedOrgId = organizationId
      mocks.readSelectedOrganizationId.mockReturnValue(organizationId)
      const store = useContactsStore()
      await loadTranscript(store, rows, selectedAccount)
      return { store, auth }
    }

    beforeEach(() => {
      vi.useFakeTimers()
      mocks.quoteUpdates.mockReset()
      mocks.currentTabSession.mockReturnValue('session-a')
    })

    afterEach(() => {
      vi.clearAllTimers()
      vi.useRealTimers()
    })

    it.each(['message and status', 'status only'] as const)(
      'coalesces matching %s events and merges only the older loaded reply fields',
      async events => {
        const older = pendingQuote()
        const queued: Message = {
          ...message('queued-outgoing', contactId),
          direction: 'outgoing',
          status: 'pending',
          whatsapp_account: account,
          ingested_at: '2026-01-01T00:01:00Z',
        }
        const secondQueued = { ...queued, id: 'second-queued-outgoing' }
        const { store } = await openTranscript([older, queued, secondQueued])
        const reply = deferred<ReturnType<typeof quoteResponse>>()
        mocks.quoteUpdates.mockReturnValueOnce(reply.promise)

        if (events === 'message and status') {
          store.addMessage(socketMessage('original', account))
          store.addMessage(socketMessage('another-message', account))
        } else {
          store.updateMessageStatus(secondQueued.id, 'delivered')
        }
        store.updateMessageStatus(queued.id, 'sent')
        expect(mocks.quoteUpdates).not.toHaveBeenCalled()
        await vi.runOnlyPendingTimersAsync()

        expect(mocks.quoteUpdates).toHaveBeenCalledExactlyOnceWith(
          contactId, [older.id], account, organizationId, expect.any(AbortSignal),
        )
        expect(store.messages.find(row => row.id === older.id)).toEqual(older)
        const beforeResponse = JSON.parse(JSON.stringify(store.messages)) as Message[]
        reply.resolve(quoteResponse(older.id))
        await reply.promise
        await nextTick()

        expect(store.messages).toEqual(beforeResponse.map(row => row.id === older.id ? {
          ...row,
          quote_pending: false,
          is_reply: true,
          reply_to_message_id: preview.id,
          reply_to_message: preview,
        } : row))
        expect(store.messages.find(row => row.id === queued.id)?.status).toBe('sent')
        expect(store.hasMoreMessages).toBe(true)
        expect(mocks.listMessages).toHaveBeenCalledTimes(1)
        expect(mocks.sendMessage).not.toHaveBeenCalled()
      },
    )

    it('ignores unrelated account and fieldless events in an all-accounts transcript', async () => {
      const older = pendingQuote()
      const otherOutgoing: Message = {
        ...message('other-outgoing', contactId),
        direction: 'outgoing',
        status: 'pending',
        whatsapp_account: 'account-b',
      }
      const { store } = await openTranscript([older, otherOutgoing], null)

      store.addMessage(socketMessage('other-new', 'account-b'))
      store.addMessage(socketMessage('unknown-account'))
      store.updateMessageStatus(otherOutgoing.id, 'delivered')
      await vi.runOnlyPendingTimersAsync()

      expect(store.messages.map(row => row.id)).toEqual(expect.arrayContaining(['other-new', 'unknown-account']))
      expect(store.messages.find(row => row.id === otherOutgoing.id)?.status).toBe('delivered')
      expect(store.messages.find(row => row.id === older.id)).toEqual(older)
      expect(mocks.quoteUpdates).not.toHaveBeenCalled()
      expect(mocks.listMessages).toHaveBeenCalledTimes(1)
    })

    it('defers new quote hints until the latest canonical load and ignores the superseded lookup', async () => {
      const older = pendingQuote()
      const canonical = { ...older, content: { body: 'Canonical replacement' }, status: 'read' }
      const { store } = await openTranscript([older])
      const superseded = deferred<ReturnType<typeof quoteResponse>>()
      const current = deferred<ReturnType<typeof quoteResponse>>()
      mocks.quoteUpdates.mockReturnValueOnce(superseded.promise).mockReturnValueOnce(current.promise)
      store.addMessage(socketMessage('first-original-event', account))
      await vi.runOnlyPendingTimersAsync()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(1)
      const supersededSignal = mocks.quoteUpdates.mock.calls[0][4] as AbortSignal

      const response = deferred<MessageListResponse>()
      mocks.listMessages.mockReturnValueOnce(response.promise)
      const load = store.fetchMessages(contactId, { account }, { preserveExisting: true })
      expect(supersededSignal.aborted).toBe(true)
      store.addMessage(socketMessage('second-original-event', account))
      await vi.runOnlyPendingTimersAsync()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(1)

      response.resolve({ data: { data: { messages: [canonical], has_more: true } } })
      await load
      await vi.runOnlyPendingTimersAsync()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(2)
      expect(mocks.quoteUpdates).toHaveBeenLastCalledWith(
        contactId, [older.id], account, organizationId, expect.any(AbortSignal),
      )
      const currentSignal = mocks.quoteUpdates.mock.calls[1][4] as AbortSignal

      superseded.resolve(quoteResponse(older.id))
      await superseded.promise
      await nextTick()
      expect(store.messages.find(row => row.id === older.id)).toEqual(canonical)
      expect(currentSignal.aborted).toBe(false)

      current.resolve(quoteResponse(older.id))
      await current.promise
      await nextTick()
      expect(store.messages.find(row => row.id === older.id)).toEqual({
        ...canonical,
        quote_pending: false,
        is_reply: true,
        reply_to_message_id: preview.id,
        reply_to_message: preview,
      })
    })

    it('does not request a pending quote without a current reader owner', async () => {
      const older = pendingQuote()
      const { store, auth } = await openTranscript([older])
      const initial = user('agent-a')
      auth.user = { ...initial, role: { ...initial.role, permissions: [] } }

      store.addMessage(socketMessage('original', account))
      await vi.runOnlyPendingTimersAsync()

      expect(mocks.quoteUpdates).not.toHaveBeenCalled()
      expect(store.messages.find(row => row.id === older.id)).toMatchObject({ quote_pending: true, is_reply: false })
    })

    it.each(['chat', 'contacts'])('drops a pending quote after same-session %s read authority is revoked and restored', async resource => {
      const older = pendingQuote()
      const { store, auth } = await openTranscript([older])
      const initial = user('agent-a')
      const authorized = {
        ...initial,
        role: {
          ...initial.role,
          permissions: [...initial.role.permissions, { id: 'contacts-read', resource: 'contacts', action: 'read' }],
        },
      }
      auth.user = authorized
      const reply = deferred<ReturnType<typeof quoteResponse>>()
      mocks.quoteUpdates.mockReturnValueOnce(reply.promise)
      store.addMessage(socketMessage('original', account))
      await vi.runOnlyPendingTimersAsync()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(1)
      const signal = mocks.quoteUpdates.mock.calls[0][4] as AbortSignal

      auth.user = {
        ...authorized,
        role: { ...authorized.role, permissions: authorized.role.permissions.filter(permission => permission.resource !== resource) },
      }
      expect(signal.aborted).toBe(true)
      auth.user = authorized
      reply.resolve(quoteResponse(older.id))
      await reply.promise
      await nextTick()

      const row = store.messages.find(item => item.id === older.id)
      expect(row).toMatchObject({ quote_pending: true, is_reply: false })
      expect(row?.reply_to_message).toBeUndefined()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(1)
    })

    it('keeps an assigned-contact quote lookup after an auth refresh with unchanged read authority', async () => {
      const older = pendingQuote()
      const { store, auth } = await openTranscript([older])
      const reply = deferred<ReturnType<typeof quoteResponse>>()
      mocks.quoteUpdates.mockReturnValueOnce(reply.promise)
      store.addMessage(socketMessage('original', account))
      await vi.runOnlyPendingTimersAsync()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(1)
      const signal = mocks.quoteUpdates.mock.calls[0][4] as AbortSignal

      auth.user = { ...user('agent-a'), full_name: 'Refreshed profile' }
      expect(signal.aborted).toBe(false)
      const scopedPreview: ReplyPreview = { ...preview, scoped_context: true }
      reply.resolve(quoteResponse(older.id, scopedPreview))
      await reply.promise
      await nextTick()

      const row = store.messages.find(item => item.id === older.id)!
      expect(row).toMatchObject({
        quote_pending: false, is_reply: true, reply_to_message_id: preview.id, reply_to_message: scopedPreview,
      })
      expect(store.ownsReplyContext(row)).toBe(true)
    })

    it.each(['account', 'identity', 'tab session'] as const)('drops an in-flight quote response after a %s change', async owner => {
      const older = pendingQuote()
      const { store, auth } = await openTranscript([older])
      const reply = deferred<ReturnType<typeof quoteResponse>>()
      mocks.quoteUpdates.mockReturnValueOnce(reply.promise)
      store.addMessage(socketMessage('original', account))
      await vi.runOnlyPendingTimersAsync()
      expect(mocks.quoteUpdates).toHaveBeenCalledExactlyOnceWith(
        contactId, [older.id], account, organizationId, expect.any(AbortSignal),
      )
      const signal = mocks.quoteUpdates.mock.calls[0][4] as AbortSignal

      if (owner === 'account') {
        // Return to the same filter before delivery to exercise the generation fence.
        store.setAccountFilter('account-b')
        store.setAccountFilter(account)
        expect(signal.aborted).toBe(true)
      } else if (owner === 'identity') {
        store.resetForIdentityChange()
        auth.user = user('agent-b')
        await loadTranscript(store, [{ ...older, content: { body: 'Replacement transcript' } }], account)
        expect(signal.aborted).toBe(true)
      } else {
        // A tab session can change before the reactive transcript is reset.
        mocks.currentTabSession.mockReturnValue('session-b')
        expect(signal.aborted).toBe(false)
      }
      const beforeResponse = JSON.parse(JSON.stringify(store.messages)) as Message[]
      reply.resolve(quoteResponse(older.id))
      await reply.promise
      await nextTick()

      expect(store.messages).toEqual(beforeResponse)
      expect(store.messages.find(row => row.id === older.id)).toMatchObject({ quote_pending: true, is_reply: false })
      expect(store.messages.find(row => row.id === older.id)?.reply_to_message).toBeUndefined()
      expect(mocks.quoteUpdates).toHaveBeenCalledTimes(1)
    })
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

  it('orders canonical merges by ingestion time and then message id', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const activeContact = contact('arrival-order')
    const existing = {
      ...message('message-b', activeContact.id),
      created_at: '2026-08-24T12:00:00Z',
      ingested_at: '2026-08-24T12:01:00Z',
    }
    const delayedHigherID = {
      ...message('message-z', activeContact.id),
      created_at: '2026-08-20T09:00:00Z',
      ingested_at: '2026-08-24T12:02:00.000100Z',
    }
    const delayedLowerID = {
      ...message('message-a', activeContact.id),
      created_at: '2026-08-19T09:00:00Z',
      ingested_at: '2026-08-24T12:02:00.000900Z',
    }
    mocks.listMessages.mockResolvedValue({
      data: {
        data: {
          messages: [delayedHigherID, delayedLowerID],
          has_more: false,
        },
      },
    })

    store.setCurrentContact(activeContact)
    store.messages = [existing]
    await store.refreshCurrentMessages()

    expect(store.messages.map(item => item.id)).toEqual([
      'message-b',
      'message-z',
      'message-a',
    ])
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

  describe('transcript workspace for chat media', () => {
    function messagesResponse(messages: Message[]): MessageListResponse {
      return { data: { data: { messages, has_more: false } } }
    }

    it('requires a fresh authorized transcript read after quote ownership retires', async () => {
      const { useAuthStore } = await import('./auth')
      const { useOrganizationsStore } = await import('./organizations')
      const { useContactsStore } = await import('./contacts')
      const organizationId = 'organization-a'
      const account = 'account-a'
      const chatRead = { id: 'chat-read', resource: 'chat', action: 'read' }
      const contactsRead = { id: 'contacts-read', resource: 'contacts', action: 'read' }
      const fullRole = { id: 'role-a', name: 'Reader', is_system: false, permissions: [chatRead, contactsRead] }
      const user = {
        id: 'agent-a',
        email: 'agent-a@example.test',
        full_name: 'Agent A',
        organization_id: organizationId,
        role: fullRole,
      }
      const auth = useAuthStore()
      auth.user = user
      useOrganizationsStore().selectedOrgId = organizationId
      mocks.readSelectedOrganizationId.mockReturnValue(organizationId)
      mocks.currentTabSession.mockReturnValue('session-a')

      const store = useContactsStore()
      store.setCurrentContact(contact('first'))
      store.setAccountFilter(account)
      const storedReply = (): Message => ({
        ...message('child', 'first'),
        whatsapp_account: account,
        is_reply: true,
        reply_to_message_id: 'parent',
        reply_to_message: {
          id: 'parent',
          content: { body: 'Original image caption' },
          message_type: 'image',
          direction: 'outgoing',
          scoped_context: true,
          image_revision: 'a'.repeat(64),
        },
      })
      async function readReply() {
        const protectedReply = storedReply()
        const ordinaryReply = {
          ...protectedReply,
          id: 'ordinary-child',
          reply_to_message: { ...protectedReply.reply_to_message!, scoped_context: undefined },
        }
        mocks.listMessages.mockResolvedValueOnce(messagesResponse([protectedReply, ordinaryReply]))
        await store.fetchMessages('first', { account })
        const row = store.messages[0]!
        expect(store.ownsReplyContext(row)).toBe(true)
        expect(store.ownsReplyContext(store.messages[1]!)).toBe(false)
        return row
      }

      const first = await readReply()
      expect(mocks.listMessages).toHaveBeenLastCalledWith(
        'first', { account }, expect.any(AbortSignal),
        expect.objectContaining({ organizationId, isCurrent: expect.any(Function) }),
      )
      // Even with identical ID and nested quote data, a replacement row
      // introduced outside the authenticated read cannot inherit the proof.
      store.messages = [{ ...first }]
      expect(store.ownsReplyContext(store.messages[0]!)).toBe(false)

      const second = await readReply()
      auth.user = { ...user, role: { ...fullRole, id: 'role-b', permissions: [chatRead] } }
      expect(auth.hasPermission('chat', 'read')).toBe(true)
      expect(auth.hasPermission('contacts', 'read')).toBe(false)
      expect(store.ownsReplyContext(second)).toBe(false)

      auth.user = user
      expect(auth.hasPermission('contacts', 'read')).toBe(true)
      expect(store.ownsReplyContext(second)).toBe(false)
      const latest = await readReply()
      expect(latest).not.toBe(second)
      expect(store.ownsReplyContext(second)).toBe(false)
    })

    it('records the workspace the transcript request is sent to when the load starts', async () => {
      const { useAuthStore } = await import('./auth')
      useAuthStore().user = { organization_id: 'organization-session' } as never
      const { useContactsStore } = await import('./contacts')
      const store = useContactsStore()
      const response = deferred<MessageListResponse>()
      mocks.listMessages.mockReturnValueOnce(response.promise)

      // The API client sends the persisted selection as X-Organization-ID.
      mocks.readSelectedOrganizationId.mockReturnValue('organization-persisted')
      store.setCurrentContact(contact('first'))
      const load = store.fetchMessages('first')
      expect(store.messagesOrganizationScope).toBe('organization-persisted')

      response.resolve(messagesResponse([message('first-message', 'first')]))
      await load
      expect(store.messagesOrganizationScope).toBe('organization-persisted')

      // Without a persisted selection the server uses the session workspace.
      mocks.readSelectedOrganizationId.mockReturnValue(null)
      mocks.listMessages.mockResolvedValueOnce(messagesResponse([]))
      await store.fetchMessages('first')
      expect(store.messagesOrganizationScope).toBe('organization-session')
    })

    it('keeps the transcript workspace through a realtime refresh', async () => {
      const { useContactsStore } = await import('./contacts')
      const store = useContactsStore()
      mocks.readSelectedOrganizationId.mockReturnValue('organization-a')
      mocks.listMessages.mockResolvedValue(messagesResponse([message('first-message', 'first')]))
      store.setCurrentContact(contact('first'))
      await store.fetchMessages('first')

      // Another tab changed the persisted selection; the merged transcript
      // still belongs to the workspace it was loaded from.
      mocks.readSelectedOrganizationId.mockReturnValue('organization-b')
      await store.refreshCurrentMessages()
      expect(store.messagesOrganizationScope).toBe('organization-a')
    })

    it('drops the transcript workspace together with the transcript', async () => {
      const { useOrganizationsStore } = await import('./organizations')
      const organizationsStore = useOrganizationsStore()
      organizationsStore.selectedOrgId = 'organization-a'
      const { useContactsStore } = await import('./contacts')
      const store = useContactsStore()
      mocks.readSelectedOrganizationId.mockReturnValue('organization-a')
      mocks.listMessages.mockResolvedValue(messagesResponse([message('first-message', 'first')]))
      store.setCurrentContact(contact('first'))
      await store.fetchMessages('first')
      expect(store.messagesOrganizationScope).toBe('organization-a')

      // A workspace switch clears the transcript rather than leaving its media
      // to be refetched under the new workspace.
      organizationsStore.selectedOrgId = 'organization-b'
      await nextTick()
      expect(store.messages).toEqual([])
      expect(store.messagesOrganizationScope).toBe('')

      await store.fetchMessages('first')
      store.clearMessages()
      expect(store.messagesOrganizationScope).toBe('')
    })
  })

  it('polls a newly arriving hold into the selected contact without changing selection', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const selected = contact('selected')
    store.contacts = [selected]
    store.setCurrentContact(selected)

    const blocked = {
      ...contact('selected'),
      identity_review_ai_state: {
        known: true,
        ai_allowed: false,
        blocked: true,
        open_hold_count: 1,
        reason: 'identity_review_open',
        latest_hold_id: 'hold-new',
        latest_generation: 2,
      },
    }
    mocks.listContacts.mockResolvedValue({
      data: { data: { contacts: [blocked], total: 1 } },
    })

    await store.fetchContacts()

    expect(store.currentContact?.id).toBe('selected')
    expect(store.currentContact?.identity_review_ai_state).toEqual(blocked.identity_review_ai_state)
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

  it('does not append a message from a different WhatsApp account filter', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const activeContact = contact('multi-account')
    store.contacts = [activeContact]
    store.setCurrentContact(activeContact)
    store.setAccountFilter('account-a')
    store.messages = [{
      ...message('account-a-message', activeContact.id),
      whatsapp_account: 'account-a',
    }]

    store.addMessage({
      ...message('account-b-message', activeContact.id),
      whatsapp_account: 'account-b',
    })

    expect(store.messages.map(item => item.id)).toEqual(['account-a-message'])
    expect(store.contacts[0].unread_count).toBe(1)
  })

  it('does not append a fieldless rolling-deployment event to an account-filtered transcript', async () => {
    const { useContactsStore } = await import('./contacts')
    const store = useContactsStore()
    const activeContact = contact('mixed-version')
    store.contacts = [activeContact]
    store.setCurrentContact(activeContact)
    store.setAccountFilter('account-a')
    store.messages = [{
      ...message('account-a-message', activeContact.id),
      whatsapp_account: 'account-a',
    }]

    store.addMessage(message('fieldless-message', activeContact.id))

    expect(store.messages.map(item => item.id)).toEqual(['account-a-message'])
    expect(store.contacts[0].unread_count).toBe(1)
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
