<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, CheckCheck, Clock, Loader2, ShieldAlert } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { contactsService } from '@/services/api'
import { wsService } from '@/services/websocket'
import { getErrorMessage } from '@/lib/api-utils'
import { useContactsStore, type Contact, type Message } from '@/stores/contacts'
import {
  MAX_FORWARD_TARGETS,
  classifyForwardError,
  forwardSourceBlock,
  forwardTargetBlock,
  type ForwardTargetBlock,
} from './messageForward'

/**
 * Forward one stored message to up to five contacts. WhatsApp has no forward
 * call, so each target gets a new message (one request per target). Contacts
 * whose 24h window is closed or whose identity is under review cannot be
 * picked; the confirmation step lists every recipient by name (PDPA).
 *
 * A 200 only means the message was queued: delivery runs after commit. Each
 * target therefore shows Queued, then Sent or Failed from the live status
 * events for its message id, or a "not confirmed" note after a timeout.
 *
 * A failed request follows the server's contract (classifyForwardError): a
 * definite "not sent" (503) offers a manual Try again for that target only; an
 * ambiguous failure says it may have been forwarded and is never retried.
 */
const props = defineProps<{
  open: boolean
  message: Message | null
  /** Short description of the message being forwarded. */
  preview: string
}>()

const emit = defineEmits<{
  'update:open': [open: boolean]
}>()

const { t } = useI18n()
const contactsStore = useContactsStore()

type Step = 'pick' | 'confirm' | 'results'
type ResultState = 'pending' | 'sending' | 'queued' | 'sent' | 'failed' | 'unconfirmed' | 'retryable' | 'uncertain'
interface TargetResult {
  contact: Contact
  state: ResultState
  messageId?: string
  error?: string
}

const SEARCH_DEBOUNCE_MS = 250
const SEARCH_LIMIT = 25
// After this, a still-queued target is shown as not confirmed.
const DELIVERY_CONFIRM_TIMEOUT_MS = 60_000
const DELIVERED_STATUSES = new Set(['sent', 'delivered', 'read'])

const step = ref<Step>('pick')
const search = ref('')
const candidates = ref<Contact[]>([])
const isLoading = ref(false)
const loadFailed = ref(false)
const selected = ref<Contact[]>([])
const results = ref<TargetResult[]>([])

const sourceBlock = computed(() => (props.message ? forwardSourceBlock(props.message) : 'unsupported'))
const isSending = computed(() => results.value.some(r => r.state === 'pending' || r.state === 'sending'))
const hasUnconfirmed = computed(() => results.value.some(r => r.state === 'unconfirmed'))
const hasUncertain = computed(() => results.value.some(r => r.state === 'uncertain'))
const sentCount = computed(() => results.value.filter(r => r.state === 'sent').length)
const limitReached = computed(() => selected.value.length >= MAX_FORWARD_TARGETS)

let searchTimer: ReturnType<typeof setTimeout> | undefined
let searchController: AbortController | null = null

function displayName(contact: Contact): string {
  return contact.profile_name || contact.name || contact.phone_number
}

function isSelected(contact: Contact): boolean {
  return selected.value.some(c => c.id === contact.id)
}

function targetBlock(contact: Contact): ForwardTargetBlock {
  return forwardTargetBlock(contact)
}

function isPickable(contact: Contact): boolean {
  return targetBlock(contact) === null && (isSelected(contact) || !limitReached.value)
}

function statusLabel(contact: Contact): string {
  switch (targetBlock(contact)) {
    case 'identity_review':
      return t('chat.forward.identityReview')
    case 'window_closed':
      return t('chat.forward.windowClosed')
    default:
      return t('chat.forward.windowOpen')
  }
}

function toggle(contact: Contact) {
  if (isSelected(contact)) {
    selected.value = selected.value.filter(c => c.id !== contact.id)
    return
  }
  if (!isPickable(contact)) return
  selected.value = [...selected.value, contact]
}

async function loadCandidates() {
  searchController?.abort()
  const controller = new AbortController()
  searchController = controller
  isLoading.value = true
  loadFailed.value = false
  try {
    const response = await contactsService.list(
      { search: search.value.trim() || undefined, page: 1, limit: SEARCH_LIMIT },
      controller.signal,
    )
    if (controller !== searchController) return
    const data = response.data?.data || response.data
    candidates.value = (data?.contacts || []) as Contact[]
  } catch {
    if (controller !== searchController || controller.signal.aborted) return
    loadFailed.value = true
    candidates.value = []
  } finally {
    if (controller === searchController) isLoading.value = false
  }
}

