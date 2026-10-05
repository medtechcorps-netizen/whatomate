<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ShieldAlert } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { unwrapItemResponse } from '@/lib/api-utils'
import { contactsService } from '@/services/api'
import ContactIdentityReviewDialog from './ContactIdentityReviewDialog.vue'

// Workspace-level entry point to the protected staged identity-review queue.
// Held WhatsApp messages have no contact or conversation, so without this
// notice an empty workspace showed nothing at all. It reads only the existing
// protected list endpoint (contacts:write + contacts.identity_review:write);
// viewers without that authority never request it and see nothing.
const props = withDefaults(defineProps<{
  canView: boolean
  refreshIntervalMs?: number
}>(), {
  refreshIntervalMs: 60_000,
})

const total = ref(0)
// Held messages that no decision can resolve: their sender matches no unique
// contact, so they stay as read-only records until the account is onboarded
// again. Saying so keeps a count that cannot reach zero from looking stuck.
const readOnlyTotal = ref(0)
const dialogOpen = ref(false)
// Mounted on first use only: no hidden dialog per view, and it then stays
// mounted so its close transition and request cancellation run normally.
const dialogMounted = ref(false)
let controller: AbortController | null = null
let generation = 0
let timer: ReturnType<typeof setInterval> | null = null

async function refresh() {
  if (!props.canView) {
    total.value = 0
    readOnlyTotal.value = 0
    return
  }
  if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return
  const current = ++generation
  controller?.abort()
  const request = new AbortController()
  controller = request
  try {
    const response = await contactsService.listStagedIdentityReviews({ page: 1, limit: 1 }, request.signal)
    if (current !== generation) return
    const payload = unwrapItemResponse<{ total: number; read_only_total?: number }>(response)
    total.value = payload && Number.isSafeInteger(payload.total) && payload.total > 0 ? payload.total : 0
    const readOnly = payload?.read_only_total
    readOnlyTotal.value = typeof readOnly === 'number' && Number.isSafeInteger(readOnly) && readOnly > 0
      ? Math.min(readOnly, total.value)
      : 0
  } catch {
    // A failed count must never surface protected data or block the page;
    // the next refresh tries again.
  } finally {
    if (controller === request) controller = null
  }
}

function onVisibilityChange() {
  if (document.visibilityState === 'visible') void refresh()
}

function startTimer() {
  stopTimer()
  if (props.canView && props.refreshIntervalMs > 0) {
    timer = setInterval(() => { void refresh() }, props.refreshIntervalMs)
  }
}

function stopTimer() {
  if (timer) clearInterval(timer)
  timer = null
}

function openQueue() {
  dialogMounted.value = true
  dialogOpen.value = true
}

function onStagedTotal(value: number) {
  if (Number.isSafeInteger(value) && value >= 0) {
    total.value = value
    readOnlyTotal.value = Math.min(readOnlyTotal.value, value)
  }
}

watch(() => props.canView, () => {
  void refresh()
  startTimer()
})

watch(dialogOpen, open => {
  if (!open) void refresh()
})

onMounted(() => {
  void refresh()
  startTimer()
  document.addEventListener('visibilitychange', onVisibilityChange)
})

onBeforeUnmount(() => {
  generation += 1
  controller?.abort()
  stopTimer()
  document.removeEventListener('visibilitychange', onVisibilityChange)
})
</script>

<template>
  <div v-if="canView && total > 0" class="border-b border-amber-300/15 bg-amber-300/[0.06] px-3 py-2 light:border-amber-200 light:bg-amber-50">
    <div class="flex items-center gap-2" data-testid="staged-identity-review-notice">
      <ShieldAlert class="h-4 w-4 shrink-0 text-amber-300 light:text-amber-700" />
      <p class="min-w-0 flex-1 text-xs leading-5 text-amber-50/85 light:text-amber-900">
        <span class="font-semibold" data-testid="staged-identity-review-count">{{ total }}</span>
        {{ total === 1 ? 'WhatsApp message is' : 'WhatsApp messages are' }} held for identity review<template v-if="readOnlyTotal > 0">
          <span
            class="text-amber-50/60 light:text-amber-800/80"
            title="These held messages match no single contact, so no decision can resolve them. They stay readable here until the WhatsApp number is onboarded again."
            data-testid="staged-identity-review-read-only"
          >
            · {{ readOnlyTotal === total ? (total === 1 ? 'read-only for now' : 'all read-only for now') : `${readOnlyTotal} read-only for now` }}
          </span>
        </template>
      </p>
      <Button
        size="sm"
        variant="outline"
        class="h-7 shrink-0 px-2 text-xs"
        data-testid="staged-identity-review-open"
        @click="openQueue"
      >
        Review
      </Button>
    </div>
  </div>
  <!-- Outside the notice's v-if so an emptied queue never unmounts an open dialog. -->
  <ContactIdentityReviewDialog
    v-if="canView && dialogMounted"
    v-model:open="dialogOpen"
    :contact-id="null"
    :can-review="canView"
    :can-view-staged="canView"
    initial-section="staged"
    @staged-total="onStagedTotal"
  />
</template>
