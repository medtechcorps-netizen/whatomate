<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useMediaQuery } from '@vueuse/core'
import { onBeforeRouteLeave } from 'vue-router'
import {
  AtSign,
  Bot,
  CheckCircle2,
  AlertCircle,
  ArrowLeft,
  Facebook,
  Globe2,
  Inbox,
  Instagram,
  Loader2,
  Mail,
  MessageCircle,
  PauseCircle,
  Play,
  Plus,
  PanelRightOpen,
  RefreshCw,
  Send,
  Settings2,
  Smartphone,
  Unplug,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetTitle,
} from '@/components/ui/sheet'
import CustomerRevenueWorkspace from '@/components/chat/CustomerRevenueWorkspace.vue'
import ContactIdentityReviewDialog from '@/components/chat/ContactIdentityReviewDialog.vue'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import { useOmnichannelUnreadStore } from '@/stores/omnichannelUnread'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import {
  effectiveAIIsAllowed,
  type ContactIdentityReviewEffectiveState,
} from '@/services/api'
import { compareMessageIngestionOrder } from '@/lib/messageOrdering'
import {
  clearLegacyWhatsAppReplyAttempt,
  getOrCreateLegacyWhatsAppReplyAttempt,
  LegacyWhatsAppReplyAttemptStorageError,
} from '@/lib/legacyWhatsAppReplyAttempts'
import {
  threadsPublicEngagementAccountReady,
  threadsPublicEngagementTarget,
} from '@/lib/threadsPublicEngagement'
import {
  isInboxContentRefreshActivity,
  wsService,
  type InboxActivityEvent,
  type WebSocketConnectionState,
} from '@/services/websocket'
import {
  metaMessengerOnboarding,
  MetaMessengerAuthorizationCancelledError,
  MetaMessengerOrganizationChangedError,
  type MetaMessengerPage,
  type MetaMessengerSelection,
} from '@/services/metaMessengerOnboarding'
import {
  type MetaInstagramAvailabilityState,
  metaInstagramOAuthAvailable,
  metaInstagramOnboarding,
  metaInstagramReconciliationAvailable,
  metaInstagramStaticCreationAvailable,
  metaInstagramTeardownAvailable,
  MetaInstagramOffPilotError,
  MetaInstagramOrganizationChangedError,
  type MetaInstagramStatus,
} from '@/services/metaInstagramOnboarding'
import {
  channelsService,
  type ChannelAccount,
  type ChannelType,
  type InboxConversation,
} from '@/services/productSuite'

interface InboxMessage {
  id: string
  direction: 'incoming' | 'outgoing'
  message_type: string
  content: string
  status: string
  ingested_at?: string
  created_at: string
}

interface InboxMessageEnvelope {
  message: InboxMessage
  parts?: Array<{
    type: string
    text?: string
    caption?: string
  }>
}

interface LegacyWhatsAppMessage {
  id: string
  direction: 'incoming' | 'outgoing'
  message_type: string
  content: string | { body?: string } | null
  status: string
  ingested_at?: string
  created_at: string
}

const acknowledgedLegacyWhatsAppStatuses = new Set(['sent', 'delivered', 'read'])

interface CreatedConnection {
  account: ChannelAccount
  inbound_secret: string
  webhook_path: string
}

const toast = useAppToast()
const authStore = useAuthStore()
const organizationsStore = useOrganizationsStore()
const omnichannelUnreadStore = useOmnichannelUnreadStore()
const loading = ref(true)
const loadingMessages = ref(false)
const loadingMore = ref(false)
const loadingOlderMessages = ref(false)
const sending = ref(false)
const aiStateUpdating = ref(false)
const isIdentityReviewOpen = ref(false)
const showConnect = ref(false)
const accounts = ref<ChannelAccount[]>([])
const conversations = ref<InboxConversation[]>([])
const selectedConversation = ref<InboxConversation | null>(null)
const isWorkspaceOpen = ref(false)
const isWorkspaceRail = useMediaQuery('(min-width: 1280px)')
const messages = ref<InboxMessage[]>([])
const messagesViewport = ref<HTMLElement | null>(null)
const messagesContent = ref<HTMLElement | null>(null)
const channelFilter = ref<ChannelType | 'all'>('all')
const search = ref('')
const composer = ref('')
const createdConnection = ref<CreatedConnection | null>(null)
const metaMessengerSelection = ref<MetaMessengerSelection | null>(null)
const selectedMetaMessengerPageKey = ref('')
const metaMessengerBusy = ref(false)
const metaMessengerReconnectName = ref('')
const metaMessengerOnboardingReady = ref(false)
const metaMessengerSelectionOrganizationId = ref('')
const metaMessengerAuthorizationController = ref<AbortController | null>(null)
const metaInstagramBusy = ref(false)
const metaInstagramState = ref<MetaInstagramAvailabilityState>({
  availability: 'loading',
  organizationId: '',
  status: null,
})
const settingsAccount = ref<ChannelAccount | null>(null)
const bookingSettingsSaving = ref(false)
const bookingSettingsError = ref('')
let bookingSettingsRequest = 0
const conversationPage = ref(1)
const conversationTotal = ref(0)
const messageTotal = ref(0)
const newAccount = reactive({
  channel: 'instagram' as ChannelType,
  name: '',
  external_account_id: '',
  relay_url: '',
})
const canManageAccounts = computed(() => authStore.hasPermission('channel_accounts', 'write'))
const canManageAIBooking = computed(() => canManageAccounts.value
  && authStore.hasPermission('chatbot.ai', 'write')
  && authStore.hasPermission('booking.settings', 'write'))
const canDeleteAccounts = computed(() => authStore.hasPermission('channel_accounts', 'delete'))
const canManageIntegrations = computed(() => authStore.hasPermission('settings.integrations', 'write'))
const canManageMetaMessenger = computed(
  () => canManageAccounts.value && canManageIntegrations.value,
)
const canReconcileMetaMessenger = computed(
  () => canManageMetaMessenger.value && canDeleteAccounts.value,
)
const canManageMetaInstagram = computed(
  () => canManageAccounts.value && canManageIntegrations.value,
)
const canReconcileMetaInstagram = computed(
  () => canManageMetaInstagram.value && canDeleteAccounts.value,
)
const canReadConversations = computed(() => authStore.hasPermission('conversations', 'read'))
const canManageConversations = computed(() => authStore.hasPermission('conversations', 'write'))
const canReviewContactIdentity = computed(() =>
  authStore.hasPermission('contacts', 'write')
  && authStore.hasPermission('contacts.identity_review', 'write')
)
const canViewStagedContactIdentity = computed(() =>
  canReviewContactIdentity.value
)
const threadsPublicEngagementEntitlement = 'threads.public_engagement.enabled'
const absoluteWebhookURL = computed(() => {
  if (!createdConnection.value) return ''
  return new URL(createdConnection.value.webhook_path, window.location.origin).toString()
})
const accountSettingsDraft = reactive({
  name: '',
  relay_url: '',
  outbound_secret: '',
  ai_reply_enabled: false,
  ai_booking_enabled: false,
})
const connectionState = ref<WebSocketConnectionState>(wsService.getConnectionState())
let stopInboxActivity: (() => void) | null = null
let stopConnectionState: (() => void) | null = null
let pollTimer: number | null = null
let syncDebounceTimer: number | null = null
let filterDebounceTimer: number | null = null
let refreshInFlight = false
let refreshQueued = false
let organizationReloadQueued = false
let viewMounted = false
let conversationViewSequence = 0
let conversationProjectionGeneration = 0
let organizationGeneration = 0
let messageViewportScrollFrame: number | null = null
let messagesContentResizeObserver: ResizeObserver | null = null
let messagesContentObservedHeight = 0
let messageViewportFollowingBottom = true
let readCursorInFlightKey: string | null = null
let readCursorCheckQueued = false
let metaMessengerContextVersion = 0
let metaMessengerStatusSequence = 0
let metaInstagramStatusSequence = 0
const metaMessengerSwitchLockOwner = 'meta-messenger-onboarding'
const metaMessengerSwitchLockMessage =
  'Finish the managed Meta connection before switching workspaces.'
const messageBottomThreshold = 80

const activeOrganizationId = computed(
  () => organizationsStore.selectedOrgId || authStore.organizationId || null,
)

interface OrganizationRequestContext {
  generation: number
  organizationId: string | null
}

function captureOrganizationContext(): OrganizationRequestContext {
  return {
    generation: organizationGeneration,
    organizationId: activeOrganizationId.value,
  }
}

function isCurrentOrganizationContext(context: OrganizationRequestContext): boolean {
  return (
    context.generation === organizationGeneration
    && context.organizationId === activeOrganizationId.value
  )
}
const metaInstagramAvailability = computed(() =>
  metaInstagramState.value.organizationId === (activeOrganizationId.value ?? '')
    ? metaInstagramState.value.availability
    : 'loading',
)
const metaInstagramStatus = computed<MetaInstagramStatus | null>(() =>
  metaInstagramAvailability.value === 'managed'
    ? metaInstagramState.value.status
    : null,
)
const metaInstagramStaticConnectionAvailable = computed(() =>
  metaInstagramStaticCreationAvailable(
    metaInstagramState.value,
    activeOrganizationId.value,
  ),
)
const metaMessengerWorkspaceLocked = computed(
  () => metaMessengerBusy.value || metaMessengerSelection.value !== null || metaInstagramBusy.value,
)
const metaInstagramOnboardingReady = computed(() =>
  metaInstagramOAuthAvailable(metaInstagramStatus.value),
)
const metaInstagramTeardownReady = computed(() =>
  metaInstagramTeardownAvailable(metaInstagramStatus.value),
)

function isCurrentConversationView(
  sequence: number,
  conversationId: string,
  organizationContext: OrganizationRequestContext,
) {
  return (
    isCurrentOrganizationContext(organizationContext) &&
    sequence === conversationViewSequence &&
    selectedConversation.value?.id === conversationId
  )
}

function refreshOmnichannelUnread(organizationId: string | null, userId: string | undefined) {
  if (
    !organizationId
    || !userId
    || activeOrganizationId.value !== organizationId
    || authStore.user?.id !== userId
  ) return
  void omnichannelUnreadStore.refresh(organizationId, userId)
}

async function markOpenConversationRead(
  conversation: InboxConversation,
  viewSequence: number,
  organizationContext: OrganizationRequestContext,
) {
  const lastVisibleMessageID = messages.value.at(-1)?.id
  if (
    conversation.unread_count <= 0
    || !canReadConversations.value
    || !lastVisibleMessageID
    || !isCurrentConversationView(viewSequence, conversation.id, organizationContext)
  ) return

  const cursorKey = `${organizationContext.generation}:${conversation.id}:${lastVisibleMessageID}`
  if (readCursorInFlightKey !== null) {
    readCursorCheckQueued = true
    return
  }

  const organizationId = organizationContext.organizationId
  const userId = authStore.user?.id
  if (!organizationId) return
  readCursorInFlightKey = cursorKey
  try {
    const response = await channelsService.markRead(conversation.id, {
      last_visible_message_id: lastVisibleMessageID,
    }, organizationId)
    // Refresh the still-active tenant even if the operator changed threads while
    // the POST was in flight. A no-op cursor advance emits no websocket event.
    refreshOmnichannelUnread(organizationId, userId)
    if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
    const payload = response?.data?.data ?? response?.data
    if (Number.isSafeInteger(payload?.unread_count) && payload.unread_count >= 0) {
      const unreadCount = payload.unread_count as number
      conversation.unread_count = unreadCount
      const listed = conversations.value.find(item => item.id === conversation.id)
      if (listed) listed.unread_count = unreadCount
      if (selectedConversation.value?.id === conversation.id) {
        selectedConversation.value.unread_count = unreadCount
      }
    }
  } finally {
    if (readCursorInFlightKey === cursorKey) readCursorInFlightKey = null
    if (readCursorCheckQueued && viewMounted) {
      readCursorCheckQueued = false
      scheduleMessageViewportReadCheck()
    }
  }
}

const supportedChannels: Array<{
  key: ChannelType
  label: string
  icon: any
  provider: string
  gated: boolean
  connectable: boolean
  entitlement?: string
}> = [
  { key: 'whatsapp', label: 'WhatsApp', icon: MessageCircle, provider: 'meta', gated: false, connectable: false },
  { key: 'instagram', label: 'Instagram', icon: Instagram, provider: 'relay', gated: true, connectable: true },
  { key: 'messenger', label: 'Messenger', icon: Facebook, provider: 'relay', gated: true, connectable: true },
  {
    key: 'threads',
    label: 'Threads public replies',
    icon: AtSign,
    provider: 'threads',
    gated: true,
    connectable: false,
    entitlement: threadsPublicEngagementEntitlement,
  },
  { key: 'email', label: 'Email', icon: Mail, provider: 'relay', gated: true, connectable: true },
  { key: 'webchat', label: 'Web chat', icon: Globe2, provider: 'relay', gated: true, connectable: true },
  { key: 'tiktok', label: 'TikTok', icon: Smartphone, provider: 'tiktok', gated: true, connectable: false },
]

const visibleChannels = computed(() =>
  supportedChannels.filter(
    channel =>
      !channel.entitlement || authStore.hasProductEntitlement(channel.entitlement),
  ),
)
const connectableChannels = computed(() =>
  visibleChannels.value.filter(channel => channel.connectable),
)
const filteredConversations = computed(() => conversations.value)
const hasOlderMessages = computed(() => messages.value.length < messageTotal.value)
const inboxGridClass = computed(() =>
  isWorkspaceOpen.value && isWorkspaceRail.value
    ? 'lg:grid-cols-[320px_minmax(0,1fr)] xl:grid-cols-[320px_minmax(0,1fr)_400px] 2xl:grid-cols-[250px_340px_minmax(0,1fr)_400px]'
    : 'lg:grid-cols-[320px_minmax(0,1fr)] 2xl:grid-cols-[250px_340px_minmax(0,1fr)]',
)

const selectedAccount = computed(() =>
  selectedConversation.value
    ? accounts.value.find((account) => account.id === selectedConversation.value?.channel_account_id)
    : undefined,
)
const whatsappServiceWindowOpen = computed(() => {
  const end = selectedConversation.value?.service_window_ends_at
  if (!end) return false
  const endTime = Date.parse(end)
  return Number.isFinite(endTime) && endTime > Date.now()
})
const canSendWhatsAppText = computed(
  () =>
    selectedConversation.value?.channel === 'whatsapp' &&
    Boolean(activeOrganizationId.value) &&
    canManageConversations.value &&
    authStore.hasPermission('chat', 'write') &&
    selectedAccount.value?.status === 'active' &&
    selectedAccount.value?.provider === 'meta_legacy' &&
    selectedAccount.value?.config?.reply_route === 'chat' &&
    selectedAccount.value?.config?.legacy_read_only === true &&
    selectedAccount.value?.config?.outbound_enabled === false &&
    selectedAccount.value?.capabilities?.legacy_text_reply_endpoint === true &&
    selectedAccount.value?.capabilities?.text === true &&
    selectedAccount.value?.capabilities?.replies === true &&
    selectedAccount.value?.capabilities?.service_window === true &&
    whatsappServiceWindowOpen.value,
)
const whatsappReplyUnavailableReason = computed(() => {
  if (!canManageConversations.value || !authStore.hasPermission('chat', 'write')) {
    return 'You need both conversation and chat reply permission to send from this inbox.'
  }
  if (selectedAccount.value?.provider !== 'meta_legacy') {
    return 'Open WhatsApp to reply safely; this conversation is not backed by the native WhatsApp adapter.'
  }
  if (
    selectedAccount.value?.config?.reply_route !== 'chat' ||
    selectedAccount.value?.config?.legacy_read_only !== true ||
    selectedAccount.value?.config?.outbound_enabled !== false
  ) {
    return 'Open WhatsApp to reply safely; this native conversation has an incompatible routing configuration.'
  }
  if (selectedAccount.value?.status !== 'active') {
    return 'This WhatsApp account is not active. Open WhatsApp to review the connection.'
  }
  if (
    selectedAccount.value?.capabilities?.legacy_text_reply_endpoint !== true ||
    selectedAccount.value?.capabilities?.text !== true ||
    selectedAccount.value?.capabilities?.replies !== true ||
    selectedAccount.value?.capabilities?.service_window !== true
  ) {
    return 'Open WhatsApp to reply safely; this connection does not advertise every required reply capability.'
  }
  if (!whatsappServiceWindowOpen.value) {
    return 'The service window is closed. Open WhatsApp to send an approved template.'
  }
  return 'This WhatsApp adapter does not currently support text replies from the Omnichannel inbox.'
})
const canControlConversationAI = computed(
  () =>
    canManageConversations.value &&
    ['instagram', 'messenger'].includes(selectedConversation.value?.channel ?? '') &&
    selectedAccount.value?.config?.ai_reply_enabled === true,
)
const selectedConversationAIBlocked = computed(() =>
  !effectiveAIIsAllowed(selectedConversation.value?.identity_review_ai_state)
)
const selectedConversationHasOpenIdentityReview = computed(() =>
  identityReviewIsOpen(selectedConversation.value?.identity_review_ai_state)
)
const conversationAIControlLabel = computed(() => {
  if (!selectedConversation.value?.ai_paused) {
    return aiStateUpdating.value ? 'Pausing AI' : 'Pause AI'
  }
  if (selectedConversationAIBlocked.value) {
    return aiStateUpdating.value ? 'Ending conversation pause' : 'End conversation pause'
  }
  return aiStateUpdating.value ? 'Resuming AI' : 'Resume AI'
})

