<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  AtSign,
  Bot,
  CheckCircle2,
  AlertCircle,
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
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import { wsService } from '@/services/websocket'
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

interface CreatedConnection {
  account: ChannelAccount
  inbound_secret: string
  webhook_path: string
}

const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const loadingMessages = ref(false)
const loadingMore = ref(false)
const loadingOlderMessages = ref(false)
const sending = ref(false)
const aiStateUpdating = ref(false)
const showConnect = ref(false)
const accounts = ref<ChannelAccount[]>([])
const conversations = ref<InboxConversation[]>([])
const selectedConversation = ref<InboxConversation | null>(null)
const messages = ref<InboxMessage[]>([])
const channelFilter = ref<ChannelType | 'all'>('all')
const search = ref('')
const composer = ref('')
const createdConnection = ref<CreatedConnection | null>(null)
const settingsAccount = ref<ChannelAccount | null>(null)
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
const canDeleteAccounts = computed(() => authStore.hasPermission('channel_accounts', 'delete'))
const canManageConversations = computed(() => authStore.hasPermission('conversations', 'write'))
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
})
let stopChannelSync: (() => void) | null = null
let pollTimer: number | null = null
let syncDebounceTimer: number | null = null
let filterDebounceTimer: number | null = null
let refreshInFlight = false

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
    provider: 'relay',
    gated: true,
    connectable: true,
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

const selectedAccount = computed(() =>
  selectedConversation.value
    ? accounts.value.find((account) => account.id === selectedConversation.value?.channel_account_id)
    : undefined,
)
const canControlConversationAI = computed(
  () =>
    canManageConversations.value &&
    ['instagram', 'messenger'].includes(selectedConversation.value?.channel ?? '') &&
    selectedAccount.value?.config?.ai_reply_enabled === true,
)

const hasThreadsPublicReplyTarget = computed(() => {
  if (selectedConversation.value?.channel !== 'threads') return true
  const engagementType =
    typeof selectedConversation.value.metadata?.engagement_type === 'string'
      ? selectedConversation.value.metadata.engagement_type.trim().toLowerCase()
      : ''
  return (
    Boolean(selectedConversation.value.external_conversation_id?.trim()) &&
    ['reply', 'mention'].includes(engagementType)
  )
})
const canSendText = computed(() =>
  canManageConversations.value &&
  selectedAccount.value?.status === 'active' &&
  selectedAccount.value?.config?.outbound_enabled === true &&
  selectedAccount.value?.capabilities?.text !== false &&
  hasThreadsPublicReplyTarget.value,
)
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

function accountReadyForOutbound(account: ChannelAccount) {
  if (account.status !== 'active') return false
  if (account.provider === 'meta_legacy') return true
  return account.config?.outbound_enabled === true
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

function totalFromResponse(response: any) {
  const payload = response?.data?.data ?? response?.data
  return typeof payload?.total === 'number' ? payload.total : 0
}

async function load(silent = false, append = false) {
  if (refreshInFlight) return
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
    accounts.value = unwrapListResponse<ChannelAccount>(accountResponse, 'accounts')
    const incoming = unwrapListResponse<InboxConversation>(conversationResponse, 'conversations')
    conversationTotal.value = totalFromResponse(conversationResponse)
    if (append) {
      const incomingIDs = new Set(incoming.map((item) => item.id))
      conversations.value = [
        ...conversations.value.filter((item) => !incomingIDs.has(item.id)),
        ...incoming,
      ]
      conversationPage.value = page
    } else if (silent) {
      const firstPageIDs = new Set(incoming.map((item) => item.id))
      conversations.value = [
        ...incoming,
        ...conversations.value.filter((item) => !firstPageIDs.has(item.id)),
      ].slice(0, Math.max(conversationTotal.value, incoming.length))
    } else {
      conversations.value = incoming
      conversationPage.value = 1
    }

    if (selectedConversation.value) {
      selectedConversation.value =
        conversations.value.find((item) => item.id === selectedConversation.value?.id) ?? null
    }
    if (silent && selectedConversation.value) {
      const messageResponse = await channelsService.messages(selectedConversation.value.id, { limit: 100 })
      const latest = unwrapListResponse<InboxMessageEnvelope>(messageResponse, 'messages')
        .map(normalizeMessage)
        .reverse()
      messageTotal.value = totalFromResponse(messageResponse)
      const merged = new Map(messages.value.map((message) => [message.id, message]))
      for (const message of latest) merged.set(message.id, message)
      messages.value = [...merged.values()].sort(
        (left, right) => new Date(left.created_at).getTime() - new Date(right.created_at).getTime(),
      )
    }
  } catch (error) {
    if (!silent) toast.error('Channels could not be loaded', getErrorMessage(error))
  } finally {
    if (!silent) loading.value = false
    loadingMore.value = false
    refreshInFlight = false
  }
}