function reset() {
  step.value = 'pick'
  search.value = ''
  selected.value = []
  results.value = []
  candidates.value = []
  loadFailed.value = false
}

watch(
  () => props.open,
  open => {
    if (!open) {
      if (searchTimer) clearTimeout(searchTimer)
      searchController?.abort()
      return
    }
    reset()
    if (sourceBlock.value !== 'unsupported') void loadCandidates()
  },
  { immediate: true },
)

watch(search, () => {
  if (!props.open || step.value !== 'pick') return
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => void loadCandidates(), SEARCH_DEBOUNCE_MS)
})

// ---- Delivery status ------------------------------------------------------

let stopStatusWatch: (() => void) | null = null
let confirmTimer: ReturnType<typeof setTimeout> | undefined
// Status events can arrive before the POST response that names the message.
const earlyStatuses = new Map<string, { status: string; error?: string }>()

function applyStatus(messageId: string, status: string, error?: string) {
  const result = results.value.find(r => r.messageId === messageId)
  if (!result) {
    earlyStatuses.set(messageId, { status, error })
    return
  }
  if (result.state !== 'queued' && result.state !== 'unconfirmed') return
  if (status === 'failed') {
    result.state = 'failed'
    result.error = error || t('chat.forward.failed')
  } else if (DELIVERED_STATUSES.has(status)) {
    result.state = 'sent'
  }
  if (!results.value.some(r => r.state === 'queued' || r.state === 'unconfirmed' || r.state === 'pending' || r.state === 'sending')) {
    stopWatchingStatus()
  }
}

function watchStatus() {
  if (stopStatusWatch) return
  earlyStatuses.clear()
  stopStatusWatch = wsService.onInboxActivity(event => {
    const payload = event.payload
    if (!payload?.message_id) return
    if (event.type === 'status_update') {
      applyStatus(String(payload.message_id), String(payload.status ?? ''), payload.error_message || undefined)
    } else if (event.type === 'realtime_sync' && payload.kind === 'message_status_changed') {
      applyStatus(String(payload.message_id), String(payload.status ?? ''))
    }
  })
}

function stopWatchingStatus() {
  stopStatusWatch?.()
  stopStatusWatch = null
  if (confirmTimer) clearTimeout(confirmTimer)
  confirmTimer = undefined
  earlyStatuses.clear()
}

watch(() => props.open, open => {
  if (!open) stopWatchingStatus()
})

onBeforeUnmount(() => {
  if (searchTimer) clearTimeout(searchTimer)
  searchController?.abort()
  stopWatchingStatus()
})

async function forwardAll() {
  const message = props.message
  if (!message || sourceBlock.value !== null || selected.value.length === 0) return
  results.value = selected.value
    .slice(0, MAX_FORWARD_TARGETS)
    .map((contact): TargetResult => ({ contact, state: 'pending' }))
  step.value = 'results'
  watchStatus()
  // Sequential on purpose: one short request at a time keeps the server's
  // connection use flat, and each result can be shown as it lands.
  for (const result of results.value) {
    await sendTo(result, message.id)
  }
  settleWatching()
}

/** One forward request for one target. Exactly one request per call. */
async function sendTo(result: TargetResult, sourceId: string) {
  result.state = 'sending'
  result.error = undefined
  try {
    const created = await contactsStore.sendMessage(result.contact.id, 'text', {}, undefined, undefined, {
      forwardOfMessageId: sourceId,
    })
    result.messageId = created?.id ? String(created.id) : undefined
    result.state = 'queued'
    if (result.messageId) {
      const early = earlyStatuses.get(result.messageId)
      earlyStatuses.delete(result.messageId)
      applyStatus(result.messageId, early?.status ?? String(created?.status ?? ''), early?.error ?? created?.error_message)
    }
  } catch (error) {
    const failure = classifyForwardError(error)
    result.state = failure
    result.error = failure === 'uncertain' ? undefined : getErrorMessage(error, t('chat.forward.failed'))
  }
}

