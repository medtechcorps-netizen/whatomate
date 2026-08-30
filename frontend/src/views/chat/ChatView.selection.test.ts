/** @vitest-environment happy-dom */

import { flushPromises, shallowMount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ChatViewComponent from './ChatView.vue'

const mocks = vi.hoisted(() => ({
  routeSource: { params: { contactId: 'first' as string | undefined } },
  route: null as { params: { contactId?: string } } | null,
  routerPush: vi.fn(),
  contactsStore: null as Record<string, any> | null,
  fetchContacts: vi.fn(),
  fetchContact: vi.fn(),
  fetchMessages: vi.fn(),
  refreshCurrentMessages: vi.fn(),
  setCurrentContact: vi.fn(),
  setAccountFilter: vi.fn(),
  clearMessages: vi.fn(),
  fetchTransfers: vi.fn(),
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

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isAuthenticated: true,
    organizationId: 'organization-1',
    userRole: 'agent',
    user: { id: 'agent-1' },
    hasPermission: () => false,
    restoreSession: vi.fn(),
  }),
}))

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => ({ refresh: mocks.refreshUnread }),
}))

vi.mock('@/stores/organizations', () => ({
  useOrganizationsStore: () => mocks.organizationStore,
}))

vi.mock('@/stores/users', () => ({
  useUsersStore: () => ({ users: [], fetchUsers: vi.fn() }),
}))

vi.mock('@/stores/transfers', () => ({
  useTransfersStore: () => ({
    fetchTransfers: mocks.fetchTransfers,
    getActiveTransferForContact: () => null,
  }),
}))

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
  contactsService: {
    getSessionData: mocks.getSessionData,
    markRead: mocks.markRead,
    assign: vi.fn(),
  },
  chatbotService: { createTransfer: vi.fn(), resumeTransfer: vi.fn() },
  messagesService: { sendReaction: vi.fn() },
  customActionsService: { list: vi.fn(), execute: vi.fn() },
  accountsService: { list: mocks.listAccounts },
  cannedResponsesService: { use: vi.fn() },
  getRequestHeaders: vi.fn(() => ({})),
}))

vi.mock('vue-sonner', () => ({
  toast: { error: vi.fn(), success: vi.fn(), info: vi.fn(), warning: vi.fn() },
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
    mocks.setCurrentContact.mockImplementation(value => {
      mocks.contactsStore!.currentContact = value
    })
    mocks.fetchContacts.mockResolvedValue(undefined)
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
})