function loadMoreConversations() {
  void load(true, true)
}

function scheduleChannelRefresh() {
  if (syncDebounceTimer !== null) window.clearTimeout(syncDebounceTimer)
  syncDebounceTimer = window.setTimeout(() => {
    syncDebounceTimer = null
    if (document.visibilityState === 'visible') void load(true)
  }, 300)
}

async function selectConversation(conversation: InboxConversation) {
  selectedConversation.value = conversation
  loadingMessages.value = true
  try {
    const response = await channelsService.messages(conversation.id, { limit: 100 })
    messages.value = unwrapListResponse<InboxMessageEnvelope>(response, 'messages')
      .map(normalizeMessage)
      .reverse()
    messageTotal.value = totalFromResponse(response)
    if (conversation.unread_count > 0 && canManageConversations.value) {
      await channelsService.markRead(conversation.id)
      conversation.unread_count = 0
    }
  } catch (error) {
    toast.error('Messages could not be loaded', getErrorMessage(error))
  } finally {
    loadingMessages.value = false
  }
}

async function loadOlderMessages() {
  const conversation = selectedConversation.value
  const oldest = messages.value[0]
  if (!conversation || !oldest || loadingOlderMessages.value || !hasOlderMessages.value) return
  loadingOlderMessages.value = true
  try {
    const response = await channelsService.messages(conversation.id, {
      before: oldest.id,
      limit: 100,
    })
    const older = unwrapListResponse<InboxMessageEnvelope>(response, 'messages')
      .map(normalizeMessage)
      .reverse()
    messageTotal.value = totalFromResponse(response)
    const existingIDs = new Set(messages.value.map((message) => message.id))
    messages.value = [
      ...older.filter((message) => !existingIDs.has(message.id)),
      ...messages.value,
    ]
  } catch (error) {
    toast.error('Older messages could not be loaded', getErrorMessage(error))
  } finally {
    loadingOlderMessages.value = false
  }
}

async function sendMessage() {
  if (!selectedConversation.value || !composer.value.trim()) return
  if (
    selectedConversation.value.channel === 'threads' &&
    !hasThreadsPublicReplyTarget.value
  ) {
    toast.warning('Select an existing public Threads reply or mention before replying')
    return
  }
  if (!canSendText.value) return
  sending.value = true
  try {
    const payload: Record<string, unknown> = {
      idempotency_key: crypto.randomUUID(),
      purpose: 'service',
      parts: [{ type: 'text', text: composer.value.trim() }],
    }
    if (selectedConversation.value.channel === 'threads') {
      payload.reply_to_external_id = selectedConversation.value.external_conversation_id
    }
    const response = await channelsService.send(selectedConversation.value.id, payload)
    const result = unwrapItemResponse<InboxMessageEnvelope>(response)
    messages.value.push(normalizeMessage(result))
    updateLocalConversationAIState(true, 'human_reply')
    composer.value = ''
  } catch (error) {
    toast.error('Message was not sent', getErrorMessage(error))
  } finally {
    sending.value = false
  }
}

