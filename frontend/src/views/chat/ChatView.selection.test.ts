/** @vitest-environment happy-dom */

import { flushPromises, shallowMount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ChatViewComponent from './ChatView.vue'
import ProtectedMessageMedia from '@/components/chat/ProtectedMessageMedia.vue'

const mocks = vi.hoisted(() => ({
  routeSource: { params: { contactId: 'first' as string | undefined } },
  route: null as { params: { contactId?: string } } | null,
  routerPush: vi.fn(),
  contactsStore: null as Record<string, any> | null,
  authStore: null as Record<string, any> | null,
  transfersStore: null as Record<string, any> | null,
  hasPermission: vi.fn(),
  hasProductEntitlement: vi.fn(),
  fetchContacts: vi.fn(),
  fetchContact: vi.fn(),
  fetchMessages: vi.fn(),
  refreshCurrentMessages: vi.fn(),
  setCurrentContact: vi.fn(),
  setAccountFilter: vi.fn(),
  clearMessages: vi.fn(),
  fetchTransfers: vi.fn(),
  fetchActiveTransferForContact: vi.fn(),
  upsertTransfer: vi.fn(),
  updateTransfer: vi.fn(),
  createTransfer: vi.fn(),
  resumeTransfer: vi.fn(),
  fetchTags: vi.fn(),
  fetchNotes: vi.fn(),
  clearNotes: vi.fn(),
  listAccounts: vi.fn(),
  getSessionData: vi.fn(),
  markRead: vi.fn(),
  refreshUnread: vi.fn(),
  organizationStore: { selectedOrgId: null as string | null },
  setWebSocketContact: vi.fn(),
  scrollIntoView: vi.fn(),
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  resizeObservers: [] as Array<{
    callback: ResizeObserverCallback
    observe: ReturnType<typeof vi.fn>
    disconnect: ReturnType<typeof vi.fn>
  }>,
  infiniteControllers: [] as Array<{
    scrollAreaRef: { value: unknown }
    setup: ReturnType<typeof vi.fn>
    cleanup: ReturnType<typeof vi.fn>
    getViewport: ReturnType<typeof vi.fn>
    preserveScrollPosition: ReturnType<typeof vi.fn>
    onScroll?: (event: Event) => void
  }>,
}))

vi.mock('vue-router', async () => {
  const { reactive } = await import('vue')
  const route = reactive(mocks.routeSource)
  mocks.route = route
  return {
    useRoute: () => route,
    useRouter: () => ({ push: mocks.routerPush }),
  }
})

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({
    t: (key: string, fallback?: string) => fallback ?? key,
  }),
}))

vi.mock('@vueuse/core', async () => {
  const { ref } = await import('vue')
  return { useMediaQuery: () => ref(false) }
})

vi.mock('@/composables/useColorMode', async () => {
  const { ref } = await import('vue')
  return { useColorMode: () => ({ isDark: ref(true) }) }
})

vi.mock('@/composables/useHeaderMedia', async () => {
  const { ref } = await import('vue')
  return {
    useHeaderMedia: () => ({
      file: ref(null),
      preview: ref(''),
      acceptTypes: ref(''),
      handleFileChange: vi.fn(),
      clear: vi.fn(),
    }),
  }
})

vi.mock('@/composables/useInfiniteScroll', () => ({
  useInfiniteScroll: (options: { onScroll?: (event: Event) => void }) => {
    const controller = {
      scrollAreaRef: { value: null },
      setup: vi.fn(),
      cleanup: vi.fn(),
      getViewport: vi.fn(() => null as HTMLElement | null),
      preserveScrollPosition: vi.fn(async (callback: () => Promise<void>) => callback()),
      onScroll: options.onScroll,
    }
    mocks.infiniteControllers.push(controller)
    return controller
  },
}))

vi.mock('@/stores/contacts', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    contacts: [] as Array<Record<string, unknown>>,
    sortedContacts: [] as Array<Record<string, unknown>>,
    currentContact: null as Record<string, any> | null,
    messages: [] as Array<Record<string, unknown>>,
    replyingTo: null,
    searchQuery: '',
    selectedTags: [] as string[],
    hasMoreContacts: false,
    hasMoreMessages: false,
    isLoadingMessages: false,
    isLoadingMoreContacts: false,
    isLoadingOlderMessages: false,
    fetchContacts: mocks.fetchContacts,
    fetchContact: mocks.fetchContact,
    fetchMessages: mocks.fetchMessages,
    refreshCurrentMessages: mocks.refreshCurrentMessages,
    fetchOlderMessages: vi.fn(),
    loadMoreContacts: vi.fn(),
    setCurrentContact: mocks.setCurrentContact,
    setAccountFilter: mocks.setAccountFilter,
    clearMessages: mocks.clearMessages,
    clearReplyingTo: vi.fn(),
    setReplyingTo: vi.fn(),
    sendMessage: vi.fn(),
    sendTemplate: vi.fn(),
    addMessage: vi.fn(),
    updateMessageReactions: vi.fn(),
    updateContactTags: vi.fn(),
  })
  mocks.contactsStore = store
  return {
    normalizeContactSearch: (raw: string) => {
      const trimmed = raw.trim().replace(/^\+/, '')
      return trimmed && /^[\d\s+()-]+$/.test(trimmed)
        ? trimmed.replace(/[\s+()-]/g, '')
        : trimmed
    },
    useContactsStore: () => store,
  }
})

vi.mock('@/stores/auth', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    isAuthenticated: true,
    organizationId: 'organization-1',
    userRole: 'agent',
    user: { id: 'agent-1' },
    permissionRevision: 0,
    hasPermission: (resource: string, action: string) => {
      void store.permissionRevision
      return mocks.hasPermission(resource, action)
    },
    hasProductEntitlement: (key: string) => {
      void store.permissionRevision
      return mocks.hasProductEntitlement(key)
    },
    restoreSession: vi.fn(),
  })
  mocks.authStore = store
  return { useAuthStore: () => store }
})

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => ({ refresh: mocks.refreshUnread }),
}))

vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => mocks.organizationStore,
}))

vi.mock('@/stores/users', () => ({
  useUsersStore: () => ({ users: [], fetchUsers: vi.fn() }),
}))

vi.mock('@/stores/transfers', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    activeByContact: {} as Record<string, Record<string, any> | undefined>,
    fetchTransfers: mocks.fetchTransfers,
    fetchActiveTransferForContact: mocks.fetchActiveTransferForContact,
    upsertTransfer: mocks.upsertTransfer,
    updateTransfer: mocks.updateTransfer,
    getActiveTransferForContact: (contactId: string) => store.activeByContact[contactId],
  })
  mocks.transfersStore = store
  return { useTransfersStore: () => store }
})

vi.mock('@/stores/tags', () => ({
  useTagsStore: () => ({
    tags: [{ name: 'existing-tag' }],
    fetchTags: mocks.fetchTags,
    getTagByName: () => null,
  }),
}))

vi.mock('@/stores/notes', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    notes: [] as unknown[],
    hasMore: false,
    fetchNotes: mocks.fetchNotes,
    clearNotes: mocks.clearNotes,
  })
  return { useNotesStore: () => store }
})

vi.mock('@/services/websocket', () => ({
  wsService: { setCurrentContact: mocks.setWebSocketContact },
}))

vi.mock('@/services/api', () => ({
  effectiveAIIsAllowed: (state: { known?: boolean; ai_allowed?: boolean; blocked?: boolean } | null | undefined) => (
    state?.known === true && state.ai_allowed === true && state.blocked === false
  ),
  contactsService: {
    getSessionData: mocks.getSessionData,
    markRead: mocks.markRead,
    assign: vi.fn(),
  },
  chatbotService: {
    createTransfer: mocks.createTransfer,
    resumeTransfer: mocks.resumeTransfer,
  },
  messagesService: { sendReaction: vi.fn() },
  customActionsService: { list: vi.fn(), execute: vi.fn() },
  accountsService: { list: mocks.listAccounts },
  cannedResponsesService: { use: vi.fn() },
  getRequestHeaders: vi.fn(() => ({})),
}))