const selectedThreadsTarget = computed(() =>
  threadsPublicEngagementTarget(selectedConversation.value),
)
const threadsPublicReplyReady = computed(() =>
  threadsPublicEngagementAccountReady(
    selectedAccount.value,
    selectedThreadsTarget.value,
  ),
)
const canSendText = computed(() => {
  const conversation = selectedConversation.value
  if (!canManageConversations.value || !conversation) return false
  if (conversation.channel === 'whatsapp') return canSendWhatsAppText.value
  if (conversation.channel === 'tiktok') return false
  if (conversation.channel === 'threads') return threadsPublicReplyReady.value
  return (
    selectedAccount.value?.status === 'active' &&
    selectedAccount.value?.config?.outbound_enabled === true &&
    selectedAccount.value?.capabilities?.text !== false
  )
})
const activeCount = computed(() => accounts.value.filter(accountReadyForOutbound).length)
const attentionCount = computed(() =>
  accounts.value.filter(
    (account) =>
      ['degraded', 'suspended'].includes(account.status) ||
      account.outbox_failed > 0 ||
      (account.provider === 'relay' &&
        account.status === 'active' &&
        account.config?.outbound_enabled !== true),
  ).length,
)
const connectionLabel = computed(() => {
  switch (connectionState.value) {
    case 'connected':
      return 'Live'
    case 'connecting':
      return 'Connecting'
    case 'reconnecting':
      return 'Reconnecting'
    default:
      return 'Offline'
  }
})
const connectionBadgeClass = computed(() =>
  connectionState.value === 'connected'
    ? 'border-emerald-400/20 text-emerald-300 light:border-emerald-300 light:text-emerald-700'
    : connectionState.value === 'disconnected'
      ? 'border-rose-400/20 text-rose-300 light:border-rose-300 light:text-rose-700'
      : 'border-amber-400/20 text-amber-300 light:border-amber-300 light:text-amber-700',
)

function accountReadyForOutbound(account: ChannelAccount) {
  if (account.status !== 'active') return false
  if (account.provider === 'meta_legacy') return true
  return account.config?.outbound_enabled === true
}

function isManagedMetaMessenger(account: ChannelAccount) {
  return (
    account.channel === 'messenger' &&
    account.provider === 'relay' &&
    (account.config?.meta_registry_managed === true ||
      account.config?.meta_management_mode === 'platform_oauth')
  )
}

function isManagedMetaInstagram(account: ChannelAccount) {
  return (
    account.channel === 'instagram' &&
    account.provider === 'relay' &&
    account.config?.instagram_api_mode === 'instagram_login' &&
    (account.config?.meta_registry_managed === true ||
      account.config?.meta_management_mode === 'platform_oauth')
  )
}

function isManagedMetaAccount(account: ChannelAccount) {
  return isManagedMetaMessenger(account) || isManagedMetaInstagram(account)
}

function managedMetaLifecycleReady(account: ChannelAccount) {
  if (isManagedMetaMessenger(account)) return metaMessengerOnboardingReady.value
  if (isManagedMetaInstagram(account)) return metaInstagramOnboardingReady.value
  return true
}

function managedMetaTeardownReady(account: ChannelAccount) {
  if (isManagedMetaMessenger(account)) return metaMessengerOnboardingReady.value
  if (isManagedMetaInstagram(account)) return metaInstagramTeardownReady.value
  return true
}

function managedMetaActionBusy(account: ChannelAccount) {
  if (isManagedMetaMessenger(account)) return metaMessengerBusy.value
  if (isManagedMetaInstagram(account)) return metaInstagramBusy.value
  return false
}

function approveManagedMeta(account: ChannelAccount) {
  if (isManagedMetaMessenger(account)) return approveMetaMessenger(account)
  if (isManagedMetaInstagram(account)) return approveMetaInstagram(account)
}

function metaMessengerPageKey(page: MetaMessengerPage) {
  return `${page.business_id ?? ''}:${page.page_id}`
}

const selectedMetaMessengerPage = computed(() =>
  metaMessengerSelection.value?.pages.find(
    page => metaMessengerPageKey(page) === selectedMetaMessengerPageKey.value,
  ),
)

function accountConnectionLabel(account: ChannelAccount) {
  if (isManagedMetaAccount(account) && account.status === 'pending') {
    return account.last_health_check_at && !account.last_error
      ? 'Awaiting admin approval'
      : 'Test connection to continue'
  }
  if (accountReadyForOutbound(account)) return 'Outbound ready'
  if (account.status === 'active') return 'Awaiting outbound approval'
  return account.status
}

function channelMeta(channel: ChannelType) {
  return supportedChannels.find((item) => item.key === channel) ?? supportedChannels[0]
}

function conversationName(conversation: InboxConversation) {
  return (
    conversation.contact_identity?.display_name ||
    conversation.contact?.profile_name ||
    conversation.contact_identity?.external_id ||
    conversation.contact?.phone_number ||
    'Unknown contact'
  )
}

function normalizeMessage(envelope: InboxMessageEnvelope): InboxMessage {
  const part = envelope.parts?.find((item) => item.text || item.caption)
  return {
    ...envelope.message,
    content: envelope.message.content || part?.text || part?.caption || '',
  }
}

function normalizeLegacyWhatsAppMessage(message: LegacyWhatsAppMessage): InboxMessage {
  const content =
    typeof message.content === 'string'
      ? message.content
      : typeof message.content?.body === 'string'
        ? message.content.body
        : ''
  return { ...message, content }
}

function upsertInboxMessage(message: InboxMessage) {
  const existingIndex = messages.value.findIndex(item => item.id === message.id)
  if (existingIndex >= 0) {
    messages.value.splice(existingIndex, 1, message)
  } else {
    messages.value.push(message)
    messageTotal.value = Math.max(messageTotal.value + 1, messages.value.length)
  }
  messages.value.sort(compareMessageIngestionOrder)
}

function isMessageViewportNearBottom() {
  const viewport = messagesViewport.value
  if (!viewport) return true
  return (
    viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight <= messageBottomThreshold
  )
}

function prefersReducedMotion() {
  return window.matchMedia?.('(prefers-reduced-motion: reduce)').matches === true
}

async function scrollMessagesToBottom(
  smooth = false,
  shouldScroll: () => boolean = () => true,
) {
  await nextTick()
  if (!shouldScroll()) return
  const viewport = messagesViewport.value
  if (!viewport) return
  if (smooth && !prefersReducedMotion() && typeof viewport.scrollTo === 'function') {
    viewport.scrollTo({ top: viewport.scrollHeight, behavior: 'smooth' })
    messageViewportFollowingBottom = true
    return
  }
  viewport.scrollTop = viewport.scrollHeight
  messageViewportFollowingBottom = true
}

function stopMessagesContentResizeObserver() {
  messagesContentResizeObserver?.disconnect()
  messagesContentResizeObserver = null
  messagesContentObservedHeight = 0
}

function startMessagesContentResizeObserver(
  sequence: number,
  conversationId: string,
  organizationContext: OrganizationRequestContext,
) {
  stopMessagesContentResizeObserver()
  const content = messagesContent.value
  if (!content || typeof ResizeObserver === 'undefined') return

  messagesContentObservedHeight = content.getBoundingClientRect().height
  const observer = new ResizeObserver(entries => {
    if (
      messagesContentResizeObserver !== observer
      || messagesContent.value !== content
      || !isCurrentConversationView(sequence, conversationId, organizationContext)
    ) return

    const entry = entries.find(candidate => candidate.target === content)
    const nextHeight = entry?.contentRect.height ?? content.getBoundingClientRect().height
    const contentGrew = nextHeight > messagesContentObservedHeight + 0.5
    messagesContentObservedHeight = nextHeight
    if (!contentGrew || !messageViewportFollowingBottom) return

    // Media, fonts, and rich previews may resize after the transcript first
    // renders. Preserve bottom-following for an operator already at the
    // newest message without pulling a scrolled-up reader away from history.
    void scrollMessagesToBottom(false, () =>
      messagesContentResizeObserver === observer
      && messagesContent.value === content
      && isCurrentConversationView(sequence, conversationId, organizationContext)
      && messageViewportFollowingBottom,
    )
  })
  messagesContentResizeObserver = observer
  observer.observe(content)
}

function updateLocalConversationPreview(conversationId: string, message: InboxMessage) {
  const apply = (conversation: InboxConversation) => {
    conversation.last_message_preview = message.content
    conversation.last_message_at = message.created_at
  }
  const listed = conversations.value.find(conversation => conversation.id === conversationId)
  if (listed) apply(listed)
  if (selectedConversation.value?.id === conversationId && selectedConversation.value !== listed) {
    apply(selectedConversation.value)
  }
}

function totalFromResponse(response: any) {
  const payload = response?.data?.data ?? response?.data
  return typeof payload?.total === 'number' ? payload.total : 0
}

function unavailableIdentityReviewState(): ContactIdentityReviewEffectiveState {
  return {
    known: false,
    ai_allowed: false,
    blocked: true,
    open_hold_count: 0,
    reason: 'identity_review_state_unavailable',
  }
}

function identityReviewIsOpen(
  state: ContactIdentityReviewEffectiveState | null | undefined,
): boolean {
  return state?.known === true
    && state.blocked === true
    && state.open_hold_count > 0
    && state.reason === 'identity_review_open'
}

function updateLocalConversationIdentityReviewState(
  conversationId: string,
  state: ContactIdentityReviewEffectiveState,
) {
  const selected = selectedConversation.value
  if (selected?.id === conversationId) selected.identity_review_ai_state = state
  const listed = conversations.value.find(conversation => conversation.id === conversationId)
  if (listed && listed !== selected) listed.identity_review_ai_state = state
}

function replaceLocalConversationSnapshot(
  conversationId: string,
  snapshot: InboxConversation,
) {
  const index = conversations.value.findIndex(conversation => conversation.id === conversationId)
  if (index >= 0) conversations.value[index] = snapshot
  if (selectedConversation.value?.id === conversationId) selectedConversation.value = snapshot
}

async function load(silent = false, append = false) {
  if (refreshInFlight) {
    // Realtime events can arrive while the canonical fetch is in flight. Keep
    // one trailing refresh instead of silently dropping the newest state.
    if (!append) refreshQueued = true
    return
  }
  const organizationContext = captureOrganizationContext()
  const projectionGeneration = conversationProjectionGeneration
  refreshInFlight = true
  if (!silent) loading.value = true
  if (append) loadingMore.value = true
  try {
    const page = append ? conversationPage.value + 1 : 1
    const conversationParams: Record<string, string | number> = { page, limit: 100 }
    if (channelFilter.value !== 'all') conversationParams.channel = channelFilter.value
    if (search.value.trim()) conversationParams.search = search.value.trim()
    const [accountResponse, conversationResponse] = await Promise.all([
      channelsService.accounts(),
      channelsService.conversations(conversationParams),
    ])
    if (
      !isCurrentOrganizationContext(organizationContext)
      || projectionGeneration !== conversationProjectionGeneration
    ) return
    accounts.value = unwrapListResponse<ChannelAccount>(accountResponse, 'accounts')
    if (settingsAccount.value) {
      settingsAccount.value =
        accounts.value.find(account => account.id === settingsAccount.value?.id) ?? null
    }
    const incoming = unwrapListResponse<InboxConversation>(conversationResponse, 'conversations')
    const incomingIDs = new Set(incoming.map((item) => item.id))
    conversationTotal.value = totalFromResponse(conversationResponse)
    if (append) {
      conversations.value = [
        ...conversations.value.filter((item) => !incomingIDs.has(item.id)),
        ...incoming,
      ]
      conversationPage.value = page
    } else if (silent) {
      conversations.value = [
        ...incoming,
        ...conversations.value.filter((item) => !incomingIDs.has(item.id)),
      ].slice(0, Math.max(conversationTotal.value, incoming.length))
    } else {
      conversations.value = incoming
      conversationPage.value = 1
    }

    if (selectedConversation.value) {
      const selectedConversationId = selectedConversation.value.id
      const refreshedSelection = conversations.value.find(item => item.id === selectedConversationId)
      // A silent page-one refresh can legitimately trim an off-page selection.
      // Retain it until the exact-ID canonical lookup below refreshes or fences it.
      if (refreshedSelection) {
        selectedConversation.value = refreshedSelection
      } else if (!silent || append) {
        selectedConversation.value = null
        stopMessagesContentResizeObserver()
        messageViewportFollowingBottom = true
      }
    }
    if (
      silent
      && !append
      && selectedConversation.value
      && !incomingIDs.has(selectedConversation.value.id)
    ) {
      const conversationId = selectedConversation.value.id
      const viewSequence = conversationViewSequence
      let canonicalConversation: InboxConversation | null = null
      let canonicalLookupSucceeded = false
      try {
        const selectedResponse = await channelsService.conversations({
          conversation_id: conversationId,
          page: 1,
          limit: 1,
        })
        const selectedRows = unwrapListResponse<InboxConversation>(selectedResponse, 'conversations')
        canonicalConversation = selectedRows.length === 1 && selectedRows[0]?.id === conversationId
          ? selectedRows[0]
          : null
        canonicalLookupSucceeded = true
      } catch {
        // A selected conversation outside page one must never retain a stale
        // allow decision when its canonical projection cannot be refreshed.
      }
      if (
        !isCurrentConversationView(viewSequence, conversationId, organizationContext)
        || projectionGeneration !== conversationProjectionGeneration
      ) return
      if (canonicalConversation) replaceLocalConversationSnapshot(conversationId, canonicalConversation)
      else if (canonicalLookupSucceeded) {
        selectedConversation.value = null
        stopMessagesContentResizeObserver()
        messageViewportFollowingBottom = true
      } else {
        updateLocalConversationIdentityReviewState(conversationId, unavailableIdentityReviewState())
      }
    }
    if (silent && selectedConversation.value) {
      const conversationId = selectedConversation.value.id
      const viewSequence = conversationViewSequence
      const messageResponse = await channelsService.messages(conversationId, { limit: 100 })
      if (
        !isCurrentConversationView(viewSequence, conversationId, organizationContext)
        || projectionGeneration !== conversationProjectionGeneration
      ) return
      const latest = unwrapListResponse<InboxMessageEnvelope>(messageResponse, 'messages')
        .map(normalizeMessage)
        .reverse()
      const shouldFollowLatestMessage = isMessageViewportNearBottom()
      messageTotal.value = totalFromResponse(messageResponse)
      const merged = new Map(messages.value.map((message) => [message.id, message]))
      for (const message of latest) merged.set(message.id, message)
      messages.value = [...merged.values()].sort(compareMessageIngestionOrder)
      if (shouldFollowLatestMessage) {
        // A smooth scroll returns before the viewport reaches its destination.
        // Use an immediate positioning step whenever this refresh may advance
        // the read cursor, then verify the boundary again below.
        const willAcknowledge = selectedConversation.value.unread_count > 0
          && document.visibilityState === 'visible'
          && document.hasFocus()
        await scrollMessagesToBottom(!willAcknowledge)
      }
      if (
        shouldFollowLatestMessage
        && document.visibilityState === 'visible'
        && document.hasFocus()
        && isMessageViewportNearBottom()
      ) {
        try {
          await markOpenConversationRead(
            selectedConversation.value,
            viewSequence,
            organizationContext,
          )
        } catch {
          // Keep it unread when the cursor update fails. A later activity
          // event or poll will retry without creating repeated error toasts.
        }
      }
    }
  } catch (error) {
    if (!silent && isCurrentOrganizationContext(organizationContext)) {
      toast.error('Channels could not be loaded', getErrorMessage(error))
    }
  } finally {
    if (!silent) loading.value = false
    loadingMore.value = false
    refreshInFlight = false
    if (organizationReloadQueued && viewMounted && activeOrganizationId.value) {
      organizationReloadQueued = false
      refreshQueued = false
      void load()
    } else if (refreshQueued && viewMounted) {
      refreshQueued = false
      void load(true)
    }
  }
}