function updateLocalConversationAIState(paused: boolean, reason = '') {
  const selected = selectedConversation.value
  if (!selected) return
  selected.ai_paused = paused
  selected.ai_pause_reason = reason || undefined
  const listed = conversations.value.find(conversation => conversation.id === selected.id)
  if (listed && listed !== selected) {
    listed.ai_paused = paused
    listed.ai_pause_reason = reason || undefined
  }
}

async function toggleConversationAI() {
  const conversation = selectedConversation.value
  if (!conversation || !canControlConversationAI.value || aiStateUpdating.value) return
  const paused = !conversation.ai_paused
  aiStateUpdating.value = true
  try {
    const response = await channelsService.setAIState(conversation.id, paused)
    const state = unwrapItemResponse<{
      conversation_id: string
      ai_paused: boolean
      ai_pause_reason?: string
    }>(response)
    updateLocalConversationAIState(state.ai_paused, state.ai_pause_reason)
    toast.success(
      state.ai_paused ? 'AI replies paused' : 'AI replies resumed',
      state.ai_paused
        ? 'Only human replies will be sent in this conversation.'
        : 'The next eligible customer message can receive an automatic reply.',
    )
  } catch (error) {
    toast.error('AI reply state was not changed', getErrorMessage(error))
  } finally {
    aiStateUpdating.value = false
  }
}

