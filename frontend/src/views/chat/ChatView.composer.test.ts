/** @vitest-environment happy-dom */

// Composer wiring in ChatView: multi-file attachments (paperclip, paste,
// drop), formatting shortcuts/toolbar, caret-aware emoji and the WhatsApp
// text renderer in bubbles. Child components are stubbed; their own tests
// cover their internals.

import { flushPromises, shallowMount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ChatViewComponent from './ChatView.vue'
import MediaSendDialog from '@/components/chat/MediaSendDialog.vue'
import ComposerFormatToolbar from '@/components/chat/ComposerFormatToolbar.vue'
import WhatsAppText from '@/components/chat/WhatsAppText.vue'
import ChatDropZoneOverlay from '@/components/chat/ChatDropZoneOverlay.vue'

const mocks = vi.hoisted(() => ({
  routeSource: { params: { contactId: 'first' as string | undefined } },
  route: null as { params: { contactId?: string } } | null,
  contactsStore: null as Record<string, any> | null,
  sendMedia: vi.fn(),
  fetchMessages: vi.fn(),
  setCurrentContact: vi.fn(),
  toastSuccess: vi.fn(),
  toastWarning: vi.fn(),
  toastError: vi.fn(),
  convertImageToJpeg: vi.fn(),
  t: vi.fn((key: string) => key),
  blockOrganizationSwitch: vi.fn(),
  unblockOrganizationSwitch: vi.fn(),
  organizationsStore: null as any,
  realtimeListeners: new Set<(event: { type?: string; payload?: any }) => void>(),
}))

vi.mock('vue-router', async () => {
  const { reactive } = await import('vue')
  const route = reactive(mocks.routeSource)
  mocks.route = route
  return { useRoute: () => route, useRouter: () => ({ push: vi.fn() }) }
})

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: mocks.t }),
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
  useInfiniteScroll: () => ({
    scrollAreaRef: { value: null },
    setup: vi.fn(),
    cleanup: vi.fn(),
    getViewport: vi.fn(() => null),
    preserveScrollPosition: vi.fn(async (callback: () => Promise<void>) => callback()),
  }),
}))

