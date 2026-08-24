/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { nextTick, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ChannelsView from './ChannelsView.vue'
import {
  clearLegacyWhatsAppReplyAttemptNamespace,
  LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX,
} from '@/lib/legacyWhatsAppReplyAttempts'

const mocks = vi.hoisted(() => ({
  accounts: vi.fn(),
  conversations: vi.fn(),
  messages: vi.fn(),
  markRead: vi.fn(),
  refreshUnread: vi.fn(),
  sendChannelMessage: vi.fn(),
  sendLegacyWhatsAppReply: vi.fn(),
  hasPermission: vi.fn(),
  hasProductEntitlement: vi.fn(),
  connectWebSocket: vi.fn(),
  onInboxActivity: vi.fn(),
  onConnectionStateChange: vi.fn(),
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  toastError: vi.fn(),
  blockOrganizationSwitch: vi.fn(),
  unblockOrganizationSwitch: vi.fn(),
  organizationStore: {
    selectedOrgId: 'organization-1',
  },
}))

vi.mock('vue-router', () => ({
  onBeforeRouteLeave: vi.fn(),
}))

vi.mock('@vueuse/core', () => ({
  useMediaQuery: () => ref(false),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    organizationId: 'organization-1',
    user: { id: 'agent-1' },
    hasPermission: mocks.hasPermission,
    hasProductEntitlement: mocks.hasProductEntitlement,
  }),
}))

vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => ({
    get selectedOrgId() {
      return mocks.organizationStore.selectedOrgId
    },
    blockOrganizationSwitch: mocks.blockOrganizationSwitch,
    unblockOrganizationSwitch: mocks.unblockOrganizationSwitch,
  }),
}))

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => ({
    refresh: mocks.refreshUnread,
  }),
}))

vi.mock('@/composables/useAppToast', () => ({
  useAppToast: () => ({
    success: mocks.toastSuccess,
    warning: mocks.toastWarning,
    error: mocks.toastError,
  }),
}))

vi.mock('@/services/productSuite', () => ({
  channelsService: {
    accounts: mocks.accounts,
    conversations: mocks.conversations,
    messages: mocks.messages,
    markRead: mocks.markRead,
    send: mocks.sendChannelMessage,
    replyLegacyWhatsApp: mocks.sendLegacyWhatsAppReply,
  },
}))

vi.mock('@/services/metaMessengerOnboarding', () => {
  class MetaMessengerAuthorizationCancelledError extends Error {}
  class MetaMessengerOrganizationChangedError extends Error {}
  return {
    MetaMessengerAuthorizationCancelledError,
    MetaMessengerOrganizationChangedError,
    metaMessengerOnboarding: {
      status: vi.fn().mockResolvedValue(false),
    },
  }
})

vi.mock('@/services/metaInstagramOnboarding', () => {
  class MetaInstagramOffPilotError extends Error {}
  class MetaInstagramOrganizationChangedError extends Error {}
  return {
    MetaInstagramOffPilotError,
    MetaInstagramOrganizationChangedError,
    metaInstagramOAuthAvailable: () => false,
    metaInstagramReconciliationAvailable: () => false,
    metaInstagramStaticCreationAvailable: () => false,
    metaInstagramTeardownAvailable: () => false,
    metaInstagramOnboarding: {
      status: vi.fn().mockResolvedValue({ configured: false }),
    },
  }
})

vi.mock('@/services/websocket', () => ({
  isInboxContentRefreshActivity: (event: { type: string; payload?: any }) =>
    event.type !== 'status_update'
    && !(event.type === 'realtime_sync' && event.payload?.kind === 'message_status_changed'),
  wsService: {
    getConnectionState: () => 'connected',
    connect: mocks.connectWebSocket,
    onInboxActivity: mocks.onInboxActivity,
    onConnectionStateChange: mocks.onConnectionStateChange,
  },
}))

interface InboxOptions {
  selectedProvider?: string
  legacyReplyEndpoint?: boolean
  serviceWindowEndsAt?: string
  includeOtherLegacyAccount?: boolean
  unreadCount?: number
}

let wrapper: VueWrapper | null = null
let inboxActivityHandler: ((event?: unknown) => void) | null = null

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function storedLegacyReplyAttempts() {
  const attempts: Array<{ key: string; value: string }> = []
  for (let index = 0; index < sessionStorage.length; index += 1) {
    const key = sessionStorage.key(index)
    if (key?.startsWith(LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX)) {
      attempts.push({ key, value: sessionStorage.getItem(key) ?? '' })
    }
  }
  return attempts
}

function sessionStorageWith(overrides: Partial<Storage>): Storage {
  const actual = window.sessionStorage
  return {
    get length() {
      return actual.length
    },
    clear: () => actual.clear(),
    getItem: key => actual.getItem(key),
    key: index => actual.key(index),
    removeItem: key => actual.removeItem(key),
    setItem: (key, value) => actual.setItem(key, value),
    ...overrides,
  }
}

