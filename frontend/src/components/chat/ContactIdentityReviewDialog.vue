<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { AlertCircle, CheckCircle2, FileSearch, Loader2, ShieldAlert } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { ScrollArea } from '@/components/ui/scroll-area'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import {
  contactsService,
  effectiveAIIsAllowed,
  type ContactIdentityReviewDecisionRequest,
  type ContactIdentityReviewEffectiveState,
  type ContactIdentityReviewPreview,
  type StagedIdentityReviewDetail,
  type StagedIdentityReviewItem,
} from '@/services/api'

const props = defineProps<{
  open: boolean
  contactId: string | null
  contactLabel?: string
  effectiveState?: ContactIdentityReviewEffectiveState | null
  canReview: boolean
  canViewStaged: boolean
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  resolved: [state: ContactIdentityReviewEffectiveState]
}>()

type DialogSection = 'contact' | 'staged'

const section = ref<DialogSection>('contact')
const state = ref<ContactIdentityReviewEffectiveState | null>(null)
const preview = ref<ContactIdentityReviewPreview | null>(null)
const selectedTarget = ref('')
const loading = ref(false)
const deciding = ref(false)
const error = ref('')
const staged = ref<StagedIdentityReviewItem[]>([])
const stagedPage = ref(1)
const stagedTotal = ref(0)
const stagedDetail = ref<StagedIdentityReviewDetail | null>(null)
const stagedLoading = ref(false)
const mediaLoading = ref(false)
const mediaURL = ref('')
let requestGeneration = 0
let requestController: AbortController | null = null
let mediaRequestController: AbortController | null = null
let pendingDecision: ContactIdentityReviewDecisionRequest | null = null
let decisionInFlightToken: symbol | null = null
const stagedPageSize = 100

interface DecisionAttemptContext {
  token: symbol
  generation: number
  contactID: string
  targetContactID: string
  preview: ContactIdentityReviewPreview
}

const blocked = computed(() => !effectiveAIIsAllowed(state.value))
const previewIsComplete = computed(() => {
  const value = preview.value
  const snapshot = value?.snapshot
  if (!value || !snapshot || snapshot.disposition !== 'open' || !snapshot.supported) return false
  if (snapshot.protocol_version !== 1) return false
  if (!Number.isSafeInteger(snapshot.version) || snapshot.version < 1) return false
  if (!Number.isSafeInteger(snapshot.principal_generation) || snapshot.principal_generation < 1) return false
  if (!Number.isSafeInteger(snapshot.member_count) || snapshot.member_count < 1) return false
  if (!/^[a-f0-9]{64}$/.test(snapshot.member_digest)) return false
  if (!Array.isArray(snapshot.candidates) || !Array.isArray(value.union_candidates) || !Array.isArray(value.open_generations)) return false
  if (snapshot.candidates.length !== snapshot.member_count) return false
  if (snapshot.candidates.some(candidate => (
    !candidate.contact_id
    || !Number.isSafeInteger(candidate.selector_reasons)
    || candidate.selector_reasons < 1
    || candidate.selector_reasons > 7
  ))) return false
  const currentIDs = new Set(snapshot.candidates.map(candidate => candidate.contact_id))
  if (currentIDs.size !== snapshot.member_count) return false
  if (value.union_candidates.length < snapshot.member_count) return false
  if (value.union_candidates.some(candidate => (
    !candidate.contact_id
    || !Number.isSafeInteger(candidate.selector_reasons)
    || candidate.selector_reasons < 1
    || candidate.selector_reasons > 7
  ))) return false
  const unionIDs = new Set(value.union_candidates.map(candidate => candidate.contact_id))
  if (unionIDs.size !== value.union_candidates.length) return false
  if (![...currentIDs].every(id => unionIDs.has(id))) return false
  const unionReasons = new Map(value.union_candidates.map(candidate => [candidate.contact_id, candidate.selector_reasons]))
  if (snapshot.candidates.some(candidate => (
    ((unionReasons.get(candidate.contact_id) ?? 0) & candidate.selector_reasons) !== candidate.selector_reasons
  ))) return false
  if (!/^[a-f0-9]{64}$/.test(value.chain_digest)) return false
  if (value.open_generations.length < 1) return false
  if (value.open_generations.some(generation => !Number.isSafeInteger(generation) || generation < 1)) return false
  const uniqueGenerations = new Set(value.open_generations)
  if (uniqueGenerations.size !== value.open_generations.length) return false
  return uniqueGenerations.has(snapshot.principal_generation)
})
const canDecide = computed(() => (
  props.canReview
  && previewIsComplete.value
  && preview.value?.snapshot.candidates.some(candidate => candidate.contact_id === selectedTarget.value) === true
  && !deciding.value
))
const stagedPageCount = computed(() => Math.max(1, Math.ceil(stagedTotal.value / stagedPageSize)))
const stagedRangeStart = computed(() => (
  stagedTotal.value === 0 ? 0 : ((stagedPage.value - 1) * stagedPageSize) + 1
))
const stagedRangeEnd = computed(() => Math.min(
  stagedTotal.value,
  ((stagedPage.value - 1) * stagedPageSize) + staged.value.length,
))