vi.mock('vue-sonner', () => ({
  toast: { error: vi.fn(), success: mocks.toastSuccess, info: vi.fn(), warning: mocks.toastWarning },
}))

function deferred() {
  let resolve!: () => void
  const promise = new Promise<void>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function deferredValue<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function contact(id: string) {
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

function mountChatView() {
  return shallowMount(ChatViewComponent, {
    global: {
      mocks: {
        $t: (key: string, fallback?: string) => fallback ?? key,
      },
      stubs: {
        ScrollArea: { template: '<div><slot /></div>' },
        Transition: { template: '<div><slot /></div>' },
        Teleport: true,
        Tooltip: { template: '<div><slot /></div>' },
        TooltipTrigger: { template: '<div><slot /></div>' },
        TooltipContent: { template: '<div><slot /></div>' },
        Button: { template: '<button><slot /></button>' },
        Badge: { template: '<span><slot /></span>' },
      },
    },
  })
}

describe('ChatView conversation selection', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    mocks.infiniteControllers = []
    mocks.resizeObservers = []
    mocks.routeSource.params.contactId = 'first'
    mocks.organizationStore.selectedOrgId = null
    if (mocks.route) mocks.route.params.contactId = 'first'

    const first = contact('first')
    const second = contact('second')
    Object.assign(mocks.contactsStore!, {
      contacts: [first, second],
      sortedContacts: [first, second],
      currentContact: null,
      messages: [],
      searchQuery: '',
      isLoadingMessages: false,
    })
    mocks.transfersStore!.activeByContact = {}
    mocks.setCurrentContact.mockImplementation(value => {
      mocks.contactsStore!.currentContact = value
    })
    mocks.hasPermission.mockReturnValue(false)
    mocks.hasProductEntitlement.mockReturnValue(false)
    mocks.authStore!.permissionRevision += 1
    mocks.fetchContacts.mockResolvedValue(undefined)
    mocks.fetchActiveTransferForContact.mockImplementation(async (contactId: string) =>
      mocks.transfersStore!.activeByContact[contactId],
    )
    mocks.upsertTransfer.mockImplementation(transfer => {
      mocks.transfersStore!.activeByContact[transfer.contact_id] = transfer
      return transfer
    })
    mocks.updateTransfer.mockImplementation((transferId: string, updates: Record<string, any>) => {
      const activeByContact = mocks.transfersStore!.activeByContact as Record<
        string,
        Record<string, any> | undefined
      >
      const entry = Object.entries(activeByContact)
        .find(([, transfer]) => transfer?.id === transferId)
      if (!entry) return false
      const [contactId, transfer] = entry
      const updated = { ...transfer!, ...updates }
      mocks.transfersStore!.activeByContact[contactId] =
        updated.status === 'active' ? updated : undefined
      return true
    })
    mocks.createTransfer.mockResolvedValue({ data: { data: {} } })
    mocks.resumeTransfer.mockResolvedValue({ data: { data: {} } })
    mocks.refreshCurrentMessages.mockResolvedValue(undefined)
    mocks.fetchNotes.mockResolvedValue(undefined)
    mocks.listAccounts.mockResolvedValue({ data: { data: { accounts: [] } } })
    mocks.getSessionData.mockRejectedValue(new Error('not configured'))
    mocks.markRead.mockResolvedValue({ data: { data: { cursor_synced: true } } })
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      callback(0)
      return 1
    })
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: mocks.scrollIntoView,
    })
    class MockResizeObserver {
      observe = vi.fn()
      unobserve = vi.fn()
      disconnect = vi.fn()

      constructor(callback: ResizeObserverCallback) {
        mocks.resizeObservers.push({
          callback,
          observe: this.observe,
          disconnect: this.disconnect,
        })
      }
    }
    vi.stubGlobal('ResizeObserver', MockResizeObserver)
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.runOnlyPendingTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('exposes stable contact and message identity selectors', async () => {
    mocks.contactsStore!.messages = [{
      id: 'message-selector-1',
      direction: 'incoming',
      message_type: 'text',
      content: { body: 'Selector-bound message' },
      status: 'received',
      created_at: '2026-08-24T04:00:00Z',
    }]
    mocks.fetchMessages.mockResolvedValue(undefined)

    wrapper = mountChatView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()

    const contacts = wrapper.findAll('[data-testid="chat-contact"]')
    expect(contacts).toHaveLength(2)
    expect(contacts[0].attributes('data-contact-id')).toBe('first')
    expect(contacts[1].attributes('data-contact-id')).toBe('second')

    const transcript = wrapper.get('[data-testid="chat-message-list"]')
    expect(transcript.attributes('data-contact-id')).toBe('first')
    const message = transcript.get('[data-testid="chat-message"]')
    expect(message.attributes('data-message-id')).toBe('message-selector-1')
    expect(message.attributes('data-message-direction')).toBe('incoming')
  })

  describe('chat media', () => {
    const mediaMessage = (id: string, messageType: string, mimeType: string, mediaURL = `stored/${id}`) => ({
      id,
      direction: 'incoming',
      message_type: messageType,
      content: messageType === 'template' ? { body: 'Template body' } : {},
      media_url: mediaURL,
      media_mime_type: mimeType,
      media_filename: messageType === 'document' ? 'lab-report.pdf' : undefined,
      status: 'received',
      created_at: '2026-08-24T04:00:00Z',
    })

    async function renderMedia(messages: Array<Record<string, unknown>>) {
      mocks.contactsStore!.messages = messages
      mocks.fetchMessages.mockResolvedValue(undefined)
      wrapper = mountChatView()
      await flushPromises()
      await vi.advanceTimersByTimeAsync(50)
      await nextTick()
      return wrapper.findAllComponents(ProtectedMessageMedia)
    }

    afterEach(() => {
      localStorage.removeItem('selected_organization_id')
    })

    it('renders every media type through the workspace-pinned media component', async () => {
      mocks.organizationStore.selectedOrgId = 'workspace-b'
      const media = await renderMedia([
        mediaMessage('media-image', 'image', 'image/jpeg'),
        mediaMessage('media-sticker', 'sticker', 'image/webp'),
        mediaMessage('media-video', 'video', 'video/mp4'),
        mediaMessage('media-audio', 'audio', 'audio/ogg'),
        mediaMessage('media-document', 'document', 'application/pdf'),
        mediaMessage('media-template', 'template', 'image/png'),
        mediaMessage('media-pending', 'image', 'image/jpeg', ''),
      ])

      expect(media.map(component => component.props('message').id)).toEqual([
        'media-image',
        'media-sticker',
        'media-video',
        'media-audio',
        'media-document',
        'media-template',
      ])
      for (const component of media) {
        expect(component.props('organizationId')).toBe('workspace-b')
      }
      // Native elements can no longer request the header-less media URL.
      expect(wrapper!.html()).not.toContain('/api/media/')
      expect(wrapper!.text()).toContain('[Image]')
    })

    it('pins media to the persisted workspace before the switcher restores it into the store', async () => {
      localStorage.setItem('selected_organization_id', 'workspace-persisted')
      const media = await renderMedia([mediaMessage('media-image', 'image', 'image/jpeg')])
      expect(media).toHaveLength(1)
      expect(media[0].props('organizationId')).toBe('workspace-persisted')
    })

    it('uses the session workspace when no other workspace is selected', async () => {
      const media = await renderMedia([mediaMessage('media-image', 'image', 'image/jpeg')])
      expect(media).toHaveLength(1)
      expect(media[0].props('organizationId')).toBe('organization-1')
    })
  })

  it('finishes account and conversation selection while AI status is still loading', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && action === 'read',
    )
    mocks.contactsStore!.contacts[0].whatsapp_account = 'clinic-account'
    mocks.fetchMessages.mockResolvedValue(undefined)
    const transferLookup = deferred()
    mocks.fetchActiveTransferForContact.mockReturnValueOnce(transferLookup.promise)

    wrapper = mountChatView()
    await flushPromises()

    expect((wrapper.vm as any).selectedAccount).toBe('clinic-account')
    expect(mocks.setWebSocketContact).toHaveBeenCalledWith('first')
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').attributes('aria-label'))
      .toBe('Checking AI status')

    transferLookup.resolve()
    await flushPromises()
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').exists()).toBe(false)
  })

  it('keeps conversation selection usable when AI status lookup rejects unexpectedly', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && action === 'read',
    )
    mocks.contactsStore!.contacts[0].whatsapp_account = 'clinic-account'
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.fetchActiveTransferForContact.mockRejectedValueOnce(new Error('transfer lookup failed'))

    wrapper = mountChatView()
    await flushPromises()

    expect((wrapper.vm as any).selectedAccount).toBe('clinic-account')
    expect(mocks.setWebSocketContact).toHaveBeenCalledWith('first')
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').exists()).toBe(false)
  })

  it('lets only the current contact completion schedule the initial bottom scroll', async () => {
    const firstLoad = deferred()
    const secondLoad = deferred()
    mocks.fetchMessages.mockImplementation((id: string) => {
      return id === 'first' ? firstLoad.promise : secondLoad.promise
    })

    wrapper = shallowMount(ChatViewComponent, {
      global: {
        mocks: {
          $t: (key: string, fallback?: string) => fallback ?? key,
        },
        stubs: {
          ScrollArea: { template: '<div><slot /></div>' },
          Transition: { template: '<div><slot /></div>' },
          Teleport: true,
        },
      },
    })
    await flushPromises()
    expect(mocks.fetchMessages).toHaveBeenCalledWith('first')

    mocks.route!.params.contactId = 'second'
    await nextTick()
    await flushPromises()
    expect(mocks.fetchMessages).toHaveBeenCalledWith('second')

    secondLoad.resolve()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()

    expect(mocks.setWebSocketContact).toHaveBeenCalledTimes(1)
    expect(mocks.setWebSocketContact).toHaveBeenLastCalledWith('second')
    expect(mocks.scrollIntoView).toHaveBeenCalledTimes(1)
    expect(mocks.scrollIntoView).toHaveBeenCalledWith({
      behavior: 'instant',
      block: 'end',
    })

    firstLoad.resolve()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(100)
    await nextTick()

    expect(mocks.setWebSocketContact).toHaveBeenCalledTimes(1)
    expect(mocks.scrollIntoView).toHaveBeenCalledTimes(1)
    expect(mocks.contactsStore!.currentContact.id).toBe('second')
  })

  it('cancels the first contact bottom-scroll timer when another contact is selected', async () => {
    const firstLoad = deferred()
    const secondLoad = deferred()
    mocks.fetchMessages.mockImplementation((id: string) => {
      return id === 'first' ? firstLoad.promise : secondLoad.promise
    })

    wrapper = shallowMount(ChatViewComponent, {
      global: {
        mocks: {
          $t: (key: string, fallback?: string) => fallback ?? key,
        },
        stubs: {
          ScrollArea: { template: '<div><slot /></div>' },
          Transition: { template: '<div><slot /></div>' },
          Teleport: true,
        },
      },
    })
    await flushPromises()
    firstLoad.resolve()
    await flushPromises()
    expect(mocks.setWebSocketContact).toHaveBeenLastCalledWith('first')

    mocks.route!.params.contactId = 'second'
    await nextTick()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()
    expect(mocks.scrollIntoView).not.toHaveBeenCalled()

    secondLoad.resolve()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()
    expect(mocks.setWebSocketContact).toHaveBeenLastCalledWith('second')
    expect(mocks.scrollIntoView).toHaveBeenCalledTimes(1)
  })

  it('preserves a scrolled-up reader on canonical refresh and follows new messages near the bottom', async () => {
    const focusSpy = vi.spyOn(document, 'hasFocus').mockReturnValue(true)
    const visibilitySpy = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    const initialMessage = {
      id: 'message-initial',
      direction: 'incoming',
      created_at: '2026-08-23T09:00:00Z',
    }
    mocks.contactsStore!.messages = [initialMessage]
    mocks.fetchMessages.mockResolvedValue(undefined)

    try {
      wrapper = shallowMount(ChatViewComponent, {
        global: {
          mocks: {
            $t: (key: string, fallback?: string) => fallback ?? key,
          },
          stubs: {
            ScrollArea: { template: '<div><slot /></div>' },
            Transition: { template: '<div><slot /></div>' },
            Teleport: true,
          },
        },
      })
      await flushPromises()
      await vi.advanceTimersByTimeAsync(50)
      await nextTick()
      mocks.scrollIntoView.mockClear()

      const messagesController = mocks.infiniteControllers[1]
      expect(messagesController?.onScroll).toBeTypeOf('function')
      const viewport = document.createElement('div')
      Object.defineProperties(viewport, {
        scrollHeight: { configurable: true, value: 1_000 },
        clientHeight: { configurable: true, value: 300 },
      })
      viewport.scrollTop = 100
      messagesController.onScroll?.({ target: viewport } as unknown as Event)

      mocks.contactsStore!.messages = [
        initialMessage,
        {
          id: 'message-while-reading',
          direction: 'incoming',
          created_at: '2026-08-23T10:00:00Z',
        },
      ]
      await nextTick()
      await nextTick()
      expect(mocks.scrollIntoView).not.toHaveBeenCalled()

      viewport.scrollTop = 695
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      mocks.contactsStore!.messages = [
        ...mocks.contactsStore!.messages,
        {
          id: 'message-near-bottom',
          direction: 'incoming',
          created_at: '2026-08-23T11:00:00Z',
        },
      ]
      await nextTick()
      await nextTick()

      expect(mocks.scrollIntoView).toHaveBeenCalledTimes(1)
      expect(mocks.scrollIntoView).toHaveBeenCalledWith({
        behavior: 'smooth',
        block: 'end',
      })
    } finally {
      focusSpy.mockRestore()
      visibilitySpy.mockRestore()
    }
  })

  it('follows late media layout growth only while the reader remains at the bottom', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    wrapper = mountChatView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()

    const resizeObserver = mocks.resizeObservers[0]
    expect(resizeObserver).toBeDefined()
    expect(resizeObserver.observe).toHaveBeenCalledTimes(1)
    const observedContent = resizeObserver.observe.mock.calls[0][0] as Element
    const messagesController = mocks.infiniteControllers[1]
    const viewport = document.createElement('div')
    Object.defineProperties(viewport, {
      scrollHeight: { configurable: true, value: 1_000 },
      clientHeight: { configurable: true, value: 300 },
    })
    messagesController.getViewport.mockReturnValue(viewport)

    viewport.scrollTop = 700
    messagesController.onScroll?.({ target: viewport } as unknown as Event)
    mocks.scrollIntoView.mockClear()
    resizeObserver.callback([
      { target: observedContent, contentRect: { height: 600 } } as ResizeObserverEntry,
    ], {} as ResizeObserver)
    await nextTick()
    await nextTick()

    expect(mocks.scrollIntoView).toHaveBeenCalledTimes(1)
    expect(mocks.scrollIntoView).toHaveBeenCalledWith({
      behavior: 'instant',
      block: 'end',
    })

    viewport.scrollTop = 100
    messagesController.onScroll?.({ target: viewport } as unknown as Event)
    mocks.scrollIntoView.mockClear()
    resizeObserver.callback([
      { target: observedContent, contentRect: { height: 700 } } as ResizeObserverEntry,
    ], {} as ResizeObserver)
    await nextTick()
    await nextTick()

    expect(mocks.scrollIntoView).not.toHaveBeenCalled()

    mocks.route!.params.contactId = 'second'
    await nextTick()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()

    expect(resizeObserver.disconnect).toHaveBeenCalledTimes(1)
    const replacementObserver = mocks.resizeObservers[1]
    expect(replacementObserver).toBeDefined()
    mocks.scrollIntoView.mockClear()
    resizeObserver.callback([
      { target: observedContent, contentRect: { height: 800 } } as ResizeObserverEntry,
    ], {} as ResizeObserver)
    await nextTick()
    await nextTick()
    expect(mocks.scrollIntoView).not.toHaveBeenCalled()

    wrapper.unmount()
    wrapper = null
    expect(replacementObserver.disconnect).toHaveBeenCalledTimes(1)
  })

  it('keeps a focused scrolled-up message unread and refreshes the selected workspace at bottom', async () => {
    const focusSpy = vi.spyOn(document, 'hasFocus').mockReturnValue(true)
    const visibilitySpy = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.organizationStore.selectedOrgId = 'selected-organization'

    try {
      wrapper = mountChatView()
      await flushPromises()
      await vi.advanceTimersByTimeAsync(50)
      await nextTick()

      const messagesController = mocks.infiniteControllers[1]
      const viewport = document.createElement('div')
      Object.defineProperties(viewport, {
        scrollHeight: { configurable: true, value: 1_000 },
        clientHeight: { configurable: true, value: 300 },
      })
      viewport.scrollTop = 100
      messagesController.getViewport.mockReturnValue(viewport)

      const initialMessage = {
        id: 'message-initial',
        direction: 'incoming',
        created_at: '2026-08-23T09:00:00Z',
      }
      mocks.contactsStore!.messages = [initialMessage]
      await nextTick()
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      mocks.markRead.mockClear()
      mocks.scrollIntoView.mockClear()

      mocks.contactsStore!.messages = [
        initialMessage,
        {
          id: 'message-focused-while-scrolled-up',
          direction: 'incoming',
          created_at: '2026-08-23T10:00:00Z',
        },
      ]
      await nextTick()
      await nextTick()

      expect(mocks.markRead).not.toHaveBeenCalled()
      expect(mocks.scrollIntoView).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('1 unread message')

      viewport.scrollTop = 700
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()

      expect(mocks.markRead).toHaveBeenCalledTimes(1)
      expect(mocks.markRead).toHaveBeenCalledWith(
        'first',
        'message-focused-while-scrolled-up',
        'selected-organization',
      )
      expect(mocks.refreshUnread).toHaveBeenCalledWith('selected-organization', 'agent-1')
      expect(wrapper.text()).not.toContain('1 unread message')
    } finally {
      focusSpy.mockRestore()
      visibilitySpy.mockRestore()
    }
  })

  it('keeps an unverified visible cursor unread and retryable after a resolved POST', async () => {
    const focusSpy = vi.spyOn(document, 'hasFocus').mockReturnValue(true)
    const visibilitySpy = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.markRead.mockResolvedValue({ data: { data: { cursor_synced: false } } })

    try {
      wrapper = mountChatView()
      await flushPromises()
      await vi.advanceTimersByTimeAsync(50)
      await nextTick()

      const messagesController = mocks.infiniteControllers[1]
      const viewport = document.createElement('div')
      Object.defineProperties(viewport, {
        scrollHeight: { configurable: true, value: 1_000 },
        clientHeight: { configurable: true, value: 300 },
      })
      viewport.scrollTop = 100
      messagesController.getViewport.mockReturnValue(viewport)

      const initialMessage = {
        id: 'message-before-unverified',
        direction: 'incoming',
        created_at: '2026-08-23T09:00:00Z',
      }
      mocks.contactsStore!.messages = [initialMessage]
      await nextTick()
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      mocks.markRead.mockClear()
      mocks.fetchContacts.mockClear()
      mocks.refreshUnread.mockClear()

      mocks.contactsStore!.messages = [
        initialMessage,
        {
          id: 'message-unverified-visible',
          direction: 'incoming',
          created_at: '2026-08-23T10:00:00Z',
        },
      ]
      await nextTick()
      await nextTick()
      expect(wrapper.text()).toContain('1 unread message')

      viewport.scrollTop = 700
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()

      expect(mocks.markRead).toHaveBeenCalledWith(
        'first',
        'message-unverified-visible',
        'organization-1',
      )
      expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')
      expect(mocks.fetchContacts).toHaveBeenCalled()
      expect(wrapper.text()).toContain('1 unread message')

      mocks.markRead.mockClear()
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()
      expect(mocks.markRead).toHaveBeenCalledWith(
        'first',
        'message-unverified-visible',
        'organization-1',
      )
    } finally {
      focusSpy.mockRestore()
      visibilitySpy.mockRestore()
    }
  })

  it('shows the first queued unread on focus without acknowledging unseen later messages', async () => {
    let focused = false
    const focusSpy = vi.spyOn(document, 'hasFocus').mockImplementation(() => focused)
    const visibilitySpy = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('visible')
    mocks.fetchMessages.mockResolvedValue(undefined)
    const firstUnreadElement = document.createElement('div')
    firstUnreadElement.id = 'message-message-first-unread'
    document.body.appendChild(firstUnreadElement)

    try {
      wrapper = mountChatView()
      await flushPromises()
      await vi.advanceTimersByTimeAsync(50)
      await nextTick()

      const messagesController = mocks.infiniteControllers[1]
      const viewport = document.createElement('div')
      Object.defineProperties(viewport, {
        scrollHeight: { configurable: true, value: 1_200 },
        clientHeight: { configurable: true, value: 300 },
      })
      viewport.scrollTop = 200
      messagesController.getViewport.mockReturnValue(viewport)

      const initialMessage = {
        id: 'message-initial',
        direction: 'incoming',
        created_at: '2026-08-23T09:00:00Z',
      }
      mocks.contactsStore!.messages = [initialMessage]
      await nextTick()
      mocks.contactsStore!.messages = [
        initialMessage,
        {
          id: 'message-first-unread',
          direction: 'incoming',
          created_at: '2026-08-23T10:00:00Z',
        },
        {
          id: 'message-later-unread',
          direction: 'incoming',
          created_at: '2026-08-23T11:00:00Z',
        },
      ]
      await nextTick()
      await nextTick()
      expect(wrapper.text()).toContain('2 unread messages')

      mocks.markRead.mockClear()
      mocks.scrollIntoView.mockClear()
      focused = true
      window.dispatchEvent(new Event('focus'))
      await nextTick()
      await nextTick()

      expect(mocks.scrollIntoView).toHaveBeenCalledWith({
        behavior: 'smooth',
        block: 'start',
      })
      expect(mocks.markRead).not.toHaveBeenCalled()
      expect(wrapper.text()).toContain('2 unread messages')
    } finally {
      firstUnreadElement.remove()
      focusSpy.mockRestore()
      visibilitySpy.mockRestore()
    }
  })

  it('does not clear or scroll contact B when a deferred contact A send completes', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    const pendingSend = deferredValue<unknown>()
    mocks.contactsStore!.sendMessage.mockReturnValue(pendingSend.promise)

    wrapper = shallowMount(ChatViewComponent, {
      global: {
        mocks: {
          $t: (key: string, fallback?: string) => fallback ?? key,
        },
        stubs: {
          ScrollArea: { template: '<div><slot /></div>' },
          Transition: { template: '<div><slot /></div>' },
          Teleport: true,
        },
      },
    })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()

    const composer = wrapper.find('textarea')
    expect(composer.exists()).toBe(true)
    await composer.setValue('Contact A draft')
    const composerForm = wrapper
      .findAll('form')
      .find(form => form.find('textarea').exists())
    expect(composerForm).toBeDefined()
    await composerForm!.trigger('submit')
    await Promise.resolve()
    expect(mocks.contactsStore!.sendMessage).toHaveBeenCalledWith(
      'first',
      'text',
      { body: 'Contact A draft' },
      undefined,
      undefined,
    )

    mocks.route!.params.contactId = 'second'
    await nextTick()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()
    await wrapper.find('textarea').setValue('Contact B draft')
    mocks.scrollIntoView.mockClear()
    mocks.contactsStore!.clearReplyingTo.mockClear()

    pendingSend.resolve({ id: 'sent-for-first' })
    await flushPromises()
    await nextTick()

    expect((wrapper.find('textarea').element as HTMLTextAreaElement).value).toBe(
      'Contact B draft',
    )
    expect(mocks.contactsStore!.clearReplyingTo).not.toHaveBeenCalled()
    expect(mocks.scrollIntoView).not.toHaveBeenCalled()
    expect(mocks.contactsStore!.currentContact.id).toBe('second')
  })

  it('catches up canonical native Chat state every thirty visible seconds', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.contactsStore!.searchQuery = '  +60 (12) 345-6789  '
    wrapper = mountChatView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()
    mocks.fetchContacts.mockClear()
    mocks.refreshCurrentMessages.mockClear()

    await vi.advanceTimersByTimeAsync(30_000)
    await flushPromises()

    expect(mocks.fetchContacts).toHaveBeenCalledTimes(1)
    expect(mocks.fetchContacts).toHaveBeenCalledWith({
      search: '60123456789',
    })
    expect(mocks.refreshCurrentMessages).toHaveBeenCalledTimes(1)
  })

  it('catches up the visible Chat sidebar when no conversation is selected', async () => {
    mocks.routeSource.params.contactId = undefined
    mocks.route!.params.contactId = undefined
    mocks.contactsStore!.currentContact = null
    wrapper = mountChatView()
    await flushPromises()
    mocks.fetchContacts.mockClear()
    mocks.refreshCurrentMessages.mockClear()

    await vi.advanceTimersByTimeAsync(30_000)
    await flushPromises()

    expect(mocks.fetchContacts).toHaveBeenCalledTimes(1)
    expect(mocks.refreshCurrentMessages).toHaveBeenCalledTimes(1)
    expect(mocks.contactsStore!.currentContact).toBeNull()
  })

  it('skips hidden polling and catches up immediately on visibility and online events', async () => {
    let visibility: DocumentVisibilityState = 'hidden'
    const visibilitySpy = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockImplementation(() => visibility)
    mocks.fetchMessages.mockResolvedValue(undefined)

    try {
      wrapper = mountChatView()
      await flushPromises()
      mocks.fetchContacts.mockClear()
      mocks.refreshCurrentMessages.mockClear()

      await vi.advanceTimersByTimeAsync(60_000)
      expect(mocks.fetchContacts).not.toHaveBeenCalled()
      expect(mocks.refreshCurrentMessages).not.toHaveBeenCalled()

      mocks.contactsStore!.searchQuery = '  Search Customer  '
      visibility = 'visible'
      document.dispatchEvent(new Event('visibilitychange'))
      await flushPromises()
      expect(mocks.fetchContacts).toHaveBeenCalledTimes(1)
      expect(mocks.fetchContacts).toHaveBeenLastCalledWith({
        search: 'Search Customer',
      })
      expect(mocks.refreshCurrentMessages).toHaveBeenCalledTimes(1)

      window.dispatchEvent(new Event('online'))
      await flushPromises()
      expect(mocks.fetchContacts).toHaveBeenCalledTimes(2)
      expect(mocks.refreshCurrentMessages).toHaveBeenCalledTimes(2)
    } finally {
      visibilitySpy.mockRestore()
    }
  })

  it('coalesces catch-up and never applies an old contact continuation to the new selection', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    const firstCatchUp = deferred()
    const refreshedContacts: string[] = []
    mocks.refreshCurrentMessages.mockImplementation(() => {
      refreshedContacts.push(mocks.contactsStore!.currentContact?.id ?? '')
      return refreshedContacts.length === 1
        ? firstCatchUp.promise
        : Promise.resolve()
    })
    wrapper = mountChatView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    mocks.fetchContacts.mockClear()
    mocks.refreshCurrentMessages.mockClear()
    refreshedContacts.length = 0

    await vi.advanceTimersByTimeAsync(30_000)
    expect(refreshedContacts).toEqual(['first'])

    mocks.route!.params.contactId = 'second'
    await nextTick()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    mocks.scrollIntoView.mockClear()
    window.dispatchEvent(new Event('online'))
    await Promise.resolve()
    expect(mocks.refreshCurrentMessages).toHaveBeenCalledTimes(1)

    firstCatchUp.resolve()
    await flushPromises()

    expect(refreshedContacts).toEqual(['first', 'second'])
    expect(mocks.contactsStore!.currentContact.id).toBe('second')
    expect(mocks.scrollIntoView).not.toHaveBeenCalled()
  })

  it('removes catch-up listeners and timers on unmount', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    wrapper = mountChatView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    mocks.fetchContacts.mockClear()
    mocks.refreshCurrentMessages.mockClear()

    wrapper.unmount()
    wrapper = null
    await vi.advanceTimersByTimeAsync(60_000)
    document.dispatchEvent(new Event('visibilitychange'))
    window.dispatchEvent(new Event('online'))
    await flushPromises()

    expect(mocks.fetchContacts).not.toHaveBeenCalled()
    expect(mocks.refreshCurrentMessages).not.toHaveBeenCalled()
  })

  it('clears contact A session data synchronously while contact B is loading', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    const secondSession = deferredValue<any>()
    mocks.getSessionData
      .mockResolvedValueOnce({ data: { data: { private_value: 'contact-a' } } })
      .mockReturnValueOnce(secondSession.promise)
    wrapper = mountChatView()
    await flushPromises()

    expect((wrapper.vm as any).contactSessionData).toEqual({
      private_value: 'contact-a',
    })

    mocks.route!.params.contactId = 'second'
    await nextTick()
    expect((wrapper.vm as any).contactSessionData).toBeNull()
    await flushPromises()
    expect((wrapper.vm as any).contactSessionData).toBeNull()

    secondSession.resolve({ data: { data: { private_value: 'contact-b' } } })
    await flushPromises()
    expect((wrapper.vm as any).contactSessionData).toEqual({
      private_value: 'contact-b',
    })
  })

  it('reacts only after both contact-write and dedicated review authority are granted', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    wrapper = mountChatView()
    await flushPromises()

    const dialog = wrapper.findComponent({ name: 'ContactIdentityReviewDialog' })
    expect(dialog.props('canReview')).toBe(false)

    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'contacts' && action === 'write',
    )
    mocks.authStore!.permissionRevision += 1
    await nextTick()

    expect(dialog.props('canReview')).toBe(false)

    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      action === 'write'
      && (resource === 'contacts' || resource === 'contacts.identity_review'),
    )
    mocks.authStore!.permissionRevision += 1
    await nextTick()

    expect(dialog.props('canReview')).toBe(true)
  })

  it('projects a resolved identity state before the canonical contact refresh completes', async () => {
    mocks.fetchMessages.mockResolvedValue(undefined)
    wrapper = mountChatView()
    await flushPromises()
    const canonicalRefresh = deferred()
    mocks.fetchContacts.mockReturnValueOnce(canonicalRefresh.promise)
    const resolvedState = {
      known: true,
      ai_allowed: false,
      blocked: true,
      open_hold_count: 1,
      latest_generation: 7,
      reason: 'identity_review_open',
    }

    wrapper.findComponent({ name: 'ContactIdentityReviewDialog' }).vm.$emit('resolved', resolvedState)
    await nextTick()

    expect(mocks.contactsStore!.currentContact.identity_review_ai_state).toEqual(resolvedState)
    expect(mocks.contactsStore!.contacts[0].identity_review_ai_state).toEqual(resolvedState)
    canonicalRefresh.resolve()
    await flushPromises()
  })

  it('pauses AI for the selected contact and immediately exposes the resume state', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && (action === 'read' || action === 'write'),
    )
    mocks.contactsStore!.contacts[0].whatsapp_account = 'clinic-account'
    mocks.fetchMessages.mockResolvedValue(undefined)
    const transfer = {
      id: 'transfer-first',
      contact_id: 'first',
      contact_name: 'Contact first',
      phone_number: 'phone-first',
      whatsapp_account: 'clinic-account',
      status: 'active',
      source: 'manual',
      transferred_by: 'agent-1',
      transferred_at: '2026-01-01T00:00:00Z',
      sla_breached: false,
      escalation_level: 0,
    }
    mocks.createTransfer.mockResolvedValue({
      data: { data: { transfer } },
    })

    wrapper = mountChatView()
    await flushPromises()

    const toggle = wrapper.find('[data-testid="conversation-ai-toggle"]')
    expect(toggle.exists()).toBe(true)
    expect(toggle.attributes('aria-label')).toBe('Pause AI')

    await toggle.trigger('click')
    await flushPromises()

    expect(mocks.createTransfer).toHaveBeenCalledWith(expect.objectContaining({
      contact_id: 'first',
      agent_id: 'agent-1',
      whatsapp_account: 'clinic-account',
      source: 'manual',
    }))
    expect(mocks.upsertTransfer).toHaveBeenCalledWith(transfer)
    expect(wrapper.text()).toContain('AI paused')
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').attributes('aria-label'))
      .toBe('Resume AI')
  })

  it.each([
    [404, 'Agent not found'],
    [400, 'Agent is currently away'],
  ])('retries a pause without self-assignment when the backend answers %s %s', async (status, message) => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && (action === 'read' || action === 'write'),
    )
    mocks.contactsStore!.contacts[0].whatsapp_account = 'clinic-account'
    mocks.fetchMessages.mockResolvedValue(undefined)
    const transfer = {
      id: 'transfer-first',
      contact_id: 'first',
      status: 'active',
      source: 'manual',
      transferred_by: 'agent-1',
      transferred_at: '2026-01-01T00:00:00Z',
      sla_breached: false,
      escalation_level: 0,
    }
    mocks.createTransfer
      .mockRejectedValueOnce({ response: { status, data: { message } } })
      .mockResolvedValueOnce({ data: { data: { transfer } } })

    wrapper = mountChatView()
    await flushPromises()

    await wrapper.find('[data-testid="conversation-ai-toggle"]').trigger('click')
    await flushPromises()

    expect(mocks.createTransfer).toHaveBeenCalledTimes(2)
    expect(mocks.createTransfer.mock.calls[0][0]).toMatchObject({ agent_id: 'agent-1' })
    expect(mocks.createTransfer.mock.calls[1][0]).not.toHaveProperty('agent_id')
    expect(mocks.createTransfer.mock.calls[1][0]).toMatchObject({
      contact_id: 'first',
      whatsapp_account: 'clinic-account',
      source: 'manual',
    })
    expect(mocks.upsertTransfer).toHaveBeenCalledWith(transfer)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('AI replies paused', {
      description: 'This conversation is now in the team handover queue.',
    })
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').attributes('aria-label'))
      .toBe('Resume AI')
  })

  it.each([
    [404, 'Contact not found'],
    [409, 'Contact already has an active transfer'],
    [403, "You don't have permission to create transfers"],
  ])('does not retry a pause rejected with %s %s', async (status, message) => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && (action === 'read' || action === 'write'),
    )
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.createTransfer.mockRejectedValueOnce({ response: { status, data: { message } } })

    wrapper = mountChatView()
    await flushPromises()

    await wrapper.find('[data-testid="conversation-ai-toggle"]').trigger('click')
    await flushPromises()

    expect(mocks.createTransfer).toHaveBeenCalledTimes(1)
    expect(mocks.upsertTransfer).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').attributes('aria-label'))
      .toBe('Pause AI')
  })

  it('resumes a selected contact transfer owned by the current agent', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && (action === 'read' || action === 'write'),
    )
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.transfersStore!.activeByContact.first = {
      id: 'transfer-first',
      contact_id: 'first',
      status: 'active',
      source: 'manual',
      transferred_by: 'agent-1',
    }

    wrapper = mountChatView()
    await flushPromises()

    const toggle = wrapper.find('[data-testid="conversation-ai-toggle"]')
    expect(toggle.attributes('aria-label')).toBe('Resume AI')
    await toggle.trigger('click')
    await flushPromises()

    expect(mocks.resumeTransfer).toHaveBeenCalledWith('transfer-first')
    expect(mocks.updateTransfer).toHaveBeenCalledWith('transfer-first', {
      status: 'resumed',
    })
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').attributes('aria-label'))
      .toBe('Pause AI')
  })

  it('does not claim AI resumed when durable identity review still blocks the contact', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && (action === 'read' || action === 'write'),
    )
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.contactsStore!.contacts[0].identity_review_ai_state = {
      known: true,
      ai_allowed: false,
      blocked: true,
      open_hold_count: 1,
      reason: 'identity_review_open',
      latest_hold_id: 'hold-1',
    }
    mocks.transfersStore!.activeByContact.first = {
      id: 'transfer-first',
      contact_id: 'first',
      status: 'active',
      source: 'manual',
      transferred_by: 'agent-1',
    }

    wrapper = mountChatView()
    await flushPromises()
    expect(wrapper.get('[data-testid="conversation-ai-toggle"]').attributes('aria-label'))
      .toBe('End conversation pause')
    await wrapper.get('[data-testid="conversation-ai-toggle"]').trigger('click')
    await flushPromises()

    expect(mocks.resumeTransfer).toHaveBeenCalledWith('transfer-first')
    expect(mocks.toastSuccess).not.toHaveBeenCalledWith(
      'AI replies resumed',
      expect.anything(),
    )
    expect(mocks.toastWarning).toHaveBeenCalledWith(
      'Human handover ended',
      expect.objectContaining({
        description: expect.stringContaining('identity review'),
      }),
    )
  })

  it('shows that AI is paused without allowing an agent to resume another user transfer', async () => {
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      resource === 'transfers' && (action === 'read' || action === 'write'),
    )
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.transfersStore!.activeByContact.first = {
      id: 'transfer-first',
      contact_id: 'first',
      status: 'active',
      source: 'manual',
      transferred_by: 'agent-2',
    }

    wrapper = mountChatView()
    await flushPromises()

    expect(wrapper.text()).toContain('AI paused')
    expect(wrapper.find('[data-testid="conversation-ai-toggle"]').exists()).toBe(false)
  })
})