function loadMoreConversations() {
  void load(true, true)
}

function scheduleChannelRefresh(delay = 300) {
  if (document.visibilityState !== 'visible') return
  if (syncDebounceTimer !== null) window.clearTimeout(syncDebounceTimer)
  syncDebounceTimer = window.setTimeout(() => {
    syncDebounceTimer = null
    void load(true)
  }, delay)
}

function scheduleMessageViewportReadCheck() {
  if (messageViewportScrollFrame !== null) return
  messageViewportScrollFrame = window.requestAnimationFrame(() => {
    messageViewportScrollFrame = null
    const conversation = selectedConversation.value
    const viewSequence = conversationViewSequence
    const organizationContext = captureOrganizationContext()
    if (
      !conversation
      || document.visibilityState !== 'visible'
      || !document.hasFocus()
      || !isMessageViewportNearBottom()
    ) return
    void markOpenConversationRead(conversation, viewSequence, organizationContext).catch(() => {
      // Preserve the unread marker. Focus, realtime, polling, or another
      // bottom scroll will retry the exact visible boundary.
    })
  })
}

function handleMessageViewportScroll() {
  messageViewportFollowingBottom = isMessageViewportNearBottom()
  scheduleMessageViewportReadCheck()
}

function applyInboxMessageStatus(event: InboxActivityEvent) {
  const isStatusActivity = event.type === 'status_update'
    || (event.type === 'realtime_sync' && event.payload?.kind === 'message_status_changed')
  if (!isStatusActivity) return false

  const messageID = event.payload?.message_id
  const status = event.payload?.status
  if (typeof messageID === 'string' && typeof status === 'string') {
    const message = messages.value.find(item => item.id === messageID)
    if (message) message.status = status
  }
  return true
}

function handleInboxActivity(event: InboxActivityEvent) {
  if (applyInboxMessageStatus(event)) return
  if (isInboxContentRefreshActivity(event)) scheduleChannelRefresh()
}

function catchUpInbox() {
  if (document.visibilityState !== 'visible') return
  void wsService.connect()
  scheduleChannelRefresh(0)
}

function handleOnline() {
  void wsService.connect()
  if (document.visibilityState === 'visible') scheduleChannelRefresh(0)
}

async function selectConversation(conversation: InboxConversation) {
  const organizationContext = captureOrganizationContext()
  const viewSequence = ++conversationViewSequence
  stopMessagesContentResizeObserver()
  messageViewportFollowingBottom = true
  selectedConversation.value = conversation
  messages.value = []
  messageTotal.value = 0
  loadingOlderMessages.value = false
  sending.value = false
  aiStateUpdating.value = false
  if (isWorkspaceRail.value) isWorkspaceOpen.value = true
  loadingMessages.value = true
  let loaded = false
  try {
    const response = await channelsService.messages(conversation.id, { limit: 100 })
    if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
    messages.value = unwrapListResponse<InboxMessageEnvelope>(response, 'messages')
      .map(normalizeMessage)
      .reverse()
    messageTotal.value = totalFromResponse(response)
    loaded = true
  } catch (error) {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      toast.error('Messages could not be loaded', getErrorMessage(error))
    }
  } finally {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      loadingMessages.value = false
    }
  }

  if (!loaded || !isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
  await scrollMessagesToBottom()
  if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
  startMessagesContentResizeObserver(viewSequence, conversation.id, organizationContext)

  // A tab switch can happen while the transcript request is in flight. Keep
  // the viewport ready at the latest message, but do not acknowledge anything
  // until this document is actually active and the bottom boundary is proven.
  if (
    document.visibilityState !== 'visible'
    || !document.hasFocus()
    || !isMessageViewportNearBottom()
  ) return

  // Rendering and positioning the transcript must never wait on the cursor
  // mutation. The exact visible boundary is posted only after the scroll.
  void markOpenConversationRead(conversation, viewSequence, organizationContext).catch(error => {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      toast.error('Read state could not be updated', getErrorMessage(error))
    }
  })
}

function closeMobileConversation() {
  conversationViewSequence += 1
  stopMessagesContentResizeObserver()
  messageViewportFollowingBottom = true
  selectedConversation.value = null
  isWorkspaceOpen.value = false
  messages.value = []
  messageTotal.value = 0
  composer.value = ''
  loadingMessages.value = false
  loadingOlderMessages.value = false
  sending.value = false
  aiStateUpdating.value = false
  readCursorCheckQueued = false
}

function resetOrganizationScopedInboxState(organizationId: string | null) {
  bookingSettingsRequest += 1
  bookingSettingsSaving.value = false
  bookingSettingsError.value = ''
  conversationProjectionGeneration += 1
  conversationViewSequence += 1
  stopMessagesContentResizeObserver()
  messageViewportFollowingBottom = true
  accounts.value = []
  conversations.value = []
  selectedConversation.value = null
  messages.value = []
  settingsAccount.value = null
  createdConnection.value = null
  isWorkspaceOpen.value = false
  isIdentityReviewOpen.value = false
  showConnect.value = false
  conversationPage.value = 1
  conversationTotal.value = 0
  messageTotal.value = 0
  composer.value = ''
  loading.value = Boolean(organizationId)
  loadingMessages.value = false
  loadingMore.value = false
  loadingOlderMessages.value = false
  sending.value = false
  aiStateUpdating.value = false
  readCursorInFlightKey = null
  readCursorCheckQueued = false
}

async function loadOlderMessages() {
  const conversation = selectedConversation.value
  const oldest = messages.value[0]
  if (!conversation || !oldest || loadingOlderMessages.value || !hasOlderMessages.value) return
  const organizationContext = captureOrganizationContext()
  const viewSequence = conversationViewSequence
  const viewport = messagesViewport.value
  const previousScrollHeight = viewport?.scrollHeight ?? 0
  const previousScrollTop = viewport?.scrollTop ?? 0
  loadingOlderMessages.value = true
  try {
    const response = await channelsService.messages(conversation.id, {
      before: oldest.id,
      limit: 100,
    })
    if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
    const older = unwrapListResponse<InboxMessageEnvelope>(response, 'messages')
      .map(normalizeMessage)
      .reverse()
    messageTotal.value = totalFromResponse(response)
    const existingIDs = new Set(messages.value.map((message) => message.id))
    messages.value = [
      ...older.filter((message) => !existingIDs.has(message.id)),
      ...messages.value,
    ]
    await nextTick()
    if (
      isCurrentConversationView(viewSequence, conversation.id, organizationContext) &&
      viewport &&
      messagesViewport.value === viewport
    ) {
      viewport.scrollTop = previousScrollTop + (viewport.scrollHeight - previousScrollHeight)
    }
  } catch (error) {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      toast.error('Older messages could not be loaded', getErrorMessage(error))
    }
  } finally {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      loadingOlderMessages.value = false
    }
  }
}

async function sendMessage() {
  const conversation = selectedConversation.value
  const messageText = composer.value.trim()
  if (!conversation || !messageText || sending.value) return
  const threadsTarget =
    conversation.channel === 'threads'
      ? threadsPublicEngagementTarget(conversation)
      : null
  if (conversation.channel === 'threads' && !threadsTarget) {
    toast.warning(
      'Select an existing public Threads reply or mention before replying.',
    )
    return
  }
  if (!canSendText.value) return
  const organizationContext = captureOrganizationContext()
  const viewSequence = conversationViewSequence
  sending.value = true
  try {
    if (conversation.channel === 'whatsapp') {
      const organizationId = organizationContext.organizationId
      const serviceWindowEndsAt = conversation.service_window_ends_at
      if (!organizationId || !serviceWindowEndsAt) return
      const attempt = await getOrCreateLegacyWhatsAppReplyAttempt({
        organizationId,
        conversationId: conversation.id,
        body: messageText,
        serviceWindowEndsAt,
      })
      if (
        !isCurrentConversationView(viewSequence, conversation.id, organizationContext) ||
        activeOrganizationId.value !== organizationId
      ) {
        // A newly-created key has not reached the server and is safe to drop.
        // A reused key represents an earlier ambiguous request and must remain.
        if (!attempt.reused) {
          clearLegacyWhatsAppReplyAttempt(
            { organizationId, conversationId: conversation.id },
            attempt.key,
          )
        }
        return
      }
      const response = await channelsService.replyLegacyWhatsApp(conversation.id, {
        type: 'text',
        content: { body: messageText },
        idempotency_key: attempt.key,
      }, organizationId)
      const result = unwrapItemResponse<LegacyWhatsAppMessage>(response)
      const message = normalizeLegacyWhatsAppMessage(result)
      const replyAcknowledged = acknowledgedLegacyWhatsAppStatuses.has(message.status)
      let acknowledgedAttemptCleanupFailed = false
      if (replyAcknowledged) {
        try {
          clearLegacyWhatsAppReplyAttempt(
            { organizationId, conversationId: conversation.id },
            attempt.key,
          )
        } catch (error) {
          if (error instanceof LegacyWhatsAppReplyAttemptStorageError) {
            acknowledgedAttemptCleanupFailed = true
          } else {
            throw error
          }
        }
      }
      // A settled response belongs to the original conversation even if the
      // operator switched away while it was in flight. Retire its retry key
      // before applying the view guard, otherwise a later same-body send could
      // replay the already-sent backend row. All UI mutations remain guarded.
      if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
      upsertInboxMessage(message)
      updateLocalConversationPreview(conversation.id, message)
      if (replyAcknowledged) {
        if (acknowledgedAttemptCleanupFailed) {
          toast.warning(
            'WhatsApp reply sent',
            'The server confirmed the reply, but secure retry state could not be cleared. Open native Chat before sending another reply.',
          )
        }
        composer.value = ''
      }
      void scrollMessagesToBottom(true)
      scheduleChannelRefresh(0)
      return
    }

    const payload: Record<string, unknown> = {
      idempotency_key: crypto.randomUUID(),
      purpose: 'service',
      parts: [{ type: 'text', text: messageText }],
    }
    if (threadsTarget) {
      payload.reply_to_external_id = threadsTarget.externalId
    }
    const response = await channelsService.send(conversation.id, payload)
    if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
    const result = unwrapItemResponse<InboxMessageEnvelope>(response)
    const message = normalizeMessage(result)
    upsertInboxMessage(message)
    updateLocalConversationPreview(conversation.id, message)
    updateLocalConversationAIState(conversation.id, true, 'human_reply')
    composer.value = ''
    void scrollMessagesToBottom(true)
    scheduleChannelRefresh(0)
  } catch (error) {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      const status = (error as { response?: { status?: number } })?.response?.status
      if (
        conversation.channel === 'whatsapp' &&
        error instanceof LegacyWhatsAppReplyAttemptStorageError
      ) {
        toast.error(
          'WhatsApp reply unavailable',
          'Secure retry storage is unavailable. No message was sent; open native Chat to reply safely.',
        )
      } else if (conversation.channel === 'whatsapp' && status === 404) {
        toast.error(
          'WhatsApp reply unavailable',
          'Omnichannel direct reply is not enabled on this server. No message was sent.',
        )
      } else if (conversation.channel === 'whatsapp' && status === 409) {
        toast.warning('WhatsApp reply unavailable', getErrorMessage(error))
      } else {
        toast.error('Message was not sent', getErrorMessage(error))
      }
    }
  } finally {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      sending.value = false
    }
  }
}

function updateLocalConversationAIState(
  conversationId: string,
  paused: boolean,
  reason = '',
  effectiveAIState?: ContactIdentityReviewEffectiveState,
) {
  const selected = selectedConversation.value
  if (selected?.id === conversationId) {
    selected.ai_paused = paused
    selected.ai_pause_reason = reason || undefined
    if (effectiveAIState) selected.identity_review_ai_state = effectiveAIState
  }
  const listed = conversations.value.find(conversation => conversation.id === conversationId)
  if (listed && listed !== selected) {
    listed.ai_paused = paused
    listed.ai_pause_reason = reason || undefined
    if (effectiveAIState) listed.identity_review_ai_state = effectiveAIState
  }
}

async function toggleConversationAI() {
  const conversation = selectedConversation.value
  if (!conversation || !canControlConversationAI.value || aiStateUpdating.value) return
  const organizationContext = captureOrganizationContext()
  const viewSequence = conversationViewSequence
  const paused = !conversation.ai_paused
  aiStateUpdating.value = true
  try {
    const response = await channelsService.setAIState(conversation.id, paused)
    const state = unwrapItemResponse<{
      conversation_id: string
      ai_paused: boolean
      ai_pause_reason?: string
      identity_review_ai_state: ContactIdentityReviewEffectiveState
    }>(response)
    if (!isCurrentConversationView(viewSequence, conversation.id, organizationContext)) return
    updateLocalConversationAIState(
      conversation.id,
      state.ai_paused,
      state.ai_pause_reason,
      state.identity_review_ai_state,
    )
    if (state.ai_paused) {
      toast.success('AI replies paused', 'Only human replies will be sent in this conversation.')
    } else if (effectiveAIIsAllowed(state.identity_review_ai_state)) {
      toast.success('AI replies resumed', 'The next eligible customer message can receive an automatic reply.')
    } else {
      toast.warning(
        'Conversation pause removed',
        'Automated replies remain blocked until the identity review is resolved.',
      )
    }
  } catch (error) {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      toast.error('AI reply state was not changed', getErrorMessage(error))
    }
  } finally {
    if (isCurrentConversationView(viewSequence, conversation.id, organizationContext)) {
      aiStateUpdating.value = false
    }
  }
}

async function refreshAfterIdentityReview(state: ContactIdentityReviewEffectiveState) {
  const conversationID = selectedConversation.value?.id
  if (conversationID) {
    // Invalidate any older same-organization list response before projecting
    // the durable decision. The queued canonical refresh may then replace it.
    conversationProjectionGeneration += 1
    const selected = selectedConversation.value
    if (selected?.id === conversationID) selected.identity_review_ai_state = state
    const listed = conversations.value.find(conversation => conversation.id === conversationID)
    if (listed) listed.identity_review_ai_state = state
  }
  await load(true)
}

async function connectAccount() {
  if (newAccount.channel === 'messenger') {
    toast.warning('Messenger must be connected through managed Facebook authorization')
    return
  }
  if (newAccount.channel === 'instagram' && !metaInstagramStaticConnectionAvailable.value) {
    toast.warning(
      metaInstagramAvailability.value === 'managed'
        ? 'This workspace connects Instagram through managed Instagram authorization'
        : 'Instagram connection policy is not available yet',
    )
    return
  }
  if (
    !newAccount.name.trim() ||
    !newAccount.external_account_id.trim() ||
    !newAccount.relay_url.trim()
  ) {
    toast.warning('Connection name, external account ID and HTTPS relay URL are required')
    return
  }
  try {
    const meta = connectableChannels.value.find(channel => channel.key === newAccount.channel)
    if (!meta) {
      toast.warning('This channel is not included in the active workspace plan')
      return
    }
    const response = await channelsService.createAccount({
      channel: newAccount.channel,
      provider: meta.provider,
      name: newAccount.name.trim(),
      external_account_id: newAccount.external_account_id.trim(),
      config: {
        relay_url: newAccount.relay_url.trim(),
      },
    })
    const created = unwrapItemResponse<CreatedConnection>(response)
    createdConnection.value = created
    showConnect.value = false
    newAccount.name = ''
    newAccount.external_account_id = ''
    newAccount.relay_url = ''
    toast.success('Connection created', 'Copy the one-time signing secret now; it will not be shown again.')
    await load()
  } catch (error) {
    toast.error('Connection was not created', getErrorMessage(error))
  }
}

