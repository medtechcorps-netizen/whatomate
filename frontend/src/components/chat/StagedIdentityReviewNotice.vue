<script setup lang="ts">
import { ref, watch } from 'vue'
import { ShieldAlert } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { unwrapItemResponse } from '@/lib/api-utils'
import { parseStagedReviewList } from '@/lib/stagedIdentityReview'
import { useIdentityReviewObservation, type IdentityReviewScope } from '@/composables/useIdentityReviewObservation'
import { contactsService, type StagedIdentityReviewList } from '@/services/api'
import ContactIdentityReviewDialog from './ContactIdentityReviewDialog.vue'

const props = withDefaults(defineProps<{
  canView: boolean
  refreshIntervalMs?: number
  invalidation?: IdentityReviewScope | null
}>(), { refreshIntervalMs: 60_000, invalidation: null })

const aggregate = ref<StagedIdentityReviewList | null>(null)
const status = ref<'loading' | 'ready' | 'unavailable' | 'forbidden'>('loading')
const dialogOpen = ref(false)
const dialogMounted = ref(false)
const observation = useIdentityReviewObservation({
  enabled: () => props.canView,
  requireReview: true,
  query: () => props.canView,
  intervalMs: () => props.refreshIntervalMs,
  clear: () => {
    aggregate.value = null
    status.value = 'loading'
    dialogOpen.value = false
    dialogMounted.value = false
  },
  refresh,
})

async function refresh() {
  const scope = observation.capture()
  if (!scope) return
  const request = observation.controller()
  aggregate.value = null
  status.value = 'loading'
  try {
    const response = await contactsService.listStagedIdentityReviews({ page: 1, limit: 1 }, { organizationId: scope.organizationId, signal: request.signal })
    if (!observation.current(scope) || request.signal.aborted) return
    aggregate.value = parseStagedReviewList(unwrapItemResponse(response), 1, 1)
    status.value = 'ready'
  } catch (error) {
    if (!observation.current(scope) || request.signal.aborted) return
    const code = (error as { response?: { status?: number } })?.response?.status
    status.value = code === 403 ? 'forbidden' : 'unavailable'
    if (code === 403) observation.pause()
  } finally { observation.release(request) }
}

function openQueue() {
  if (!observation.capture() || status.value === 'forbidden') return
  dialogMounted.value = true
  dialogOpen.value = true
}

function invalidate(scope: IdentityReviewScope) {
  if (observation.belongs(scope)) void observation.refresh(true)
}

watch(() => props.invalidation, value => { if (value) invalidate(value) }, { flush: 'sync' })
watch(dialogOpen, open => { if (!open && props.canView) void observation.refresh(true) })
</script>

<template>
  <div v-if="canView && observation.canReview() && (status !== 'ready' || (aggregate?.total ?? 0) > 0)" class="border-b border-amber-300/15 bg-amber-300/[0.06] px-3 py-2 light:border-amber-200 light:bg-amber-50">
    <div class="flex flex-wrap items-center gap-2" data-testid="staged-identity-review-notice">
      <ShieldAlert class="h-4 w-4 shrink-0 text-amber-300 light:text-amber-700" />
      <p class="min-w-0 flex-1 text-xs leading-5 text-amber-50/85 light:text-amber-900" role="status" aria-live="polite">
        <template v-if="status === 'ready' && aggregate">
          <span class="font-semibold" data-testid="staged-identity-review-count">{{ aggregate.actionable_chain_total }}</span>
          {{ aggregate.actionable_chain_total === 1 ? 'review available' : 'reviews available' }}
          <span data-testid="staged-identity-review-held-count"> · {{ aggregate.total }} held {{ aggregate.total === 1 ? 'message' : 'messages' }}</span>
          <span v-if="aggregate.actionable_chain_total === 0" data-testid="staged-identity-review-read-only"> · No reviews are available right now. Held messages remain readable.</span>
        </template>
        <template v-else-if="status === 'loading'">Checking review availability…</template>
        <template v-else-if="status === 'forbidden'">Review access is not currently available.</template>
        <template v-else>Review availability could not be checked.</template>
      </p>
      <Button v-if="status === 'unavailable' || status === 'forbidden'" size="sm" variant="outline" class="h-7 shrink-0 px-2 text-xs" data-testid="staged-identity-review-retry" @click="observation.refresh(true)">Retry</Button>
      <Button v-if="status !== 'forbidden'" size="sm" variant="outline" class="h-7 shrink-0 px-2 text-xs" data-testid="staged-identity-review-open" @click="openQueue">View held messages</Button>
    </div>
  </div>
  <ContactIdentityReviewDialog v-if="canView && observation.canReview() && dialogMounted" v-model:open="dialogOpen" :contact-id="null" :can-review="canView" :can-view-staged="canView" initial-section="staged" @queue-invalidated="invalidate" />
</template>
