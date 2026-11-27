import { defineStore } from 'pinia'
import { ref, computed, toRaw, watch } from 'vue'
import {
  contactsService,
  messagesService,
  type ContactIdentityReviewEffectiveState,
} from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import { readWorkspaceIdentity } from '@/composables/useChatAttachments'
import { compareMessageIngestionOrder } from '@/lib/messageOrdering'
import { readSelectedOrganizationId } from '@/lib/browserIdentity'
import { currentTabSession, isTabSessionCurrent } from '@/lib/tabSession'

// Phones are stored without leading + or whitespace (see CreateContact in
// internal/handlers/contacts.go). Strip them from a digit-only query so a user
// typing "+91 98765 43210" still matches a stored "919876543210" via the
// server's substring LIKE.
export function normalizeContactSearch(raw: string): string {
  const trimmed = raw.trim().replace(/^\+/, '')
  if (trimmed && /^[\d\s+()-]+$/.test(trimmed)) {
    return trimmed.replace(/[\s+()-]/g, '')
  }
  return trimmed
}

export interface Contact {
  id: string
  phone_number: string
  name: string
  profile_name?: string
  avatar_url?: string
  status: string
  tags: string[]
  metadata: Record<string, any>
  last_message_at?: string
  last_inbound_at?: string
  service_window_open?: boolean
  unread_count: number
  // Team-shared "Mark as unread"; cleared by any read.
  manual_unread?: boolean
  assigned_user_id?: string
  assigned_user_name?: string
  // Agent of the active transfer, which also makes the chat theirs.
  transfer_agent_id?: string
  transfer_agent_name?: string
  whatsapp_account?: string
  marketing_opt_out?: boolean
  // Where profile_name came from: a manual CRM edit, the name saved in the
  // WhatsApp Business app, or the customer's own WhatsApp profile.
  name_source?: ContactNameSource
  // The customer's latest WhatsApp profile name or username.
  wa_profile_name?: string
  wa_business_saved_name?: string
  // The contact was removed from the WhatsApp Business app's contacts; its
  // last saved name is kept.
  wa_business_saved_name_removed?: boolean
  identity_review_ai_state: ContactIdentityReviewEffectiveState
  created_at: string
  updated_at: string
}

export type ContactNameSource = 'manual' | 'wa_business' | 'wa_profile'

// Chat list views. 'all' is the default list the release canary reads.
export type ContactListFilter = 'all' | 'unread' | 'mine' | 'unassigned'

export function contactListFilterParams(
  filter: ContactListFilter,
): { unread?: 'true'; assigned?: 'me' | 'unassigned' } {
  switch (filter) {
    case 'unread':
      return { unread: 'true' }
    case 'mine':
      return { assigned: 'me' }
    case 'unassigned':
      return { assigned: 'unassigned' }
    default:
      return {}
  }
}

const CONTACT_UPDATE_REFRESH_DELAY_MS = 250
const QUOTE_REFRESH_DELAY_MS = 150
const QUOTE_REFRESH_BATCH_SIZE = 50

export interface ReplyPreview {
  id: string
  content: any
  message_type: string
  direction: 'incoming' | 'outgoing'
  scoped_context?: true
  image_revision?: string
}

export interface Reaction {
  emoji: string
  from_phone?: string
  from_user?: string
}

export interface Message {
  id: string
  contact_id: string
  direction: 'incoming' | 'outgoing'
  message_type: string
  content: any
  media_url?: string
  media_mime_type?: string
  media_filename?: string
  interactive_data?: {
    type?: string
    body?: string
    buttons?: Array<{
      type?: string
      reply?: { id: string; title: string }
      id?: string
      title?: string
    }>
    rows?: Array<{
      id?: string
      title?: string
    }>
  }
  status: string
  wamid?: string
  error_message?: string
  is_reply?: boolean
  quote_pending?: boolean
  is_forwarded?: boolean
  reply_to_message_id?: string
  reply_to_message?: ReplyPreview
  reply_context_unavailable?: boolean
  bookmark_unavailable?: boolean
  reactions?: Reaction[]
  whatsapp_account?: string
  ingested_at?: string
  created_at: string
  updated_at: string
}