/** Keep listening only while a target waits for its delivery status. */
function settleWatching() {
  if (!results.value.some(r => r.state === 'queued')) {
    if (!results.value.some(r => r.state === 'unconfirmed')) stopWatchingStatus()
    return
  }
  if (confirmTimer) return
  confirmTimer = setTimeout(() => {
    confirmTimer = undefined
    for (const result of results.value) {
      if (result.state === 'queued') result.state = 'unconfirmed'
    }
  }, DELIVERY_CONFIRM_TIMEOUT_MS)
}

/** Manual retry, offered only when the server proved nothing was sent. */
async function retryTarget(result: TargetResult) {
  const message = props.message
  if (!message || result.state !== 'retryable' || isSending.value) return
  watchStatus()
  await sendTo(result, message.id)
  settleWatching()
}

function resultLabel(state: ResultState): string {
  switch (state) {
    case 'queued':
      return t('chat.forward.queued')
    case 'sent':
      return t('chat.forward.sent')
    case 'failed':
      return t('chat.forward.failed')
    case 'unconfirmed':
      return t('chat.forward.unconfirmed')
    case 'retryable':
      return t('chat.forward.retryable')
    case 'uncertain':
      return t('chat.forward.uncertain')
    default:
      return t('chat.forward.sending')
  }
}

function requestClose(open: boolean) {
  if (!open && isSending.value) return
  emit('update:open', open)
}
</script>