function isMetaMessengerContextCurrent(organizationId: string, version: number) {
  return (
    organizationId.length > 0 &&
    activeOrganizationId.value === organizationId &&
    metaMessengerContextVersion === version
  )
}

function metaMessengerNeedsSubscriptionReconciliation(account: ChannelAccount) {
  return (
    isManagedMetaMessenger(account) &&
    account.meta_subscription_reconciliation_required
  )
}

function metaInstagramNeedsSubscriptionReconciliation(account: ChannelAccount) {
  return (
    isManagedMetaInstagram(account) &&
    account.meta_subscription_reconciliation_required
  )
}

function metaInstagramCanReconcile(account: ChannelAccount) {
  if (!metaInstagramNeedsSubscriptionReconciliation(account)) return false
  return metaInstagramReconciliationAvailable(
    metaInstagramStatus.value,
    account.meta_subscription_reconciliation_desired_state,
  )
}

function setMetaMessengerSelection(
  selection: MetaMessengerSelection,
  organizationId: string,
  reconnectName = '',
) {
  if (selection.workspace.organization_id !== organizationId) {
    throw new MetaMessengerOrganizationChangedError()
  }
  metaMessengerSelection.value = selection
  metaMessengerSelectionOrganizationId.value = organizationId
  metaMessengerReconnectName.value = reconnectName
  const firstSelectable = selection.pages.find(page => page.selectable && page.business_id)
  selectedMetaMessengerPageKey.value = firstSelectable
    ? metaMessengerPageKey(firstSelectable)
    : ''
}

async function connectMetaMessenger() {
  if (
    !canManageMetaMessenger.value ||
    !metaMessengerOnboardingReady.value ||
    metaMessengerBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) {
    toast.error('Select a workspace before connecting Messenger')
    return
  }
  const contextVersion = metaMessengerContextVersion
  const controller = new AbortController()
  metaMessengerAuthorizationController.value?.abort()
  metaMessengerAuthorizationController.value = controller
  metaMessengerBusy.value = true
  try {
    const selection = await metaMessengerOnboarding.begin(
      organizationId,
      () => isMetaMessengerContextCurrent(organizationId, contextVersion),
      controller.signal,
    )
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    setMetaMessengerSelection(selection, organizationId)
    showConnect.value = false
  } catch (error) {
    if (
      isMetaMessengerContextCurrent(organizationId, contextVersion) &&
      !(error instanceof MetaMessengerAuthorizationCancelledError) &&
      !(error instanceof MetaMessengerOrganizationChangedError)
    ) {
      toast.error('Messenger authorization did not finish', getErrorMessage(error))
    }
  } finally {
    if (metaMessengerAuthorizationController.value === controller) {
      metaMessengerAuthorizationController.value = null
    }
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaMessengerBusy.value = false
    }
  }
}

async function reconnectMetaMessenger(account: ChannelAccount) {
  if (
    !canManageMetaMessenger.value ||
    !metaMessengerOnboardingReady.value ||
    !isManagedMetaMessenger(account) ||
    metaMessengerBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) return
  const contextVersion = metaMessengerContextVersion
  const controller = new AbortController()
  metaMessengerAuthorizationController.value?.abort()
  metaMessengerAuthorizationController.value = controller
  metaMessengerBusy.value = true
  try {
    const selection = await metaMessengerOnboarding.reconnect(
      account.id,
      organizationId,
      () => isMetaMessengerContextCurrent(organizationId, contextVersion),
      controller.signal,
    )
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    setMetaMessengerSelection(selection, organizationId, account.name)
    settingsAccount.value = null
  } catch (error) {
    if (
      isMetaMessengerContextCurrent(organizationId, contextVersion) &&
      !(error instanceof MetaMessengerAuthorizationCancelledError) &&
      !(error instanceof MetaMessengerOrganizationChangedError)
    ) {
      toast.error('Messenger reconnect did not finish', getErrorMessage(error))
    }
  } finally {
    if (metaMessengerAuthorizationController.value === controller) {
      metaMessengerAuthorizationController.value = null
    }
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaMessengerBusy.value = false
    }
  }
}

function cancelMetaMessengerAuthorization() {
  if (!metaMessengerAuthorizationController.value) return
  metaMessengerAuthorizationController.value.abort()
  metaMessengerAuthorizationController.value = null
  metaMessengerContextVersion += 1
  metaMessengerBusy.value = false
  closeMetaMessengerSelection()
  toast.error('Messenger authorization was cancelled')
}

function closeMetaMessengerSelection() {
  metaMessengerSelection.value = null
  metaMessengerSelectionOrganizationId.value = ''
  selectedMetaMessengerPageKey.value = ''
  metaMessengerReconnectName.value = ''
}

async function confirmMetaMessengerPage() {
  const selection = metaMessengerSelection.value
  const page = selectedMetaMessengerPage.value
  if (
    !canManageMetaMessenger.value ||
    !metaMessengerOnboardingReady.value ||
    !selection ||
    !page?.business_id ||
    !page.selectable ||
    metaMessengerBusy.value
  ) return
  const organizationId = metaMessengerSelectionOrganizationId.value
  const contextVersion = metaMessengerContextVersion
  if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) {
    closeMetaMessengerSelection()
    return
  }
  metaMessengerBusy.value = true
  try {
    await metaMessengerOnboarding.select(
      organizationId,
      selection.session_id,
      page.business_id,
      page.page_id,
    )
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    const reconnected = Boolean(metaMessengerReconnectName.value)
    closeMetaMessengerSelection()
    toast.success(
      reconnected ? 'Messenger credentials reconnected' : 'Messenger Page connected safely',
      'Run Test, review the result, then approve activation. Outbound and AI remain off until then.',
    )
    await load()
  } catch (error) {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      toast.error('Messenger Page was not connected', getErrorMessage(error))
    }
  } finally {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaMessengerBusy.value = false
    }
  }
}

async function connectMetaInstagram() {
  if (
    !canManageMetaInstagram.value ||
    !metaInstagramOnboardingReady.value ||
    metaInstagramBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) {
    toast.error('Select a workspace before connecting Instagram')
    return
  }
  const contextVersion = metaMessengerContextVersion
  metaInstagramBusy.value = true
  try {
    const start = await metaInstagramOnboarding.begin(organizationId)
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      throw new MetaInstagramOrganizationChangedError()
    }
    window.location.assign(start.authorization_url)
  } catch (error) {
    if (
      isMetaMessengerContextCurrent(organizationId, contextVersion) &&
      !(error instanceof MetaInstagramOrganizationChangedError)
    ) {
      toast.error('Instagram authorization did not start', getErrorMessage(error))
    }
    metaInstagramBusy.value = false
  }
}

async function reconnectMetaInstagram(account: ChannelAccount) {
  if (
    !canManageMetaInstagram.value ||
    !metaInstagramOnboardingReady.value ||
    !isManagedMetaInstagram(account) ||
    metaInstagramBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) return
  const contextVersion = metaMessengerContextVersion
  metaInstagramBusy.value = true
  try {
    const start = await metaInstagramOnboarding.reconnect(account.id, organizationId)
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      throw new MetaInstagramOrganizationChangedError()
    }
    settingsAccount.value = null
    window.location.assign(start.authorization_url)
  } catch (error) {
    if (
      isMetaMessengerContextCurrent(organizationId, contextVersion) &&
      !(error instanceof MetaInstagramOrganizationChangedError)
    ) {
      toast.error('Instagram reconnect did not start', getErrorMessage(error))
    }
    metaInstagramBusy.value = false
  }
}

async function copyConnectionValue(label: string, value: string) {
  try {
    await navigator.clipboard.writeText(value)
    toast.success(`${label} copied`)
  } catch {
    toast.error(`${label} was not copied`, 'Select the value and copy it manually.')
  }
}

async function testAccount(account: ChannelAccount) {
  try {
    const response = await channelsService.testAccount(account.id)
    const result = unwrapItemResponse<{
      success: boolean
      error?: string
    }>(response)
    if (!result.success) {
      toast.error(`${account.name} needs attention`, result.error || 'The adapter test failed')
      await load()
      return
    }
    toast.success(
      `${account.name} is reachable`,
      account.provider === 'relay'
        ? 'Review the result, then explicitly approve outbound delivery.'
        : undefined,
    )
    await load()
  } catch (error) {
    toast.error(`${account.name} needs attention`, getErrorMessage(error))
  }
}

async function approveOutbound(account: ChannelAccount) {
  try {
    await channelsService.updateAccount(account.id, { outbound_enabled: true })
    toast.success(
      `${account.name} outbound approved`,
      'Agents may now send through this tested relay.',
    )
    await load()
  } catch (error) {
    toast.error('Outbound approval was not changed', getErrorMessage(error))
  }
}

async function approveMetaMessenger(account: ChannelAccount) {
  if (
    !canManageMetaMessenger.value ||
    !metaMessengerOnboardingReady.value ||
    !isManagedMetaMessenger(account) ||
    metaMessengerBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) return
  const contextVersion = metaMessengerContextVersion
  metaMessengerBusy.value = true
  try {
    await metaMessengerOnboarding.approve(account.id, organizationId)
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    toast.success(
      `${account.name} activated`,
      'Agent replies are enabled. Automatic AI replies remain off until you opt in.',
    )
    await load()
  } catch (error) {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      toast.error('Messenger activation was not approved', getErrorMessage(error))
    }
  } finally {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaMessengerBusy.value = false
    }
  }
}

async function approveMetaInstagram(account: ChannelAccount) {
  if (
    !canManageMetaInstagram.value ||
    !metaInstagramOnboardingReady.value ||
    !isManagedMetaInstagram(account) ||
    metaInstagramBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) return
  const contextVersion = metaMessengerContextVersion
  metaInstagramBusy.value = true
  try {
    await metaInstagramOnboarding.approve(account.id, organizationId)
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    toast.success(
      `${account.name} activated`,
      'Agent replies are enabled. Automatic AI replies remain off until you opt in.',
    )
    await load()
  } catch (error) {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      toast.error('Instagram activation was not approved', getErrorMessage(error))
    }
  } finally {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaInstagramBusy.value = false
    }
  }
}

async function reconcileMetaMessenger(account: ChannelAccount) {
  if (
    !canReconcileMetaMessenger.value ||
    !metaMessengerOnboardingReady.value ||
    !metaMessengerNeedsSubscriptionReconciliation(account) ||
    metaMessengerBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) return
  const contextVersion = metaMessengerContextVersion
  if (!window.confirm(
    `Reconcile ${account.name}? ReReply will repeat only the already-recorded Meta subscription operation and verify its exact result.`,
  )) return
  metaMessengerBusy.value = true
  try {
    await metaMessengerOnboarding.reconcile(account.id, organizationId)
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    settingsAccount.value = null
    toast.success(
      'Messenger subscription reconciled',
      'The recorded operation was verified. Run Test and approve activation if the account is pending.',
    )
    await load()
  } catch (error) {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      toast.error('Messenger subscription was not reconciled', getErrorMessage(error))
    }
  } finally {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaMessengerBusy.value = false
    }
  }
}

async function reconcileMetaInstagram(account: ChannelAccount) {
  if (
    !canReconcileMetaInstagram.value ||
    !metaInstagramCanReconcile(account) ||
    metaInstagramBusy.value
  ) return
  const organizationId = activeOrganizationId.value
  if (!organizationId) return
  const contextVersion = metaMessengerContextVersion
  if (!window.confirm(
    `Reconcile ${account.name}? ReReply will repeat only the recorded Instagram subscription operation and verify its exact result.`,
  )) return
  metaInstagramBusy.value = true
  try {
    await metaInstagramOnboarding.reconcile(account.id, organizationId)
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    settingsAccount.value = null
    toast.success(
      'Instagram subscription reconciled',
      'The recorded operation was verified. Run Test and approve activation if the account is pending.',
    )
    await load()
  } catch (error) {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      toast.error('Instagram subscription was not reconciled', getErrorMessage(error))
    }
  } finally {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      metaInstagramBusy.value = false
    }
  }
}

function openAccountSettings(account: ChannelAccount) {
  if (bookingSettingsSaving.value) return
  settingsAccount.value = account
  bookingSettingsError.value = ''
  accountSettingsDraft.name = account.name
  accountSettingsDraft.relay_url =
    typeof account.config?.relay_url === 'string' ? account.config.relay_url : ''
  accountSettingsDraft.outbound_secret = ''
  accountSettingsDraft.ai_reply_enabled = account.config?.ai_reply_enabled === true
  accountSettingsDraft.ai_booking_enabled = account.config?.ai_booking_enabled === true
}

function supportsAIBooking(account: ChannelAccount) {
  return (account.channel === 'whatsapp' && account.provider === 'meta_legacy')
    || (['instagram', 'messenger'].includes(account.channel) && account.provider === 'relay')
}

function canEnableAIBooking(account: ChannelAccount) {
  return account.status === 'active' && (account.provider === 'meta_legacy'
    || (account.config?.outbound_enabled === true && account.config?.ai_reply_enabled === true))
}

async function saveAIBookingSettings() {
  const account = settingsAccount.value
  const organizationId = activeOrganizationId.value
  if (!account || !organizationId || !canManageAIBooking.value || bookingSettingsSaving.value
    || !supportsAIBooking(account)
    || (accountSettingsDraft.ai_booking_enabled && !canEnableAIBooking(account))) return
  const generation = organizationGeneration
  const request = ++bookingSettingsRequest
  const isCurrent = () => request === bookingSettingsRequest
    && generation === organizationGeneration && organizationId === activeOrganizationId.value
    && settingsAccount.value?.id === account.id
  bookingSettingsSaving.value = true
  bookingSettingsError.value = ''
  try {
    // Native shadows accept this dedicated field only: never submit a name,
    // raw config, profile setting, routing value or generic outbound approval.
    await channelsService.updateAccount(account.id, {
      ai_booking_enabled: accountSettingsDraft.ai_booking_enabled,
    })
    if (!isCurrent()) return
    settingsAccount.value = null
    toast.success('AI booking setting saved', 'Only this channel was changed. Customers must explicitly confirm an offered scheduled slot.')
    await load()
  } catch (error) {
    if (!isCurrent()) return
    bookingSettingsError.value = getErrorMessage(error)
    toast.error('AI booking setting was not saved', bookingSettingsError.value)
  } finally {
    if (request === bookingSettingsRequest) bookingSettingsSaving.value = false
  }
}

async function saveAccountSettings() {
  const account = settingsAccount.value
  if (!account || account.provider === 'meta_legacy' || bookingSettingsSaving.value
    || !canManageAccounts.value || !accountSettingsDraft.name.trim()) return
  const managedMeta = isManagedMetaAccount(account)
  if (
    account.provider === 'relay' &&
    !managedMeta &&
    !accountSettingsDraft.relay_url.trim()
  ) {
    toast.warning('A signed-relay URL is required')
    return
  }
  try {
    const update: Record<string, unknown> = {
      name: accountSettingsDraft.name.trim(),
    }
    if (account.provider === 'relay' && !managedMeta) {
      const config: Record<string, unknown> = {
        ...account.config,
        relay_url: accountSettingsDraft.relay_url.trim(),
      }
      for (const key of ['ai_booking_enabled', 'ai_booking_revision', 'ai_booking_enabled_at', 'ai_booking_route_sha256']) {
        delete config[key]
      }
      update.config = config
    }
    if (!managedMeta && accountSettingsDraft.outbound_secret.trim()) {
      update.outbound_secret = accountSettingsDraft.outbound_secret
    }
    if (['instagram', 'messenger'].includes(account.channel)) {
      update.ai_reply_enabled = accountSettingsDraft.ai_reply_enabled
    }
    await channelsService.updateAccount(account.id, update)
    settingsAccount.value = null
    accountSettingsDraft.outbound_secret = ''
    toast.success('Channel settings saved', 'Run Test again after changing the relay URL or signing credential.')
    await load()
  } catch (error) {
    toast.error('Channel settings were not saved', getErrorMessage(error))
  }
}

