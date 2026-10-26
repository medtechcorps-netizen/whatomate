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
  authStore: null as Record<string, any> | null,
  organizationsStore: null as { selectedOrgId: string | null } | null,
  hasPermission: vi.fn(),
  fetchContacts: vi.fn(),
  fetchMessages: vi.fn(),
  setCurrentContact: vi.fn(),
  clearMessages: vi.fn(),
  setListFilter: vi.fn(),
  markContactReadState: vi.fn(),
  markRead: vi.fn(),
  assignableMembers: vi.fn(),
  fetchUsers: vi.fn(),
  refreshUnread: vi.fn(),
  toastError: vi.fn(),
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
  useI18n: () => ({ t: (key: string, fallback?: string) => fallback ?? key }),
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
    contacts: [] as Array<Record<string, any>>,
    sortedContacts: [] as Array<Record<string, any>>,
    currentContact: null as Record<string, any> | null,
    messages: [] as Array<Record<string, unknown>>,
    messagesOrganizationScope: '',
    replyingTo: null,
    searchQuery: '',
    selectedTags: [] as string[],
    listFilter: 'all',
    hasMoreContacts: false,
    hasMoreMessages: false,
    isLoadingMessages: false,
    isLoadingMoreContacts: false,
    isLoadingOlderMessages: false,
    fetchContacts: mocks.fetchContacts,
    fetchContact: vi.fn(),
    fetchMessages: mocks.fetchMessages,
    refreshCurrentMessages: vi.fn().mockResolvedValue(undefined),
    fetchOlderMessages: vi.fn(),
    loadMoreContacts: vi.fn(),
    setCurrentContact: mocks.setCurrentContact,
    setAccountFilter: vi.fn(),
    clearMessages: mocks.clearMessages,
    clearReplyingTo: vi.fn(),
    setReplyingTo: vi.fn(),
    sendMessage: vi.fn(),
    sendTemplate: vi.fn(),
    addMessage: vi.fn(),
    updateMessageReactions: vi.fn(),
    updateContactTags: vi.fn(),
    setListFilter: mocks.setListFilter,
    markContactReadState: mocks.markContactReadState,
  })
  mocks.contactsStore = store
  return {
    normalizeContactSearch: (raw: string) => raw.trim(),
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
    hasProductEntitlement: () => false,
    restoreSession: vi.fn(),
  })
  mocks.authStore = store
  return { useAuthStore: () => store }
})

vi.mock('@/stores/omnichannelUnread', () => ({
  useOmnichannelUnreadStore: () => ({ refresh: mocks.refreshUnread }),
}))
vi.mock('@/stores/organizations', async () => {
  const { reactive } = await import('vue')
  const store = reactive({ selectedOrgId: null as string | null })
  mocks.organizationsStore = store
  return { useOrganizationsStore: () => store }
})
vi.mock('@/stores/users', () => ({
  useUsersStore: () => ({ users: [], fetchUsers: mocks.fetchUsers }),
}))
vi.mock('@/stores/transfers', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    fetchTransfers: vi.fn(),
    fetchActiveTransferForContact: vi.fn().mockResolvedValue(undefined),
    upsertTransfer: vi.fn(),
    updateTransfer: vi.fn(),
    getActiveTransferForContact: () => undefined,
  })
  return { useTransfersStore: () => store }
})
vi.mock('@/stores/tags', () => ({
  useTagsStore: () => ({ tags: [{ name: 'existing-tag' }], fetchTags: vi.fn(), getTagByName: () => null }),
}))
vi.mock('@/stores/notes', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    notes: [] as unknown[],
    hasMore: false,
    fetchNotes: vi.fn().mockResolvedValue(undefined),
    clearNotes: vi.fn(),
  })
  return { useNotesStore: () => store }
})
vi.mock('@/services/websocket', () => ({
  wsService: { setCurrentContact: vi.fn() },
}))
vi.mock('@/services/api', () => ({
  effectiveAIIsAllowed: () => true,
  contactsService: {
    getSessionData: vi.fn().mockRejectedValue(new Error('not configured')),
    markRead: mocks.markRead,
    assignableMembers: mocks.assignableMembers,
    assign: vi.fn(),
  },
  chatbotService: { createTransfer: vi.fn(), resumeTransfer: vi.fn() },
  messagesService: { sendReaction: vi.fn() },
  customActionsService: { list: vi.fn().mockResolvedValue({ data: { data: { custom_actions: [] } } }), execute: vi.fn() },
  accountsService: { list: vi.fn().mockResolvedValue({ data: { data: { accounts: [] } } }) },
  cannedResponsesService: { use: vi.fn() },
  getRequestHeaders: vi.fn(() => ({})),
}))
vi.mock('vue-sonner', () => ({
  toast: { error: mocks.toastError, success: vi.fn(), info: vi.fn(), warning: vi.fn() },
}))

function laneDContact(id: string, overrides: Record<string, unknown> = {}) {
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
    ...overrides,
  }
}