export const useContactsStore = defineStore('contacts', () => {
  const authStore = useAuthStore()
  const organizationsStore = useOrganizationsStore()
  const contacts = ref<Contact[]>([])
  const currentContact = ref<Contact | null>(null)
  const messages = ref<Message[]>([])
  const isLoading = ref(false)
  const contactListError = ref<'refresh' | 'more' | null>(null)
  let failedContactListParams: Parameters<typeof contactsService.list>[0]
  let failedContactListQuery: string | null = null
  const isLoadingMessages = ref(false)
  const isLoadingOlderMessages = ref(false)
  const hasMoreMessages = ref(false)
  const searchQuery = ref('')
  const selectedTags = ref<string[]>([])
  const replyingTo = ref<Message | null>(null)
  const accountFilter = ref<string | null>(null)
  const transcriptAccountFilter = computed(() => accountFilter.value)
  const listFilter = ref<ContactListFilter>('all')
  let contactUpdateRefreshHandle: ReturnType<typeof setTimeout> | null = null
  const pendingContactUpdates = new Set<string>()
  const pendingAssignmentUpdates = new Set<string>()
  let contactSelectionGeneration = 0
  let selectedAssignmentGeneration = 0
  const activeOrganizationScope = computed(
    () => organizationsStore.selectedOrgId || authStore.organizationId || '',
  )
  // Workspace the open transcript was requested from. Chat media must be
  // fetched from that same workspace, so it is captured when a transcript load
  // starts instead of following the live selection: a workspace switch clears
  // the transcript rather than refetching its media under the new workspace.
  const messagesOrganizationScope = ref('')

  function transcriptRequestOrganizationScope() {
    // Mirrors the API client's request interceptor: a persisted selection is
    // sent as X-Organization-ID, and without one the server uses the
    // session's own workspace.
    return readSelectedOrganizationId() || authStore.organizationId || ''
  }

  type ReplyContextOwner = { organizationId: string; isCurrent: () => boolean }
  const replyContextEpoch = ref(0)
  const replyContextIdentity = computed(() => JSON.stringify([
    authStore.isAuthenticated, authStore.user?.id, authStore.organizationId, activeOrganizationScope.value,
    authStore.user?.role?.id ?? authStore.user?.role_id, authStore.user?.is_super_admin,
    authStore.hasPermission('chat', 'read'), authStore.hasPermission('contacts', 'read'),
  ]))
  const replyContextProofs = new WeakMap<Message, {
    owner: ReplyContextOwner
    descriptor: string
    quote: ReplyPreview | undefined
  }>()
  // A websocket or locally merged row can reuse a message ID without carrying
  // the authenticated quote projection for that row.
  const transcriptRows = computed(() => new Set(messages.value.map(message => toRaw(message))))

  function retireReplyContexts() {
    replyContextEpoch.value++
  }

  watch(replyContextIdentity, () => {
    retireReplyContexts()
    cancelQuoteRefresh()
  }, { flush: 'sync' })

  function captureReplyContextOwner(contactId: string): ReplyContextOwner | undefined {
    const organizationId = activeOrganizationScope.value
    const actor = authStore.user?.id
    const session = currentTabSession()
    if (!organizationId || !actor || session === null || !authStore.isAuthenticated ||
      !authStore.hasPermission('chat', 'read')) return
    const browserIdentity = JSON.stringify([organizationId, authStore.organizationId ?? '', actor])
    const identity = replyContextIdentity.value
    const epoch = replyContextEpoch.value
    const isCurrent = () => replyContextEpoch.value === epoch && replyContextIdentity.value === identity &&
      currentContact.value?.id === contactId && messagesOrganizationScope.value === organizationId &&
      transcriptRequestOrganizationScope() === organizationId &&
      readWorkspaceIdentity(authStore.organizationId, authStore.user?.id) === browserIdentity &&
      isTabSessionCurrent() && currentTabSession() === session
    if (isCurrent()) return { organizationId, isCurrent }
  }

  function replyContextDescriptor(message: Message): string | undefined {
    const { id, contact_id, whatsapp_account, is_reply, reply_to_message_id, reply_to_message: quote } = message
    if (!id || !contact_id || typeof whatsapp_account !== 'string' || !whatsapp_account.trim() ||
      message.bookmark_unavailable) return
    if (message.reply_context_unavailable === true) {
      if (!quote) return JSON.stringify([id, contact_id, whatsapp_account, 'unavailable'])
      return
    }
    if (is_reply !== true || quote?.scoped_context !== true || typeof quote.id !== 'string' || !quote.id ||
      quote.id !== reply_to_message_id ||
      (quote.message_type !== 'image' && quote.message_type !== 'text') ||
      (quote.direction !== 'incoming' && quote.direction !== 'outgoing') ||
      typeof quote.content?.body !== 'string' ||
      (quote.image_revision !== undefined && typeof quote.image_revision !== 'string')) return
    return JSON.stringify([id, contact_id, whatsapp_account, quote.id, quote.message_type,
      quote.direction, quote.content.body, quote.image_revision])
  }

  function acceptReplyContexts(rows: Message[], owner: ReplyContextOwner | undefined, contactId: string, account?: string | null) {
    if (!owner?.isCurrent()) return
    for (const message of rows) {
      if (message.contact_id !== contactId || (account && message.whatsapp_account !== account)) continue
      const descriptor = replyContextDescriptor(message)
      if (!descriptor) continue
      const quote = message.reply_to_message
      replyContextProofs.set(toRaw(message), { owner, descriptor, quote: quote && toRaw(quote) })
    }
  }

  function ownsReplyContext(message: Message): boolean {
    const row = toRaw(message)
    const proof = replyContextProofs.get(row)
    const quote = message.reply_to_message
    return !!proof && proof.owner.isCurrent() && transcriptRows.value.has(row) &&
      (!accountFilter.value || message.whatsapp_account === accountFilter.value) &&
      proof.quote === (quote && toRaw(quote)) && proof.descriptor === replyContextDescriptor(message)
  }

  let identityGeneration = 0
  let contactLoadGeneration = 0
  let contactLoadAbortController: AbortController | null = null
  let moreContactsLoadGeneration = 0
  let moreContactsLoadAbortController: AbortController | null = null
  let messageLoadGeneration = 0
  let messageLoadAbortController: AbortController | null = null
  let messageRefreshQueued = false
  let quoteRefreshGeneration = 0
  let quoteRefreshController: AbortController | null = null
  let quoteRefreshTimer: ReturnType<typeof setTimeout> | null = null
  let quoteRefreshCursor = 0
  const quoteRefreshAccounts = new Set<string>()
  let olderMessageLoadGeneration = 0
  let olderMessageLoadAbortController: AbortController | null = null
  let searchDebounceHandle: ReturnType<typeof setTimeout> | null = null
  let searchDebounceGeneration = 0
  let suppressEmptySearchReset = false

  // Contacts pagination
  const contactsPage = ref(1)
  const contactsLimit = ref(50)
  const contactsTotal = ref(0)
  const isLoadingMoreContacts = ref(false)
  const hasMoreContacts = computed(() => contacts.value.length < contactsTotal.value)

  // Search is now driven server-side via fetchContacts({ search }), so the
  // visible list is whatever the server returned — no extra local filtering.
  // The one addition: under a non-default filter the open chat stays listed
  // after it stops matching (for example once it has been read), so the
  // selection does not vanish from under the reader. It is added only when
  // absent, which keeps exactly one row per contact.
  const filteredContacts = computed(() => {
    const open = currentContact.value
    if (
      listFilter.value === 'all' ||
      !open ||
      normalizeContactSearch(searchQuery.value) !== '' ||
      contacts.value.some(contact => contact.id === open.id)
    ) {
      return contacts.value
    }
    return [...contacts.value, open]
  })

  const sortedContacts = computed(() => {
    return [...filteredContacts.value].sort((a, b) => {
      const dateA = a.last_message_at ? new Date(a.last_message_at).getTime() : 0
      const dateB = b.last_message_at ? new Date(b.last_message_at).getTime() : 0
      return dateB - dateA
    })
  })

  function isAbortError(error: unknown) {
    return (
      (error instanceof DOMException && error.name === 'AbortError') ||
      (typeof error === 'object' && error !== null && 'code' in error && (error as any).code === 'ERR_CANCELED')
    )
  }

  function resetForIdentityChange() {
    retireReplyContexts()
    cancelQuoteRefresh()
    identityGeneration++
    contactSelectionGeneration++
    contactLoadGeneration++
    moreContactsLoadGeneration++
    messageLoadGeneration++
    olderMessageLoadGeneration++
    contactLoadAbortController?.abort()
    contactLoadAbortController = null
    moreContactsLoadAbortController?.abort()
    moreContactsLoadAbortController = null
    messageLoadAbortController?.abort()
    messageLoadAbortController = null
    olderMessageLoadAbortController?.abort()
    olderMessageLoadAbortController = null
    searchDebounceGeneration++
    if (searchDebounceHandle !== null) {
      clearTimeout(searchDebounceHandle)
      searchDebounceHandle = null
    }
    if (searchQuery.value !== '') {
      suppressEmptySearchReset = true
      searchQuery.value = ''
    }

    contacts.value = []
    currentContact.value = null
    messages.value = []
    messagesOrganizationScope.value = ''
    contactsPage.value = 1
    contactsTotal.value = 0
    isLoading.value = false
    contactListError.value = null
    failedContactListParams = undefined
    failedContactListQuery = null
    isLoadingMoreContacts.value = false
    isLoadingMessages.value = false
    isLoadingOlderMessages.value = false
    hasMoreMessages.value = false
    selectedTags.value = []
    replyingTo.value = null
    accountFilter.value = null
    listFilter.value = 'all'
    if (contactUpdateRefreshHandle !== null) {
      clearTimeout(contactUpdateRefreshHandle)
      contactUpdateRefreshHandle = null
    }
    pendingContactUpdates.clear()
    pendingAssignmentUpdates.clear()
    messageRefreshQueued = false
  }

  // Every list refresh (realtime, catch-up poll, search, tags, read
  // acknowledgement) inherits the active filter unless a caller overrides it.
  function contactListQuerySnapshot() {
    return JSON.stringify([
      normalizeContactSearch(searchQuery.value), selectedTags.value,
      listFilter.value, contactsLimit.value,
    ])
  }

  async function fetchContacts(params?: {
    search?: string
    page?: number
    limit?: number
    tags?: string
    unread?: 'true'
    assigned?: 'me' | 'unassigned'
  }) {
    const loadGeneration = ++contactLoadGeneration
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    const querySnapshot = contactListQuerySnapshot()
    contactLoadAbortController?.abort()
    moreContactsLoadGeneration++
    moreContactsLoadAbortController?.abort()
    moreContactsLoadAbortController = null
    isLoadingMoreContacts.value = false
    const controller = new AbortController()
    contactLoadAbortController = controller
    isLoading.value = true
    contactListError.value = null
    failedContactListParams = undefined
    failedContactListQuery = null
    try {
      const tagsParam = selectedTags.value.length > 0 ? selectedTags.value.join(',') : undefined
      const currentSearch = normalizeContactSearch(searchQuery.value) || undefined
      const response = await contactsService.list({
        page: 1,
        limit: contactsLimit.value,
        tags: tagsParam,
        search: currentSearch,
        ...contactListFilterParams(listFilter.value),
        ...params
      }, controller.signal)
      if (
        loadGeneration !== contactLoadGeneration ||
        identity !== identityGeneration ||
        organizationScope !== activeOrganizationScope.value
      ) return
      // API returns { status: "success", data: { contacts: [...], total: number } }
      const data = response.data.data || response.data
      contacts.value = data.contacts || []
      const selectedID = currentContact.value?.id
      if (selectedID) {
        const canonicalSelected = contacts.value.find(contact => contact.id === selectedID)
        if (canonicalSelected) currentContact.value = canonicalSelected
      }
      contactsTotal.value = data.total ?? contacts.value.length
      contactsPage.value = 1
    } catch (error) {
      if (
        loadGeneration === contactLoadGeneration &&
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value &&
        !isAbortError(error)
      ) {
        contactListError.value = 'refresh'
        failedContactListParams = params ? { ...params } : undefined
        failedContactListQuery = querySnapshot
        console.error('Failed to fetch contacts:', error)
      }
    } finally {
      if (loadGeneration === contactLoadGeneration) {
        isLoading.value = false
        if (contactLoadAbortController === controller) contactLoadAbortController = null
      }
    }
  }

  async function loadMoreContacts() {
    // A failed page waits for an explicit retry, rather than another scroll
    // observer notification repeatedly requesting the same failing page.
    if (contactListError.value || isLoading.value || isLoadingMoreContacts.value || !hasMoreContacts.value) return

    const loadGeneration = ++moreContactsLoadGeneration
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    const querySnapshot = contactListQuerySnapshot()
    moreContactsLoadAbortController?.abort()
    const controller = new AbortController()
    moreContactsLoadAbortController = controller
    isLoadingMoreContacts.value = true
    try {
      const nextPage = contactsPage.value + 1
      const tagsParam = selectedTags.value.length > 0 ? selectedTags.value.join(',') : undefined
      const search = normalizeContactSearch(searchQuery.value) || undefined
      const response = await contactsService.list({
        page: nextPage,
        limit: contactsLimit.value,
        tags: tagsParam,
        search,
        ...contactListFilterParams(listFilter.value),
      }, controller.signal)
      if (
        loadGeneration !== moreContactsLoadGeneration ||
        identity !== identityGeneration ||
        organizationScope !== activeOrganizationScope.value
      ) return
      const data = response.data.data || response.data
      const newContacts = data.contacts || []

      if (newContacts.length > 0) {
        // Append new contacts, avoiding duplicates
        const existingIds = new Set(contacts.value.map(c => c.id))
        const uniqueNew = newContacts.filter((c: Contact) => !existingIds.has(c.id))
        contacts.value = [...contacts.value, ...uniqueNew]
        contactsPage.value = nextPage
      }
      contactsTotal.value = data.total ?? contactsTotal.value
    } catch (error) {
      if (
        loadGeneration === moreContactsLoadGeneration &&
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value &&
        !isAbortError(error)
      ) {
        contactListError.value = 'more'
        failedContactListQuery = querySnapshot
        console.error('Failed to load more contacts:', error)
      }
    } finally {
      if (loadGeneration === moreContactsLoadGeneration) {
        isLoadingMoreContacts.value = false
        if (moreContactsLoadAbortController === controller) moreContactsLoadAbortController = null
      }
    }
  }

  async function retryContactList() {
    if (!contactListError.value || isLoading.value || isLoadingMoreContacts.value) return
    const phase = contactListError.value
    const params = failedContactListParams
    const queryChanged = failedContactListQuery !== contactListQuerySnapshot()
    contactListError.value = null
    failedContactListParams = undefined
    failedContactListQuery = null
    if (queryChanged) {
      // Search input changes before its debounced fetch starts. A retry in
      // that window must not replay old explicit params or append a new
      // query's page two to the previous query's cached rows.
      searchDebounceGeneration++
      if (searchDebounceHandle !== null) {
        clearTimeout(searchDebounceHandle)
        searchDebounceHandle = null
      }
      await fetchContacts()
    } else if (phase === 'more') await loadMoreContacts()
    else await fetchContacts(params)
  }

  async function fetchContact(id: string) {
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    try {
      const response = await contactsService.get(id)
      if (
        identity !== identityGeneration ||
        organizationScope !== activeOrganizationScope.value
      ) return null
      // API returns { status: "success", data: { ... } }
      const data = response.data.data || response.data
      return data
    } catch (error) {
      if (
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value
      ) {
        console.error('Failed to fetch contact:', error)
      }
      return null
    }
  }

  async function fetchMessages(
    contactId: string,
    params?: { page?: number; limit?: number; account?: string },
    options?: { preserveExisting?: boolean },
  ) {
    const preserveExisting = options?.preserveExisting === true
    const loadGeneration = ++messageLoadGeneration
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    messageLoadAbortController?.abort()
    // Let the canonical response establish the current quote state first.
    quoteRefreshController?.abort()
    quoteRefreshController = null
    const controller = new AbortController()
    messageLoadAbortController = controller
    if (!preserveExisting) {
      isLoadingMessages.value = true
      // Selection loads intentionally clear the previous contact immediately.
      messages.value = []
      messagesOrganizationScope.value = transcriptRequestOrganizationScope()
    }
    const replyOwner = captureReplyContextOwner(contactId)
    try {
      const response = await (replyOwner
        ? messagesService.list(contactId, params, controller.signal, { ...replyOwner, signal: controller.signal })
        : messagesService.list(contactId, params, controller.signal))
      if (
        loadGeneration !== messageLoadGeneration ||
        identity !== identityGeneration ||
        organizationScope !== activeOrganizationScope.value ||
        (replyOwner && !replyOwner.isCurrent())
      ) return
      // API returns { status: "success", data: { messages: [...], has_more: boolean } }
      const data = response.data.data || response.data
      const fetchedMessages = data.messages || []
      acceptReplyContexts(fetchedMessages, replyOwner, contactId, params?.account)
      if (preserveExisting) {
        // A realtime refresh returns the canonical latest page. Merge it into
        // already-loaded history so a reader who paged upward does not lose
        // older messages, while canonical status/content wins for duplicate IDs.
        const mergedById = new Map(messages.value.map(message => [message.id, message]))
        for (const message of fetchedMessages) mergedById.set(message.id, message)
        messages.value = Array.from(mergedById.values()).sort(compareMessageIngestionOrder)
      } else {
        messages.value = fetchedMessages
        hasMoreMessages.value = data.has_more === true
      }
    } catch (error) {
      if (
        loadGeneration === messageLoadGeneration &&
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value &&
        !isAbortError(error)
      ) {
        console.error('Failed to fetch messages:', error)
      }
    } finally {
      if (loadGeneration === messageLoadGeneration) {
        if (!preserveExisting) isLoadingMessages.value = false
        if (messageLoadAbortController === controller) messageLoadAbortController = null
        if (messageRefreshQueued && currentContact.value?.id === contactId) {
          messageRefreshQueued = false
          void refreshCurrentMessages()
        }
        scheduleQuoteRefresh()
      }
    }
  }

  async function refreshCurrentMessages() {
    const contactId = currentContact.value?.id
    if (!contactId) return
    if (messageLoadAbortController || isLoadingMessages.value) {
      messageRefreshQueued = true
      return
    }
    const quoteGeneration = quoteRefreshGeneration
    await fetchMessages(contactId, {
      account: accountFilter.value || undefined,
    }, { preserveExisting: true })
    if (quoteGeneration === quoteRefreshGeneration) await refreshPendingQuotes()
  }

  function cancelQuoteRefresh() {
    quoteRefreshGeneration++
    quoteRefreshController?.abort()
    quoteRefreshController = null
    if (quoteRefreshTimer !== null) clearTimeout(quoteRefreshTimer)
    quoteRefreshTimer = null
    quoteRefreshCursor = 0
    quoteRefreshAccounts.clear()
  }

  function scheduleQuoteRefresh() {
    if (quoteRefreshTimer !== null || quoteRefreshController || messageLoadAbortController ||
      quoteRefreshAccounts.size === 0) return
    quoteRefreshTimer = setTimeout(() => {
      quoteRefreshTimer = null
      if (quoteRefreshController || messageLoadAbortController) return
      const accounts = new Set(quoteRefreshAccounts)
      quoteRefreshAccounts.clear()
      void refreshPendingQuotes(accounts)
    }, QUOTE_REFRESH_DELAY_MS)
  }

  function notePossibleQuoteOriginal(account?: string) {
    if (!account || !messages.value.some(message => message.quote_pending === true && message.whatsapp_account === account)) return
    quoteRefreshAccounts.add(account)
    scheduleQuoteRefresh()
  }

  async function refreshPendingQuotes(eventAccounts?: ReadonlySet<string>) {
    if (quoteRefreshController || messageLoadAbortController || isLoadingMessages.value) return
    const contactId = currentContact.value?.id
    if (!contactId) return
    const replyOwner = captureReplyContextOwner(contactId)
    if (!replyOwner) return
    const { organizationId } = replyOwner
    const account = accountFilter.value
    const generation = quoteRefreshGeneration

    const eligible = [...new Set(messages.value.filter(message =>
      message.quote_pending === true && message.contact_id === contactId &&
      (!account || message.whatsapp_account === account) &&
      (!eventAccounts || (!!message.whatsapp_account && eventAccounts.has(message.whatsapp_account))),
    ).map(message => message.id))]
    if (eligible.length === 0) return
    const start = quoteRefreshCursor % eligible.length
    const ids = [...eligible.slice(start), ...eligible.slice(0, start)].slice(0, QUOTE_REFRESH_BATCH_SIZE)
    quoteRefreshCursor = start + ids.length
    const requested = new Set(ids)
    const requestedRows = new Set(messages.value.filter(row => requested.has(row.id)).map(row => toRaw(row)))

    const controller = new AbortController()
    quoteRefreshController = controller
    try {
      const response = await messagesService.quoteUpdates(contactId, ids, account, organizationId, controller.signal)
      if (controller.signal.aborted || generation !== quoteRefreshGeneration ||
        account !== accountFilter.value || !replyOwner.isCurrent()) return
      const updates: { id: string; reply_to_message_id: string; reply_to_message: ReplyPreview }[] | undefined =
        response.data.data?.quote_updates
      if (!Array.isArray(updates)) return
      const byId = new Map(updates.filter(update => update && requested.has(update.id) &&
        typeof update.reply_to_message_id === 'string' && update.reply_to_message?.id === update.reply_to_message_id,
      ).map(update => [update.id, update]))
      if (byId.size === 0) return
      messages.value = messages.value.map(message => {
        const update = byId.get(message.id)
        if (!update || message.quote_pending !== true || message.contact_id !== contactId ||
          (account && message.whatsapp_account !== account) ||
          !requestedRows.has(toRaw(message))) return message
        const next = {
          ...message,
          quote_pending: false,
          is_reply: true,
          reply_to_message_id: update.reply_to_message_id,
          reply_to_message: update.reply_to_message,
          reply_context_unavailable: undefined,
        }
        acceptReplyContexts([next], replyOwner, contactId, account)
        return next
      })
    } catch {
      // The existing transcript remains usable when the optional lookup fails.
    } finally {
      if (quoteRefreshController === controller) {
        quoteRefreshController = null
        scheduleQuoteRefresh()
      }
    }
  }

  async function fetchOlderMessages(contactId: string, account?: string) {
    if (isLoadingOlderMessages.value || !hasMoreMessages.value || messages.value.length === 0) {
      return
    }

    const loadGeneration = ++olderMessageLoadGeneration
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    olderMessageLoadAbortController?.abort()
    const controller = new AbortController()
    olderMessageLoadAbortController = controller
    isLoadingOlderMessages.value = true
    const replyOwner = captureReplyContextOwner(contactId)
    try {
      // Get the oldest message ID for cursor-based pagination
      const oldestMessageId = messages.value[0].id
      const params = { before_id: oldestMessageId, account }
      const response = await (replyOwner
        ? messagesService.list(contactId, params, controller.signal, { ...replyOwner, signal: controller.signal })
        : messagesService.list(contactId, params, controller.signal))
      if (
        loadGeneration !== olderMessageLoadGeneration ||
        identity !== identityGeneration ||
        organizationScope !== activeOrganizationScope.value ||
        currentContact.value?.id !== contactId ||
        (replyOwner && !replyOwner.isCurrent())
      ) return
      const data = response.data.data || response.data
      const olderMessages = data.messages || []
      acceptReplyContexts(olderMessages, replyOwner, contactId, account)

      if (olderMessages.length > 0) {
        // Prepend older messages (they come in chronological order, oldest first)
        messages.value = [...olderMessages, ...messages.value]
      }
      hasMoreMessages.value = data.has_more === true
    } catch (error) {
      if (
        loadGeneration === olderMessageLoadGeneration &&
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value &&
        !isAbortError(error)
      ) {
        console.error('Failed to fetch older messages:', error)
      }
    } finally {
      if (loadGeneration === olderMessageLoadGeneration) {
        isLoadingOlderMessages.value = false
        if (olderMessageLoadAbortController === controller) olderMessageLoadAbortController = null
      }
    }
  }

  async function sendMessage(
    contactId: string,
    type: string,
    content: any,
    replyToMessageId?: string,
    whatsappAccount?: string,
    extra?: {
      interactive?: Parameters<typeof messagesService.send>[1]['interactive']
      forwardOfMessageId?: string
      // Captured by the native text composer before its first await.
      organizationId?: string
    },
  ) {
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    const explicitOrganization = extra?.organizationId
    if (explicitOrganization !== undefined && (
      !explicitOrganization.trim() || explicitOrganization !== organizationScope ||
      explicitOrganization !== messagesOrganizationScope.value ||
      currentContact.value?.id !== contactId
    )) {
      throw new Error('Text send workspace changed before dispatch')
    }
    try {
      const request = {
        type,
        content,
        reply_to_message_id: replyToMessageId,
        whatsapp_account: whatsappAccount,
        ...(extra?.interactive ? { interactive: extra.interactive } : {}),
        ...(extra?.forwardOfMessageId ? { forward_of_message_id: extra.forwardOfMessageId } : {}),
      }
      const response = explicitOrganization === undefined
        ? await messagesService.send(contactId, request)
        : await messagesService.send(contactId, request, { organizationId: explicitOrganization })
      // API returns { status: "success", data: { ... } }
      const newMessage = response.data.data || response.data
      if (explicitOrganization !== undefined && (
        !newMessage || typeof newMessage.id !== 'string' || !newMessage.id.trim() ||
        newMessage.contact_id !== contactId || newMessage.direction !== 'outgoing' ||
        newMessage.message_type !== type || typeof newMessage.whatsapp_account !== 'string' ||
        !newMessage.whatsapp_account.trim() || (whatsappAccount && newMessage.whatsapp_account !== whatsappAccount) ||
        !['pending', 'sent', 'delivered', 'read', 'failed'].includes(newMessage.status)
      )) {
        throw new Error('Text send acceptance is unconfirmed')
      }
      if (
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value &&
        currentContact.value?.id === contactId &&
        (explicitOrganization === undefined || messagesOrganizationScope.value === explicitOrganization) &&
        newMessage?.contact_id === contactId
      ) {
        // Use addMessage which has duplicate checking (WebSocket may also broadcast this).
        addMessage(newMessage)
      }

      return newMessage
    } catch (error) {
      console.error('Failed to send message:', error)
      throw error
    }
  }

  async function sendTemplate(
    contactId: string,
    templateName: string,
    templateParams?: Record<string, string>,
    accountName?: string,
    headerFile?: File,
    buttonParams?: Record<string, string>,
    headerParams?: Record<string, string>
  ) {
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    try {
      const response = await messagesService.sendTemplate(contactId, {
        template_name: templateName,
        template_params: templateParams,
        header_params: headerParams,
        button_params: buttonParams,
        account_name: accountName
      }, headerFile)
      const data = response.data.data || response.data
      if (
        identity === identityGeneration &&
        organizationScope === activeOrganizationScope.value &&
        currentContact.value?.id === contactId &&
        data?.contact_id === contactId
      ) {
        // Use addMessage which has duplicate checking (WebSocket may also broadcast this).
        addMessage(data)
      }
      return data
    } catch (error) {
      console.error('Failed to send template:', error)
      throw error
    }
  }

  function setReplyingTo(message: Message | null) {
    replyingTo.value = message
  }

  function clearReplyingTo() {
    replyingTo.value = null
  }

  function addMessage(message: Message) {
    // Update contact metadata regardless of account filter
    const contact = contacts.value.find(c => c.id === message.contact_id)
    if (contact) {
      contact.last_message_at = message.created_at
      if (message.direction === 'incoming') {
        contact.unread_count++
        contact.last_inbound_at = message.created_at
        contact.service_window_open = true
      }
    }
    // Also update currentContact if it matches
    if (currentContact.value && currentContact.value.id === message.contact_id && message.direction === 'incoming') {
      currentContact.value.last_inbound_at = message.created_at
      currentContact.value.service_window_open = true
    }

    // The shared message list belongs exclusively to currentContact. A send
    // response or socket event for another contact may still update that
    // contact's sidebar metadata, but it must never enter the open transcript.
    if (!currentContact.value || currentContact.value.id !== message.contact_id) {
      return
    }

    // Fail closed while an account-specific transcript is open. During a
    // rolling deployment an older socket publisher may omit whatsapp_account;
    // the canonical account-filtered fetch will safely pick up that message.
    if (accountFilter.value && message.whatsapp_account !== accountFilter.value) {
      return
    }

    // Check if message already exists
    const index = messages.value.findIndex(m => m.id === message.id)
    if (index === -1) {
      messages.value.push(message)
      messages.value.sort(compareMessageIngestionOrder)
      if (message.wamid) notePossibleQuoteOriginal(message.whatsapp_account)
      return
    }
    // The socket copy can arrive before the send response. Keep its fields
    // (a newer status included) and only fill what it lacks, such as the
    // reply previews an older socket publisher does not carry.
    const current = messages.value[index] as unknown as Record<string, unknown>
    const late = message as unknown as Record<string, unknown>
    const missing = Object.keys(late).filter(key =>
      (current[key] === undefined || current[key] === null) && late[key] !== undefined && late[key] !== null,
    )
    if (missing.length > 0) {
      const merged = { ...current }
      for (const key of missing) merged[key] = late[key]
      messages.value[index] = merged as unknown as Message
    }
  }

  function updateMessageStatus(messageId: string, status: string, errorMessage?: string) {
    const index = messages.value.findIndex(m => m.id === messageId)
    if (index !== -1) {
      const prior = messages.value[index]
      messages.value[index] = {
        ...prior,
        status,
        ...(errorMessage ? { error_message: errorMessage } : {})
      }
      if (prior.direction === 'outgoing' && prior.status === 'pending' && ['sent', 'delivered', 'read'].includes(status)) {
        notePossibleQuoteOriginal(prior.whatsapp_account)
      }
    }
  }

  function setCurrentContact(contact: Contact | null) {
    if (currentContact.value?.id !== contact?.id) {
      retireReplyContexts()
      cancelQuoteRefresh()
      contactSelectionGeneration++
      // Invalidate a slower message request for the previous contact before it
      // can replace the active conversation's messages.
      messageLoadGeneration++
      olderMessageLoadGeneration++
      messageLoadAbortController?.abort()
      messageLoadAbortController = null
      olderMessageLoadAbortController?.abort()
      olderMessageLoadAbortController = null
      isLoadingMessages.value = false
      isLoadingOlderMessages.value = false
      messageRefreshQueued = false
    }
    currentContact.value = contact
    replyingTo.value = null // Clear reply state when switching contacts
    if (contact) {
      contact.unread_count = 0
    }
  }

  function setAccountFilter(account: string | null) {
    if (accountFilter.value !== account) {
      retireReplyContexts()
      cancelQuoteRefresh()
    }
    accountFilter.value = account
  }

  function clearMessages() {
    retireReplyContexts()
    cancelQuoteRefresh()
    messageLoadGeneration++
    olderMessageLoadGeneration++
    messageLoadAbortController?.abort()
    messageLoadAbortController = null
    olderMessageLoadAbortController?.abort()
    olderMessageLoadAbortController = null
    messages.value = []
    messagesOrganizationScope.value = ''
    isLoadingMessages.value = false
    isLoadingOlderMessages.value = false
    hasMoreMessages.value = false
    accountFilter.value = null
    messageRefreshQueued = false
  }

  function updateMessageReactions(messageId: string, reactions: Reaction[]) {
    const message = messages.value.find(m => m.id === messageId)
    if (message) {
      message.reactions = reactions
    }
  }

  function updateContactTags(contactId: string, tags: string[]) {
    // Update in contacts list
    const contact = contacts.value.find(c => c.id === contactId)
    if (contact) {
      contact.tags = tags
    }
    // Update current contact if it matches
    if (currentContact.value?.id === contactId) {
      currentContact.value = { ...currentContact.value, tags }
    }
  }

  function setListFilter(filter: ContactListFilter) {
    if (listFilter.value === filter) return
    listFilter.value = filter
    void fetchContacts()
  }

  function applyLocalReadState(contactId: string, mark: 'read' | 'unread', manualUnread: boolean) {
    const apply = (contact: Contact) => {
      contact.manual_unread = manualUnread
      // A shared read clears the badge at once; the refetch below restores
      // the canonical count (a read-only reader leaves it unchanged).
      if (mark === 'read' && !manualUnread) contact.unread_count = 0
    }
    const listed = contacts.value.find(contact => contact.id === contactId)
    if (listed) apply(listed)
    if (currentContact.value?.id === contactId && currentContact.value !== listed) {
      apply(currentContact.value)
    }
  }

  // Manual "Mark as read" / "Mark as unread" from a row or header menu. Read
  // state is shared by the whole team. Errors propagate to the caller.
  async function markContactReadState(contactId: string, mark: 'read' | 'unread') {
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    const response = await contactsService.markManualReadState(contactId, mark)
    if (
      identity !== identityGeneration ||
      organizationScope !== activeOrganizationScope.value
    ) return
    const data = (response.data as any)?.data ?? response.data
    applyLocalReadState(contactId, mark, data?.manual_unread === true)
    void fetchContacts()
  }

  // A contact_update event (assignment, transfer or read state changed) can
  // move rows in or out of the active filter. Bursts coalesce into one fetch.
  // The list refresh also refreshes the open chat when it is listed; a chat
  // pinned outside the filtered list is re-read on its own when it changed.
  function scheduleContactsRefresh(contactId?: string, kind?: string) {
    if (contactId) pendingContactUpdates.add(contactId)
    if (contactId && kind === 'assignment') {
      pendingAssignmentUpdates.add(contactId)
      if (currentContact.value?.id === contactId) selectedAssignmentGeneration++
    }
    if (contactUpdateRefreshHandle !== null) return
    const identity = identityGeneration
    const organizationScope = activeOrganizationScope.value
    const userId = authStore.user?.id
    contactUpdateRefreshHandle = setTimeout(async () => {
      contactUpdateRefreshHandle = null
      const changed = new Set(pendingContactUpdates)
      const assignments = new Set(pendingAssignmentUpdates)
      pendingContactUpdates.clear()
      pendingAssignmentUpdates.clear()
      const selection = contactSelectionGeneration
      const assignment = selectedAssignmentGeneration
      if (identity !== identityGeneration || organizationScope !== activeOrganizationScope.value || userId !== authStore.user?.id) return
      await fetchContacts()
      const open = currentContact.value
      if (
        identity !== identityGeneration ||
        organizationScope !== activeOrganizationScope.value ||
        userId !== authStore.user?.id ||
        selection !== contactSelectionGeneration ||
        !open ||
        !changed.has(open.id)
      ) return
      if (assignments.has(open.id)) {
        // The event grants no access. Revalidate even if an older list page
        // still contained this row; only a current scoped refusal clears it.
        if (!organizationScope || !userId) return
        const isCurrent = () => identity === identityGeneration &&
          organizationScope === activeOrganizationScope.value && userId === authStore.user?.id &&
          selection === contactSelectionGeneration && assignment === selectedAssignmentGeneration &&
          currentContact.value?.id === open.id
        try {
          const response = await contactsService.get(open.id, organizationScope)
          if (!isCurrent()) return
          const fresh = response.data.data || response.data
          Object.assign(currentContact.value!, {
            unread_count: fresh.unread_count,
            manual_unread: fresh.manual_unread,
            assigned_user_id: fresh.assigned_user_id,
            assigned_user_name: fresh.assigned_user_name,
            transfer_agent_id: fresh.transfer_agent_id,
            transfer_agent_name: fresh.transfer_agent_name,
          })
        } catch (error) {
          const status = (error as { response?: { status?: number } })?.response?.status
          if (!isCurrent() || (status !== 403 && status !== 404)) return
          contactLoadGeneration++
          moreContactsLoadGeneration++
          contactLoadAbortController?.abort()
          contactLoadAbortController = null
          moreContactsLoadAbortController?.abort()
          moreContactsLoadAbortController = null
          isLoading.value = false
          isLoadingMoreContacts.value = false
          const before = contacts.value.length
          contacts.value = contacts.value.filter(contact => contact.id !== open.id)
          contactsTotal.value = Math.max(0, contactsTotal.value - (before - contacts.value.length))
          setCurrentContact(null)
          clearMessages()
        }
        return
      }
      if (contacts.value.some(contact => contact.id === open.id)) return
      const fresh = await fetchContact(open.id)
      if (!fresh || currentContact.value !== open) return
      Object.assign(open, {
        unread_count: fresh.unread_count,
        manual_unread: fresh.manual_unread,
        assigned_user_id: fresh.assigned_user_id,
        assigned_user_name: fresh.assigned_user_name,
        transfer_agent_id: fresh.transfer_agent_id,
        transfer_agent_name: fresh.transfer_agent_name,
      })
    }, CONTACT_UPDATE_REFRESH_DELAY_MS)
  }

  watch(activeOrganizationScope, (organizationScope, previousScope) => {
    if (organizationScope !== previousScope) resetForIdentityChange()
  })

  // Debounce server-side search so each keystroke doesn't fire a request.
  watch(searchQuery, (query) => {
    if (suppressEmptySearchReset && query === '') {
      suppressEmptySearchReset = false
      return
    }
    suppressEmptySearchReset = false
    const debounceGeneration = ++searchDebounceGeneration
    if (searchDebounceHandle !== null) clearTimeout(searchDebounceHandle)
    searchDebounceHandle = setTimeout(() => {
      searchDebounceHandle = null
      if (debounceGeneration !== searchDebounceGeneration) return
      const search = normalizeContactSearch(query) || undefined
      void fetchContacts({ search })
    }, 300)
  })

  return {
    contacts,
    currentContact,
    messages,
    messagesOrganizationScope,
    transcriptAccountFilter,
    ownsReplyContext,
    retireReplyContexts,
    isLoading,
    contactListError,
    retryContactList,
    isLoadingMessages,
    isLoadingOlderMessages,
    hasMoreMessages,
    searchQuery,
    selectedTags,
    replyingTo,
    filteredContacts,
    sortedContacts,
    // Contacts pagination
    contactsTotal,
    hasMoreContacts,
    isLoadingMoreContacts,
    fetchContacts,
    loadMoreContacts,
    // Other
    fetchContact,
    fetchMessages,
    refreshCurrentMessages,
    fetchOlderMessages,
    sendMessage,
    sendTemplate,
    addMessage,
    updateMessageStatus,
    setCurrentContact,
    clearMessages,
    resetForIdentityChange,
    setAccountFilter,
    setReplyingTo,
    clearReplyingTo,
    updateMessageReactions,
    updateContactTags,
    // Inbox state
    listFilter,
    setListFilter,
    markContactReadState,
    scheduleContactsRefresh
  }
})