async function disableOutbound() {
  const account = settingsAccount.value
  if (!account || !canManageAccounts.value) return
  try {
    await channelsService.updateAccount(account.id, { outbound_enabled: false })
    settingsAccount.value = null
    toast.success('Outbound delivery disabled', 'Queued work will remain visible while new agent replies are blocked.')
    await load()
  } catch (error) {
    toast.error('Outbound delivery was not disabled', getErrorMessage(error))
  }
}

async function disconnectAccount() {
  const account = settingsAccount.value
  if (!account || !canDeleteAccounts.value || metaMessengerBusy.value || metaInstagramBusy.value) return
  if (isManagedMetaAccount(account) && !canManageIntegrations.value) return
  const organizationId = activeOrganizationId.value
  const contextVersion = metaMessengerContextVersion
  if (!organizationId) return
  const confirmed = window.confirm(
    isManagedMetaMessenger(account)
      ? `Disconnect ${account.name}? ReReply will unsubscribe this exact Facebook Page and revoke both credential versions. Existing CRM history remains.`
      : isManagedMetaInstagram(account)
        ? `Disconnect ${account.name}? ReReply will unsubscribe this exact Instagram profile and revoke both credential versions. Existing CRM history remains.`
        : `Disconnect ${account.name}? New inbound and outbound traffic for this relay will stop. Existing CRM history remains.`,
  )
  if (!confirmed) return
  if (isManagedMetaMessenger(account)) metaMessengerBusy.value = true
  if (isManagedMetaInstagram(account)) metaInstagramBusy.value = true
  try {
    if (isManagedMetaMessenger(account)) {
      if (!metaMessengerOnboardingReady.value) {
        throw new Error('Managed Messenger lifecycle is unavailable')
      }
      if (!account.external_account_id) {
        throw new Error('The managed Messenger Page ID is missing')
      }
      await metaMessengerOnboarding.disconnect(
        account.id,
        account.external_account_id,
        organizationId,
      )
    } else if (isManagedMetaInstagram(account)) {
      if (!metaInstagramTeardownReady.value) {
        throw new Error('Managed Instagram lifecycle is unavailable')
      }
      if (!account.external_account_id) {
        throw new Error('The managed Instagram profile ID is missing')
      }
      await metaInstagramOnboarding.disconnect(
        account.id,
        account.external_account_id,
        organizationId,
      )
    } else {
      await channelsService.disconnectAccount(account.id)
    }
    if (!isMetaMessengerContextCurrent(organizationId, contextVersion)) return
    settingsAccount.value = null
    if (selectedConversation.value?.channel_account_id === account.id) {
      closeMobileConversation()
    }
    toast.success('Channel disconnected')
    await load()
  } catch (error) {
    if (isMetaMessengerContextCurrent(organizationId, contextVersion)) {
      toast.error('Channel was not disconnected', getErrorMessage(error))
    }
  } finally {
    if (
      isManagedMetaMessenger(account) &&
      isMetaMessengerContextCurrent(organizationId, contextVersion)
    ) {
      metaMessengerBusy.value = false
    }
    if (
      isManagedMetaInstagram(account) &&
      isMetaMessengerContextCurrent(organizationId, contextVersion)
    ) {
      metaInstagramBusy.value = false
    }
  }
}

function formatTime(value?: string) {
  if (!value) return ''
  const date = new Date(value)
  const today = new Date()
  if (date.toDateString() === today.toDateString()) {
    return new Intl.DateTimeFormat('en-MY', { hour: '2-digit', minute: '2-digit' }).format(date)
  }
  return new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short' }).format(date)
}

async function loadMetaMessengerOnboardingStatus() {
  const organizationId = activeOrganizationId.value
  const requestSequence = ++metaMessengerStatusSequence
  metaMessengerOnboardingReady.value = false
  if (!organizationId) return
  try {
    const enabled = await metaMessengerOnboarding.status(organizationId)
    if (
      requestSequence === metaMessengerStatusSequence &&
      activeOrganizationId.value === organizationId
    ) {
      metaMessengerOnboardingReady.value = enabled
    }
  } catch {
    if (
      requestSequence === metaMessengerStatusSequence &&
      activeOrganizationId.value === organizationId
    ) {
      metaMessengerOnboardingReady.value = false
    }
  }
}

async function loadMetaInstagramOnboardingStatus() {
  const organizationId = activeOrganizationId.value
  const requestSequence = ++metaInstagramStatusSequence
  metaInstagramState.value = {
    availability: organizationId ? 'loading' : 'error',
    organizationId: organizationId ?? '',
    status: null,
  }
  if (!organizationId) return
  try {
    const status = await metaInstagramOnboarding.status(organizationId)
    if (
      requestSequence === metaInstagramStatusSequence &&
      activeOrganizationId.value === organizationId
    ) {
      metaInstagramState.value = {
        availability: 'managed',
        organizationId,
        status,
      }
    }
  } catch (error) {
    if (
      requestSequence === metaInstagramStatusSequence &&
      activeOrganizationId.value === organizationId
    ) {
      metaInstagramState.value = {
        availability: error instanceof MetaInstagramOffPilotError ? 'off_pilot' : 'error',
        organizationId,
        status: null,
      }
    }
  }
}

function consumeMetaInstagramCallbackResult() {
  const currentURL = new URL(window.location.href)
  const result = currentURL.searchParams.get('managed_instagram')
  if (!result) return
  currentURL.searchParams.delete('managed_instagram')
  window.history.replaceState(
    {},
    '',
    `${currentURL.pathname}${currentURL.search}${currentURL.hash}`,
  )
  switch (result) {
    case 'pending':
      toast.success(
        'Instagram profile connected safely',
        'It remains pending. Run Test, review the result, then approve activation.',
      )
      void load()
      break
    case 'reconcile':
      toast.error(
        'Instagram subscription needs reconciliation',
        'Open connection settings and reconcile the recorded provider operation.',
      )
      void load()
      break
    case 'cancelled':
      toast.warning('Instagram authorization was cancelled')
      break
    default:
      toast.error('Instagram authorization did not finish')
  }
}

onMounted(() => {
  viewMounted = true
  window.addEventListener('beforeunload', guardMetaMessengerNavigation)
  document.addEventListener('visibilitychange', catchUpInbox)
  window.addEventListener('focus', catchUpInbox)
  window.addEventListener('online', handleOnline)
  void load()
  void loadMetaMessengerOnboardingStatus()
  void loadMetaInstagramOnboardingStatus()
  consumeMetaInstagramCallbackResult()
  stopInboxActivity = wsService.onInboxActivity(handleInboxActivity)
  stopConnectionState = wsService.onConnectionStateChange(state => {
    connectionState.value = state
  })
  void wsService.connect()
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') scheduleChannelRefresh(0)
  }, 30000)
})

watch(metaMessengerWorkspaceLocked, locked => {
  if (locked) {
    organizationsStore.blockOrganizationSwitch(
      metaMessengerSwitchLockOwner,
      metaMessengerSwitchLockMessage,
    )
  } else {
    organizationsStore.unblockOrganizationSwitch(metaMessengerSwitchLockOwner)
  }
})

watch(activeOrganizationId, (organizationId, previousOrganizationId) => {
  organizationGeneration += 1
  resetOrganizationScopedInboxState(organizationId)
  if (syncDebounceTimer !== null) {
    window.clearTimeout(syncDebounceTimer)
    syncDebounceTimer = null
  }
  if (filterDebounceTimer !== null) {
    window.clearTimeout(filterDebounceTimer)
    filterDebounceTimer = null
  }
  refreshQueued = false
  if (viewMounted && organizationId) {
    if (refreshInFlight) organizationReloadQueued = true
    else void load()
  } else {
    organizationReloadQueued = false
  }
  metaMessengerAuthorizationController.value?.abort()
  metaMessengerAuthorizationController.value = null
  metaMessengerContextVersion += 1
  metaMessengerStatusSequence += 1
  metaInstagramStatusSequence += 1
  const detached = Boolean(
    previousOrganizationId &&
    previousOrganizationId !== organizationId &&
    (metaMessengerBusy.value || metaMessengerSelection.value || metaInstagramBusy.value),
  )
  metaMessengerBusy.value = false
  metaInstagramBusy.value = false
  metaMessengerOnboardingReady.value = false
  metaInstagramState.value = {
    availability: organizationId ? 'loading' : 'error',
    organizationId: organizationId ?? '',
    status: null,
  }
  closeMetaMessengerSelection()
  if (detached) {
    toast.error('Managed Meta onboarding was detached because the active workspace changed')
  }
  void loadMetaMessengerOnboardingStatus()
  void loadMetaInstagramOnboardingStatus()
})

watch([search, channelFilter], () => {
  if (filterDebounceTimer !== null) window.clearTimeout(filterDebounceTimer)
  filterDebounceTimer = window.setTimeout(() => {
    filterDebounceTimer = null
    closeMobileConversation()
    void load()
  }, 300)
})

watch(
  connectableChannels,
  channels => {
    if (!channels.some(channel => channel.key === newAccount.channel) && channels[0]) {
      newAccount.channel = channels[0].key
    }
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  viewMounted = false
  refreshQueued = false
  organizationReloadQueued = false
  organizationGeneration += 1
  metaMessengerAuthorizationController.value?.abort()
  metaMessengerAuthorizationController.value = null
  conversationViewSequence += 1
  stopMessagesContentResizeObserver()
  metaMessengerContextVersion += 1
  metaMessengerStatusSequence += 1
  metaInstagramStatusSequence += 1
  metaMessengerBusy.value = false
  metaInstagramBusy.value = false
  metaInstagramState.value = {
    availability: 'loading',
    organizationId: '',
    status: null,
  }
  closeMetaMessengerSelection()
  window.removeEventListener('beforeunload', guardMetaMessengerNavigation)
  document.removeEventListener('visibilitychange', catchUpInbox)
  window.removeEventListener('focus', catchUpInbox)
  window.removeEventListener('online', handleOnline)
  if (messageViewportScrollFrame !== null) {
    window.cancelAnimationFrame(messageViewportScrollFrame)
    messageViewportScrollFrame = null
  }
  readCursorCheckQueued = false
  organizationsStore.unblockOrganizationSwitch(metaMessengerSwitchLockOwner)
  stopInboxActivity?.()
  stopConnectionState?.()
  if (pollTimer !== null) window.clearInterval(pollTimer)
  if (syncDebounceTimer !== null) window.clearTimeout(syncDebounceTimer)
  if (filterDebounceTimer !== null) window.clearTimeout(filterDebounceTimer)
})

onBeforeRouteLeave(() => {
  if (!metaMessengerWorkspaceLocked.value) return true
  toast.error(metaMessengerSwitchLockMessage)
  return false
})

function guardMetaMessengerNavigation(event: BeforeUnloadEvent) {
  if (!metaMessengerWorkspaceLocked.value) return
  event.preventDefault()
  event.returnValue = ''
}
</script>