function closeMediaURL() {
  mediaRequestController?.abort()
  mediaRequestController = null
  if (mediaURL.value) URL.revokeObjectURL(mediaURL.value)
  mediaURL.value = ''
  mediaLoading.value = false
}

function resetProtectedState() {
  decisionInFlightToken = null
  deciding.value = false
  preview.value = null
  selectedTarget.value = ''
  staged.value = []
  stagedPage.value = 1
  stagedTotal.value = 0
  stagedDetail.value = null
  pendingDecision = null
  closeMediaURL()
}

function safeFallbackState(): ContactIdentityReviewEffectiveState {
  return {
    known: false,
    ai_allowed: false,
    blocked: true,
    open_hold_count: 0,
    reason: 'identity_review_state_unavailable',
  }
}

async function sha256Hex(value: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))
  return [...new Uint8Array(digest)]
    .map(byte => byte.toString(16).padStart(2, '0'))
    .join('')
}

function decisionContextIsCurrent(context: DecisionAttemptContext): boolean {
  return (
    decisionInFlightToken === context.token
    && deciding.value
    && props.open
    && section.value === 'contact'
    && requestGeneration === context.generation
    && props.contactId === context.contactID
    && selectedTarget.value === context.targetContactID
    && preview.value === context.preview
    && previewIsComplete.value
    && context.preview.snapshot.candidates.some(
      candidate => candidate.contact_id === context.targetContactID,
    )
  )
}

function pendingDecisionMatches(
  request: ContactIdentityReviewDecisionRequest,
  context: DecisionAttemptContext,
): boolean {
  return (
    request.target_contact_id === context.targetContactID
    && request.hold_id === context.preview.snapshot.hold_id
    && request.expected_version === context.preview.snapshot.version
    && request.expected_member_digest === context.preview.snapshot.member_digest
    && request.expected_chain_digest === context.preview.chain_digest
  )
}

async function decisionRequestFor(
  context: DecisionAttemptContext,
): Promise<ContactIdentityReviewDecisionRequest | null> {
  const value = context.preview
  if (!decisionContextIsCurrent(context)) return null
  if (
    pendingDecision
    && pendingDecisionMatches(pendingDecision, context)
  ) return pendingDecision

  const requestID = crypto.randomUUID()
  const identity = [
    'whatsapp_identity_review_decision_v1',
    value.snapshot.hold_id,
    context.targetContactID,
    String(value.snapshot.version),
    value.snapshot.member_digest,
    value.chain_digest,
    requestID,
  ].join('\n')
  const request: ContactIdentityReviewDecisionRequest = {
    request_id: requestID,
    hold_id: value.snapshot.hold_id,
    target_contact_id: context.targetContactID,
    expected_version: value.snapshot.version,
    expected_member_digest: value.snapshot.member_digest,
    expected_chain_digest: value.chain_digest,
    request_digest: await sha256Hex(identity),
  }
  if (!decisionContextIsCurrent(context)) return null
  pendingDecision = request
  return request
}