function mountChatView() {
  return shallowMount(ChatViewComponent, {
    global: {
      mocks: { $t: (key: string, fallback?: string) => fallback ?? key },
      stubs: {
        ScrollArea: { template: '<div><slot /></div>' },
        Transition: { template: '<div><slot /></div>' },
        Teleport: true,
        Tooltip: { template: '<div><slot /></div>' },
        TooltipTrigger: { template: '<div><slot /></div>' },
        TooltipContent: { template: '<div><slot /></div>' },
        Button: { template: '<button><slot /></button>' },
        Badge: { template: '<span><slot /></span>' },
        DropdownMenu: { template: '<div><slot /></div>' },
        DropdownMenuTrigger: { template: '<div><slot /></div>' },
        DropdownMenuContent: { template: '<div><slot /></div>' },
      },
    },
  })
}

function grant(...keys: string[]) {
  mocks.hasPermission.mockImplementation((resource: string, action: string) =>
    keys.includes(`${resource}:${action}`),
  )
  mocks.authStore!.permissionRevision += 1
}

describe('ChatView inbox state', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    mocks.infiniteControllers = []
    mocks.organizationsStore!.selectedOrgId = null
    Object.assign(mocks.authStore!, { isAuthenticated: true, organizationId: 'organization-1', user: { id: 'agent-1' } })
    mocks.routeSource.params.contactId = 'first'
    if (mocks.route) mocks.route.params.contactId = 'first'
    const first = laneDContact('first')
    const second = laneDContact('second', { unread_count: 2 })
    Object.assign(mocks.contactsStore!, {
      contacts: [first, second],
      sortedContacts: [first, second],
      currentContact: null,
      messages: [],
      searchQuery: '',
      listFilter: 'all',
      isLoadingMessages: false,
    })
    mocks.setCurrentContact.mockImplementation(value => {
      mocks.contactsStore!.currentContact = value
    })
    grant()
    mocks.fetchContacts.mockResolvedValue(undefined)
    mocks.fetchMessages.mockResolvedValue(undefined)
    mocks.markRead.mockResolvedValue({ data: { data: { cursor_synced: true } } })
    mocks.markContactReadState.mockResolvedValue(undefined)
    mocks.fetchUsers.mockResolvedValue(undefined)
    mocks.assignableMembers.mockResolvedValue({ data: { data: { members: [], total: 0 } } })
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      callback(0)
      return 1
    })
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() })
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

  async function mountSettled() {
    wrapper = mountChatView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await nextTick()
    return wrapper
  }

  it('changes the message-menu scope before a requested contact loads and on auth/org changes', async () => {
    const view = await mountSettled()
    const menu = () => view.findComponent({ name: 'MessageContextMenu' })
    let previous = menu().props('scopeKey')
    mocks.fetchMessages.mockImplementation(() => new Promise(() => {}))
    mocks.route!.params.contactId = 'second'
    await nextTick()
    expect(menu().props('scopeKey')).not.toBe(previous)
    previous = menu().props('scopeKey')
    mocks.organizationsStore!.selectedOrgId = 'organization-2'
    await nextTick()
    expect(menu().props('scopeKey')).not.toBe(previous)
    previous = menu().props('scopeKey')
    mocks.authStore!.user = { id: 'agent-2' }
    await nextTick()
    expect(menu().props('scopeKey')).not.toBe(previous)
    previous = menu().props('scopeKey')
    mocks.authStore!.isAuthenticated = false
    await nextTick()
    expect(menu().props('scopeKey')).not.toBe(previous)
  })

  it('loads the default list and wires the filter chips to the store', async () => {
    const view = await mountSettled()
    expect(mocks.fetchContacts.mock.calls[0]).toEqual([])
    const chips = view.findComponent({ name: 'ChatListFilters' })
    expect(chips.props('modelValue')).toBe('all')
    chips.vm.$emit('update:modelValue', 'unread')
    expect(mocks.setListFilter).toHaveBeenCalledWith('unread')
  })

  it('offers Unassigned only to users who can read every contact', async () => {
    let view = await mountSettled()
    expect(view.findComponent({ name: 'ChatListFilters' }).props('showUnassigned')).toBe(false)
    view.unmount()
    grant('contacts:read')
    view = await mountSettled()
    expect(view.findComponent({ name: 'ChatListFilters' }).props('showUnassigned')).toBe(true)
  })

  it('keeps one chat-contact row per contact, each with its own actions menu and badge', async () => {
    const view = await mountSettled()
    const rows = view.findAll('[data-testid="chat-contact"]')
    expect(rows.map(row => row.attributes('data-contact-id'))).toEqual(['first', 'second'])
    expect(view.findAllComponents({ name: 'ChatRowActionsMenu' })).toHaveLength(2)
    expect(view.findAllComponents({ name: 'ContactAssigneeBadge' }).length).toBeGreaterThanOrEqual(2)
  })

  it('shows a dot for a chat marked unread with no unread messages', async () => {
    mocks.contactsStore!.sortedContacts = [laneDContact('first', { manual_unread: true }), laneDContact('second')]
    const view = await mountSettled()
    const dots = view.findAll('[data-testid="chat-contact-manual-unread"]')
    expect(dots).toHaveLength(1)
    expect(view.get('[data-contact-id="first"]').find('[data-testid="chat-contact-manual-unread"]').exists()).toBe(true)
  })

  it('shows the Assign control from contacts:write instead of the role name', async () => {
    let view = await mountSettled()
    expect(view.find('[aria-label="chat.assignToAgent"]').exists()).toBe(false)
    view.unmount()
    grant('contacts:write')
    view = await mountSettled()
    expect(view.find('[aria-label="chat.assignToAgent"]').exists()).toBe(true)
  })

  it('loads assignable people from organization members without users:read', async () => {
    grant('contacts:write', 'organizations:read')
    await mountSettled()
    expect(mocks.assignableMembers).toHaveBeenCalledTimes(1)
    expect(mocks.fetchUsers).not.toHaveBeenCalled()
  })

  it('keeps the users list for callers with users:read', async () => {
    grant('contacts:write', 'users:read', 'organizations:read')
    await mountSettled()
    expect(mocks.fetchUsers).toHaveBeenCalledTimes(1)
    expect(mocks.assignableMembers).not.toHaveBeenCalled()
  })

  it('marks a row read and refreshes the navbar unread count', async () => {
    const view = await mountSettled()
    view.findAllComponents({ name: 'ChatRowActionsMenu' })[1].vm.$emit('mark-read', 'second')
    await flushPromises()
    expect(mocks.markContactReadState).toHaveBeenCalledWith('second', 'read')
    expect(mocks.refreshUnread).toHaveBeenCalledWith('organization-1', 'agent-1')
    expect(mocks.routerPush).not.toHaveBeenCalled()
  })

  it('leaves the open chat after marking it unread', async () => {
    grant('chat:write')
    const view = await mountSettled()
    expect(mocks.contactsStore!.currentContact?.id).toBe('first')
    view.findAllComponents({ name: 'ChatRowActionsMenu' })[0].vm.$emit('mark-unread', 'first')
    await flushPromises()
    expect(mocks.markContactReadState).toHaveBeenCalledWith('first', 'unread')
    expect(mocks.setCurrentContact).toHaveBeenLastCalledWith(null)
    expect(mocks.clearMessages).toHaveBeenCalled()
    expect(mocks.routerPush).toHaveBeenCalledWith('/chat')
  })

  it('reports a failed manual read-state change', async () => {
    mocks.markContactReadState.mockRejectedValue(new Error('forbidden'))
    const view = await mountSettled()
    view.findAllComponents({ name: 'ChatRowActionsMenu' })[0].vm.$emit('mark-unread', 'first')
    await flushPromises()
    expect(mocks.toastError).toHaveBeenCalled()
    expect(mocks.routerPush).not.toHaveBeenCalled()
  })

  it('offers header read-state items from the chat state', async () => {
    grant('chat:write')
    let view = await mountSettled()
    expect(view.find('[data-testid="chat-menu-mark-unread"]').exists()).toBe(true)
    expect(view.find('[data-testid="chat-menu-mark-read"]').exists()).toBe(false)
    view.unmount()
    Object.assign(mocks.contactsStore!.contacts[0], { manual_unread: true })
    view = await mountSettled()
    expect(view.find('[data-testid="chat-menu-mark-read"]').exists()).toBe(true)
  })

  it('acknowledges a manually unread chat again with the same automatic body', async () => {
    const focusSpy = vi.spyOn(document, 'hasFocus').mockReturnValue(true)
    const visibilitySpy = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
    try {
      await mountSettled()
      const messagesController = mocks.infiniteControllers[1]
      const viewport = document.createElement('div')
      Object.defineProperties(viewport, {
        scrollHeight: { configurable: true, value: 1_000 },
        clientHeight: { configurable: true, value: 300 },
      })
      viewport.scrollTop = 700
      messagesController.getViewport.mockReturnValue(viewport)
      mocks.contactsStore!.messages = [{ id: 'message-latest', direction: 'incoming', created_at: '2026-08-23T09:00:00Z' }]
      await nextTick()

      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()
      expect(mocks.markRead).toHaveBeenCalledTimes(1)
      expect(mocks.markRead).toHaveBeenLastCalledWith('first', 'message-latest', 'organization-1')

      // Without a manual mark the same cursor is not sent twice.
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()
      expect(mocks.markRead).toHaveBeenCalledTimes(1)

      // A teammate marked it unread: the next boundary read acknowledges it
      // again, through the unchanged automatic request.
      mocks.contactsStore!.currentContact!.manual_unread = true
      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()
      expect(mocks.markRead).toHaveBeenCalledTimes(2)
      expect(mocks.markRead).toHaveBeenLastCalledWith('first', 'message-latest', 'organization-1')
      expect(mocks.contactsStore!.currentContact!.manual_unread).toBe(false)

      messagesController.onScroll?.({ target: viewport } as unknown as Event)
      await flushPromises()
      expect(mocks.markRead).toHaveBeenCalledTimes(2)
    } finally {
      focusSpy.mockRestore()
      visibilitySpy.mockRestore()
    }
  })
})