vi.mock('@/stores/contacts', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    contacts: [] as Array<Record<string, unknown>>,
    sortedContacts: [] as Array<Record<string, unknown>>,
    currentContact: null as Record<string, any> | null,
    messages: [] as Array<Record<string, unknown>>,
    messagesOrganizationScope: '',
    replyingTo: null,
    searchQuery: '',
    selectedTags: [] as string[],
    hasMoreContacts: false,
    hasMoreMessages: false,
    isLoadingMessages: false,
    isLoadingMoreContacts: false,
    isLoadingOlderMessages: false,
    fetchContacts: vi.fn(async () => undefined),
    fetchContact: vi.fn(),
    fetchMessages: mocks.fetchMessages,
    refreshCurrentMessages: vi.fn(async () => undefined),
    fetchOlderMessages: vi.fn(),
    loadMoreContacts: vi.fn(),
    setCurrentContact: mocks.setCurrentContact,
    setAccountFilter: vi.fn(),
    clearMessages: vi.fn(),
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
    normalizeContactSearch: (raw: string) => raw.trim(),
    useContactsStore: () => store,
  }
})

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isAuthenticated: true,
    organizationId: 'organization-1',
    userRole: 'agent',
    user: { id: 'agent-1' },
    permissionRevision: 0,
    hasPermission: () => false,
    hasProductEntitlement: () => false,
  }),
}))
vi.mock('@/stores/omnichannelUnread', () => ({ useOmnichannelUnreadStore: () => ({ refresh: vi.fn() }) }))
vi.mock('@/stores/organizations', async () => {
  const { reactive } = await import('vue')
  const store = reactive({
    selectedOrgId: null as string | null,
    blockOrganizationSwitch: mocks.blockOrganizationSwitch,
    unblockOrganizationSwitch: mocks.unblockOrganizationSwitch,
  })
  mocks.organizationsStore = store
  return { useOrganizationsStore: () => store }
})
vi.mock('@/stores/users', () => ({ useUsersStore: () => ({ users: [], fetchUsers: vi.fn(async () => undefined) }) }))
vi.mock('@/stores/transfers', () => ({
  useTransfersStore: () => ({
    activeByContact: {},
    fetchTransfers: vi.fn(async () => undefined),
    fetchActiveTransferForContact: vi.fn(async () => undefined),
    upsertTransfer: vi.fn(),
    updateTransfer: vi.fn(),
    getActiveTransferForContact: () => undefined,
  }),
}))
vi.mock('@/stores/tags', () => ({
  useTagsStore: () => ({ tags: [], fetchTags: vi.fn(async () => undefined), getTagByName: () => null }),
}))
vi.mock('@/stores/notes', () => ({
  useNotesStore: () => ({ notes: [], hasMore: false, fetchNotes: vi.fn(async () => undefined), clearNotes: vi.fn() }),
}))
vi.mock('@/services/websocket', () => ({
  wsService: {
    setCurrentContact: vi.fn(),
    onInboxActivity: (listener: (event: { type?: string; payload?: any }) => void) => {
      mocks.realtimeListeners.add(listener)
      return () => mocks.realtimeListeners.delete(listener)
    },
  },
}))
vi.mock('@/services/api', () => ({
  effectiveAIIsAllowed: () => true,
  contactsService: {
    getSessionData: vi.fn(async () => {
      throw new Error('not configured')
    }),
    markRead: vi.fn(async () => ({ data: { data: { cursor_synced: true } } })),
    assign: vi.fn(),
  },
  chatbotService: { createTransfer: vi.fn(), resumeTransfer: vi.fn() },
  messagesService: { sendReaction: vi.fn(), sendMedia: mocks.sendMedia },
  customActionsService: { list: vi.fn(async () => ({ data: { data: [] } })), execute: vi.fn() },
  accountsService: { list: vi.fn(async () => ({ data: { data: { accounts: [] } } })) },
  cannedResponsesService: { use: vi.fn() },
}))
vi.mock('@/lib/chatImageConversion', () => ({ convertImageToJpeg: mocks.convertImageToJpeg }))
vi.mock('vue3-emoji-picker', () => ({
  default: { name: 'EmojiPickerMock', emits: ['select'], template: '<div data-testid="emoji-picker-mock" />' },
}))
vi.mock('vue3-emoji-picker/css', () => ({}))
vi.mock('vue-sonner', () => ({
  toast: { error: mocks.toastError, success: mocks.toastSuccess, info: vi.fn(), warning: mocks.toastWarning },
}))