async function loadContactReview() {
  const contactID = props.contactId
  const generation = ++requestGeneration
  requestController?.abort()
  const controller = new AbortController()
  requestController = controller
  resetProtectedState()
  error.value = ''
  loading.value = true
  state.value = props.effectiveState ?? safeFallbackState()
  if (!contactID) {
    loading.value = false
    return
  }
  try {
    const response = await contactsService.getIdentityReviewState(contactID, controller.signal)
    if (generation !== requestGeneration || props.contactId !== contactID) return
    state.value = unwrapItemResponse<ContactIdentityReviewEffectiveState>(response)

    if (props.canReview && !effectiveAIIsAllowed(state.value)) {
      const previewResponse = await contactsService.previewIdentityReview(contactID, controller.signal)
      if (generation !== requestGeneration || props.contactId !== contactID) return
      const nextPreview = unwrapItemResponse<ContactIdentityReviewPreview>(previewResponse)
      preview.value = nextPreview
      if (nextPreview.snapshot.candidates.length === 1) {
        selectedTarget.value = nextPreview.snapshot.candidates[0].contact_id
      }
      if (!previewIsComplete.value) {
        error.value = 'The complete current candidate set could not be verified. No decision is allowed.'
      }
    }
  } catch (requestError) {
    if (generation !== requestGeneration || controller.signal.aborted) return
    state.value = safeFallbackState()
    error.value = getErrorMessage(requestError, 'Identity review state is unavailable. AI remains blocked.')
  } finally {
    if (generation === requestGeneration) {
      loading.value = false
      if (requestController === controller) requestController = null
    }
  }
}

async function decide() {
  const contactID = props.contactId
  const targetContactID = selectedTarget.value
  const currentPreview = preview.value
  if (!contactID || !currentPreview || !canDecide.value || decisionInFlightToken) return

  const context: DecisionAttemptContext = {
    token: Symbol('identity-review-decision'),
    generation: requestGeneration,
    contactID,
    targetContactID,
    preview: currentPreview,
  }
  decisionInFlightToken = context.token
  deciding.value = true
  error.value = ''
  try {
    const request = await decisionRequestFor(context)
    if (!request || !decisionContextIsCurrent(context)) return
    await contactsService.decideIdentityReview(contactID, request)
    if (!decisionContextIsCurrent(context)) return
    pendingDecision = null
    decisionInFlightToken = null
    deciding.value = false
    await loadContactReview()
    if (state.value) emit('resolved', state.value)
  } catch (requestError) {
    if (decisionContextIsCurrent(context)) {
      // Retain the exact request ID/body so an ambiguous retry is idempotent.
      error.value = getErrorMessage(requestError, 'The review decision was not confirmed. Retry the same decision.')
    }
  } finally {
    if (decisionInFlightToken === context.token) {
      decisionInFlightToken = null
      deciding.value = false
    }
  }
}

async function loadStagedQueue(page = 1) {
  if (!props.canViewStaged) return
  const requestedPage = Number.isSafeInteger(page) && page > 0 ? page : 1
  section.value = 'staged'
  const generation = ++requestGeneration
  requestController?.abort()
  const controller = new AbortController()
  requestController = controller
  stagedLoading.value = true
  stagedDetail.value = null
  error.value = ''
  closeMediaURL()
  try {
    const response = await contactsService.listStagedIdentityReviews(
      { page: requestedPage, limit: stagedPageSize },
      controller.signal,
    )
    if (generation !== requestGeneration || section.value !== 'staged') return
    const payload = unwrapItemResponse<{
      reviews: StagedIdentityReviewItem[]
      total: number
    }>(response)
    if (
      !payload
      || !Array.isArray(payload.reviews)
      || !Number.isSafeInteger(payload.total)
      || payload.total < 0
    ) {
      throw new Error('The protected staged queue returned invalid pagination data.')
    }
    const pageCount = Math.max(1, Math.ceil(payload.total / stagedPageSize))
    if (requestedPage > pageCount) {
      await loadStagedQueue(pageCount)
      return
    }
    staged.value = payload.reviews
    stagedPage.value = requestedPage
    stagedTotal.value = payload.total
  } catch (requestError) {
    if (generation === requestGeneration && !controller.signal.aborted) {
      error.value = getErrorMessage(requestError, 'The protected staged queue could not be loaded.')
    }
  } finally {
    if (generation === requestGeneration) {
      stagedLoading.value = false
      if (requestController === controller) requestController = null
    }
  }
}