const WorkspaceStub = defineComponent({
  name: 'CustomerRevenueWorkspace',
  props: {
    contactId: { type: String, default: '' },
    contact: { type: Object, default: null },
    sessionData: { type: Object, default: null },
    surface: { type: String, default: 'chat' },
    channel: { type: String, default: null },
    requestedAction: { type: Object, default: null },
  },
  emits: ['action-consumed', 'close', 'tags-updated'],
  template: '<div data-testid="workspace-stub" />',
})

function mountChatViewWithMenu(extraStubs: Record<string, unknown> = {}) {
  const slotStub = { template: '<div><slot /></div>' }
  return shallowMount(ChatViewComponent, {
    global: {
      mocks: {
        $t: (key: string, fallback?: string) => fallback ?? key,
      },
      stubs: {
        ScrollArea: slotStub,
        Transition: slotStub,
        Teleport: true,
        Tooltip: slotStub,
        TooltipTrigger: slotStub,
        TooltipContent: slotStub,
        Button: { template: '<button><slot /></button>' },
        Badge: { template: '<span><slot /></span>' },
        DropdownMenu: slotStub,
        DropdownMenuTrigger: slotStub,
        DropdownMenuContent: slotStub,
        DropdownMenuItem: slotStub,
        DropdownMenuLabel: slotStub,
        DropdownMenuSeparator: { template: '<hr />' },
        Sheet: slotStub,
        SheetContent: slotStub,
        SheetTitle: slotStub,
        SheetDescription: slotStub,
        CustomerRevenueWorkspace: WorkspaceStub,
        ...extraStubs,
      },
    },
  })
}