function contact(id: string) {
  return {
    id,
    phone_number: `6012345${id.length}`,
    name: `Patient ${id}`,
    status: 'active',
    tags: [],
    metadata: {},
    unread_count: 0,
    identity_review_ai_state: { known: true, ai_allowed: true, blocked: false, open_hold_count: 0, reason: '' },
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function png(name: string) {
  return new File(['png'], name, { type: 'image/png' })
}

function mountView() {
  return shallowMount(ChatViewComponent, {
    attachTo: document.body,
    global: {
      renderStubDefaultSlot: true,
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
      },
    },
  })
}

async function settle() {
  await flushPromises()
  await vi.advanceTimersByTimeAsync(60)
  await nextTick()
}

function setFiles(input: HTMLInputElement, files: File[]) {
  Object.defineProperty(input, 'files', { configurable: true, value: files })
}

describe('ChatView composer', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    mocks.route!.params.contactId = 'first'
    const first = contact('first')
    const second = contact('second')
    Object.assign(mocks.contactsStore!, {
      contacts: [first, second],
      sortedContacts: [first, second],
      currentContact: null,
      messages: [],
    })
    mocks.setCurrentContact.mockImplementation(value => {
      mocks.contactsStore!.currentContact = value
    })
    mocks.fetchMessages.mockResolvedValue(undefined)
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      callback(0)
      return 1
    })
    vi.stubGlobal(
      'ResizeObserver',
      class {
        observe = vi.fn()
        unobserve = vi.fn()
        disconnect = vi.fn()
      },
    )
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() })
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
    for (const key of ['whm_session_id', 'selected_organization_id', 'user']) localStorage.removeItem(key)
    if (mocks.organizationsStore) mocks.organizationsStore.selectedOrgId = null
    vi.runOnlyPendingTimers()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('keeps the canary composer contract', async () => {
    wrapper = mountView()
    await settle()
    const form = wrapper.findAll('form').find(f => f.find('textarea').exists())!
    const inputs = form.findAll('input[type="file"]')
    expect(inputs).toHaveLength(1)
    expect(wrapper.findAll('input[type="file"]')).toHaveLength(1)
    expect(inputs[0].attributes('multiple')).toBeDefined()
    expect(inputs[0].attributes('accept')).toContain('image/*')
    expect(form.get('textarea').attributes('placeholder')).toBe('chat.typeMessage...')

    const submit = form.findAll('button[type="submit"]')
    expect(submit).toHaveLength(1)
    expect(submit[0].attributes('disabled')).toBeDefined()
    await form.get('textarea').setValue('   ')
    expect(submit[0].attributes('disabled')).toBeDefined()

    // The formatting toolbar sits outside the form and adds no submit buttons.
    expect(wrapper.findComponent(ComposerFormatToolbar).exists()).toBe(true)
    expect(form.findComponent(ComposerFormatToolbar).exists()).toBe(false)
  })

  it('stages every chosen file and sends them one request at a time', async () => {
    wrapper = mountView()
    await settle()
    const input = wrapper.get('input[type="file"]')
    setFiles(input.element as HTMLInputElement, [png('a.png'), png('b.png'), png('c.png')])
    await input.trigger('change')
    await nextTick()

    const dialog = wrapper.findComponent(MediaSendDialog)
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('items')).toHaveLength(3)
    expect(dialog.props('recipientName')).toBe('Patient first')

    const calls: Array<ReturnType<typeof deferred<unknown>>> = []
    mocks.sendMedia.mockImplementation(() => {
      const call = deferred<unknown>()
      calls.push(call)
      return call.promise
    })

    dialog.vm.$emit('send')
    await flushPromises()
    expect(mocks.sendMedia).toHaveBeenCalledTimes(1)
    expect(mocks.sendMedia.mock.calls[0][0]).toMatchObject({ contact_id: 'first', type: 'image' })
    expect(mocks.sendMedia.mock.calls[0][0].file.name).toBe('a.png')
    expect(wrapper.findComponent(MediaSendDialog).props('sending')).toBe(true)

    calls[0].resolve({ data: { data: { id: 'msg-a' } } })
    await flushPromises()
    expect(mocks.contactsStore!.addMessage).toHaveBeenCalledWith({ id: 'msg-a' })
    expect(mocks.sendMedia).toHaveBeenCalledTimes(2)
    expect(mocks.sendMedia.mock.calls[1][0].file.name).toBe('b.png')

    calls[1].resolve({ data: { data: { id: 'msg-b' } } })
    await flushPromises()
    calls[2].resolve({ data: { data: { id: 'msg-c' } } })
    await flushPromises()
    expect(mocks.sendMedia).toHaveBeenCalledTimes(3)
    expect(mocks.toastSuccess).toHaveBeenCalledWith('chat.composer.batchSentMany')
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(false)
    // The input is cleared so the same files can be picked again.
    expect((input.element as HTMLInputElement).value).toBe('')
  })

  it('warns when more than 10 files are chosen', async () => {
    wrapper = mountView()
    await settle()
    const input = wrapper.get('input[type="file"]')
    setFiles(input.element as HTMLInputElement, Array.from({ length: 12 }, (_, i) => png(`p${i}.png`)))
    await input.trigger('change')
    expect(wrapper.findComponent(MediaSendDialog).props('items')).toHaveLength(10)
    expect(mocks.toastWarning).toHaveBeenCalledWith('chat.composer.tooManyFiles')
  })

  it('attaches pasted images but leaves text paste alone', async () => {
    wrapper = mountView()
    await settle()
    const textarea = wrapper.get('textarea')

    const textPaste = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent
    Object.defineProperty(textPaste, 'clipboardData', {
      value: {
        getData: (type: string) => (type === 'text/html' ? '<p>hello</p>' : 'hello'),
        files: [png('x.png')],
        items: [],
        types: ['text/plain', 'text/html'],
      },
    })
    textarea.element.dispatchEvent(textPaste)
    expect(textPaste.defaultPrevented).toBe(false)
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(false)

    const imagePaste = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent
    Object.defineProperty(imagePaste, 'clipboardData', {
      value: { getData: () => '', files: [png('screenshot.png')], items: [], types: ['Files'] },
    })
    textarea.element.dispatchEvent(imagePaste)
    await nextTick()
    expect(imagePaste.defaultPrevented).toBe(true)
    const dialog = wrapper.findComponent(MediaSendDialog)
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('items')[0].file.name).toBe('screenshot.png')
  })

  it('wires the drop overlay to the chat panel and stages dropped files', async () => {
    wrapper = mountView()
    await settle()
    const overlay = wrapper.findComponent(ChatDropZoneOverlay)
    expect(overlay.exists()).toBe(true)
    const target = overlay.props('target') as HTMLElement
    expect(target).toBeInstanceOf(HTMLElement)
    expect(target.contains(wrapper.get('textarea').element)).toBe(true)
    expect(target.classList.contains('relative')).toBe(true)
    expect(overlay.props('recipientName')).toBe('Patient first')

    overlay.vm.$emit('files', [png('dropped.png')])
    await nextTick()
    expect(wrapper.findComponent(MediaSendDialog).props('items')[0].file.name).toBe('dropped.png')
  })

  it('drops staged files when the conversation changes', async () => {
    wrapper = mountView()
    await settle()
    wrapper.findComponent(ChatDropZoneOverlay).vm.$emit('files', [png('for-first.png')])
    await nextTick()
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(true)

    mocks.route!.params.contactId = 'second'
    await settle()
    const dialog = wrapper.findComponent(MediaSendDialog)
    expect(dialog.props('open')).toBe(false)
    expect(dialog.props('items')).toHaveLength(0)
  })

  it('wraps the selection with Ctrl+B / Ctrl+I / Ctrl+Shift+X and the toolbar', async () => {
    wrapper = mountView()
    await settle()
    const textarea = wrapper.get('textarea')
    const element = textarea.element as HTMLTextAreaElement
    await textarea.setValue('take this now')

    element.setSelectionRange(5, 9)
    await textarea.trigger('keydown', { key: 'b', ctrlKey: true })
    await nextTick()
    expect(element.value).toBe('take *this* now')
    expect([element.selectionStart, element.selectionEnd]).toEqual([6, 10])

    await textarea.trigger('keydown', { key: 'i', ctrlKey: true })
    await nextTick()
    expect(element.value).toBe('take *_this_* now')

    element.setSelectionRange(0, 4)
    await textarea.trigger('keydown', { key: 'X', ctrlKey: true, shiftKey: true })
    await nextTick()
    expect(element.value).toBe('~take~ *_this_* now')

    element.setSelectionRange(element.value.length, element.value.length)
    wrapper.findComponent(ComposerFormatToolbar).vm.$emit('format', 'mono')
    await nextTick()
    expect(element.value).toBe('~take~ *_this_* now``````')
    // A plain Enter still sends; the format handler leaves it alone.
    expect(mocks.contactsStore!.sendMessage).not.toHaveBeenCalled()
  })

  it('inserts emoji at the caret instead of appending', async () => {
    wrapper = mountView()
    await settle()
    const textarea = wrapper.get('textarea')
    const element = textarea.element as HTMLTextAreaElement
    await textarea.setValue('Hi there')
    element.setSelectionRange(2, 2)

    await flushPromises()
    // shallowMount stubs the lazily loaded picker; drive its select event.
    const picker = wrapper.findAllComponents({ name: 'EmojiPicker' })
      .find(component => component.element.tagName === 'EMOJI-PICKER-STUB')!
    expect(picker?.exists()).toBe(true)
    picker.vm.$emit('select', { i: '👋' })
    await nextTick()
    expect(element.value).toBe('Hi👋 there')
    expect(element.selectionStart).toBe(4)
  })

  it('renders bubble text and captions through WhatsAppText', async () => {
    mocks.contactsStore!.messages = [
      {
        id: 'text-1',
        direction: 'incoming',
        message_type: 'text',
        content: { body: '*Bold* hello' },
        status: 'received',
        created_at: '2026-08-24T04:00:00Z',
      },
      {
        id: 'doc-1',
        direction: 'outgoing',
        message_type: 'document',
        content: { body: 'Your _results_' },
        status: 'sent',
        created_at: '2026-08-24T04:01:00Z',
      },
    ]
    wrapper = mountView()
    await settle()
    const texts = wrapper.findAllComponents(WhatsAppText).map(c => c.props('text'))
    expect(texts).toEqual(['*Bold* hello', 'Your _results_'])
  })

  it('tells the dialog when the 24-hour window is closed and wires conversion', async () => {
    wrapper = mountView()
    await settle()
    expect(wrapper.findComponent(MediaSendDialog).props('serviceWindowExpired')).toBe(false)
    mocks.contactsStore!.currentContact = { ...mocks.contactsStore!.currentContact, service_window_open: false }
    await nextTick()
    expect(wrapper.findComponent(MediaSendDialog).props('serviceWindowExpired')).toBe(true)

    wrapper.findComponent(ChatDropZoneOverlay).vm.$emit('files', [new File(['x'], 'IMG.HEIC', { type: 'image/heic' })])
    await nextTick()
    const [heic] = wrapper.findComponent(MediaSendDialog).props('items')
    expect(heic.status).toBe('invalid')
    mocks.convertImageToJpeg.mockResolvedValueOnce(new File(['jpg'], 'IMG.jpg', { type: 'image/jpeg' }))
    wrapper.findComponent(MediaSendDialog).vm.$emit('convert', heic.id)
    await flushPromises()
    expect(mocks.convertImageToJpeg).toHaveBeenCalledTimes(1)
    expect(wrapper.findComponent(MediaSendDialog).props('items')[0]).toMatchObject({
      status: 'ready',
      convertedFrom: 'image/heic',
    })
  })

  function sendMediaDeferred() {
    const calls: Array<{
      resolve: (value: unknown) => void
      reject: (error: unknown) => void
      signal: AbortSignal
      request: any
      progress: (loaded: number, total: number | undefined) => void
    }> = []
    mocks.sendMedia.mockImplementation(
      (request: any, options: { signal: AbortSignal; onUploadProgress: (loaded: number, total: number | undefined) => void }) =>
        new Promise((resolve, reject) => {
          options.signal.addEventListener('abort', () => reject(new Error('canceled')))
          calls.push({ resolve, reject, signal: options.signal, request, progress: options.onUploadProgress })
        }),
    )
    return calls
  }

  async function stageAndSend(names: string[]) {
    wrapper!.findComponent(ChatDropZoneOverlay).vm.$emit('files', names.map(png))
    await nextTick()
    wrapper!.findComponent(MediaSendDialog).vm.$emit('send')
    await flushPromises()
  }

  it('keeps sending to the confirmed patient after the agent switches chats, and reports by name', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['lab-1.png', 'lab-2.png'])
    expect(calls).toHaveLength(1)

    mocks.route!.params.contactId = 'second'
    await settle()
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(false)
    // A new batch is refused until the running one settles.
    wrapper.findComponent(ChatDropZoneOverlay).vm.$emit('files', [png('for-second.png')])
    await nextTick()
    expect(mocks.toastWarning).toHaveBeenCalledWith('chat.composer.busySending')

    calls[0].resolve({ data: { data: { id: 'msg-1', status: 'sent' } } })
    await flushPromises()
    expect(calls).toHaveLength(2)
    expect(calls[1].request).toMatchObject({ contact_id: 'first' })
    calls[1].resolve({ data: { data: { id: 'msg-2', status: 'sent' } } })
    await flushPromises()
    expect(mocks.toastSuccess).toHaveBeenCalledWith('chat.composer.batchSentToMany')
    // The second patient's open chat never received the first patient's files.
    expect(mocks.contactsStore!.addMessage).not.toHaveBeenCalled()
  })

  it('paces uploads on realtime status events', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png', 'b.png'])
    calls[0].resolve({ data: { data: { id: 'msg-a', status: 'pending' } } })
    await flushPromises()
    expect(calls).toHaveLength(1)
    for (const listener of mocks.realtimeListeners) {
      listener({ type: 'status_update', payload: { message_id: 'msg-a', status: 'sent' } })
    }
    await flushPromises()
    expect(calls).toHaveLength(2)
    calls[1].resolve({ data: { data: { id: 'msg-b', status: 'sent' } } })
    await flushPromises()
    expect(mocks.realtimeListeners.size).toBe(0)
  })

  it('stops a running batch from the dialog and reports what was not sent', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png', 'b.png'])
    expect(wrapper.findComponent(MediaSendDialog).props('sending')).toBe(true)
    wrapper.findComponent(MediaSendDialog).vm.$emit('stop')
    await nextTick()
    expect(calls[0].signal.aborted).toBe(true)
    await flushPromises()
    expect(calls).toHaveLength(1)
    const dialog = wrapper.findComponent(MediaSendDialog)
    expect(dialog.props('sending')).toBe(false)
    expect(dialog.props('items').map((item: { status: string }) => item.status)).toEqual(['unconfirmed', 'ready'])
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.batchReportWithUnconfirmed', {
      name: 'Patient first',
      sent: 0,
      failed: 0,
      notSent: 1,
      unconfirmed: 1,
    })
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.batchReportFiles', { files: 'b.png' })
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.batchReportUnconfirmedOne', { files: 'a.png' })
    expect(mocks.toastWarning).toHaveBeenCalledWith('chat.composer.batchReportWithUnconfirmed', {
      description: 'chat.composer.batchReportFiles chat.composer.batchReportUnconfirmedOne',
    })
  })

  it('ends a batch silently when the session ends', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['lab-1.png', 'lab-2.png'])
    window.dispatchEvent(new CustomEvent('rereply:session-ending'))
    expect(calls[0].signal.aborted).toBe(true)
    await flushPromises()
    expect(calls).toHaveLength(1)
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(false)
    expect(mocks.toastWarning).not.toHaveBeenCalled()
    expect(mocks.toastSuccess).not.toHaveBeenCalled()
    expect(mocks.toastError).not.toHaveBeenCalled()
  })

  it('reports a file left unconfirmed by the response deadline as a warning, not an error', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['scan.png'])
    calls[0].progress(10, 10)
    await vi.advanceTimersByTimeAsync(120_000)
    await flushPromises()
    expect(calls[0].signal.aborted).toBe(true)
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.batchReportWithUnconfirmed', {
      name: 'Patient first',
      sent: 0,
      failed: 0,
      notSent: 0,
      unconfirmed: 1,
    })
    expect(mocks.toastWarning).toHaveBeenCalledWith('chat.composer.batchReportWithUnconfirmed', {
      description: 'chat.composer.batchReportUnconfirmedOne',
    })
    expect(mocks.toastError).not.toHaveBeenCalled()
  })

  it('leaves the unconfirmed part out of the headline when nothing is unconfirmed', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png', 'b.png'])
    calls[0].reject(new Error('Failed to send message'))
    await flushPromises()
    calls[1].reject(new Error('Failed to send message'))
    await flushPromises()
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.batchReport', {
      name: 'Patient first',
      sent: 0,
      failed: 2,
      notSent: 0,
    })
    expect(mocks.t).not.toHaveBeenCalledWith('chat.composer.batchReportWithUnconfirmed', expect.anything())
    // Nothing reached the server and both failed: a red error toast.
    expect(mocks.toastError).toHaveBeenCalledWith('chat.composer.batchReport', {
      description: 'chat.composer.batchReportFiles',
    })
  })

  it('uses the plural wording for several unconfirmed files', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png', 'b.png'])
    calls[0].progress(10, 10)
    await vi.advanceTimersByTimeAsync(120_000)
    await flushPromises()
    calls[1].progress(10, 10)
    await vi.advanceTimersByTimeAsync(120_000)
    await flushPromises()
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.batchReportUnconfirmedMany', { files: 'a.png, b.png' })
    expect(mocks.toastWarning).toHaveBeenCalled()
    expect(mocks.toastError).not.toHaveBeenCalled()
  })

  it('blocks workspace switching while files are sending', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png'])
    expect(mocks.blockOrganizationSwitch).toHaveBeenCalledWith(expect.stringMatching(/^chat-media-batch:\d+$/), 'chat.composer.workspaceSwitchBlocked')
    const holdId = mocks.blockOrganizationSwitch.mock.calls[0][0]
    expect(mocks.t).toHaveBeenCalledWith('chat.composer.workspaceSwitchBlocked', { name: 'Patient first' })
    expect(mocks.unblockOrganizationSwitch).not.toHaveBeenCalled()
    calls[0].resolve({ data: { data: { id: 'msg-1', status: 'sent' } } })
    await flushPromises()
    expect(mocks.unblockOrganizationSwitch).toHaveBeenCalledWith(holdId)
  })

  it('stops the batch and shows nothing when another tab switches organization', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png', 'b.png'])
    localStorage.setItem('selected_organization_id', 'org-elsewhere')
    window.dispatchEvent(new Event('storage'))
    expect(calls[0].signal.aborted).toBe(true)
    await flushPromises()
    expect(calls).toHaveLength(1)
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(false)
    expect(mocks.toastWarning).not.toHaveBeenCalled()
    expect(mocks.toastSuccess).not.toHaveBeenCalled()
    expect(mocks.toastError).not.toHaveBeenCalled()
    expect(mocks.unblockOrganizationSwitch).toHaveBeenCalledWith(expect.stringMatching(/^chat-media-batch:/))
    localStorage.removeItem('selected_organization_id')
  })

  it('reports a gateway timeout after the full upload as may-have-been-sent, in a warning', async () => {
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['scan.png'])
    calls[0].progress(10, 10)
    calls[0].reject(Object.assign(new Error('Request failed with status code 504'), { response: { status: 504, data: '' } }))
    await flushPromises()
    expect(wrapper.findComponent(MediaSendDialog).props('items')[0].status).toBe('unconfirmed')
    expect(mocks.toastWarning).toHaveBeenCalledWith('chat.composer.batchReportWithUnconfirmed', {
      description: 'chat.composer.batchReportUnconfirmedOne',
    })
    expect(mocks.toastError).not.toHaveBeenCalled()
  })

  it('B-T2: ends the batch when the logical session changes (same user signs in again)', async () => {
    localStorage.setItem('whm_session_id', 's1')
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png', 'b.png'])
    localStorage.setItem('whm_session_id', 's2')
    window.dispatchEvent(new StorageEvent('storage', { key: 'whm_session_id' }))
    expect(calls[0].signal.aborted).toBe(true)
    await flushPromises()
    expect(calls).toHaveLength(1)
    expect(wrapper.findComponent(MediaSendDialog).props('open')).toBe(false)
    expect(mocks.contactsStore!.addMessage).not.toHaveBeenCalled()
    expect(mocks.toastWarning).not.toHaveBeenCalled()
    expect(mocks.toastSuccess).not.toHaveBeenCalled()
    expect(mocks.toastError).not.toHaveBeenCalled()
    expect(mocks.unblockOrganizationSwitch).toHaveBeenCalledWith(expect.stringMatching(/^chat-media-batch:/))
  })

  it('B-WS-3: the switcher initialising its own organization mid-batch does not cancel it', async () => {
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-H' }))
    wrapper = mountView()
    await settle()
    const calls = sendMediaDeferred()
    await stageAndSend(['a.png'])
    // OrganizationSwitcher's onMounted: selectOrganization(own org) after a slow load.
    mocks.organizationsStore.selectedOrgId = 'org-H'
    localStorage.setItem('selected_organization_id', 'org-H')
    await nextTick()
    expect(calls[0].signal.aborted).toBe(false)
    calls[0].resolve({ data: { data: { id: 'msg-1', status: 'sent' } } })
    await flushPromises()
    expect(mocks.toastSuccess).toHaveBeenCalledWith('chat.composer.batchSentOne')
  })
})
