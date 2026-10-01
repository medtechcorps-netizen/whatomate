<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Check, Loader2, ThumbsDown, Trophy } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Textarea } from '@/components/ui/textarea'
import { formatCurrencyMinorUnits } from '@/lib/currency'
import { LOST_REASONS, LOST_REASON_MAX_LENGTH, buildLostReason } from '@/lib/crmFlow'
import type { CRMLead } from '@/services/productSuite'

const props = withDefaults(
  defineProps<{
    open: boolean
    outcome: 'won' | 'lost'
    lead: CRMLead | null
    stageName?: string
    saving?: boolean
    canInvoice?: boolean
  }>(),
  {
    stageName: '',
    saving: false,
    canInvoice: false,
  },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  confirm: [payload: { reason?: string; createInvoice?: boolean }]
}>()

const selectedReason = ref('')
const note = ref('')
const createInvoice = ref(true)

watch(
  () => props.open,
  (isOpen) => {
    if (!isOpen) return
    selectedReason.value = ''
    note.value = ''
    createInvoice.value = true
  },
  { immediate: true },
)

const isWon = computed(() => props.outcome === 'won')
const leadTitle = computed(() => props.lead?.title?.trim() || 'This lead')
const formattedValue = computed(() => {
  const lead = props.lead
  if (!lead || !(lead.value_minor > 0)) return ''
  return formatCurrencyMinorUnits(lead.currency || 'MYR', lead.value_minor)
})
const targetStage = computed(() => props.stageName.trim() || (isWon.value ? 'Won' : 'Lost'))

const wonSummary = computed(
  () => `${leadTitle.value}${formattedValue.value ? ` (${formattedValue.value})` : ''} moves to ${targetStage.value}.`,
)

const canConfirm = computed(() => {
  if (props.saving || !props.lead) return false
  return isWon.value || Boolean(selectedReason.value)
})

/** Visible hint while "Mark as lost" is disabled only because no reason is chosen. */
const showReasonHint = computed(() => !isWon.value && !props.saving && Boolean(props.lead) && !selectedReason.value)

function setOpen(value: boolean) {
  if (!value && props.saving) return
  emit('update:open', value)
}

function chooseReason(reason: string) {
  if (props.saving) return
  selectedReason.value = reason
}

function confirm() {
  if (!canConfirm.value) return
  if (isWon.value) {
    emit('confirm', { createInvoice: props.canInvoice ? createInvoice.value : false })
    return
  }
  emit('confirm', { reason: buildLostReason(selectedReason.value, note.value) })
}

const chipBase =
  'inline-flex min-h-9 items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-rose-300 disabled:cursor-not-allowed disabled:opacity-60'
const chipIdle =
  'border-white/10 bg-white/[0.03] text-white/70 hover:border-rose-300/30 hover:text-white light:border-slate-200 light:bg-white light:text-slate-700 light:hover:text-slate-950'
const chipActive = 'border-rose-300/60 bg-rose-300/[0.12] text-rose-100 light:border-rose-500 light:bg-rose-50 light:text-rose-900'
</script>

<template>
  <Dialog :open="open" @update:open="setOpen">
    <DialogContent
      data-testid="lead-outcome-dialog"
      class="w-[calc(100vw-1.5rem)] max-w-md border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950"
    >
      <DialogHeader>
        <DialogTitle class="flex items-center gap-2">
          <Trophy v-if="isWon" class="h-4 w-4 text-emerald-300 light:text-emerald-600" aria-hidden="true" />
          <ThumbsDown v-else class="h-4 w-4 text-rose-300 light:text-rose-600" aria-hidden="true" />
          {{ isWon ? 'Mark as won?' : 'Mark as lost' }}
        </DialogTitle>
        <DialogDescription>
          <template v-if="isWon">{{ wonSummary }}</template>
          <template v-else>Choose why {{ leadTitle }} did not go ahead. It moves to {{ targetStage }}.</template>
        </DialogDescription>
      </DialogHeader>

      <form id="lead-outcome-form" class="space-y-4" @submit.prevent="confirm">
        <label
          v-if="isWon && canInvoice"
          class="flex cursor-pointer items-start gap-3 rounded-xl border border-white/10 bg-white/[0.02] p-3 text-sm light:border-slate-200 light:bg-slate-50"
        >
          <input
            v-model="createInvoice"
            type="checkbox"
            :disabled="saving"
            class="mt-0.5 h-4 w-4 shrink-0 accent-emerald-500"
          />
          <span>
            <span class="block font-medium">Create the invoice next</span>
            <span class="mt-0.5 block text-xs text-white/50 light:text-slate-600">
              Opens the invoice form for this customer after the lead is marked as won.
            </span>
          </span>
        </label>

        <template v-if="!isWon">
          <div class="space-y-1.5">
            <span id="lead-lost-reason-label" class="block text-xs font-medium text-white/60 light:text-slate-700">
              Reason
            </span>
            <div role="group" aria-labelledby="lead-lost-reason-label" class="flex flex-wrap gap-2">
              <button
                v-for="reason in LOST_REASONS"
                :key="reason"
                type="button"
                data-testid="lead-lost-reason"
                :aria-pressed="selectedReason === reason"
                :disabled="saving"
                :class="[chipBase, selectedReason === reason ? chipActive : chipIdle]"
                @click="chooseReason(reason)"
              >
                <Check v-if="selectedReason === reason" class="h-3.5 w-3.5" aria-hidden="true" />
                {{ reason }}
              </button>
            </div>
          </div>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">
              Add a note (optional)
            </span>
            <Textarea
              v-model="note"
              :rows="3"
              :disabled="saving"
              :maxlength="LOST_REASON_MAX_LENGTH - 40"
              placeholder="Anything the team should know"
              class="resize-y rounded-xl border-white/10 bg-black/20 light:border-slate-300 light:bg-white"
            />
          </label>
        </template>
      </form>

      <p
        v-if="showReasonHint"
        id="lead-lost-reason-hint"
        data-testid="lead-lost-reason-hint"
        class="text-xs text-white/55 light:text-slate-600"
        aria-live="polite"
      >
        Choose a reason
      </p>

      <DialogFooter class="gap-2">
        <Button variant="outline" :disabled="saving" @click="setOpen(false)">Cancel</Button>
        <Button
          type="submit"
          form="lead-outcome-form"
          data-testid="lead-outcome-confirm"
          :class="
            isWon ? 'gap-2 bg-emerald-500 text-black hover:bg-emerald-400' : 'gap-2 bg-rose-600 text-white hover:bg-rose-500'
          "
          :disabled="!canConfirm"
          :aria-describedby="showReasonHint ? 'lead-lost-reason-hint' : undefined"
        >
          <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
          {{ isWon ? 'Mark as won' : 'Mark as lost' }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