async function connectAccount() {
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

function openAccountSettings(account: ChannelAccount) {
  settingsAccount.value = account
  accountSettingsDraft.name = account.name
  accountSettingsDraft.relay_url =
    typeof account.config?.relay_url === 'string' ? account.config.relay_url : ''
  accountSettingsDraft.outbound_secret = ''
  accountSettingsDraft.ai_reply_enabled = account.config?.ai_reply_enabled === true
}

async function saveAccountSettings() {
  const account = settingsAccount.value
  if (!account || !canManageAccounts.value || !accountSettingsDraft.name.trim()) return
  if (account.provider === 'relay' && !accountSettingsDraft.relay_url.trim()) {
    toast.warning('A signed-relay URL is required')
    return
  }
  try {
    const update: Record<string, unknown> = {
      name: accountSettingsDraft.name.trim(),
    }
    if (account.provider === 'relay') {
      update.config = {
        ...account.config,
        relay_url: accountSettingsDraft.relay_url.trim(),
      }
    }
    if (accountSettingsDraft.outbound_secret.trim()) {
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
  if (!account || !canDeleteAccounts.value) return
  const confirmed = window.confirm(
    `Disconnect ${account.name}? New inbound and outbound traffic for this relay will stop. Existing CRM history remains.`,
  )
  if (!confirmed) return
  try {
    await channelsService.disconnectAccount(account.id)
    settingsAccount.value = null
    if (selectedConversation.value?.channel_account_id === account.id) {
      selectedConversation.value = null
      messages.value = []
    }
    toast.success('Channel disconnected')
    await load()
  } catch (error) {
    toast.error('Channel was not disconnected', getErrorMessage(error))
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

onMounted(() => {
  void load()
  stopChannelSync = wsService.onChannelSync(scheduleChannelRefresh)
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === 'visible') void load(true)
  }, 30000)
})

watch([search, channelFilter], () => {
  if (filterDebounceTimer !== null) window.clearTimeout(filterDebounceTimer)
  filterDebounceTimer = window.setTimeout(() => {
    filterDebounceTimer = null
    selectedConversation.value = null
    messages.value = []
    messageTotal.value = 0
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
  stopChannelSync?.()
  if (pollTimer !== null) window.clearInterval(pollTimer)
  if (syncDebounceTimer !== null) window.clearTimeout(syncDebounceTimer)
  if (filterDebounceTimer !== null) window.clearTimeout(filterDebounceTimer)
})
</script>

<template>
  <div class="flex h-full flex-col bg-[#08090a] light:bg-[#f5f6f7]">
    <PageHeader
      title="Omnichannel inbox"
      description="One conversation layer across WhatsApp, social, email and web chat."
      :icon="Inbox"
      icon-gradient="bg-gradient-to-br from-sky-400 to-indigo-700 shadow-sky-500/20"
    >
      <template #actions>
        <div class="flex items-center gap-2">
          <Badge variant="outline" class="hidden gap-1.5 border-emerald-400/20 text-emerald-300 md:flex">
            <CheckCircle2 class="h-3.5 w-3.5" />
            {{ activeCount }} active
          </Badge>
          <Badge v-if="attentionCount" variant="outline" class="hidden gap-1.5 border-amber-400/20 text-amber-300 md:flex">
            <AlertCircle class="h-3.5 w-3.5" />
            {{ attentionCount }} attention
          </Badge>
          <Button variant="outline" size="icon" @click="load"><RefreshCw class="h-4 w-4" /></Button>
          <Button v-if="canManageAccounts" class="bg-sky-400 text-black hover:bg-sky-300" @click="showConnect = !showConnect">
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
      <div
        v-if="newAccount.channel === 'threads'"
        data-testid="threads-public-engagement-notice"
        class="flex items-start gap-3 rounded-xl border border-amber-300/20 bg-amber-300/[0.06] px-4 py-3 md:col-span-2 xl:col-span-5"
      >
        <AtSign class="mt-0.5 h-4 w-4 shrink-0 text-amber-200 light:text-amber-700" />
        <div>
          <p class="text-xs font-semibold text-amber-50 light:text-amber-900">Beta: public engagement only</p>
          <p class="mt-1 text-xs leading-5 text-amber-50/60 light:text-amber-800">
            This connection is limited to existing public Threads replies and mentions. Direct
            messages and standalone posts are not supported. It remains pending until an approved
            compatible Threads relay adapter is installed and passes Test.
          </p>
        </div>
      </div>
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
      class="group shrink-0 border-b border-white/[0.08] bg-[#0b0c0d] light:border-gray-200 light:bg-white 2xl:hidden"
    >
      <summary
        class="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-xs font-semibold text-white light:text-gray-900"
      >
        <span>Channel connections</span>
        <span class="rounded-full bg-white/[0.06] px-2 py-1 text-[10px] text-white/45 light:bg-gray-100 light:text-gray-500">
          {{ accounts.length }} configured · open to test or approve
        </span>
      </summary>
      <div class="max-h-60 space-y-2 overflow-y-auto border-t border-white/[0.06] p-3 light:border-gray-100">
        <div
          v-for="account in accounts"
          :key="`compact-${account.id}`"
          class="flex flex-wrap items-center gap-3 rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
        >
          <button
            type="button"
            class="flex min-w-0 flex-1 items-center gap-3 text-left"
            @click="channelFilter = account.channel"
          >
            <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-white/[0.04] text-white/55 light:bg-white light:text-gray-600">
              <component :is="channelMeta(account.channel).icon" class="h-4 w-4" />
            </span>
            <span class="min-w-0">
              <span class="block truncate text-xs font-medium text-white light:text-gray-900">{{ account.name }}</span>
              <span
                class="mt-0.5 block text-[10px]"
                :class="accountReadyForOutbound(account) ? 'text-emerald-300' : 'text-amber-300'"
              >
                {{
                  accountReadyForOutbound(account)
                    ? 'Outbound ready'
                    : account.status === 'active'
                      ? 'Awaiting outbound approval'
                      : account.status
                }}
                <span v-if="account.outbox_pending"> · {{ account.outbox_pending }} queued</span>
                <span v-if="account.outbox_failed" class="text-red-300"> · {{ account.outbox_failed }} failed</span>
              </span>
            </span>
          </button>
          <Button
            v-if="
              canManageAccounts &&
              account.provider === 'relay' &&
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
            v-if="canManageAccounts"
            variant="outline"
            size="sm"
            class="h-8"
            :aria-label="`Test ${account.name}`"
            @click="testAccount(account)"
          >
            <RefreshCw class="mr-1.5 h-3.5 w-3.5" />
            Test
          </Button>
          <Button
            v-if="(canManageAccounts || canDeleteAccounts) && account.provider !== 'meta_legacy'"
            variant="outline"
            size="sm"
            class="h-8"
            @click="openAccountSettings(account)"
          >
            <Settings2 class="mr-1.5 h-3.5 w-3.5" />
            Settings
          </Button>
        </div>
        <div v-if="accounts.length === 0" class="rounded-xl border border-dashed border-white/[0.08] p-5 text-center light:border-gray-200">
          <Unplug class="mx-auto h-5 w-5 text-white/25 light:text-gray-400" />
          <p class="mt-2 text-xs text-white/35 light:text-gray-500">No channel connections yet</p>
        </div>
      </div>
    </details>

    <div v-if="loading" class="flex flex-1 items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-sky-300" />
    </div>

    <div v-else class="grid min-h-0 flex-1 lg:grid-cols-[320px_1fr] 2xl:grid-cols-[250px_340px_1fr]">
      <aside class="hidden overflow-y-auto border-r border-white/[0.08] bg-[#0b0c0d] p-3 light:border-gray-200 light:bg-white 2xl:block">
        <p class="px-2 pb-3 pt-1 text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">
          Connections
        </p>
        <div class="space-y-1.5">
          <div
            v-for="account in accounts"
            :key="account.id"
            role="button"
            tabindex="0"
            class="flex w-full cursor-pointer items-center gap-3 rounded-xl border border-transparent px-2.5 py-2.5 text-left transition hover:border-white/[0.06] hover:bg-white/[0.025] light:hover:border-gray-200 light:hover:bg-gray-50"
            @click="channelFilter = account.channel"
            @keydown.enter="channelFilter = account.channel"
          >
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-white/[0.04] text-white/55 light:bg-gray-100 light:text-gray-600">
              <component :is="channelMeta(account.channel).icon" class="h-4 w-4" />
            </div>
            <div class="min-w-0 flex-1">
              <p class="truncate text-xs font-medium text-white light:text-gray-900">{{ account.name }}</p>
              <p class="mt-0.5 text-[10px] capitalize text-white/35 light:text-gray-500">{{ account.channel }} · {{ account.provider }}</p>
              <p
                class="mt-1 text-[9px]"
                :class="accountReadyForOutbound(account) ? 'text-emerald-300' : 'text-amber-300'"
              >
                {{
                  accountReadyForOutbound(account)
                    ? 'Outbound ready'
                    : account.status === 'active'
                      ? 'Awaiting outbound approval'
                      : account.status
                }}
                <span v-if="account.outbox_pending"> · {{ account.outbox_pending }} queued</span>
                <span v-if="account.outbox_failed" class="text-red-300"> · {{ account.outbox_failed }} failed</span>
              </p>
              <p v-if="account.last_error" class="mt-1 line-clamp-2 text-[9px] text-red-300/75">
                {{ account.last_error }}
              </p>
            </div>
            <button
              v-if="
                canManageAccounts &&
                account.provider === 'relay' &&
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
              v-if="canManageAccounts"
              type="button"
              class="rounded-lg p-1.5 text-white/25 transition hover:bg-white/[0.05] hover:text-sky-300 light:text-gray-400"
              :aria-label="`Test ${account.name}`"
              @click.stop="testAccount(account)"
            >
              <RefreshCw class="h-3.5 w-3.5" />
            </button>
            <button
              v-if="(canManageAccounts || canDeleteAccounts) && account.provider !== 'meta_legacy'"
              type="button"
              class="rounded-lg p-1.5 text-white/25 transition hover:bg-white/[0.05] hover:text-sky-300 light:text-gray-400"
              :aria-label="`Manage ${account.name}`"
              @click.stop="openAccountSettings(account)"
            >
              <Settings2 class="h-3.5 w-3.5" />
            </button>
          </div>
          <div v-if="accounts.length === 0" class="rounded-xl border border-dashed border-white/[0.08] p-5 text-center light:border-gray-200">
            <Unplug class="mx-auto h-5 w-5 text-white/25 light:text-gray-400" />
            <p class="mt-2 text-xs text-white/35 light:text-gray-500">No channel connections yet</p>
          </div>
        </div>

        <p class="mt-7 px-2 text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">
          Available adapters
        </p>
        <div class="mt-2 grid grid-cols-2 gap-2">
          <div
            v-for="channel in visibleChannels"
            :key="channel.key"
            :data-testid="channel.key === 'threads' ? 'threads-public-engagement-adapter' : undefined"
            class="rounded-xl border border-white/[0.06] bg-white/[0.018] p-2.5 light:border-gray-200 light:bg-gray-50"
          >
            <component :is="channel.icon" class="h-4 w-4 text-white/45 light:text-gray-500" />
            <p class="mt-2 text-[10px] font-medium text-white/65 light:text-gray-700">{{ channel.label }}</p>
            <p class="mt-0.5 text-[9px] text-white/25 light:text-gray-400">
              {{
                channel.key === 'threads'
                  ? 'Beta public replies + mentions; no DMs'
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

      <section class="flex min-h-0 flex-col border-r border-white/[0.08] bg-[#0d0f10] light:border-gray-200 light:bg-white">
        <div class="border-b border-white/[0.07] p-3 light:border-gray-100">
          <div class="relative">
            <AtSign class="absolute left-3 top-2.5 h-4 w-4 text-white/25 light:text-gray-400" />
            <Input v-model="search" class="pl-9" placeholder="Search conversations" />
          </div>
          <div class="mt-2 flex gap-1 overflow-x-auto">
            <button
              class="shrink-0 rounded-full px-2.5 py-1 text-[10px] font-medium transition"
              :class="channelFilter === 'all' ? 'bg-sky-300 text-black' : 'bg-white/[0.04] text-white/45 light:bg-gray-100 light:text-gray-500'"
              @click="channelFilter = 'all'"
            >
              All
            </button>
            <button
              v-for="channel in supportedChannels.filter((item) => accounts.some((account) => account.channel === item.key))"
              :key="channel.key"
              class="shrink-0 rounded-full px-2.5 py-1 text-[10px] font-medium transition"
              :class="
                channelFilter === channel.key
                  ? 'bg-sky-300 text-black'
                  : 'bg-white/[0.04] text-white/45 light:bg-gray-100 light:text-gray-500'
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
            class="flex w-full gap-3 border-b border-white/[0.055] px-3 py-3.5 text-left transition light:border-gray-100"
            :class="
              selectedConversation?.id === conversation.id
                ? 'bg-sky-300/[0.08]'
                : 'hover:bg-white/[0.025] light:hover:bg-gray-50'
            "
            @click="selectConversation(conversation)"
          >
            <div class="relative flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-white/[0.045] text-white/55 light:bg-gray-100 light:text-gray-600">
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
                <p class="truncate text-xs font-semibold text-white light:text-gray-900">
                  {{ conversationName(conversation) }}
                </p>
                <span class="shrink-0 text-[9px] text-white/25 light:text-gray-400">{{ formatTime(conversation.last_message_at) }}</span>
              </div>
              <p v-if="conversation.subject" class="mt-1 truncate text-[10px] font-medium text-white/50 light:text-gray-600">{{ conversation.subject }}</p>
              <p class="mt-1 truncate text-[11px] text-white/32 light:text-gray-500">{{ conversation.last_message_preview || 'No preview' }}</p>
            </div>
          </button>
          <div v-if="filteredConversations.length === 0" class="p-8 text-center">
            <Inbox class="mx-auto h-6 w-6 text-white/20 light:text-gray-300" />
            <p class="mt-3 text-xs text-white/35 light:text-gray-500">No matching conversations</p>
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

      <main class="flex min-h-0 flex-col bg-[#090a0b] light:bg-[#f8f9fa]">
        <template v-if="selectedConversation">
          <header class="flex items-center justify-between border-b border-white/[0.08] bg-[#0d0f10] px-5 py-3.5 light:border-gray-200 light:bg-white">
            <div class="flex items-center gap-3">
              <div class="flex h-10 w-10 items-center justify-center rounded-xl bg-sky-300/10 text-sky-200">
                <component :is="channelMeta(selectedConversation.channel).icon" class="h-4 w-4" />
              </div>
              <div>
                <p class="text-sm font-semibold text-white light:text-gray-900">
                  {{ conversationName(selectedConversation) }}
                </p>
                <p class="mt-0.5 text-[10px] capitalize text-white/35 light:text-gray-500">
                  {{ selectedConversation.channel }} · {{ selectedAccount?.name || 'Channel' }}
                </p>
              </div>
            </div>
            <Button
              v-if="canControlConversationAI"
              data-testid="conversation-ai-toggle"
              type="button"
              variant="outline"
              size="sm"
              :disabled="aiStateUpdating"
              :aria-pressed="selectedConversation.ai_paused"
              @click="toggleConversationAI"
            >
              <Loader2 v-if="aiStateUpdating" class="mr-2 h-3.5 w-3.5 animate-spin" />
              <Play v-else-if="selectedConversation.ai_paused" class="mr-2 h-3.5 w-3.5" />
              <PauseCircle v-else class="mr-2 h-3.5 w-3.5" />
              {{ selectedConversation.ai_paused ? 'Resume AI' : 'Pause AI' }}
            </Button>
          </header>

          <div v-if="loadingMessages" class="flex flex-1 items-center justify-center">
            <Loader2 class="h-5 w-5 animate-spin text-sky-300" />
          </div>
          <div v-else class="flex-1 space-y-3 overflow-y-auto p-5 md:p-7">
            <div v-if="hasOlderMessages" class="pb-2 text-center">
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
              class="flex"
              :class="message.direction === 'outgoing' ? 'justify-end' : 'justify-start'"
            >
              <div
                class="max-w-[72%] rounded-2xl px-4 py-3 text-sm leading-6"
                :class="
                  message.direction === 'outgoing'
                    ? 'rounded-br-md bg-sky-300 text-slate-950'
                    : 'rounded-bl-md border border-white/[0.07] bg-white/[0.04] text-white/80 light:border-gray-200 light:bg-white light:text-gray-800'
                "
              >
                <p>{{ message.content }}</p>
                <p class="mt-1 text-right text-[9px] opacity-50">{{ formatTime(message.created_at) }} · {{ message.status }}</p>
              </div>
            </div>
          </div>

          <footer class="border-t border-white/[0.08] bg-[#0d0f10] p-4 light:border-gray-200 light:bg-white">
            <div
              v-if="selectedConversation.channel === 'whatsapp'"
              class="flex items-center justify-between rounded-xl border border-emerald-300/15 bg-emerald-300/[0.04] px-4 py-3"
            >
              <p class="text-xs text-emerald-100/65">Open the WhatsApp conversation to use templates, media and service-window controls.</p>
              <RouterLink :to="`/chat/${selectedConversation.contact_id}`">
                <Button size="sm" class="bg-emerald-300 text-black hover:bg-emerald-200">Open WhatsApp</Button>
              </RouterLink>
            </div>
            <div v-else class="space-y-2">
              <div
                v-if="selectedConversation.channel === 'threads'"
                data-testid="threads-public-reply-composer-notice"
                class="rounded-xl border border-amber-300/15 bg-amber-300/[0.04] px-3 py-2 text-[11px] leading-5 text-amber-100/65 light:text-amber-800"
              >
                Beta public-only composer: the selected reply or mention is the required target.
                Threads direct messages and standalone posts are not supported.
              </div>
              <div class="flex items-end gap-3">
              <textarea
                v-model="composer"
                rows="2"
                class="min-h-11 flex-1 resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none placeholder:text-white/20 focus:border-sky-300/35 light:border-gray-200 light:bg-gray-50 light:text-gray-900"
                :disabled="!canSendText"
                :placeholder="
                  selectedConversation.channel === 'threads' && !hasThreadsPublicReplyTarget
                    ? 'Select an existing public reply or mention'
                    : canSendText
                      ? selectedConversation.channel === 'threads'
                        ? 'Write a public Threads reply...'
                        : 'Write a reply...'
                      : selectedAccount?.config?.outbound_enabled !== true
                        ? 'Outbound delivery requires explicit approval'
                        : 'This adapter does not support text replies'
                "
                @keydown.ctrl.enter.prevent="sendMessage"
              />
              <Button
                size="icon"
                class="h-11 w-11 shrink-0 bg-sky-300 text-black hover:bg-sky-200"
                :disabled="sending || !composer.trim() || !canSendText"
                :aria-label="
                  selectedConversation.channel === 'threads'
                    ? 'Send public Threads reply'
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
          <div class="flex h-16 w-16 items-center justify-center rounded-2xl border border-white/[0.08] bg-white/[0.025] text-white/25 light:border-gray-200 light:bg-white light:text-gray-400">
            <Inbox class="h-6 w-6" />
          </div>
          <h2 class="mt-5 text-lg font-semibold text-white light:text-gray-900">Every channel, one operating rhythm</h2>
          <p class="mt-2 max-w-md text-sm leading-6 text-white/40 light:text-gray-500">
            Select a conversation. Provider capabilities decide which composer actions are available, while tenant isolation
            remains the same on every channel.
          </p>
        </div>
      </main>
    </div>

    <div
      v-if="settingsAccount"
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4 backdrop-blur-sm"
      role="dialog"
      aria-modal="true"
      aria-labelledby="channel-settings-title"
      @click.self="settingsAccount = null"
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
              Repair the relay, rotate its outbound signing credential, or stop delivery.
            </p>
          </div>
          <Button type="button" variant="outline" size="sm" @click="settingsAccount = null">Close</Button>
        </div>

        <div v-if="canManageAccounts" class="mt-5 space-y-4">
          <label class="block">
            <span class="text-xs font-medium text-white/60 light:text-gray-600">Connection name</span>
            <Input v-model="accountSettingsDraft.name" class="mt-1.5" required maxlength="100" />
          </label>
          <label v-if="settingsAccount.provider === 'relay'" class="block">
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
                Automatic Qwen replies
              </span>
              <span class="mt-1 block text-[10px] leading-4 text-white/35 light:text-gray-500">
                Explicit opt-in. Requires an active tested connection, outbound approval, and an enabled Qwen profile.
                Human replies pause AI for that conversation.
              </span>
            </span>
          </label>
          <label v-if="settingsAccount.provider === 'relay'" class="block">
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
            <Button type="submit" class="bg-sky-300 text-black hover:bg-sky-200">Save changes</Button>
            <Button
              v-if="settingsAccount.config?.outbound_enabled === true"
              type="button"
              variant="outline"
              @click="disableOutbound"
            >
              Disable outbound
            </Button>
          </div>
        </div>

        <div
          v-if="canDeleteAccounts"
          class="mt-5 border-t border-red-300/15 pt-5 light:border-red-200"
        >
          <p class="text-xs font-semibold text-red-200 light:text-red-700">Disconnect relay</p>
          <p class="mt-1 text-[11px] leading-5 text-white/40 light:text-gray-500">
            Stops new traffic and removes the connection. CRM history is retained.
          </p>
          <Button type="button" variant="destructive" class="mt-3" @click="disconnectAccount">
            Disconnect connection
          </Button>
        </div>
      </form>
    </div>
  </div>
</template>