function setInbox(options: InboxOptions = {}) {
  const selectedAccount = {
    id: 'channel-account-selected',
    channel: 'whatsapp',
    provider: options.selectedProvider ?? 'meta_legacy',
    name: 'Selected WhatsApp',
    status: 'active',
    capabilities: {
      legacy_text_reply_endpoint: options.legacyReplyEndpoint ?? true,
      text: true,
      replies: true,
      service_window: true,
    },
    config: {
      reply_route: 'chat',
      legacy_read_only: true,
      outbound_enabled: false,
    },
    has_credentials: true,
    outbox_pending: 0,
    outbox_failed: 0,
  }
  const accounts = [selectedAccount]
  if (options.includeOtherLegacyAccount) {
    accounts.push({
      ...selectedAccount,
      id: 'channel-account-other',
      name: 'Other WhatsApp',
      provider: 'meta_legacy',
      capabilities: {
        legacy_text_reply_endpoint: true,
        text: true,
        replies: true,
        service_window: true,
      },
      config: {
        reply_route: 'chat',
        legacy_read_only: true,
        outbound_enabled: false,
      },
    })
  }
  const conversation = {
    id: 'conversation-1',
    channel_account_id: selectedAccount.id,
    contact_id: 'contact-1',
    channel: 'whatsapp',
    external_conversation_id: 'external-conversation-1',
    status: 'open',
    service_window_ends_at:
      options.serviceWindowEndsAt ??
      '2099-01-01T00:00:00.000Z',
    unread_count: options.unreadCount ?? 0,
    ai_paused: false,
    contact: {
      id: 'contact-1',
      profile_name: 'Customer One',
      phone_number: '+60111111111',
    },
  }

  mocks.accounts.mockResolvedValue({
    data: { data: { accounts } },
  })
  mocks.conversations.mockResolvedValue({
    data: { data: { conversations: [conversation], total: 1 } },
  })
  mocks.messages.mockResolvedValue({
    data: { data: { messages: [], total: 0 } },
  })
  return { selectedAccount, conversation }
}

async function mountAndSelectConversation() {
  wrapper = mount(ChannelsView, {
    global: {
      stubs: {
        CustomerRevenueWorkspace: true,
        RouterLink: {
          template: '<a><slot /></a>',
        },
        Sheet: {
          template: '<div><slot /></div>',
        },
        SheetContent: {
          template: '<div><slot /></div>',
        },
        SheetDescription: true,
        SheetTitle: true,
      },
    },
  })
  await flushPromises()
  const conversationButton = wrapper
    .findAll('button')
    .find((button) => button.text().includes('Customer One'))
  expect(conversationButton).toBeDefined()
  await conversationButton!.trigger('click')
  await flushPromises()
  return wrapper
}