async function loadStagedDetail(item: StagedIdentityReviewItem) {
  if (!props.canViewStaged) return
  const generation = ++requestGeneration
  requestController?.abort()
  const controller = new AbortController()
  requestController = controller
  stagedLoading.value = true
  stagedDetail.value = null
  error.value = ''
  closeMediaURL()
  try {
    const response = await contactsService.getStagedIdentityReview(item.id, controller.signal)
    if (generation !== requestGeneration || section.value !== 'staged') return
    stagedDetail.value = unwrapItemResponse<StagedIdentityReviewDetail>(response)
  } catch (requestError) {
    if (generation === requestGeneration && !controller.signal.aborted) {
      error.value = getErrorMessage(requestError, 'The staged review could not be loaded.')
    }
  } finally {
    if (generation === requestGeneration) {
      stagedLoading.value = false
      if (requestController === controller) requestController = null
    }
  }
}

async function loadStagedMedia() {
  const detail = stagedDetail.value
  if (!props.canViewStaged || !detail?.media_available || !detail.revision || mediaLoading.value) return
  const generation = requestGeneration
  mediaRequestController?.abort()
  const controller = new AbortController()
  mediaRequestController = controller
  mediaLoading.value = true
  error.value = ''
  try {
    const response = await contactsService.getStagedIdentityReviewMedia(
      detail.id,
      detail.revision,
      controller.signal,
    )
    if (generation !== requestGeneration || stagedDetail.value?.id !== detail.id || controller.signal.aborted) return
    if (mediaURL.value) URL.revokeObjectURL(mediaURL.value)
    mediaURL.value = URL.createObjectURL(response.data)
  } catch (requestError) {
    if (generation === requestGeneration && !controller.signal.aborted) {
      error.value = getErrorMessage(requestError, 'Protected staged media could not be loaded.')
    }
  } finally {
    if (mediaRequestController === controller) {
      mediaRequestController = null
      mediaLoading.value = false
    }
  }
}

function showContactReview() {
  section.value = 'contact'
  void loadContactReview()
}

watch(
  () => [props.open, props.contactId] as const,
  ([open]) => {
    if (open) {
      section.value = 'contact'
      void loadContactReview()
    } else {
      requestGeneration += 1
      requestController?.abort()
      requestController = null
      resetProtectedState()
    }
  },
  { immediate: true },
)

watch(
  () => props.effectiveState,
  value => {
    if (value && props.open && section.value === 'contact') state.value = value
  },
)