<template>
  <div class="flex h-full flex-col bg-[#08090a] light:bg-slate-100">
    <PageHeader
      title="Omnichannel inbox"
      description="One conversation layer across WhatsApp, social, email and web chat."
      :icon="Inbox"
      icon-gradient="bg-gradient-to-br from-sky-400 to-indigo-700 shadow-sky-500/20"
      compact-actions
    >
      <template #actions>
        <div class="flex items-center gap-2">
          <Badge
            data-testid="omnichannel-live-state"
            role="status"
            aria-live="polite"
            variant="outline"
            class="gap-1.5"
            :class="connectionBadgeClass"
          >
            <span
              class="h-1.5 w-1.5 rounded-full"
              :class="
                connectionState === 'connected'
                  ? 'bg-emerald-300'
                  : connectionState === 'disconnected'
                    ? 'bg-rose-300'
                    : 'bg-amber-300 animate-pulse'
              "
            />
            {{ connectionLabel }}
          </Badge>
          <Badge variant="outline" class="hidden gap-1.5 border-emerald-400/20 text-emerald-300 light:border-emerald-300 light:text-emerald-700 lg:flex">
            <CheckCircle2 class="h-3.5 w-3.5" />
            {{ activeCount }} active
          </Badge>
          <Badge v-if="attentionCount" variant="outline" class="hidden gap-1.5 border-amber-400/20 text-amber-300 light:border-amber-300 light:text-amber-700 lg:flex">
            <AlertCircle class="h-3.5 w-3.5" />
            {{ attentionCount }} attention
          </Badge>
          <Button variant="outline" size="icon" aria-label="Refresh omnichannel inbox" @click="load">
            <RefreshCw class="h-4 w-4" />
          </Button>
          <Button v-if="canManageAccounts" class="hidden bg-sky-400 text-black hover:bg-sky-300 lg:inline-flex" @click="showConnect = !showConnect">
            <Plus class="mr-2 h-4 w-4" />
            Connect
          </Button>
        </div>
      </template>
    </PageHeader>

    <div
      v-if="showConnect"
      class="grid gap-3 border-b border-sky-400/20 bg-sky-400/[0.04] px-5 py-4 md:grid-cols-2 xl:grid-cols-[.75fr_1fr_1fr_1.4fr_auto]"
    >
      <select
        v-model="newAccount.channel"
        data-testid="channel-connect-type"
        class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
      >
        <option
          v-for="channel in connectableChannels"
          :key="channel.key"
          :value="channel.key"
        >
          {{ channel.label }}
        </option>
      </select>
      <template v-if="newAccount.channel === 'messenger'">
        <template v-if="metaMessengerOnboardingReady && canManageMetaMessenger">
          <div class="md:col-span-1 xl:col-span-3">
            <p class="text-sm font-medium text-white light:text-slate-950">Connect a Facebook Page</p>
            <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
              Sign in with Facebook, complete Meta's steps, and click Finish in the Meta window. Keep both windows open; authorization can take several minutes. Then choose an owned Page here, test it, and approve activation.
            </p>
          </div>
          <div class="flex gap-2">
            <Button
              data-testid="messenger-connect-facebook"
              class="bg-[#1877f2] text-white hover:bg-[#166fe5]"
              :disabled="metaMessengerBusy"
              @click="connectMetaMessenger"
            >
              <Loader2 v-if="metaMessengerBusy" class="mr-2 h-4 w-4 animate-spin" />
              <Facebook v-else class="mr-2 h-4 w-4" />
              {{ metaMessengerBusy ? 'Waiting for Meta…' : 'Continue with Facebook' }}
            </Button>
            <Button
              v-if="metaMessengerAuthorizationController"
              type="button"
              variant="outline"
              data-testid="messenger-cancel-facebook"
              @click="cancelMetaMessengerAuthorization"
            >
              Cancel
            </Button>
          </div>
          <p
            v-if="metaMessengerAuthorizationController"
            data-testid="messenger-facebook-wait-guidance"
            role="status"
            aria-live="polite"
            class="rounded-lg border border-[#1877f2]/25 bg-[#1877f2]/10 px-3 py-2 text-xs leading-5 text-blue-100 light:border-blue-200 light:bg-blue-50 light:text-blue-900 md:col-span-2 xl:col-span-4"
          >
            Waiting for Meta. Complete the steps in the Meta window and click Finish. Do not close either window while it is loading.
          </p>
        </template>
        <div
          v-else
          data-testid="messenger-onboarding-unavailable"
          class="md:col-span-1 xl:col-span-4"
        >
          <p class="text-sm font-medium text-white light:text-slate-950">
            Messenger connection is not available for this workspace
          </p>
          <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
            {{
              metaMessengerOnboardingReady
                ? 'You need both channel and integration administrator permission to connect Facebook.'
                : 'An administrator must enable managed Facebook authorization for this workspace.'
            }}
            Manual Page IDs and relay URLs cannot be used.
          </p>
        </div>
      </template>
      <template
        v-else-if="
          newAccount.channel === 'instagram' && !metaInstagramStaticConnectionAvailable
        "
      >
        <div
          v-if="metaInstagramAvailability === 'loading'"
          data-testid="instagram-onboarding-loading"
          class="md:col-span-1 xl:col-span-4"
        >
          <p class="text-sm font-medium text-white light:text-slate-950">
            Checking Instagram connection options
          </p>
          <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
            Static Instagram setup stays unavailable until this workspace policy is confirmed.
          </p>
        </div>
        <div
          v-else-if="metaInstagramAvailability === 'error'"
          data-testid="instagram-onboarding-error"
          class="md:col-span-1 xl:col-span-4"
        >
          <p class="text-sm font-medium text-white light:text-slate-950">
            Instagram connection status is unavailable
          </p>
          <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
            No Instagram connection can be created until the workspace policy can be loaded.
          </p>
        </div>
        <template
          v-else-if="
            metaInstagramStatus && metaInstagramOnboardingReady && canManageMetaInstagram
          "
        >
          <div class="md:col-span-1 xl:col-span-3">
            <div class="flex items-center gap-2">
              <p class="text-sm font-medium text-white light:text-slate-950">
                Connect an Instagram professional profile
              </p>
              <Badge
                variant="outline"
                class="border-rose-300/20 text-[9px] uppercase tracking-wider text-rose-200 light:text-rose-700"
              >
                {{
                  metaInstagramStatus.quarantine_only
                    ? 'Quarantine only'
                    : metaInstagramStatus.app_review_status === 'approved'
                      ? 'App reviewed'
                      : 'Development role'
                }}
              </Badge>
            </div>
            <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
              Instagram verifies the exact Business or Creator profile. ReReply then confirms the
              messages subscription before creating a pending connection.
            </p>
          </div>
          <Button
            data-testid="instagram-connect-managed"
            class="bg-gradient-to-r from-[#f58529] via-[#dd2a7b] to-[#8134af] text-white shadow-lg shadow-[#dd2a7b]/15 hover:brightness-110"
            :disabled="metaInstagramBusy"
            @click="connectMetaInstagram"
          >
            <Loader2 v-if="metaInstagramBusy" class="mr-2 h-4 w-4 animate-spin" />
            <Instagram v-else class="mr-2 h-4 w-4" />
            Continue with Instagram
          </Button>
        </template>
        <div
          v-else
          data-testid="instagram-onboarding-unavailable"
          class="md:col-span-1 xl:col-span-4"
        >
          <p class="text-sm font-medium text-white light:text-slate-950">
            Managed Instagram connection is not available
          </p>
          <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
            {{
              metaInstagramStatus && metaInstagramOnboardingReady
                ? 'You need both channel and integration administrator permission.'
                : metaInstagramStatus?.quarantine_only
                  ? 'The deployment is in quarantine-only mode. OAuth, reconnect, testing, and activation are disabled; teardown remains available on existing connections.'
                  : 'Deployment-owned App Review or development app-role evidence is not ready.'
            }}
            Workspace administrators cannot supply or override that evidence.
          </p>
        </div>
      </template>
      <template v-else>
        <Input v-model="newAccount.name" placeholder="Connection name" />
        <Input v-model="newAccount.external_account_id" placeholder="External account ID" />
        <Input v-model="newAccount.relay_url" type="url" placeholder="HTTPS signed-relay URL" />
        <Button
          data-testid="channel-connect-submit"
          class="bg-sky-400 text-black hover:bg-sky-300"
          @click="connectAccount"
        >
          Create
        </Button>
      </template>
    </div>

    <div
      v-if="createdConnection"
      class="border-b border-amber-300/20 bg-amber-300/[0.06] px-5 py-4 text-sm text-amber-50"
    >
      <div class="mx-auto flex max-w-6xl flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
        <div>
          <p class="font-semibold">Save this signing secret now</p>
          <p class="mt-1 text-xs text-amber-50/60">
            Configure the relay to sign inbound payloads with this secret. ReReply will not show it again.
          </p>
          <dl class="mt-3 grid gap-2 font-mono text-xs">
            <div>
              <dt class="text-amber-50/45">Absolute webhook URL</dt>
              <dd class="mt-1 flex items-start gap-2">
                <span class="min-w-0 flex-1 break-all select-all">{{ absoluteWebhookURL }}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  class="h-7 shrink-0"
                  @click="copyConnectionValue('Webhook URL', absoluteWebhookURL)"
                >
                  Copy
                </Button>
              </dd>
            </div>
            <div>
              <dt class="text-amber-50/45">Signing secret</dt>
              <dd class="mt-1 flex items-start gap-2">
                <span class="min-w-0 flex-1 break-all select-all">{{ createdConnection.inbound_secret }}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  class="h-7 shrink-0"
                  @click="copyConnectionValue('Signing secret', createdConnection.inbound_secret)"
                >
                  Copy
                </Button>
              </dd>
            </div>
          </dl>
        </div>
        <Button variant="outline" size="sm" @click="createdConnection = null">I saved it</Button>
      </div>
    </div>

    <details
      v-if="!loading"
      class="group hidden shrink-0 border-b border-white/[0.08] bg-[#0b0c0d] light:border-slate-300 light:bg-slate-50 lg:block 2xl:hidden"
    >
      <summary
        class="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-xs font-semibold text-white light:text-slate-950"
      >
        <span>Channel connections</span>
        <span class="rounded-full bg-white/[0.06] px-2 py-1 text-[10px] text-white/45 light:bg-slate-200 light:text-slate-700">
          {{ accounts.length }} configured · open to test or approve
        </span>
      </summary>
      <div class="max-h-60 space-y-2 overflow-y-auto border-t border-white/[0.06] p-3 light:border-slate-300">
        <div
          v-for="account in accounts"
          :key="`compact-${account.id}`"
          class="flex flex-wrap items-center gap-3 rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-slate-300 light:bg-white"
        >
          <button
            type="button"
            class="flex min-w-0 flex-1 items-center gap-3 text-left"
            @click="channelFilter = account.channel"
          >
            <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-white/[0.04] text-white/55 light:bg-slate-100 light:text-slate-700">
              <component :is="channelMeta(account.channel).icon" class="h-4 w-4" />
            </span>
            <span class="min-w-0">
              <span class="block truncate text-xs font-medium text-white light:text-slate-950">{{ account.name }}</span>
              <span
                class="mt-0.5 block text-[10px]"
                :class="accountReadyForOutbound(account) ? 'text-emerald-300 light:text-emerald-700' : 'text-amber-300 light:text-amber-700'"
              >
                {{ accountConnectionLabel(account) }}
                <span v-if="account.outbox_pending"> · {{ account.outbox_pending }} queued</span>
                <span v-if="account.outbox_failed" class="text-red-300 light:text-red-700"> · {{ account.outbox_failed }} failed</span>
              </span>
            </span>
          </button>
          <Button
            v-if="
              canManageAccounts &&
              account.provider === 'relay' &&
              !isManagedMetaAccount(account) &&
              account.status === 'active' &&
              account.config?.outbound_enabled !== true
            "
            size="sm"
            class="h-8 bg-emerald-300 text-black hover:bg-emerald-200"
            @click="approveOutbound(account)"
          >
            Approve outbound
          </Button>
          <Button
            v-if="
              canManageAccounts &&
              canManageIntegrations &&
              managedMetaLifecycleReady(account) &&
              isManagedMetaAccount(account) &&
              account.status === 'pending' &&
              Boolean(account.last_health_check_at) &&
              !account.last_error
            "
            :data-testid="
              isManagedMetaInstagram(account)
                ? 'instagram-approve-activation'
                : 'messenger-approve-activation'
            "
            size="sm"
            class="h-8 bg-emerald-300 text-black hover:bg-emerald-200"
            :disabled="managedMetaActionBusy(account)"
            @click="approveManagedMeta(account)"
          >
            Approve activation
          </Button>
          <Button
            v-if="canManageAccounts && (!isManagedMetaAccount(account) || managedMetaLifecycleReady(account))"
            variant="outline"
            size="sm"
            class="h-8"
            :aria-label="`Test ${account.name}`"
            @click="testAccount(account)"
          >
            <RefreshCw class="mr-1.5 h-3.5 w-3.5" />
            Test
          </Button>
          <RouterLink
            v-if="(canManageAccounts || canDeleteAccounts) && account.provider === 'threads'"
            to="/settings/integrations"
            data-testid="threads-manage-in-integrations"
          >
            <Button variant="outline" size="sm" class="h-8">
              <Settings2 class="mr-1.5 h-3.5 w-3.5" />
              Integrations
            </Button>
          </RouterLink>
          <Button
            v-else-if="account.provider === 'meta_legacy' ? canManageAIBooking : (canManageAccounts || canDeleteAccounts)"
            variant="outline"
            size="sm"
            class="h-8"
            :aria-label="`Manage ${account.name}`"
            @click="openAccountSettings(account)"
          >
            <Settings2 class="mr-1.5 h-3.5 w-3.5" />
            Settings
          </Button>
        </div>
        <div v-if="accounts.length === 0" class="rounded-xl border border-dashed border-white/[0.08] p-5 text-center light:border-slate-300">
          <Unplug class="mx-auto h-5 w-5 text-white/25 light:text-slate-500" />
          <p class="mt-2 text-xs text-white/35 light:text-slate-600">No channel connections yet</p>
        </div>
      </div>
    </details>

    <div v-if="loading" class="flex flex-1 items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-sky-300" />
    </div>

    <div v-else class="grid min-h-0 flex-1 overflow-hidden" :class="inboxGridClass">
      <aside class="hidden overflow-y-auto border-r border-white/[0.08] bg-[#0b0c0d] p-3 light:border-slate-300 light:bg-slate-50 2xl:block">
        <p class="px-2 pb-3 pt-1 text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-slate-600">
          Connections
        </p>
        <div class="space-y-1.5">
          <div
            v-for="account in accounts"
            :key="account.id"
            role="button"
            tabindex="0"
            class="flex min-h-11 w-full cursor-pointer items-center gap-3 rounded-xl border border-transparent px-2.5 py-2.5 text-left transition hover:border-white/[0.06] hover:bg-white/[0.025] light:hover:border-slate-300 light:hover:bg-slate-200/70"
            @click="channelFilter = account.channel"
            @keydown.enter="channelFilter = account.channel"
          >
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-white/[0.04] text-white/55 light:bg-slate-200 light:text-slate-700">
              <component :is="channelMeta(account.channel).icon" class="h-4 w-4" />
            </div>
            <div class="min-w-0 flex-1">
              <p class="truncate text-xs font-medium text-white light:text-slate-950">{{ account.name }}</p>
              <p class="mt-0.5 text-[10px] capitalize text-white/35 light:text-slate-600">{{ account.channel }} · {{ account.provider }}</p>
              <p
                class="mt-1 text-[9px]"
                :class="accountReadyForOutbound(account) ? 'text-emerald-300 light:text-emerald-700' : 'text-amber-300 light:text-amber-700'"
              >
                {{ accountConnectionLabel(account) }}
                <span v-if="account.outbox_pending"> · {{ account.outbox_pending }} queued</span>
                <span v-if="account.outbox_failed" class="text-red-300 light:text-red-700"> · {{ account.outbox_failed }} failed</span>
              </p>
              <p v-if="account.last_error" class="mt-1 line-clamp-2 text-[9px] text-red-300/75 light:text-red-700">
                {{ account.last_error }}
              </p>
            </div>
            <button
              v-if="
                canManageAccounts &&
                account.provider === 'relay' &&
                !isManagedMetaAccount(account) &&
                account.status === 'active' &&
                account.config?.outbound_enabled !== true
              "
              type="button"
              class="rounded-lg bg-emerald-300/10 px-2 py-1 text-[9px] font-semibold text-emerald-200 transition hover:bg-emerald-300/15"
              @click.stop="approveOutbound(account)"
            >
              Approve
            </button>
            <button
              v-if="
                canManageAccounts &&
                canManageIntegrations &&
                managedMetaLifecycleReady(account) &&
                isManagedMetaAccount(account) &&
                account.status === 'pending' &&
                Boolean(account.last_health_check_at) &&
                !account.last_error
              "
              type="button"
              :data-testid="
                isManagedMetaInstagram(account)
                  ? 'instagram-approve-activation-rail'
                  : 'messenger-approve-activation-rail'
              "
              class="rounded-lg bg-emerald-300/10 px-2 py-1 text-[9px] font-semibold text-emerald-200 transition hover:bg-emerald-300/15"
              :disabled="managedMetaActionBusy(account)"
              @click.stop="approveManagedMeta(account)"
            >
              Activate
            </button>
            <button
              v-if="canManageAccounts && (!isManagedMetaAccount(account) || managedMetaLifecycleReady(account))"
              type="button"
              class="rounded-lg p-1.5 text-white/25 transition hover:bg-white/[0.05] hover:text-sky-300 light:text-slate-500 light:hover:text-sky-700"
              :aria-label="`Test ${account.name}`"
              @click.stop="testAccount(account)"
            >
              <RefreshCw class="h-3.5 w-3.5" />
            </button>
            <RouterLink
              v-if="(canManageAccounts || canDeleteAccounts) && account.provider === 'threads'"
              to="/settings/integrations"
              data-testid="threads-manage-in-integrations"
              class="rounded-lg p-1.5 text-white/25 transition hover:bg-white/[0.05] hover:text-sky-300 light:text-slate-500 light:hover:text-sky-700"
              :aria-label="`Manage ${account.name} in Integrations`"
              @click.stop
            >
              <Settings2 class="h-3.5 w-3.5" />
            </RouterLink>
            <button
              v-else-if="account.provider === 'meta_legacy' ? canManageAIBooking : (canManageAccounts || canDeleteAccounts)"
              type="button"
              class="rounded-lg p-1.5 text-white/25 transition hover:bg-white/[0.05] hover:text-sky-300 light:text-slate-500 light:hover:text-sky-700"
              :aria-label="`Manage ${account.name}`"
              @click.stop="openAccountSettings(account)"
            >
              <Settings2 class="h-3.5 w-3.5" />
            </button>
          </div>
          <div v-if="accounts.length === 0" class="rounded-xl border border-dashed border-white/[0.08] p-5 text-center light:border-slate-300">
            <Unplug class="mx-auto h-5 w-5 text-white/25 light:text-slate-500" />
            <p class="mt-2 text-xs text-white/35 light:text-slate-600">No channel connections yet</p>
          </div>
        </div>

        <p class="mt-7 px-2 text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-slate-600">
          Available adapters
        </p>
        <div class="mt-2 grid grid-cols-2 gap-2">
          <div
            v-for="channel in visibleChannels"
            :key="channel.key"
            :data-testid="channel.key === 'threads' ? 'threads-public-engagement-adapter' : undefined"
            class="rounded-xl border border-white/[0.06] bg-white/[0.018] p-2.5 light:border-slate-300 light:bg-white"
          >
            <component :is="channel.icon" class="h-4 w-4 text-white/45 light:text-slate-600" />
            <p class="mt-2 text-[10px] font-medium text-white/65 light:text-slate-800">{{ channel.label }}</p>
            <p class="mt-0.5 text-[9px] text-white/25 light:text-slate-500">
              {{
                channel.key === 'threads'
                  ? 'OAuth managed in Settings → Integrations'
                  : channel.key === 'instagram' && metaInstagramOnboardingReady
                    ? 'Managed Instagram Login'
                    : channel.key === 'messenger' && metaMessengerOnboardingReady
                      ? 'Facebook Login for Business'
                      : channel.connectable
                        ? 'Signed relay'
                        : channel.gated
                          ? 'Approval gated'
                          : 'Use WhatsApp setup'
              }}
            </p>
          </div>
        </div>
      </aside>

      <section
        :class="[
          'min-h-0 flex-col border-r border-white/[0.08] bg-[#0d0f10] light:border-slate-300 light:bg-slate-50',
          selectedConversation ? 'hidden lg:flex' : 'flex',
        ]"
      >
        <div class="border-b border-white/[0.07] p-3 light:border-slate-300">
          <div class="relative">
            <AtSign class="absolute left-3 top-2.5 h-4 w-4 text-white/25 light:text-slate-500" />
            <Input v-model="search" class="pl-9" placeholder="Search conversations" />
          </div>
          <div class="mt-2 flex gap-2 overflow-x-auto pb-1">
            <button
              class="min-h-11 shrink-0 rounded-full px-3 py-1 text-xs font-semibold transition lg:min-h-0 lg:px-2.5 lg:text-[10px] lg:font-medium"
              :class="channelFilter === 'all' ? 'bg-sky-300 text-black' : 'bg-white/[0.04] text-white/45 light:bg-slate-200 light:text-slate-700'"
              @click="channelFilter = 'all'"
            >
              All
            </button>
            <button
              v-for="channel in supportedChannels.filter((item) => accounts.some((account) => account.channel === item.key))"
              :key="channel.key"
              class="min-h-11 shrink-0 rounded-full px-3 py-1 text-xs font-semibold transition lg:min-h-0 lg:px-2.5 lg:text-[10px] lg:font-medium"
              :class="
                channelFilter === channel.key
                  ? 'bg-sky-300 text-black'
                  : 'bg-white/[0.04] text-white/45 light:bg-slate-200 light:text-slate-700'
              "
              @click="channelFilter = channel.key"
            >
              {{ channel.label }}
            </button>
          </div>
        </div>

        <div class="flex-1 overflow-y-auto">
          <button
            v-for="conversation in filteredConversations"
            :key="conversation.id"
            data-testid="omnichannel-conversation"
            :data-conversation-id="conversation.id"
            :data-contact-id="conversation.contact_id"
            class="flex min-h-[72px] w-full gap-3 border-b border-white/[0.055] px-3 py-3.5 text-left transition light:border-slate-300"
            :class="
              selectedConversation?.id === conversation.id
                ? 'bg-sky-300/[0.08]'
                : 'hover:bg-white/[0.025] light:hover:bg-slate-200/70'
            "
            @click="selectConversation(conversation)"
          >
            <div class="relative flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-white/[0.045] text-white/55 light:bg-slate-200 light:text-slate-700">
              <component :is="channelMeta(conversation.channel).icon" class="h-4 w-4" />
              <span
                v-if="conversation.unread_count"
                class="absolute -right-1 -top-1 flex h-5 min-w-5 items-center justify-center rounded-full bg-sky-300 px-1 text-[9px] font-bold text-black"
              >
                {{ conversation.unread_count }}
              </span>
            </div>
            <div class="min-w-0 flex-1">
              <div class="flex items-center justify-between gap-2">
                <p class="truncate text-sm font-semibold text-white light:text-slate-950 lg:text-xs">
                  {{ conversationName(conversation) }}
                </p>
                <span class="shrink-0 text-[10px] text-white/25 light:text-slate-600">{{ formatTime(conversation.last_message_at) }}</span>
              </div>
              <p v-if="conversation.subject" class="mt-1 truncate text-xs font-medium text-white/50 light:text-slate-700">{{ conversation.subject }}</p>
              <p class="mt-1 truncate text-xs text-white/40 light:text-slate-600">{{ conversation.last_message_preview || 'No preview' }}</p>
              <Badge
                v-if="identityReviewIsOpen(conversation.identity_review_ai_state)"
                class="mt-1 h-5 bg-amber-400/15 text-[10px] text-amber-200 light:bg-amber-100 light:text-amber-800"
              >
                AI blocked · review
              </Badge>
              <Badge
                v-else-if="!effectiveAIIsAllowed(conversation.identity_review_ai_state)"
                class="mt-1 h-5 bg-slate-400/15 text-[10px] text-slate-200 light:bg-slate-100 light:text-slate-700"
              >
                AI status unavailable
              </Badge>
            </div>
          </button>
          <div v-if="filteredConversations.length === 0" class="p-8 text-center">
            <Inbox class="mx-auto h-6 w-6 text-white/20 light:text-slate-400" />
            <p class="mt-3 text-xs text-white/35 light:text-slate-600">No matching conversations</p>
          </div>
          <div v-if="conversations.length < conversationTotal" class="p-3 text-center">
            <Button
              variant="outline"
              size="sm"
              :disabled="loadingMore"
              @click="loadMoreConversations"
            >
              <Loader2 v-if="loadingMore" class="mr-2 h-3.5 w-3.5 animate-spin" />
              Load more conversations
            </Button>
          </div>
        </div>
      </section>

      <main
        :class="[
          'min-h-0 min-w-0 flex-col overflow-hidden bg-[#090a0b] light:bg-slate-100',
          selectedConversation ? 'flex' : 'hidden lg:flex',
        ]"
      >
        <template v-if="selectedConversation">
          <header class="flex items-center justify-between gap-2 border-b border-white/[0.08] bg-[#0d0f10] px-3 py-2 light:border-slate-300 light:bg-slate-50 sm:px-5 sm:py-3.5">
            <div class="flex min-w-0 items-center gap-2 sm:gap-3">
              <Button
                type="button"
                variant="ghost"
                size="icon"
                class="h-11 w-11 shrink-0 lg:hidden"
                aria-label="Back to conversations"
                @click="closeMobileConversation"
              >
                <ArrowLeft class="h-5 w-5" />
              </Button>
              <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-sky-300/10 text-sky-200">
                <component :is="channelMeta(selectedConversation.channel).icon" class="h-4 w-4" />
              </div>
              <div class="min-w-0">
                <p class="truncate text-sm font-semibold text-white light:text-slate-950">
                  {{ conversationName(selectedConversation) }}
                </p>
                <p class="mt-0.5 truncate text-[10px] capitalize text-white/35 light:text-slate-600">
                  {{ selectedConversation.channel }} · {{ selectedAccount?.name || 'Channel' }}
                </p>
              </div>
            </div>
            <div class="flex shrink-0 items-center gap-2">
              <Button
                v-if="selectedConversationHasOpenIdentityReview && canReviewContactIdentity"
                data-testid="channel-identity-review"
                type="button"
                variant="outline"
                size="sm"
                class="h-11 border-amber-300/20 bg-amber-300/[0.06] text-amber-100 hover:bg-amber-300/10 light:border-amber-200 light:bg-amber-50 light:text-amber-900 sm:h-8"
                @click="isIdentityReviewOpen = true"
              >
                <AlertCircle class="h-3.5 w-3.5 sm:mr-2" />
                <span class="hidden sm:inline">Identity review</span>
              </Button>
              <Button
                v-if="canControlConversationAI"
                data-testid="conversation-ai-toggle"
                type="button"
                variant="outline"
                size="sm"
                class="h-11 w-11 p-0 sm:h-8 sm:w-auto sm:px-3"
                :disabled="aiStateUpdating"
                :aria-pressed="selectedConversation.ai_paused"
                :aria-label="conversationAIControlLabel"
                @click="toggleConversationAI"
              >
                <Loader2 v-if="aiStateUpdating" class="h-3.5 w-3.5 animate-spin sm:mr-2" />
                <Play v-else-if="selectedConversation.ai_paused" class="h-3.5 w-3.5 sm:mr-2" />
                <PauseCircle v-else class="h-3.5 w-3.5 sm:mr-2" />
                <span class="hidden sm:inline">{{ conversationAIControlLabel }}</span>
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                class="hidden h-10 shrink-0 gap-2 sm:inline-flex"
                :aria-label="isWorkspaceOpen ? 'Close customer revenue workspace' : 'Open customer revenue workspace'"
                :aria-pressed="isWorkspaceOpen"
                @click="isWorkspaceOpen = !isWorkspaceOpen"
              >
                <PanelRightOpen class="h-4 w-4" />
                <span class="hidden sm:inline">Customer workspace</span>
              </Button>
            </div>
          </header>

          <div v-if="loadingMessages" class="flex flex-1 items-center justify-center">
            <Loader2 class="h-5 w-5 animate-spin text-sky-300" />
          </div>
          <div
            v-else
            ref="messagesViewport"
            data-testid="omnichannel-message-viewport"
            :data-conversation-id="selectedConversation.id"
            class="flex-1 overflow-y-auto p-3 sm:p-5 md:p-7"
            @scroll.passive="handleMessageViewportScroll"
          >
            <div
              ref="messagesContent"
              data-testid="omnichannel-message-list"
              :data-conversation-id="selectedConversation.id"
              class="space-y-3"
            >
              <div
                v-if="hasOlderMessages"
                class="pb-2 text-center"
              >
                <Button
                  variant="outline"
                  size="sm"
                  :disabled="loadingOlderMessages"
                  @click="loadOlderMessages"
                >
                  <Loader2 v-if="loadingOlderMessages" class="mr-2 h-3.5 w-3.5 animate-spin" />
                  Load older messages
                </Button>
              </div>
              <div
                v-for="message in messages"
                :key="message.id"
                data-testid="omnichannel-message"
                :data-message-id="message.id"
                :data-message-direction="message.direction"
                class="flex"
                :class="message.direction === 'outgoing' ? 'justify-end' : 'justify-start'"
              >
                <div
                  class="max-w-[86%] rounded-2xl px-4 py-3 text-sm leading-6 sm:max-w-[72%]"
                  :class="
                    message.direction === 'outgoing'
                      ? 'rounded-br-md bg-sky-300 text-slate-950'
                      : 'rounded-bl-md border border-white/[0.07] bg-white/[0.04] text-white/80 light:border-slate-300 light:bg-slate-50 light:text-slate-900'
                  "
                >
                  <p>{{ message.content }}</p>
                  <p class="mt-1 text-right text-[9px] opacity-50">{{ formatTime(message.created_at) }} · {{ message.status }}</p>
                </div>
              </div>
            </div>
          </div>

          <footer class="border-t border-white/[0.08] bg-[#0d0f10] p-3 light:border-slate-300 light:bg-slate-50 sm:p-4">
            <div
              v-if="selectedConversation.channel === 'whatsapp'"
              data-testid="whatsapp-direct-reply-notice"
              class="flex flex-col items-start gap-3 rounded-xl border px-4 py-3 sm:flex-row sm:items-center sm:justify-between"
              :class="
                canSendWhatsAppText
                  ? 'border-emerald-300/15 bg-emerald-300/[0.04]'
                  : 'border-amber-300/15 bg-amber-300/[0.04]'
              "
            >
              <p
                class="text-xs leading-5"
                :class="canSendWhatsAppText ? 'text-emerald-100/65 light:text-emerald-800' : 'text-amber-100/65 light:text-amber-800'"
              >
                {{
                  canSendWhatsAppText
                    ? 'Text replies can be sent here during the active service window. Open WhatsApp for templates and media.'
                    : whatsappReplyUnavailableReason
                }}
              </p>
              <RouterLink :to="`/chat/${selectedConversation.contact_id}`">
                <Button size="sm" class="bg-emerald-300 text-black hover:bg-emerald-200">Open WhatsApp</Button>
              </RouterLink>
            </div>
            <div
              v-if="selectedConversation.channel !== 'whatsapp' || canSendWhatsAppText"
              class="mt-2 space-y-2 first:mt-0"
            >
              <div
                v-if="['threads', 'tiktok'].includes(selectedConversation.channel)"
                :data-testid="
                  selectedConversation.channel === 'threads'
                    ? 'threads-public-reply-composer-notice'
                    : 'tiktok-reply-composer-notice'
                "
                :class="[
                  'rounded-xl border px-3 py-2 text-[11px] leading-5',
                  selectedConversation.channel === 'threads' && threadsPublicReplyReady
                    ? 'border-emerald-300/15 bg-emerald-300/[0.04] text-emerald-100/65 light:text-emerald-800'
                    : 'border-amber-300/15 bg-amber-300/[0.04] text-amber-100/65 light:text-amber-800',
                ]"
              >
                <template v-if="selectedConversation.channel === 'threads'">
                  <template v-if="threadsPublicReplyReady">
                    This reply will be published to the selected public Threads
                    {{ selectedThreadsTarget?.engagementType }}. Direct messages and standalone
                    posts are not supported.
                  </template>
                  <template v-else-if="!selectedThreadsTarget">
                    Select an existing public Threads reply or mention before replying. Direct
                    messages and standalone posts are not supported.
                  </template>
                  <template v-else>
                    This Threads connection is not ready for public replies. Direct messages and
                    standalone posts are not supported.
                  </template>
                </template>
                <template v-else>
                  TikTok is read-only because Business Messaging approval and an approved adapter
                  are not available. Existing conversations remain visible, but replies are
                  disabled.
                </template>
              </div>
              <div class="flex items-end gap-3">
              <textarea
                v-model="composer"
                data-testid="omnichannel-reply-composer"
                rows="2"
                class="min-h-11 flex-1 resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none placeholder:text-white/20 focus:border-sky-300/35 light:border-slate-400 light:bg-white light:text-slate-950 light:placeholder:text-slate-500"
                :disabled="!canSendText"
                :placeholder="
                  selectedConversation.channel === 'whatsapp'
                    ? 'Write a WhatsApp reply...'
                    : selectedConversation.channel === 'threads'
                    ? threadsPublicReplyReady
                      ? 'Write a public Threads reply...'
                      : selectedThreadsTarget
                        ? 'Threads public replies are unavailable for this connection'
                        : 'Select an existing public Threads reply or mention'
                    : selectedConversation.channel === 'tiktok'
                      ? 'TikTok replies are unavailable — approval required'
                    : canSendText
                      ? 'Write a reply...'
                      : selectedAccount?.config?.outbound_enabled !== true
                        ? 'Outbound delivery requires explicit approval'
                        : 'This adapter does not support text replies'
                "
                @keydown.ctrl.enter.prevent="sendMessage"
              />
              <Button
                data-testid="omnichannel-send-reply"
                size="icon"
                class="h-11 w-11 shrink-0 bg-sky-300 text-black hover:bg-sky-200"
                :disabled="sending || !composer.trim() || !canSendText"
                :aria-label="
                  selectedConversation.channel === 'whatsapp'
                    ? 'Send WhatsApp reply'
                    : selectedConversation.channel === 'threads'
                    ? threadsPublicReplyReady
                      ? 'Send public Threads reply'
                      : 'Threads reply unavailable'
                    : selectedConversation.channel === 'tiktok'
                      ? 'TikTok reply unavailable'
                    : 'Send reply'
                "
                @click="sendMessage"
              >
                <Loader2 v-if="sending" class="h-4 w-4 animate-spin" />
                <Send v-else class="h-4 w-4" />
              </Button>
              </div>
            </div>
          </footer>
        </template>

        <div v-else class="flex flex-1 flex-col items-center justify-center p-8 text-center">
          <div class="flex h-16 w-16 items-center justify-center rounded-2xl border border-white/[0.08] bg-white/[0.025] text-white/25 light:border-slate-300 light:bg-slate-50 light:text-slate-500">
            <Inbox class="h-6 w-6" />
          </div>
          <h2 class="mt-5 text-lg font-semibold text-white light:text-slate-950">Every channel, one operating rhythm</h2>
          <p class="mt-2 max-w-md text-sm leading-6 text-white/40 light:text-slate-600">
            Select a conversation. Provider capabilities decide which composer actions are available, while tenant isolation
            remains the same on every channel.
          </p>
        </div>
      </main>

      <aside
        v-if="selectedConversation && isWorkspaceOpen && isWorkspaceRail"
        class="min-h-0 min-w-0"
        aria-label="Customer revenue workspace rail"
      >
        <CustomerRevenueWorkspace
          :contact-id="selectedConversation.contact_id"
          :contact="selectedConversation.contact"
          surface="omnichannel"
          @close="isWorkspaceOpen = false"
        />
      </aside>
    </div>

    <Sheet
      v-if="selectedConversation && !isWorkspaceRail"
      :open="isWorkspaceOpen"
      @update:open="isWorkspaceOpen = $event"
    >
      <SheetContent side="right" class="!w-full !max-w-[440px] !p-0 [&>button:last-child]:hidden">
        <SheetTitle class="sr-only">Customer revenue workspace</SheetTitle>
        <SheetDescription class="sr-only">
          Customer journeys, tasks, bookings, packages, revenue and activity.
        </SheetDescription>
        <CustomerRevenueWorkspace
          v-if="isWorkspaceOpen"
          :contact-id="selectedConversation.contact_id"
          :contact="selectedConversation.contact"
          surface="omnichannel"
          @close="isWorkspaceOpen = false"
        />
      </SheetContent>
    </Sheet>

    <ContactIdentityReviewDialog
      v-model:open="isIdentityReviewOpen"
      :contact-id="selectedConversation?.contact_id ?? null"
      :contact-label="selectedConversation ? conversationName(selectedConversation) : undefined"
      :effective-state="selectedConversation?.identity_review_ai_state"
      :can-review="canReviewContactIdentity"
      :can-view-staged="canViewStagedContactIdentity"
      @resolved="refreshAfterIdentityReview"
    />

    <div
      v-if="metaMessengerSelection"
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/75 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="messenger-page-title"
      @click.self="closeMetaMessengerSelection"
    >
      <div class="w-full max-w-lg rounded-2xl border border-[#1877f2]/25 bg-[#111416] p-5 shadow-2xl light:border-blue-200 light:bg-white">
        <div class="flex items-start justify-between gap-4">
          <div>
            <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-[#65a2ff]">
              Facebook Login for Business
            </p>
            <h2 id="messenger-page-title" class="mt-1 text-lg font-semibold text-white light:text-gray-900">
              {{
                metaMessengerReconnectName
                  ? `Reconnect ${metaMessengerReconnectName}`
                  : 'Choose one Facebook Page'
              }}
            </h2>
            <p class="mt-1 text-xs leading-5 text-white/45 light:text-gray-600">
              Only Pages freshly verified as owned by your Business Portfolio and assigned for messaging can be connected.
            </p>
          </div>
          <Button
            type="button"
            variant="outline"
            size="sm"
            :disabled="metaMessengerBusy"
            @click="closeMetaMessengerSelection"
          >
            Close
          </Button>
        </div>

        <label class="mt-5 block">
          <span class="text-xs font-medium text-white/65 light:text-gray-700">Owned Page</span>
          <select
            v-model="selectedMetaMessengerPageKey"
            data-testid="messenger-page-select"
            class="mt-1.5 h-11 w-full rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
          >
            <option value="" disabled>Select a Page</option>
            <option
              v-for="page in metaMessengerSelection.pages"
              :key="metaMessengerPageKey(page)"
              :value="metaMessengerPageKey(page)"
              :disabled="!page.selectable || !page.business_id"
            >
              {{ page.page_name }} · {{ page.business_name || page.business_id || 'No Business Portfolio' }}{{
                page.selectable ? '' : ` · ${page.disabled_reason || 'Not eligible'}`
              }}
            </option>
          </select>
        </label>

        <div class="mt-4 rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-3 text-[11px] leading-5 text-amber-50/70 light:border-amber-200 light:bg-amber-50 light:text-amber-900">
          The account stays pending with outbound and AI replies off until Test succeeds and an administrator explicitly approves activation.
        </div>
        <div class="mt-5 flex justify-end gap-2">
          <Button
            type="button"
            variant="outline"
            :disabled="metaMessengerBusy"
            @click="closeMetaMessengerSelection"
          >
            Cancel
          </Button>
          <Button
            type="button"
            data-testid="messenger-page-confirm"
            class="bg-[#1877f2] text-white hover:bg-[#166fe5]"
            :disabled="!selectedMetaMessengerPage || metaMessengerBusy"
            @click="confirmMetaMessengerPage"
          >
            <Loader2 v-if="metaMessengerBusy" class="mr-2 h-4 w-4 animate-spin" />
            Connect this Page
          </Button>
        </div>
      </div>
    </div>

    <div
      v-if="settingsAccount"
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="channel-settings-title"
      @click.self="!bookingSettingsSaving && (settingsAccount = null)"
    >
      <form
        class="w-full max-w-lg rounded-2xl border border-white/10 bg-[#111416] p-5 shadow-2xl light:border-gray-200 light:bg-white"
        @submit.prevent="saveAccountSettings"
      >
        <div class="flex items-start justify-between gap-4">
          <div>
            <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-sky-300">Connection control</p>
            <h2 id="channel-settings-title" class="mt-1 text-lg font-semibold text-white light:text-gray-900">
              {{ settingsAccount.name }}
            </h2>
            <p class="mt-1 text-xs text-white/40 light:text-gray-500">
              {{
                settingsAccount.provider === 'meta_legacy'
                  ? 'Native WhatsApp routing stays managed by WhatsApp setup. Only the separate AI booking setting can be changed here.'
                  : isManagedMetaMessenger(settingsAccount)
                  ? `Facebook-managed Page ${settingsAccount.external_account_id ?? ''}. Reconnect, test, approve, or disconnect it here.`
                  : isManagedMetaInstagram(settingsAccount)
                    ? metaInstagramOnboardingReady
                      ? `Managed Instagram profile ${settingsAccount.external_account_id ?? ''}. Reconnect, test, approve, or disconnect it here.`
                      : `Managed Instagram profile ${settingsAccount.external_account_id ?? ''}. Only safe unsubscribe reconciliation and disconnect remain available.`
                    : 'Repair the relay, rotate its outbound signing credential, or stop delivery.'
              }}
            </p>
          </div>
          <Button type="button" variant="outline" size="sm" :disabled="bookingSettingsSaving" @click="settingsAccount = null">Close</Button>
        </div>

        <section
          v-if="supportsAIBooking(settingsAccount)"
          aria-labelledby="channel-ai-booking-title"
          class="mt-5 rounded-xl border border-sky-300/20 bg-sky-300/[0.035] p-4 light:border-sky-200 light:bg-sky-50"
          :aria-busy="bookingSettingsSaving"
        >
          <label class="flex min-h-11 cursor-pointer items-start gap-3">
            <input
              v-model="accountSettingsDraft.ai_booking_enabled"
              type="checkbox"
              data-testid="channel-ai-booking-enabled"
              aria-describedby="channel-ai-booking-description"
              class="mt-1 h-4 w-4 rounded accent-sky-300 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-4 focus-visible:outline-sky-300"
              :disabled="!canManageAIBooking || bookingSettingsSaving
                || (!accountSettingsDraft.ai_booking_enabled && !canEnableAIBooking(settingsAccount))"
            />
            <span>
              <span id="channel-ai-booking-title" class="text-sm font-semibold text-white light:text-gray-900">AI booking for this channel</span>
              <span id="channel-ai-booking-description" class="mt-1 block text-xs leading-5 text-white/60 light:text-gray-600">
                Off by default. Connecting AI does not enable booking. When enabled, AI may offer scheduled slots;
                a customer must explicitly confirm before a place is reserved. Other channels are unchanged.
              </span>
            </span>
          </label>
          <p v-if="!canManageAIBooking" class="mt-2 text-xs text-white/60 light:text-gray-600">
            Channel, AI configuration and Booking settings permissions are required.
          </p>
          <p v-else-if="!canEnableAIBooking(settingsAccount)" class="mt-2 text-xs text-white/60 light:text-gray-600">
            Activate this channel first. Instagram and Messenger also need approved outbound delivery and automatic AI replies.
          </p>
          <p v-if="bookingSettingsError" role="alert" class="mt-3 text-xs text-red-300 light:text-red-700">{{ bookingSettingsError }}</p>
          <Button
            v-if="canManageAIBooking"
            type="button"
            data-testid="channel-ai-booking-save"
            class="mt-3 min-h-11 bg-sky-300 text-black hover:bg-sky-200"
            :disabled="bookingSettingsSaving || (accountSettingsDraft.ai_booking_enabled && !canEnableAIBooking(settingsAccount))"
            @click="saveAIBookingSettings"
          >
            <Loader2 v-if="bookingSettingsSaving" aria-hidden="true" class="mr-2 h-4 w-4 animate-spin" />
            {{ bookingSettingsSaving ? 'Saving AI booking…' : 'Save AI booking setting' }}
          </Button>
        </section>

        <div
          v-if="
            !metaInstagramOnboardingReady &&
            canReconcileMetaInstagram &&
            metaInstagramCanReconcile(settingsAccount)
          "
          class="mt-5 rounded-xl border border-amber-300/15 bg-amber-300/[0.035] p-3"
        >
          <p class="text-xs leading-5 text-white/50 light:text-gray-600">
            OAuth intake is quarantined. This action can only finish the already-recorded
            unsubscribe operation.
          </p>
          <Button
            type="button"
            variant="outline"
            class="mt-3"
            data-testid="instagram-reconcile-subscription"
            :disabled="metaInstagramBusy"
            @click="reconcileMetaInstagram(settingsAccount)"
          >
            <RefreshCw class="mr-2 h-4 w-4" />
            Reconcile Instagram unsubscribe
          </Button>
        </div>

        <div
          v-if="
            settingsAccount.provider !== 'meta_legacy' && canManageAccounts &&
            (!isManagedMetaAccount(settingsAccount) ||
              (managedMetaLifecycleReady(settingsAccount) && canManageIntegrations))
          "
          class="mt-5 space-y-4"
        >
          <label class="block">
            <span class="text-xs font-medium text-white/60 light:text-gray-600">Connection name</span>
            <Input v-model="accountSettingsDraft.name" class="mt-1.5" required maxlength="100" />
          </label>
          <label
            v-if="settingsAccount.provider === 'relay' && !isManagedMetaAccount(settingsAccount)"
            class="block"
          >
            <span class="text-xs font-medium text-white/60 light:text-gray-600">HTTPS relay URL</span>
            <Input v-model="accountSettingsDraft.relay_url" class="mt-1.5" required type="url" />
          </label>
          <label
            v-if="['instagram', 'messenger'].includes(settingsAccount.channel)"
            class="flex items-start gap-3 rounded-xl border border-sky-300/15 bg-sky-300/[0.035] p-3"
          >
            <input
              v-model="accountSettingsDraft.ai_reply_enabled"
              data-testid="channel-ai-reply-enabled"
              type="checkbox"
              class="mt-0.5 h-4 w-4 rounded border-white/20 accent-sky-300"
              :disabled="
                !accountSettingsDraft.ai_reply_enabled &&
                (settingsAccount.status !== 'active' ||
                  settingsAccount.config?.outbound_enabled !== true)
              "
            />
            <span>
              <span class="flex items-center gap-1.5 text-xs font-medium text-white/70 light:text-gray-700">
                <Bot class="h-3.5 w-3.5 text-sky-300" />
                Automatic AI replies
              </span>
              <span class="mt-1 block text-[10px] leading-4 text-white/35 light:text-gray-500">
                Explicit opt-in. Requires an active tested connection, outbound approval, and an enabled ReReply AI profile.
                Human replies pause AI for that conversation.
              </span>
            </span>
          </label>
          <label
            v-if="settingsAccount.provider === 'relay' && !isManagedMetaAccount(settingsAccount)"
            class="block"
          >
            <span class="text-xs font-medium text-white/60 light:text-gray-600">Rotate outbound signing secret</span>
            <Input
              v-model="accountSettingsDraft.outbound_secret"
              class="mt-1.5"
              type="password"
              autocomplete="new-password"
              placeholder="Leave blank to keep the current secret"
            />
            <span class="mt-1 block text-[10px] leading-4 text-white/35 light:text-gray-500">
              Coordinate this value with the relay, save, then run Test before re-approving outbound delivery.
            </span>
          </label>
          <div class="flex flex-wrap gap-2">
            <Button type="submit" :disabled="bookingSettingsSaving" class="bg-sky-300 text-black hover:bg-sky-200">Save changes</Button>
            <Button
              v-if="
                canReconcileMetaMessenger &&
                metaMessengerOnboardingReady &&
                metaMessengerNeedsSubscriptionReconciliation(settingsAccount)
              "
              type="button"
              variant="outline"
              data-testid="messenger-reconcile-subscription"
              :disabled="metaMessengerBusy"
              @click="reconcileMetaMessenger(settingsAccount)"
            >
              <RefreshCw class="mr-2 h-4 w-4" />
              Reconcile Meta subscription
            </Button>
            <Button
              v-if="canReconcileMetaInstagram && metaInstagramCanReconcile(settingsAccount)"
              type="button"
              variant="outline"
              data-testid="instagram-reconcile-subscription"
              :disabled="metaInstagramBusy"
              @click="reconcileMetaInstagram(settingsAccount)"
            >
              <RefreshCw class="mr-2 h-4 w-4" />
              Reconcile Instagram subscription
            </Button>
            <Button
              v-if="
                canManageIntegrations &&
                metaMessengerOnboardingReady &&
                isManagedMetaMessenger(settingsAccount)
              "
              type="button"
              variant="outline"
              :disabled="metaMessengerBusy"
              @click="reconnectMetaMessenger(settingsAccount)"
            >
              <Loader2 v-if="metaMessengerAuthorizationController" class="mr-2 h-4 w-4 animate-spin" />
              <Facebook v-else class="mr-2 h-4 w-4" />
              {{ metaMessengerAuthorizationController ? 'Waiting for Meta…' : 'Reconnect Facebook' }}
            </Button>
            <Button
              v-if="
                canManageIntegrations &&
                metaInstagramOnboardingReady &&
                isManagedMetaInstagram(settingsAccount)
              "
              type="button"
              variant="outline"
              :disabled="metaInstagramBusy"
              @click="reconnectMetaInstagram(settingsAccount)"
            >
              <Instagram class="mr-2 h-4 w-4" />
              Reconnect Instagram
            </Button>
            <Button
              v-if="metaMessengerAuthorizationController"
              type="button"
              variant="outline"
              data-testid="messenger-cancel-facebook"
              @click="cancelMetaMessengerAuthorization"
            >
              Cancel Facebook authorization
            </Button>
            <Button
              v-if="
                isManagedMetaAccount(settingsAccount) && managedMetaLifecycleReady(settingsAccount)
              "
              type="button"
              variant="outline"
              :disabled="managedMetaActionBusy(settingsAccount)"
              @click="testAccount(settingsAccount)"
            >
              <RefreshCw class="mr-2 h-4 w-4" />
              Test connection
            </Button>
            <Button
              v-if="
                isManagedMetaAccount(settingsAccount) &&
                canManageIntegrations &&
                managedMetaLifecycleReady(settingsAccount) &&
                settingsAccount.status === 'pending' &&
                Boolean(settingsAccount.last_health_check_at) &&
                !settingsAccount.last_error
              "
              type="button"
              class="bg-emerald-300 text-black hover:bg-emerald-200"
              :disabled="managedMetaActionBusy(settingsAccount)"
              @click="approveManagedMeta(settingsAccount)"
            >
              Approve activation
            </Button>
            <Button
              v-if="
                isManagedMetaAccount(settingsAccount) &&
                canManageIntegrations &&
                managedMetaLifecycleReady(settingsAccount) &&
                settingsAccount.status === 'active' &&
                settingsAccount.config?.outbound_enabled !== true
              "
              type="button"
              class="bg-emerald-300 text-black hover:bg-emerald-200"
              :disabled="managedMetaActionBusy(settingsAccount)"
              @click="approveOutbound(settingsAccount)"
            >
              Re-enable outbound
            </Button>
            <Button
              v-if="settingsAccount.config?.outbound_enabled === true"
              type="button"
              variant="outline"
              @click="disableOutbound"
            >
              Disable outbound
            </Button>
          </div>
          <p
            v-if="metaMessengerAuthorizationController && isManagedMetaMessenger(settingsAccount)"
            data-testid="messenger-facebook-reconnect-wait-guidance"
            role="status"
            aria-live="polite"
            class="mt-3 rounded-lg border border-[#1877f2]/25 bg-[#1877f2]/10 px-3 py-2 text-xs leading-5 text-blue-100 light:border-blue-200 light:bg-blue-50 light:text-blue-900"
          >
            Waiting for Meta. Complete the steps in the Meta window and click Finish. Do not close either window while it is loading.
          </p>
        </div>

        <div
          v-if="
            settingsAccount.provider !== 'meta_legacy' && canDeleteAccounts &&
            (!isManagedMetaAccount(settingsAccount) ||
              (managedMetaTeardownReady(settingsAccount) && canManageIntegrations))
          "
          class="mt-5 border-t border-red-300/15 pt-5 light:border-red-200"
        >
          <p class="text-xs font-semibold text-red-200 light:text-red-700">
            {{
              isManagedMetaMessenger(settingsAccount)
                ? 'Disconnect Facebook Page'
                : isManagedMetaInstagram(settingsAccount)
                  ? 'Disconnect Instagram profile'
                  : 'Disconnect relay'
            }}
          </p>
          <p class="mt-1 text-[11px] leading-5 text-white/40 light:text-gray-500">
            {{
              isManagedMetaMessenger(settingsAccount)
                ? 'Unsubscribes the exact Page, revokes both ReReply credential versions, and keeps CRM history.'
                : isManagedMetaInstagram(settingsAccount)
                  ? 'Unsubscribes the exact profile, revokes both ReReply credential versions, and keeps CRM history.'
                  : 'Stops new traffic and removes the connection. CRM history is retained.'
            }}
          </p>
          <Button
            type="button"
            variant="destructive"
            class="mt-3"
            :data-testid="
              isManagedMetaInstagram(settingsAccount) ? 'instagram-disconnect-managed' : undefined
            "
            @click="disconnectAccount"
          >
            Disconnect connection
          </Button>
        </div>
      </form>
    </div>
  </div>
</template>