describe('ChannelsView messaging behavior', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    clearLegacyWhatsAppReplyAttemptNamespace()
    mocks.organizationStore.selectedOrgId = 'organization-1'
    vi.spyOn(HTMLElement.prototype, 'scrollTo').mockImplementation(
      () => undefined,
    )
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    vi.spyOn(crypto, 'randomUUID').mockReturnValue('00000000-0000-4000-8000-000000000001')
    mocks.hasPermission.mockReturnValue(true)
    mocks.hasProductEntitlement.mockReturnValue(true)
    mocks.connectWebSocket.mockResolvedValue(undefined)
    mocks.markRead.mockResolvedValue({ data: { data: { read_at: new Date().toISOString() } } })
    mocks.refreshUnread.mockResolvedValue(true)
    inboxActivityHandler = null
    mocks.onInboxActivity.mockImplementation((callback) => {
      inboxActivityHandler = callback
      return vi.fn()
    })
    mocks.onConnectionStateChange.mockImplementation((callback) => {
      callback('connected')
      return vi.fn()
    })
    mocks.sendLegacyWhatsAppReply.mockResolvedValue({
      data: {
        data: {
          id: 'message-sent-1',
          direction: 'outgoing',
          message_type: 'text',
          content: { body: 'Hello from Omnichannel' },
          status: 'sent',
          created_at: new Date().toISOString(),
        },
      },
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    inboxActivityHandler = null
    vi.restoreAllMocks()
  })

  it('sends only through the conversation-scoped legacy WhatsApp endpoint', async () => {
    setInbox()
    const view = await mountAndSelectConversation()

    const composer = view.get('[data-testid="omnichannel-reply-composer"]')
    expect(composer.attributes('disabled')).toBeUndefined()
    await composer.setValue('  Hello from Omnichannel  ')
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendLegacyWhatsAppReply).toHaveBeenCalledTimes(1)
    expect(mocks.sendLegacyWhatsAppReply).toHaveBeenCalledWith(
      'conversation-1',
      {
        type: 'text',
        content: { body: 'Hello from Omnichannel' },
        idempotency_key: '00000000-0000-4000-8000-000000000001',
      },
      'organization-1',
    )
    expect(mocks.sendChannelMessage).not.toHaveBeenCalled()
    expect(view.text()).toContain('Hello from Omnichannel')
  })

  it('reuses one WhatsApp idempotency key after an ambiguous failure and clears it after success', async () => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000011')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000012')
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce(new Error('network timeout'))
    const view = await mountAndSelectConversation()
    const composer = view.get('[data-testid="omnichannel-reply-composer"]')
    const send = view.get('[data-testid="omnichannel-send-reply"]')

    await composer.setValue('Retry this exact draft')
    await send.trigger('click')
    await flushPromises()
    await send.trigger('click')
    await flushPromises()

    expect(mocks.sendLegacyWhatsAppReply).toHaveBeenCalledTimes(2)
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000011',
    )
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000011',
    )

    // The acknowledged response clears the attempt. Re-entering the same body
    // is a new logical send and therefore receives a different key.
    await composer.setValue('Retry this exact draft')
    await send.trigger('click')
    await flushPromises()
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[2]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000012',
    )
  })

  it.each(['delivered', 'read'])(
    'retires an acknowledged WhatsApp attempt when the canonical response is already %s',
    async status => {
      vi.mocked(crypto.randomUUID)
        .mockReturnValueOnce('00000000-0000-4000-8000-000000000031')
        .mockReturnValueOnce('00000000-0000-4000-8000-000000000032')
      setInbox()
      mocks.sendLegacyWhatsAppReply.mockResolvedValueOnce({
        data: {
          data: {
            id: `message-${status}`,
            direction: 'outgoing',
            message_type: 'text',
            content: { body: 'Already acknowledged' },
            status,
            created_at: new Date().toISOString(),
          },
        },
      })
      const view = await mountAndSelectConversation()
      const composer = view.get('[data-testid="omnichannel-reply-composer"]')
      const send = view.get('[data-testid="omnichannel-send-reply"]')

      await composer.setValue('Already acknowledged')
      await send.trigger('click')
      await flushPromises()

      expect(storedLegacyReplyAttempts()).toHaveLength(0)
      expect((composer.element as HTMLTextAreaElement).value).toBe('')

      await composer.setValue('Already acknowledged')
      await send.trigger('click')
      await flushPromises()

      expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]?.idempotency_key).toBe(
        '00000000-0000-4000-8000-000000000031',
      )
      expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]?.idempotency_key).toBe(
        '00000000-0000-4000-8000-000000000032',
      )
    },
  )

  it('retires a confirmed sent attempt after the operator switches conversations', async () => {
    const { conversation } = setInbox()
    const secondConversation = {
      ...conversation,
      id: 'conversation-2',
      external_conversation_id: 'external-conversation-2',
      contact_id: 'contact-2',
      contact: {
        ...conversation.contact,
        id: 'contact-2',
        profile_name: 'Customer Two',
      },
    }
    mocks.conversations.mockResolvedValue({
      data: {
        data: {
          conversations: [conversation, secondConversation],
          total: 2,
        },
      },
    })
    let resolveReply!: (value: unknown) => void
    mocks.sendLegacyWhatsAppReply.mockReturnValueOnce(
      new Promise(resolve => {
        resolveReply = resolve
      }),
    )
    const view = await mountAndSelectConversation()

    await view
      .get('[data-testid="omnichannel-reply-composer"]')
      .setValue('Settled while switching')
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()
    expect(storedLegacyReplyAttempts()).toHaveLength(1)

    const secondConversationButton = view
      .findAll('button')
      .find(button => button.text().includes('Customer Two'))
    expect(secondConversationButton).toBeDefined()
    await secondConversationButton!.trigger('click')
    await flushPromises()

    resolveReply({
      data: {
        data: {
          id: 'message-sent-after-switch',
          direction: 'outgoing',
          message_type: 'text',
          content: { body: 'Settled while switching' },
          status: 'sent',
          created_at: new Date().toISOString(),
        },
      },
    })
    await flushPromises()

    expect(storedLegacyReplyAttempts()).toHaveLength(0)
    expect(view.text()).not.toContain('Settled while switching')
  })

  it('persists an ambiguous WhatsApp attempt across remount without storing its body', async () => {
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce(new Error('network timeout'))
    const firstView = await mountAndSelectConversation()
    const body = 'Private patient follow-up details'
    await firstView.get('[data-testid="omnichannel-reply-composer"]').setValue(body)
    await firstView.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    const firstKey = mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]?.idempotency_key
    const stored = storedLegacyReplyAttempts()
    expect(stored).toHaveLength(1)
    expect(stored[0]?.value).not.toContain(body)
    expect(Object.keys(JSON.parse(stored[0]!.value)).sort()).toEqual([
      'bodySha256',
      'createdAt',
      'key',
      'randomSalt',
      'serviceWindowEndsAt',
    ])

    firstView.unmount()
    wrapper = null
    setInbox()
    const remounted = await mountAndSelectConversation()
    await remounted.get('[data-testid="omnichannel-reply-composer"]').setValue(body)
    await remounted.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]?.idempotency_key).toBe(firstKey)
    expect(storedLegacyReplyAttempts()).toHaveLength(0)
  })

  it.each(['pending', 'failed'])('retains the attempt after a non-sent %s response', async status => {
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockResolvedValueOnce({
      data: {
        data: {
          id: `message-${status}`,
          direction: 'outgoing',
          message_type: 'text',
          content: { body: 'Keep this attempt' },
          status,
          created_at: new Date().toISOString(),
        },
      },
    })
    const view = await mountAndSelectConversation()
    const composer = view.get('[data-testid="omnichannel-reply-composer"]')
    await composer.setValue('Keep this attempt')
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(storedLegacyReplyAttempts()).toHaveLength(1)
    expect((composer.element as HTMLTextAreaElement).value).toBe('Keep this attempt')
  })

  it('reuses an unresolved WhatsApp attempt beyond fifteen minutes while the service window remains open', async () => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000041')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000042')
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce(new Error('network timeout'))
    const firstView = await mountAndSelectConversation()
    await firstView.get('[data-testid="omnichannel-reply-composer"]').setValue('Expired draft')
    await firstView.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    const [stored] = storedLegacyReplyAttempts()
    const record = JSON.parse(stored!.value)
    record.createdAt = Date.now() - 16 * 60 * 1000
    sessionStorage.setItem(stored!.key, JSON.stringify(record))
    firstView.unmount()
    wrapper = null
    setInbox()
    const remounted = await mountAndSelectConversation()
    await remounted.get('[data-testid="omnichannel-reply-composer"]').setValue('Expired draft')
    await remounted.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000041',
    )
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000041',
    )
  })

  it('fails closed without calling the reply endpoint when secure retry storage is unavailable', async () => {
    setInbox()
    const view = await mountAndSelectConversation()
    const sessionStorageGetter = vi
      .spyOn(window, 'sessionStorage', 'get')
      .mockReturnValue(
        sessionStorageWith({
          setItem: () => {
            throw new DOMException('quota', 'QuotaExceededError')
          },
        }),
      )

    try {
      await view.get('[data-testid="omnichannel-reply-composer"]').setValue('Do not send')
      await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
      await flushPromises()

      expect(mocks.sendLegacyWhatsAppReply).not.toHaveBeenCalled()
      expect(mocks.sendChannelMessage).not.toHaveBeenCalled()
      expect(mocks.toastError).toHaveBeenCalledWith(
        'WhatsApp reply unavailable',
        'Secure retry storage is unavailable. No message was sent; open native Chat to reply safely.',
      )
    } finally {
      sessionStorageGetter.mockRestore()
    }
  })

  it('does not reuse a persisted WhatsApp attempt for another organization', async () => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000051')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000052')
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce(new Error('network timeout'))
    const firstView = await mountAndSelectConversation()
    await firstView.get('[data-testid="omnichannel-reply-composer"]').setValue('Scoped draft')
    await firstView.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    firstView.unmount()
    wrapper = null
    mocks.organizationStore.selectedOrgId = 'organization-2'
    setInbox()
    const remounted = await mountAndSelectConversation()
    await remounted.get('[data-testid="omnichannel-reply-composer"]').setValue('Scoped draft')
    await remounted.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000051',
    )
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000052',
    )
  })

  it('uses a new WhatsApp idempotency key after the draft changes', async () => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000021')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000022')
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce(new Error('network timeout'))
    const view = await mountAndSelectConversation()
    const composer = view.get('[data-testid="omnichannel-reply-composer"]')
    const send = view.get('[data-testid="omnichannel-send-reply"]')

    await composer.setValue('Original draft')
    await send.trigger('click')
    await flushPromises()
    await vi.waitFor(() => {
      expect(mocks.sendLegacyWhatsAppReply).toHaveBeenCalledTimes(1)
      expect(send.attributes('disabled')).toBeUndefined()
    })
    await composer.setValue('Edited draft')
    await send.trigger('click')
    await flushPromises()
    await vi.waitFor(() => expect(mocks.sendLegacyWhatsAppReply).toHaveBeenCalledTimes(2))

    expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]).toMatchObject({
      content: { body: 'Original draft' },
      idempotency_key: '00000000-0000-4000-8000-000000000021',
    })
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]).toMatchObject({
      content: { body: 'Edited draft' },
      idempotency_key: '00000000-0000-4000-8000-000000000022',
    })
  })

  it('uses a new WhatsApp idempotency key after the conversation changes', async () => {
    vi.mocked(crypto.randomUUID)
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000031')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000032')
    const { conversation } = setInbox()
    const secondConversation = {
      ...conversation,
      id: 'conversation-2',
      external_conversation_id: 'external-conversation-2',
      contact_id: 'contact-2',
      contact: {
        ...conversation.contact,
        id: 'contact-2',
        profile_name: 'Customer Two',
      },
    }
    mocks.conversations.mockResolvedValue({
      data: {
        data: {
          conversations: [conversation, secondConversation],
          total: 2,
        },
      },
    })
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce(new Error('network timeout'))
    const view = await mountAndSelectConversation()
    const composer = view.get('[data-testid="omnichannel-reply-composer"]')

    await composer.setValue('Same body, different destination')
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()
    await vi.waitFor(() => expect(mocks.sendLegacyWhatsAppReply).toHaveBeenCalledTimes(1))
    const secondConversationButton = view
      .findAll('button')
      .find(button => button.text().includes('Customer Two'))
    expect(secondConversationButton).toBeDefined()
    await secondConversationButton!.trigger('click')
    await flushPromises()
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[0]).toBe('conversation-1')
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[0]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000031',
    )
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[0]).toBe('conversation-2')
    expect(mocks.sendLegacyWhatsAppReply.mock.calls[1]?.[1]?.idempotency_key).toBe(
      '00000000-0000-4000-8000-000000000032',
    )
  })

  it('reports a server without the fail-closed reply endpoint and never falls back', async () => {
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce({
      isAxiosError: true,
      response: { status: 404, data: { message: 'Not found' } },
    })
    const view = await mountAndSelectConversation()

    await view.get('[data-testid="omnichannel-reply-composer"]').setValue('Hello')
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendChannelMessage).not.toHaveBeenCalled()
    expect(mocks.toastError).toHaveBeenCalledWith(
      'WhatsApp reply unavailable',
      'Omnichannel direct reply is not enabled on this server. No message was sent.',
    )
  })

  it('shows a service-window conflict from the authoritative endpoint', async () => {
    setInbox()
    mocks.sendLegacyWhatsAppReply.mockRejectedValueOnce({
      isAxiosError: true,
      response: { status: 409, data: { message: 'WhatsApp service window is closed' } },
    })
    const view = await mountAndSelectConversation()

    await view.get('[data-testid="omnichannel-reply-composer"]').setValue('Hello')
    await view.get('[data-testid="omnichannel-send-reply"]').trigger('click')
    await flushPromises()

    expect(mocks.sendChannelMessage).not.toHaveBeenCalled()
    expect(mocks.toastWarning).toHaveBeenCalledWith(
      'WhatsApp reply unavailable',
      'WhatsApp service window is closed',
    )
  })

  it('does not fall back when the selected account lacks the additive endpoint capability', async () => {
    setInbox({ legacyReplyEndpoint: false, includeOtherLegacyAccount: true })
    const view = await mountAndSelectConversation()

    expect(
      view.find('[data-testid="omnichannel-reply-composer"]').exists(),
    ).toBe(false)
    expect(view.find('[data-testid="omnichannel-send-reply"]').exists()).toBe(
      false,
    )
    expect(mocks.sendLegacyWhatsAppReply).not.toHaveBeenCalled()
  })

  it('blocks direct replies without chat write permission', async () => {
    mocks.hasPermission.mockImplementation(
      (resource: string, action: string) =>
        resource === 'conversations' && action === 'write',
    )
    setInbox()
    const view = await mountAndSelectConversation()

    expect(
      view.find('[data-testid="omnichannel-reply-composer"]').exists(),
    ).toBe(false)
    expect(mocks.sendLegacyWhatsAppReply).not.toHaveBeenCalled()
  })

  it('blocks free-text replies after the WhatsApp service window closes', async () => {
    setInbox({
      serviceWindowEndsAt: new Date(Date.now() - 60_000).toISOString(),
    })
    const view = await mountAndSelectConversation()

    expect(
      view.find('[data-testid="omnichannel-reply-composer"]').exists(),
    ).toBe(false)
    expect(
      view.get('[data-testid="whatsapp-direct-reply-notice"]').text(),
    ).toContain('service window is closed')
    expect(mocks.sendLegacyWhatsAppReply).not.toHaveBeenCalled()
  })

  it('opens a selected conversation at its newest message', async () => {
    const scrollHeight = vi
      .spyOn(HTMLElement.prototype, 'scrollHeight', 'get')
      .mockReturnValue(640)
    try {
      setInbox()
      const view = await mountAndSelectConversation()

      expect(
        (
          view.get('[data-testid="omnichannel-message-viewport"]')
            .element as HTMLElement
        ).scrollTop,
      ).toBe(640)
    } finally {
      scrollHeight.mockRestore()
    }
  })

  it('renders and scrolls an unread conversation before its cursor POST settles', async () => {
    const scrollHeight = vi
      .spyOn(HTMLElement.prototype, 'scrollHeight', 'get')
      .mockReturnValue(720)
    const pendingRead = deferred<unknown>()
    try {
      const { conversation } = setInbox({ unreadCount: 1 })
      const secondConversation = {
        ...conversation,
        id: 'conversation-2',
        contact_id: 'contact-2',
        external_conversation_id: 'external-conversation-2',
        unread_count: 0,
        contact: {
          ...conversation.contact,
          id: 'contact-2',
          profile_name: 'Customer Two',
        },
      }
      mocks.conversations.mockResolvedValue({
        data: {
          data: {
            conversations: [conversation, secondConversation],
            total: 2,
          },
        },
      })
      mocks.messages.mockImplementation((conversationId: string) => Promise.resolve({
        data: {
          data: conversationId === conversation.id
            ? {
                messages: [{
                  message: {
                    id: 'message-visible-before-read-response',
                    direction: 'incoming',
                    message_type: 'text',
                    content: 'Visible before the read POST returns',
                    status: 'received',
                    created_at: '2026-08-24T04:00:00Z',
                  },
                }],
                total: 1,
              }
            : { messages: [], total: 0 },
        },
      }))
      mocks.markRead.mockReturnValueOnce(pendingRead.promise)

      const view = await mountAndSelectConversation()

      expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
        last_visible_message_id: 'message-visible-before-read-response',
      }, 'organization-1')
      expect(
        (view.get('[data-testid="omnichannel-message-viewport"]').element as HTMLElement)
          .scrollTop,
      ).toBe(720)
      expect(view.text()).toContain('Visible before the read POST returns')

      const secondConversationButton = view
        .findAll('button')
        .find(button => button.text().includes('Customer Two'))
      expect(secondConversationButton).toBeDefined()
      await secondConversationButton!.trigger('click')
      await flushPromises()
      mocks.refreshUnread.mockClear()

      pendingRead.resolve({ data: { data: { unread_count: 0 } } })
      await flushPromises()
      expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')
      expect(view.text()).not.toContain('Visible before the read POST returns')
    } finally {
      scrollHeight.mockRestore()
    }
  })

  it('does not acknowledge a transcript when the tab becomes hidden during its fetch', async () => {
    const visibility = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    const pendingMessages = deferred<unknown>()
    setInbox({ unreadCount: 1 })
    mocks.messages.mockReturnValueOnce(pendingMessages.promise)

    wrapper = mount(ChannelsView, {
      global: {
        stubs: {
          CustomerRevenueWorkspace: true,
          RouterLink: { template: '<a><slot /></a>' },
          Sheet: { template: '<div><slot /></div>' },
          SheetContent: { template: '<div><slot /></div>' },
          SheetDescription: true,
          SheetTitle: true,
        },
      },
    })
    await flushPromises()
    const conversationButton = wrapper
      .findAll('button')
      .find(button => button.text().includes('Customer One'))
    expect(conversationButton).toBeDefined()

    const selection = conversationButton!.trigger('click')
    await Promise.resolve()
    visibility.mockReturnValue('hidden')
    pendingMessages.resolve({
      data: {
        data: {
          messages: [{
            message: {
              id: 'message-loaded-after-hide',
              direction: 'incoming',
              message_type: 'text',
              content: 'Loaded after the tab was hidden',
              status: 'received',
              created_at: '2026-08-24T04:00:00Z',
            },
          }],
          total: 1,
        },
      },
    })
    await selection
    await flushPromises()

    expect(wrapper.text()).toContain('Loaded after the tab was hidden')
    expect(mocks.markRead).not.toHaveBeenCalled()
    expect(mocks.refreshUnread).not.toHaveBeenCalled()
  })

  it('clears the navbar unread marker after an unread conversation is opened', async () => {
    setInbox({ unreadCount: 1 })
    mocks.messages.mockResolvedValue({
      data: {
        data: {
          messages: [{
            message: {
              id: 'message-visible-on-open',
              direction: 'incoming',
              message_type: 'text',
              content: 'Visible unread message',
              status: 'received',
              created_at: '2026-08-24T04:00:00Z',
            },
          }],
          total: 1,
        },
      },
    })

    await mountAndSelectConversation()

    expect(mocks.markRead).toHaveBeenCalledTimes(1)
    expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
      last_visible_message_id: 'message-visible-on-open',
    }, 'organization-1')
    expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')
  })

  it('keeps an unread conversation marked when updating its read cursor fails', async () => {
    setInbox({ unreadCount: 1 })
    mocks.messages.mockResolvedValue({
      data: {
        data: {
          messages: [{
            message: {
              id: 'message-visible-after-read-failure',
              direction: 'incoming',
              message_type: 'text',
              content: 'The message still loads',
              status: 'received',
              created_at: '2026-08-24T04:00:00Z',
            },
          }],
          total: 1,
        },
      },
    })
    mocks.markRead.mockRejectedValueOnce(new Error('read cursor unavailable'))

    const view = await mountAndSelectConversation()

    expect(view.text()).toContain('The message still loads')
    expect(mocks.refreshUnread).not.toHaveBeenCalled()
    expect(mocks.toastError).toHaveBeenCalledWith(
      'Read state could not be updated',
      'read cursor unavailable',
    )
  })

  it('lets a conversation reader acknowledge their own visible unread cursor', async () => {
    mocks.hasPermission.mockImplementation((_resource, action) => action === 'read')
    setInbox({ unreadCount: 1 })
    mocks.messages.mockResolvedValue({
      data: {
        data: {
          messages: [{
            message: {
              id: 'message-visible-to-reader',
              direction: 'incoming',
              message_type: 'text',
              content: 'Reader-visible message',
              status: 'received',
              created_at: '2026-08-24T04:00:00Z',
            },
          }],
          total: 1,
        },
      },
    })

    await mountAndSelectConversation()

    expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
      last_visible_message_id: 'message-visible-to-reader',
    }, 'organization-1')
  })

  it('marks new activity in the open conversation read only while the tab is focused', async () => {
    vi.useFakeTimers()
    const hasFocus = vi.spyOn(document, 'hasFocus').mockReturnValue(true)

    try {
      const { selectedAccount, conversation } = setInbox()
      mocks.conversations
        .mockResolvedValueOnce({
          data: { data: { conversations: [conversation], total: 1 } },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              conversations: [{ ...conversation, unread_count: 1 }],
              total: 1,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              conversations: [{ ...conversation, unread_count: 1 }],
              total: 1,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              conversations: [{ ...conversation, unread_count: 1 }],
              total: 1,
            },
          },
        })
      mocks.accounts.mockResolvedValue({
        data: { data: { accounts: [selectedAccount] } },
      })
      mocks.messages
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [{
                message: {
                  id: 'message-initial-visible',
                  direction: 'incoming',
                  message_type: 'text',
                  content: 'Initial visible message',
                  status: 'received',
                  created_at: '2026-08-24T03:00:00Z',
                },
              }],
              total: 1,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [{
                message: {
                  id: 'message-live-visible',
                  direction: 'incoming',
                  message_type: 'text',
                  content: 'Live visible message',
                  status: 'received',
                  created_at: '2026-08-24T04:00:00Z',
                },
              }],
              total: 2,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [{
                message: {
                  id: 'message-live-unfocused',
                  direction: 'incoming',
                  message_type: 'text',
                  content: 'Live unfocused message',
                  status: 'received',
                  created_at: '2026-08-24T05:00:00Z',
                },
              }],
              total: 3,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [{
                message: {
                  id: 'message-live-unfocused',
                  direction: 'incoming',
                  message_type: 'text',
                  content: 'Live unfocused message',
                  status: 'received',
                  created_at: '2026-08-24T05:00:00Z',
                },
              }],
              total: 3,
            },
          },
        })

      await mountAndSelectConversation()
      mocks.markRead.mockClear()
      mocks.refreshUnread.mockClear()

      inboxActivityHandler?.({ type: 'new_message' })
      await vi.advanceTimersByTimeAsync(300)
      await flushPromises()

      expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
        last_visible_message_id: 'message-live-visible',
      }, 'organization-1')
      expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')

      mocks.markRead.mockClear()
      mocks.refreshUnread.mockClear()
      hasFocus.mockReturnValue(false)
      inboxActivityHandler?.({ type: 'new_message' })
      await vi.advanceTimersByTimeAsync(300)
      await flushPromises()

      expect(mocks.markRead).not.toHaveBeenCalled()
      expect(mocks.refreshUnread).not.toHaveBeenCalled()

      hasFocus.mockReturnValue(true)
      window.dispatchEvent(new Event('focus'))
      await vi.advanceTimersByTimeAsync(0)
      await flushPromises()

      expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
        last_visible_message_id: 'message-live-unfocused',
      }, 'organization-1')
      expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')
    } finally {
      wrapper?.unmount()
      wrapper = null
      hasFocus.mockRestore()
      vi.useRealTimers()
    }
  })

  it('applies delivery statuses locally without refetching unread state or transcripts', async () => {
    setInbox()
    mocks.messages.mockResolvedValue({
      data: {
        data: {
          messages: [{
            message: {
              id: 'message-status-local',
              direction: 'outgoing',
              message_type: 'text',
              content: 'Status-only message',
              status: 'sent',
              created_at: '2026-08-24T04:00:00Z',
            },
          }],
          total: 1,
        },
      },
    })
    const view = await mountAndSelectConversation()
    mocks.accounts.mockClear()
    mocks.conversations.mockClear()
    mocks.messages.mockClear()
    mocks.refreshUnread.mockClear()

    inboxActivityHandler?.({
      type: 'status_update',
      payload: { message_id: 'message-status-local', status: 'delivered' },
    })
    await nextTick()

    expect(view.text()).toContain('delivered')
    expect(mocks.accounts).not.toHaveBeenCalled()
    expect(mocks.conversations).not.toHaveBeenCalled()
    expect(mocks.messages).not.toHaveBeenCalled()
    expect(mocks.refreshUnread).not.toHaveBeenCalled()
  })

  it('refreshes on inbox activity, follows the bottom only when the reader is already near it', async () => {
    vi.useFakeTimers()
    const hasFocus = vi.spyOn(document, 'hasFocus').mockReturnValue(true)
    const scrollHeight = vi
      .spyOn(HTMLElement.prototype, 'scrollHeight', 'get')
      .mockReturnValue(400)
    const clientHeight = vi
      .spyOn(HTMLElement.prototype, 'clientHeight', 'get')
      .mockReturnValue(300)

    try {
      const { conversation } = setInbox()
      mocks.conversations
        .mockResolvedValueOnce({
          data: { data: { conversations: [conversation], total: 1 } },
        })
        .mockResolvedValue({
          data: {
            data: {
              conversations: [{ ...conversation, unread_count: 1 }],
              total: 1,
            },
          },
        })
      mocks.messages
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [
                {
                  message: {
                    id: 'message-initial',
                    direction: 'incoming',
                    message_type: 'text',
                    content: 'Initial message',
                    status: 'received',
                    ingested_at: '2026-08-24T10:00:00Z',
                    created_at: '2026-08-23T09:00:00Z',
                  },
                },
              ],
              total: 1,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [
                {
                  message: {
                    id: 'message-live-z',
                    direction: 'incoming',
                    message_type: 'text',
                    content: 'Live near-bottom message',
                    status: 'received',
                    ingested_at: '2026-08-24T11:00:00.000100Z',
                    created_at: '2026-08-20T10:00:00Z',
                  },
                },
                {
                  message: {
                    id: 'message-live-a',
                    direction: 'incoming',
                    message_type: 'text',
                    content: 'Live tie-break predecessor',
                    status: 'received',
                    ingested_at: '2026-08-24T11:00:00.000900Z',
                    created_at: '2026-08-19T10:00:00Z',
                  },
                },
              ],
              total: 3,
            },
          },
        })
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [
                {
                  message: {
                    id: 'message-live-while-reading',
                    direction: 'incoming',
                    message_type: 'text',
                    content: 'Live while reading older chat',
                    status: 'received',
                    ingested_at: '2026-08-24T12:00:00Z',
                    created_at: '2026-08-23T11:00:00Z',
                  },
                },
              ],
              total: 4,
            },
          },
        })

      const view = await mountAndSelectConversation()
      const viewport = view.get('[data-testid="omnichannel-message-viewport"]')
        .element as HTMLElement
      const scrollTo = vi.mocked(HTMLElement.prototype.scrollTo)

      viewport.scrollTop = 95
      scrollHeight.mockReset()
      scrollHeight.mockReturnValueOnce(400).mockReturnValue(600)
      scrollTo.mockClear()
      inboxActivityHandler?.({ type: 'new_message' })
      await vi.advanceTimersByTimeAsync(300)
      await flushPromises()

      expect(view.text()).toContain('Live near-bottom message')
      expect(view.text()).toContain('Live tie-break predecessor')
      expect(scrollTo).not.toHaveBeenCalled()
      expect(viewport.scrollTop).toBe(600)
      expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
        last_visible_message_id: 'message-live-a',
      }, 'organization-1')

      viewport.scrollTop = 10
      scrollHeight.mockReset()
      scrollHeight.mockReturnValue(800)
      scrollTo.mockClear()
      mocks.markRead.mockClear()
      mocks.refreshUnread.mockClear()
      inboxActivityHandler?.({ type: 'new_message', payload: { direction: 'incoming' } })
      await vi.advanceTimersByTimeAsync(300)
      await flushPromises()

      expect(view.text()).toContain('Live while reading older chat')
      expect(scrollTo).not.toHaveBeenCalled()
      expect(mocks.markRead).not.toHaveBeenCalled()
      expect(mocks.refreshUnread).not.toHaveBeenCalled()

      viewport.scrollTop = 500
      viewport.dispatchEvent(new Event('scroll'))
      await vi.advanceTimersByTimeAsync(20)
      await flushPromises()

      expect(mocks.markRead).toHaveBeenCalledTimes(1)
      expect(mocks.markRead).toHaveBeenCalledWith('conversation-1', {
        last_visible_message_id: 'message-live-while-reading',
      }, 'organization-1')
      expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')
    } finally {
      wrapper?.unmount()
      wrapper = null
      scrollHeight.mockRestore()
      clientHeight.mockRestore()
      hasFocus.mockRestore()
      vi.useRealTimers()
    }
  })

  it('ignores a late response from a previously selected conversation', async () => {
    const { conversation } = setInbox()
    const secondConversation = {
      ...conversation,
      id: 'conversation-2',
      contact_id: 'contact-2',
      external_conversation_id: 'external-conversation-2',
      contact: {
        id: 'contact-2',
        profile_name: 'Customer Two',
        phone_number: '+60222222222',
      },
    }
    mocks.conversations.mockResolvedValue({
      data: {
        data: {
          conversations: [conversation, secondConversation],
          total: 2,
        },
      },
    })

    let resolveFirstConversation!: (value: unknown) => void
    const firstConversationMessages = new Promise<unknown>((resolve) => {
      resolveFirstConversation = resolve
    })
    mocks.messages.mockImplementation((conversationID: string) => {
      if (conversationID === conversation.id) return firstConversationMessages
      return Promise.resolve({
        data: {
          data: {
            messages: [
              {
                message: {
                  id: 'message-second',
                  direction: 'incoming',
                  message_type: 'text',
                  content: 'Latest message for Customer Two',
                  status: 'received',
                  created_at: '2026-08-23T10:00:00Z',
                },
              },
            ],
            total: 1,
          },
        },
      })
    })

    wrapper = mount(ChannelsView, {
      global: {
        stubs: {
          CustomerRevenueWorkspace: true,
          RouterLink: { template: '<a><slot /></a>' },
          Sheet: { template: '<div><slot /></div>' },
          SheetContent: { template: '<div><slot /></div>' },
          SheetDescription: true,
          SheetTitle: true,
        },
      },
    })
    await flushPromises()

    const firstButton = wrapper
      .findAll('button')
      .find((button) => button.text().includes('Customer One'))
    const secondButton = wrapper
      .findAll('button')
      .find((button) => button.text().includes('Customer Two'))
    expect(firstButton).toBeDefined()
    expect(secondButton).toBeDefined()
    await firstButton!.trigger('click')
    await secondButton!.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Latest message for Customer Two')

    resolveFirstConversation({
      data: {
        data: {
          messages: [
            {
              message: {
                id: 'message-first-stale',
                direction: 'incoming',
                message_type: 'text',
                content: 'Stale message for Customer One',
                status: 'received',
                created_at: '2026-08-23T11:00:00Z',
              },
            },
          ],
          total: 1,
        },
      },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('Latest message for Customer Two')
    expect(wrapper.text()).not.toContain('Stale message for Customer One')
  })

  it('preserves the visible message position when older messages are prepended', async () => {
    let viewportHeight = 400
    const scrollHeight = vi
      .spyOn(HTMLElement.prototype, 'scrollHeight', 'get')
      .mockImplementation(() => viewportHeight)
    let resolveOlderMessages!: (value: unknown) => void
    const olderMessages = new Promise<unknown>((resolve) => {
      resolveOlderMessages = resolve
    })

    try {
      setInbox()
      mocks.messages
        .mockResolvedValueOnce({
          data: {
            data: {
              messages: [
                {
                  message: {
                    id: 'message-newest',
                    direction: 'incoming',
                    message_type: 'text',
                    content: 'Newest message',
                    status: 'received',
                    created_at: '2026-08-23T10:00:00Z',
                  },
                },
              ],
              total: 2,
            },
          },
        })
        .mockReturnValueOnce(olderMessages)

      const view = await mountAndSelectConversation()
      const viewport = view.get('[data-testid="omnichannel-message-viewport"]')
        .element as HTMLElement
      viewport.scrollTop = 125

      const loadOlderButton = view
        .findAll('button')
        .find((button) => button.text().includes('Load older messages'))
      expect(loadOlderButton).toBeDefined()
      await loadOlderButton!.trigger('click')

      viewportHeight = 600
      resolveOlderMessages({
        data: {
          data: {
            messages: [
              {
                message: {
                  id: 'message-older',
                  direction: 'incoming',
                  message_type: 'text',
                  content: 'Older message',
                  status: 'received',
                  created_at: '2026-08-23T09:00:00Z',
                },
              },
            ],
            total: 2,
          },
        },
      })
      await flushPromises()

      expect(viewport.scrollTop).toBe(325)
    } finally {
      scrollHeight.mockRestore()
    }
  })
})