onBeforeUnmount(() => {
  requestGeneration += 1
  requestController?.abort()
  closeMediaURL()
})
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent class="w-[calc(100vw-1.5rem)] max-w-2xl border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950">
      <DialogHeader>
        <div class="flex items-start justify-between gap-4 pr-8">
          <div>
            <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-amber-300 light:text-amber-700">
              Identity safety
            </p>
            <DialogTitle class="mt-1">Review automated reply routing</DialogTitle>
            <DialogDescription class="mt-1 text-white/50 light:text-slate-600">
              {{ contactLabel || 'Selected contact' }} · decisions affect future messages only.
            </DialogDescription>
          </div>
          <Badge
            :class="blocked ? 'bg-amber-400/15 text-amber-200 light:bg-amber-100 light:text-amber-800' : 'bg-emerald-400/15 text-emerald-200 light:bg-emerald-100 light:text-emerald-800'"
            data-testid="identity-review-effective-state"
          >
            {{ blocked ? 'AI blocked' : 'AI allowed' }}
          </Badge>
        </div>
      </DialogHeader>

      <div v-if="canReview" class="flex gap-2 border-b border-white/10 pb-3 light:border-slate-200">
        <Button size="sm" :variant="section === 'contact' ? 'default' : 'outline'" @click="showContactReview">
          Contact review
        </Button>
        <Button v-if="canViewStaged" size="sm" :variant="section === 'staged' ? 'default' : 'outline'" @click="loadStagedQueue(1)">
          Protected staged queue
        </Button>
      </div>

      <div v-if="error" role="alert" class="rounded-xl border border-rose-400/20 bg-rose-400/[0.07] p-3 text-sm text-rose-100 light:border-rose-200 light:bg-rose-50 light:text-rose-800">
        {{ error }}
      </div>

      <div v-if="section === 'contact'" class="space-y-4">
        <div v-if="loading" class="flex min-h-32 items-center justify-center text-white/45 light:text-slate-600">
          <Loader2 class="mr-2 h-4 w-4 animate-spin" /> Checking durable state…
        </div>
        <template v-else>
          <div class="rounded-2xl border border-white/10 bg-white/[0.025] p-4 light:border-slate-200 light:bg-slate-50">
            <div class="flex items-start gap-3">
              <ShieldAlert v-if="blocked" class="mt-0.5 h-5 w-5 shrink-0 text-amber-300 light:text-amber-700" />
              <CheckCircle2 v-else class="mt-0.5 h-5 w-5 shrink-0 text-emerald-300 light:text-emerald-700" />
              <div>
                <p class="text-sm font-semibold">
                  {{ blocked ? 'Automated replies remain blocked' : 'Identity routing is clear' }}
                </p>
                <p class="mt-1 text-xs leading-5 text-white/45 light:text-slate-600">
                  {{ state?.reason || 'identity_review_state_unavailable' }}
                  <template v-if="state?.latest_generation"> · generation {{ state.latest_generation }}</template>
                </p>
              </div>
            </div>
          </div>

          <div v-if="blocked && !canReview" class="rounded-xl border border-amber-300/15 bg-amber-300/[0.05] p-3 text-xs leading-5 text-amber-50/75 light:border-amber-200 light:bg-amber-50 light:text-amber-900">
            A reviewer with identity-review permission must inspect the complete candidate set. No other contact details are visible here.
          </div>

          <div v-else-if="preview" class="space-y-3">
            <div class="flex items-center justify-between gap-3">
              <div>
                <p class="text-sm font-semibold">Choose the destination for future messages</p>
                <p class="mt-1 text-xs text-white/40 light:text-slate-600">
                  Complete reviewed union: {{ preview.union_candidates.length }} candidates · latest set {{ preview.snapshot.member_count }}
                </p>
              </div>
              <Badge variant="outline">v{{ preview.snapshot.version }}</Badge>
            </div>

            <fieldset :disabled="!previewIsComplete || deciding" class="space-y-2">
              <label
                v-for="(candidate, candidateIndex) in preview.union_candidates"
                :key="candidate.contact_id"
                data-testid="identity-review-candidate"
                class="flex min-h-14 cursor-pointer items-center gap-3 rounded-xl border border-white/10 bg-white/[0.025] px-3 py-2.5 transition hover:bg-white/[0.05] light:border-slate-200 light:bg-white light:hover:bg-slate-50"
              >
                <input
                  v-model="selectedTarget"
                  type="radio"
                  name="identity-review-target"
                  :value="candidate.contact_id"
                  :disabled="!preview.snapshot.candidates.some(current => current.contact_id === candidate.contact_id)"
                  class="h-4 w-4 accent-amber-400"
                />
                <span class="min-w-0 flex-1">
                  <span class="block truncate text-sm font-medium">Candidate {{ candidateIndex + 1 }}</span>
                  <span class="block truncate font-mono text-[10px] text-white/40 light:text-slate-600">{{ candidate.contact_id }}</span>
                </span>
                <span class="font-mono text-[10px] text-white/30 light:text-slate-500">reason {{ candidate.selector_reasons }}</span>
              </label>
            </fieldset>

            <div class="rounded-xl border border-sky-300/15 bg-sky-300/[0.04] p-3 text-xs leading-5 text-sky-50/70 light:border-sky-200 light:bg-sky-50 light:text-sky-900">
              Existing conversations, history, staged messages, and suppression records are not moved. The selected contact applies only to the first durable admission after this decision.
            </div>
          </div>
        </template>
      </div>

      <div v-else class="min-h-56">
        <div v-if="stagedLoading" class="flex min-h-40 items-center justify-center text-white/45 light:text-slate-600">
          <Loader2 class="mr-2 h-4 w-4 animate-spin" /> Loading protected records…
        </div>
        <div v-else-if="stagedDetail" class="space-y-3">
          <Button variant="ghost" size="sm" @click="loadStagedQueue(stagedPage)">← Back to queue</Button>
          <div class="rounded-2xl border border-white/10 bg-white/[0.025] p-4 light:border-slate-200 light:bg-slate-50">
            <div class="flex items-center justify-between gap-3">
              <div>
                <p class="text-sm font-semibold">Staged {{ stagedDetail.message_type }}</p>
                <p class="mt-1 text-xs text-white/40 light:text-slate-600">Revision {{ stagedDetail.revision || 'not applicable' }} · {{ stagedDetail.status }}</p>
              </div>
              <FileSearch class="h-5 w-5 text-amber-300 light:text-amber-700" />
            </div>
            <pre class="mt-4 max-h-44 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-black/20 p-3 text-xs text-white/60 light:bg-slate-100 light:text-slate-700">{{ stagedDetail.content }}</pre>
            <Button v-if="stagedDetail.media_available" class="mt-3" variant="outline" size="sm" :disabled="mediaLoading" @click="loadStagedMedia">
              <Loader2 v-if="mediaLoading" class="mr-2 h-3.5 w-3.5 animate-spin" />
              Load protected media
            </Button>
            <a v-if="mediaURL" :href="mediaURL" download class="ml-3 text-xs font-medium text-sky-300 underline light:text-sky-700">Download media</a>
          </div>
        </div>
        <ScrollArea v-else class="max-h-[22rem]">
          <button
            v-for="item in staged"
            :key="item.id"
            type="button"
            class="mb-2 flex w-full items-center justify-between gap-3 rounded-xl border border-white/10 bg-white/[0.025] p-3 text-left transition hover:bg-white/[0.05] light:border-slate-200 light:bg-white light:hover:bg-slate-50"
            @click="loadStagedDetail(item)"
          >
            <span>
              <span class="block text-sm font-medium">{{ item.message_type }}</span>
              <span class="mt-1 block text-xs text-white/40 light:text-slate-600">{{ item.received_at }} · revision {{ item.revision || 'not applicable' }}</span>
            </span>
            <Badge variant="outline">{{ item.status }}</Badge>
          </button>
          <div v-if="staged.length === 0" class="py-12 text-center text-sm text-white/40 light:text-slate-600">No staged reviews.</div>
          <div class="mt-3 flex items-center justify-between gap-3 border-t border-white/10 pt-3 text-xs text-white/45 light:border-slate-200 light:text-slate-600">
            <span data-testid="staged-pagination-status">
              Showing {{ stagedRangeStart }}–{{ stagedRangeEnd }} of {{ stagedTotal }} · Page {{ stagedPage }} of {{ stagedPageCount }}
            </span>
            <span class="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                data-testid="staged-previous-page"
                :disabled="stagedPage <= 1"
                @click="loadStagedQueue(stagedPage - 1)"
              >
                Previous
              </Button>
              <Button
                variant="outline"
                size="sm"
                data-testid="staged-next-page"
                :disabled="stagedPage >= stagedPageCount"
                @click="loadStagedQueue(stagedPage + 1)"
              >
                Next
              </Button>
            </span>
          </div>
        </ScrollArea>
      </div>

      <DialogFooter class="gap-2 sm:justify-between">
        <div class="flex items-center gap-2 text-[11px] text-white/35 light:text-slate-500">
          <AlertCircle class="h-3.5 w-3.5" /> State is re-read before and after every decision.
        </div>
        <div class="flex gap-2">
          <Button variant="outline" @click="emit('update:open', false)">Close</Button>
          <Button v-if="section === 'contact' && preview" data-testid="identity-review-decision" :disabled="!canDecide" class="bg-amber-400 text-black hover:bg-amber-300" @click="decide">
            <Loader2 v-if="deciding" class="mr-2 h-4 w-4 animate-spin" />
            Confirm future routing
          </Button>
        </div>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