describe('ChatView next-step menu', () => {
  let wrapper: VueWrapper | null = null
  const grantedPermissions = new Set<string>()
  const grantedEntitlements = new Set<string>()

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    mocks.infiniteControllers = []
    mocks.resizeObservers = []
    mocks.routeSource.params.contactId = 'first'
    mocks.organizationStore.selectedOrgId = null
    if (mocks.route) mocks.route.params.contactId = 'first'

    const first = contact('first')
    const second = contact('second')
    Object.assign(mocks.contactsStore!, {
      contacts: [first, second],
      sortedContacts: [first, second],
      currentContact: null,
      messages: [],
      searchQuery: '',
      isLoadingMessages: false,
    })
    mocks.transfersStore!.activeByContact = {}
    mocks.setCurrentContact.mockImplementation(value => {
      mocks.contactsStore!.currentContact = value
    })
    grantedPermissions.clear()
    grantedEntitlements.clear()
    mocks.hasPermission.mockImplementation((resource: string, action: string) =>
      grantedPermissions.has(`${resource}:${action}`),
    )
    mocks.hasProductEntitlement.mockImplementation((key: string) => grantedEntitlements.has(key))
    mocks.authStore!.permissionRevision += 1
    mocks.fetchContacts.mockResolvedValue(undefined)
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.fetchActiveTransferForContact.mockResolvedValue(undefined)
    mocks.fetchNotes.mockResolvedValue(undefined)
    mocks.listAccounts.mockResolvedValue({ data: { data: { accounts: [] } } })
    mocks.getSessionData.mockRejectedValue(new Error('not configured'))
    mocks.markRead.mockResolvedValue({ data: { data: { cursor_synced: true } } })
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      callback(0)
      return 1
    })
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: mocks.scrollIntoView,
    })
    vi.stubGlobal('ResizeObserver', class {
      observe = vi.fn()
      unobserve = vi.fn()
      disconnect = vi.fn()
    })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    vi.runOnlyPendingTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  function grantAllNextSteps() {
    for (const permission of [
      'crm.leads:write',
      'crm.pipelines:read',
      'tasks:write',
      'bookings:write',
      'contacts:read',
    ]) grantedPermissions.add(permission)
    grantedEntitlements.add('crm.enabled')
    grantedEntitlements.add('bookings.enabled')
    mocks.authStore!.permissionRevision += 1
  }

  async function mountReady(extraStubs: Record<string, unknown> = {}) {
    wrapper = mountChatViewWithMenu(extraStubs)
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()
    return wrapper
  }

  it('shows the gated next-step items only when permission and entitlement allow them', async () => {
    const view = await mountReady()
    expect(view.text()).not.toContain('Next step')
    expect(view.find('[data-testid="chat-next-step-lead"]').exists()).toBe(false)
    expect(view.find('[data-testid="chat-next-step-follow-up"]').exists()).toBe(false)
    expect(view.find('[data-testid="chat-next-step-booking"]').exists()).toBe(false)
    expect(view.find('[data-testid="chat-menu-contact-record"]').exists()).toBe(false)
    expect(view.get('[data-testid="chat-menu-workspace"]').text()).toBe('Show customer workspace')
    expect(view.text()).not.toContain('View contact details')

    grantedPermissions.add('crm.leads:write')
    grantedPermissions.add('crm.pipelines:read')
    grantedPermissions.add('tasks:write')
    mocks.authStore!.permissionRevision += 1
    await nextTick()
    // Permissions alone are not enough without the CRM entitlement.
    expect(view.find('[data-testid="chat-next-step-lead"]').exists()).toBe(false)

    grantAllNextSteps()
    await nextTick()
    expect(view.text()).toContain('Next step')
    expect(view.get('[data-testid="chat-next-step-lead"]').text()).toBe('Add to pipeline')
    expect(view.get('[data-testid="chat-next-step-follow-up"]').text()).toBe('Schedule follow-up')
    expect(view.get('[data-testid="chat-next-step-booking"]').text()).toBe('Book appointment')
    expect(view.get('[data-testid="chat-menu-contact-record"]').text()).toBe('Open contact record')
    expect(view.get('[data-testid="chat-menu-copy-phone"]').text()).toBe('Copy phone number')

    await view.get('[data-testid="chat-menu-contact-record"]').trigger('select')
    expect(mocks.routerPush).toHaveBeenCalledWith('/settings/contacts/first')
  })

  it('opens the workspace with a lead request and clears it once consumed', async () => {
    grantAllNextSteps()
    const view = await mountReady()
    expect(view.findComponent(WorkspaceStub).exists()).toBe(false)

    await view.get('[data-testid="chat-next-step-lead"]').trigger('select')
    await nextTick()

    const workspace = view.findComponent(WorkspaceStub)
    expect(workspace.exists()).toBe(true)
    expect(workspace.props('channel')).toBe('whatsapp')
    const request = workspace.props('requestedAction') as { kind: string; nonce: number }
    expect(request).toMatchObject({ kind: 'lead' })
    expect(view.get('[data-testid="chat-menu-workspace"]').text()).toBe('Hide customer workspace')

    // A second request keeps the workspace open and issues a fresh nonce.
    await view.get('[data-testid="chat-next-step-booking"]').trigger('select')
    await nextTick()
    const second = view.findComponent(WorkspaceStub).props('requestedAction') as {
      kind: string
      nonce: number
    }
    expect(second.kind).toBe('booking')
    expect(second.nonce).toBeGreaterThan(request.nonce)
    expect(view.findComponent(WorkspaceStub).exists()).toBe(true)

    // A stale nonce does not clear the newer request.
    view.findComponent(WorkspaceStub).vm.$emit('action-consumed', request.nonce)
    await nextTick()
    expect(view.findComponent(WorkspaceStub).props('requestedAction')).toMatchObject({ kind: 'booking' })

    view.findComponent(WorkspaceStub).vm.$emit('action-consumed', second.nonce)
    await nextTick()
    expect(view.findComponent(WorkspaceStub).props('requestedAction')).toBeNull()
    expect(view.findComponent(WorkspaceStub).exists()).toBe(true)
  })

  it('drops an unconsumed request when the workspace is closed', async () => {
    grantAllNextSteps()
    const view = await mountReady()

    await view.get('[data-testid="chat-next-step-follow-up"]').trigger('select')
    await nextTick()
    expect(view.findComponent(WorkspaceStub).props('requestedAction')).toMatchObject({
      kind: 'follow-up',
    })

    view.findComponent(WorkspaceStub).vm.$emit('close')
    await nextTick()
    expect(view.findComponent(WorkspaceStub).exists()).toBe(false)

    await view.get('[data-testid="chat-menu-workspace"]').trigger('select')
    await nextTick()
    expect(view.findComponent(WorkspaceStub).exists()).toBe(true)
    expect(view.findComponent(WorkspaceStub).props('requestedAction')).toBeNull()
  })

  it('copies a real phone number to the clipboard', async () => {
    grantAllNextSteps()
    const writeText = vi.fn().mockResolvedValue(undefined)
    const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } })
    try {
      const view = await mountReady()
      await view.get('[data-testid="chat-menu-copy-phone"]').trigger('select')
      await flushPromises()
      expect(writeText).toHaveBeenCalledWith('phone-first')
      expect(mocks.toastSuccess).toHaveBeenCalledWith('Phone number copied')
    } finally {
      if (original) Object.defineProperty(navigator, 'clipboard', original)
      else delete (navigator as unknown as Record<string, unknown>).clipboard
    }
  })

  it('hides copy and call for a contact whose WhatsApp number is hidden', async () => {
    grantAllNextSteps()
    const hidden = {
      ...contact('first'),
      phone_number: 'bsuid:contact-1',
      whatsapp_account: 'clinic-account',
      metadata: { coexistence_phone_placeholder: true },
    }
    Object.assign(mocks.contactsStore!, {
      contacts: [hidden, contact('second')],
      sortedContacts: [hidden, contact('second')],
    })

    const view = await mountReady()

    expect((view.vm as any).selectedAccount).toBe('clinic-account')
    expect(view.find('[data-testid="chat-menu-copy-phone"]').exists()).toBe(false)
    expect(view.findComponent({ name: 'CallButton' }).exists()).toBe(false)
    expect(view.text()).toContain('WhatsApp number hidden')
    // The next-step items still work for this customer.
    expect(view.find('[data-testid="chat-next-step-lead"]').exists()).toBe(true)
  })

  it('shows the call button when the contact has a real number and an account', async () => {
    const withAccount = { ...contact('first'), whatsapp_account: 'clinic-account' }
    Object.assign(mocks.contactsStore!, {
      contacts: [withAccount, contact('second')],
      sortedContacts: [withAccount, contact('second')],
    })

    const view = await mountReady()
    const callButton = view.findComponent({ name: 'CallButton' })
    expect(callButton.exists()).toBe(true)
    expect(callButton.props('contactPhone')).toBe('phone-first')
  })

  it('names the three-dots menu by what it offers and gives it a matching title', async () => {
    const view = await mountReady()
    const trigger = view.get('[data-testid="chat-more-options"]')
    expect(trigger.attributes('aria-label')).toBe('More options')
    expect(trigger.attributes('title')).toBe('More options')

    grantAllNextSteps()
    await nextTick()
    const granted = view.get('[data-testid="chat-more-options"]')
    expect(granted.attributes('aria-label')).toBe('Next step and more')
    expect(granted.attributes('title')).toBe('Next step and more')
    // The neighbouring workspace button keeps its id and name.
    expect(view.get('#info-button').attributes('aria-label')).toBe('Open customer revenue workspace')
  })

  it('shows a hidden-number customer as "WhatsApp user" in the header and list, never a placeholder', async () => {
    const hidden = {
      ...contact('first'),
      name: '',
      phone_number: 'bsuid:contact-1',
      whatsapp_account: 'clinic-account',
      metadata: { coexistence_phone_placeholder: true },
    }
    Object.assign(mocks.contactsStore!, {
      contacts: [hidden, contact('second')],
      sortedContacts: [hidden, contact('second')],
    })
    const slotStub = { template: '<div><slot /></div>' }

    const view = await mountReady({ Avatar: slotStub, AvatarFallback: slotStub })

    expect(view.get('[data-testid="chat-header-name"]').text()).toBe('WhatsApp user')
    expect(view.text()).not.toContain('bsuid:')
    const hiddenRow = view.get('[data-testid="chat-contact"][data-contact-id="first"]')
    expect(hiddenRow.text()).toContain('WhatsApp user')
    expect(hiddenRow.text()).toContain('WhatsApp number hidden')
    expect(hiddenRow.get('[title]').attributes('title')).toBe('WhatsApp user')
    // Avatar initials follow the display name (header and list row).
    expect(view.findAll('div').some(el => el.text() === 'WU')).toBe(true)
    // The other row keeps its saved name and number.
    const plainRow = view.get('[data-testid="chat-contact"][data-contact-id="second"]')
    expect(plainRow.text()).toContain('Contact second')
    expect(plainRow.text()).toContain('phone-second')
  })

  it('prefers the WhatsApp profile name for a hidden-number customer', async () => {
    const hidden = {
      ...contact('first'),
      name: '',
      profile_name: 'Clinic Patient',
      phone_number: 'bsuid:contact-1',
      metadata: { coexistence_phone_placeholder: true },
    }
    Object.assign(mocks.contactsStore!, {
      contacts: [hidden, contact('second')],
      sortedContacts: [hidden, contact('second')],
    })

    const view = await mountReady()
    expect(view.get('[data-testid="chat-header-name"]').text()).toBe('Clinic Patient')
  })

  it('keeps the original name and number output for contacts that have them', async () => {
    // The chat-contact rows are canary-visible: a saved name still wins over a
    // differing WhatsApp profile name, and a real number is shown verbatim.
    const named = {
      ...contact('first'),
      name: 'Saved Name',
      profile_name: 'Profile Name',
      phone_number: 'phone-first',
    }
    Object.assign(mocks.contactsStore!, {
      contacts: [named, contact('second')],
      sortedContacts: [named, contact('second')],
    })

    const view = await mountReady()

    expect(view.get('[data-testid="chat-header-name"]').text()).toBe('Saved Name')
    const row = view.get('[data-testid="chat-contact"][data-contact-id="first"]')
    expect(row.text()).toContain('Saved Name')
    expect(row.text()).not.toContain('Profile Name')
    expect(row.text()).toContain('phone-first')
  })

  it('gives screen readers the hidden-number hint and keeps the title', async () => {
    const hidden = {
      ...contact('first'),
      phone_number: 'bsuid:contact-1',
      whatsapp_account: 'clinic-account',
      metadata: { coexistence_phone_placeholder: true },
    }
    Object.assign(mocks.contactsStore!, {
      contacts: [hidden, contact('second')],
      sortedContacts: [hidden, contact('second')],
    })

    const view = await mountReady()
    const address = view.get('[data-testid="chat-header-address"]')
    const hint = "WhatsApp did not share this customer's number (they message with a WhatsApp username)."
    expect(address.text()).toContain('WhatsApp number hidden')
    expect(address.attributes('title')).toBe(hint)
    const srHint = address.get('[data-testid="chat-header-address-hint"]')
    expect(srHint.classes()).toContain('sr-only')
    expect(srHint.text()).toBe(hint)
  })

  it('keeps a real number dialable when a stale coexistence flag is still set', async () => {
    const stale = {
      ...contact('first'),
      whatsapp_account: 'clinic-account',
      metadata: { coexistence_phone_unavailable: true },
    }
    Object.assign(mocks.contactsStore!, {
      contacts: [stale, contact('second')],
      sortedContacts: [stale, contact('second')],
    })

    const view = await mountReady()
    const callButton = view.findComponent({ name: 'CallButton' })
    expect(callButton.exists()).toBe(true)
    expect(callButton.props('contactPhone')).toBe('phone-first')
    const address = view.get('[data-testid="chat-header-address"]')
    expect(address.text()).toBe('phone-first')
    expect(address.attributes('title')).toBeUndefined()
    expect(address.find('[data-testid="chat-header-address-hint"]').exists()).toBe(false)
    expect(view.text()).not.toContain('WhatsApp number hidden')
  })
})