<template>
  <Dialog :open="open" @update:open="requestClose">
    <DialogContent
      class="sm:max-w-lg"
      data-testid="chat-forward-dialog"
      @escape-key-down="(event: Event) => isSending && event.preventDefault()"
      @interact-outside="(event: Event) => isSending && event.preventDefault()"
    >
      <DialogHeader>
        <DialogTitle>
          {{ step === 'results' ? t('chat.forward.resultsTitle') : step === 'confirm' ? t('chat.forward.confirmTitle') : t('chat.forward.title') }}
        </DialogTitle>
        <DialogDescription>{{ t('chat.forward.description') }}</DialogDescription>
      </DialogHeader>

      <p
        class="rounded-md border border-border bg-muted/40 px-3 py-2 text-sm text-muted-foreground line-clamp-2 break-words"
        data-testid="chat-forward-preview"
      >
        {{ preview }}
      </p>

      <p
        v-if="sourceBlock"
        role="alert"
        class="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-600 light:text-amber-700"
      >
        <AlertCircle class="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
        {{ sourceBlock === 'media_pending' ? t('chat.forward.mediaPending') : t('chat.forward.unsupportedSource') }}
      </p>

      <!-- Step 1: pick up to five open-window contacts -->
      <div v-if="step === 'pick' && sourceBlock !== 'unsupported'" class="space-y-2">
        <Input
          v-model="search"
          type="search"
          :placeholder="t('chat.forward.searchPlaceholder')"
          :aria-label="t('chat.forward.searchPlaceholder')"
          data-testid="chat-forward-search"
        />
        <div class="flex items-center justify-between text-xs text-muted-foreground">
          <span aria-live="polite">{{ t('chat.forward.selectedCount', { count: selected.length, max: MAX_FORWARD_TARGETS }) }}</span>
          <span v-if="limitReached">{{ t('chat.forward.limitReached', { max: MAX_FORWARD_TARGETS }) }}</span>
        </div>
        <div class="max-h-72 overflow-y-auto rounded-md border border-border" role="group" :aria-label="t('chat.forward.title')">
          <p v-if="isLoading && candidates.length === 0" class="flex items-center gap-2 px-3 py-4 text-sm text-muted-foreground">
            <Loader2 class="h-4 w-4 animate-spin" aria-hidden="true" />{{ t('chat.forward.loading') }}
          </p>
          <p v-else-if="loadFailed" class="px-3 py-4 text-sm text-destructive">{{ t('chat.forward.loadFailed') }}</p>
          <p v-else-if="candidates.length === 0" class="px-3 py-4 text-sm text-muted-foreground">{{ t('chat.forward.noContacts') }}</p>
          <label
            v-for="contact in candidates"
            :key="contact.id"
            :class="[
              'flex items-center gap-3 border-b border-border px-3 py-2 last:border-b-0',
              isPickable(contact) ? 'cursor-pointer hover:bg-muted/50' : 'cursor-not-allowed opacity-60',
            ]"
            :data-testid="`chat-forward-target-${contact.id}`"
          >
            <input
              type="checkbox"
              class="h-4 w-4 shrink-0 accent-emerald-600"
              :checked="isSelected(contact)"
              :disabled="!isPickable(contact)"
              @change="toggle(contact)"
            />
            <span class="min-w-0 flex-1">
              <span class="block truncate text-sm font-medium">{{ displayName(contact) }}</span>
              <span class="block truncate text-xs text-muted-foreground">{{ contact.phone_number }}</span>
            </span>
            <span
              :class="[
                'shrink-0 rounded-full px-2 py-0.5 text-[11px]',
                targetBlock(contact) === null
                  ? 'bg-emerald-500/15 text-emerald-600'
                  : 'bg-muted text-muted-foreground',
              ]"
            >
              {{ statusLabel(contact) }}
            </span>
          </label>
        </div>
      </div>

      <!-- Step 2: name every recipient before anything is sent (PDPA) -->
      <div v-else-if="step === 'confirm'" class="space-y-3">
        <ul class="space-y-1 text-sm" data-testid="chat-forward-recipients">
          <li v-for="contact in selected" :key="contact.id" class="truncate">
            <span class="font-medium">{{ displayName(contact) }}</span>
            <span class="text-muted-foreground"> · {{ contact.phone_number }}</span>
          </li>
        </ul>
        <p class="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-600 light:text-amber-700">
          <ShieldAlert class="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
          {{ t('chat.forward.pdpaNotice') }}
        </p>
      </div>

      <!-- Step 3: one result per recipient -->
      <div v-else-if="step === 'results'" class="space-y-2">
        <p class="text-sm text-muted-foreground" aria-live="polite">
          {{ t('chat.forward.summary', { sent: sentCount, total: results.length }) }}
        </p>
        <ul class="space-y-1" data-testid="chat-forward-results">
          <li
            v-for="result in results"
            :key="result.contact.id"
            class="flex items-start gap-2 text-sm"
            :data-state="result.state"
          >
            <Loader2 v-if="result.state === 'pending' || result.state === 'sending'" class="mt-0.5 h-4 w-4 shrink-0 animate-spin text-muted-foreground" aria-hidden="true" />
            <Clock v-else-if="result.state === 'queued' || result.state === 'unconfirmed' || result.state === 'uncertain'" class="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden="true" />
            <CheckCheck v-else-if="result.state === 'sent'" class="mt-0.5 h-4 w-4 shrink-0 text-emerald-600" aria-hidden="true" />
            <AlertCircle v-else class="mt-0.5 h-4 w-4 shrink-0 text-destructive" aria-hidden="true" />
            <span class="min-w-0 flex-1">
              <span class="font-medium">{{ displayName(result.contact) }}</span>
              <span class="text-muted-foreground"> · {{ resultLabel(result.state) }}</span>
              <span v-if="result.error" class="block text-xs text-destructive">{{ result.error }}</span>
            </span>
            <Button
              v-if="result.state === 'retryable'"
              variant="outline"
              size="sm"
              class="h-7 shrink-0 px-2 text-xs"
              :disabled="isSending"
              :data-testid="`chat-forward-retry-${result.contact.id}`"
              @click="retryTarget(result)"
            >
              {{ t('chat.forward.retry') }}
            </Button>
          </li>
        </ul>
        <p v-if="hasUncertain" class="text-xs text-muted-foreground" data-testid="chat-forward-uncertain">
          {{ t('chat.forward.uncertainNote') }}
        </p>
        <p v-if="hasUnconfirmed" class="text-xs text-muted-foreground" data-testid="chat-forward-unconfirmed">
          {{ t('chat.forward.unconfirmedNote') }}
        </p>
      </div>

      <DialogFooter class="gap-2">
        <template v-if="step === 'pick'">
          <Button variant="outline" @click="requestClose(false)">{{ t('common.cancel') }}</Button>
          <Button
            :disabled="selected.length === 0 || sourceBlock !== null"
            data-testid="chat-forward-next"
            @click="step = 'confirm'"
          >
            {{ t('chat.forward.next') }}
          </Button>
        </template>
        <template v-else-if="step === 'confirm'">
          <Button variant="outline" @click="step = 'pick'">{{ t('chat.forward.back') }}</Button>
          <Button data-testid="chat-forward-confirm" @click="forwardAll">
            {{ t('chat.forward.confirm', { count: selected.length }) }}
          </Button>
        </template>
        <Button v-else :disabled="isSending" data-testid="chat-forward-done" @click="requestClose(false)">
          {{ t('chat.forward.done') }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
