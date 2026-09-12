<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Archive, Loader2, Save } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { getErrorMessage } from '@/lib/api-utils'
import { useAuthStore } from '@/stores/auth'
import { crmService, type CRMLead } from '@/services/productSuite'

const props = defineProps<{
  modelValue: boolean
  lead: CRMLead | null
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  saved: [action?: 'updated' | 'archived' | 'reopened']
}>()

const saving = ref(false)
const authStore = useAuthStore()
const canWriteLeads = computed(() => authStore.hasPermission('crm.leads', 'write'))
const transitioning = ref(false)
const transitionOpen = ref(false)
const transitionReason = ref('')
const transitionIdempotencyKey = ref('')
const errorMessage = ref('')
const draft = ref({
  title: '',
  value: '0.00',
  currency: 'MYR',
  next_action_at: '',
  expected_close_date: '',
})

const open = computed({
  get: () => props.modelValue,
  set: (value) => {
    if (!saving.value && !transitioning.value) emit('update:modelValue', value)
  },
})

function setTransitionOpen(value: boolean) {
  if (!transitioning.value) transitionOpen.value = value
}
const isArchived = computed(() => props.lead?.status === 'archived')

function toLocalDateTime(value?: string) {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 16)
}

function toCalendarDate(value?: string) {
  if (!value) return ''
  const match = /^(\d{4}-\d{2}-\d{2})/.exec(value)
  return match?.[1] ?? ''
}

function resetDraft() {
  if (!props.lead) return
  draft.value = {
    title: props.lead.title,
    value: (props.lead.value_minor / 100).toFixed(2),
    currency: props.lead.currency || 'MYR',
    next_action_at: toLocalDateTime(props.lead.next_action_at),
    expected_close_date: toCalendarDate(props.lead.expected_close_date),
  }
  errorMessage.value = ''
  transitionOpen.value = false
  transitionReason.value = ''
  transitionIdempotencyKey.value = ''
}

watch([() => props.modelValue, () => props.lead], ([isOpen]) => {
  if (isOpen) resetDraft()
})

async function save() {
  if (
    !props.lead ||
    isArchived.value ||
    !canWriteLeads.value ||
    saving.value ||
    transitioning.value ||
    !draft.value.title.trim()
  )
    return
  const valueMinor = Math.round(Number(draft.value.value) * 100)
  if (!Number.isFinite(valueMinor) || valueMinor < 0) {
    errorMessage.value = 'Lead value must be zero or greater.'
    return
  }

  saving.value = true
  errorMessage.value = ''
  try {
    await crmService.updateLead(props.lead.id, {
      version: props.lead.version,
      title: draft.value.title.trim(),
      value_minor: valueMinor,
      currency: draft.value.currency,
      next_action_at: draft.value.next_action_at ? new Date(draft.value.next_action_at).toISOString() : undefined,
      clear_next_action_at: !draft.value.next_action_at,
      expected_close_date: draft.value.expected_close_date
        ? `${draft.value.expected_close_date}T00:00:00.000Z`
        : undefined,
      clear_expected_close_date: !draft.value.expected_close_date,
    })
    emit('saved', 'updated')
    emit('update:modelValue', false)
  } catch (error) {
    errorMessage.value = getErrorMessage(error, 'Lead could not be saved')
  } finally {
    saving.value = false
  }
}

function requestTransition() {
  if (!isArchived.value || !canWriteLeads.value || saving.value || transitioning.value) return
  errorMessage.value = ''
  transitionReason.value = ''
  transitionIdempotencyKey.value = crypto.randomUUID()
  transitionOpen.value = true
}

async function transitionLead() {
  if (!props.lead || !isArchived.value || !canWriteLeads.value || saving.value || transitioning.value) return
  const lead = props.lead
  transitioning.value = true
  errorMessage.value = ''
  const payload = {
    version: lead.version,
    reason: transitionReason.value.trim() || undefined,
    idempotency_key: transitionIdempotencyKey.value || crypto.randomUUID(),
    metadata: { source: 'crm_pipeline' },
  }
  try {
    await crmService.reopenLead(lead.id, payload)
    transitionOpen.value = false
    emit('saved', 'reopened')
    emit('update:modelValue', false)
  } catch (error) {
    errorMessage.value = getErrorMessage(error, 'Lead could not be reopened')
  } finally {
    transitioning.value = false
  }
}
</script>

<template>
  <Dialog v-model:open="open">
    <DialogContent
      class="w-[calc(100vw-1.5rem)] max-w-xl border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950"
    >
      <DialogHeader>
        <DialogTitle>{{ isArchived ? 'Review archived lead' : 'Edit lead' }}</DialogTitle>
        <DialogDescription>
          {{
            isArchived
              ? 'Archived leads stay in the audit trail. Reopen this lead before changing its commercial details.'
              : 'Update commercial details without changing the customer or losing pipeline history.'
          }}
        </DialogDescription>
      </DialogHeader>

      <form v-if="lead" id="lead-edit-form" class="grid gap-4 sm:grid-cols-2" @submit.prevent="save">
        <div
          v-if="errorMessage"
          role="alert"
          class="flex items-start gap-2 rounded-xl border border-rose-300/20 bg-rose-300/[0.07] p-3 text-xs leading-5 text-rose-100 light:text-rose-800 sm:col-span-2"
        >
          <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" />
          <span>{{ errorMessage }}</span>
        </div>

        <fieldset :disabled="isArchived || !canWriteLeads || saving || transitioning" class="contents">
          <label class="block sm:col-span-2">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Lead title</span>
            <input
              v-model="draft.title"
              required
              maxlength="255"
              autofocus
              class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
            />
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Value</span>
            <input
              v-model="draft.value"
              type="number"
              min="0"
              step="0.01"
              required
              class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
            />
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Currency</span>
            <select
              v-model="draft.currency"
              class="h-11 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
            >
              <option value="MYR">MYR</option>
              <option value="SGD">SGD</option>
              <option value="USD">USD</option>
            </select>
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Next action</span>
            <input
              v-model="draft.next_action_at"
              type="datetime-local"
              class="h-11 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-xs outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
            />
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Expected close</span>
            <input
              v-model="draft.expected_close_date"
              type="date"
              class="h-11 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-xs outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
            />
          </label>
        </fieldset>

        <div
          v-if="isArchived"
          class="rounded-xl border border-emerald-300/20 bg-emerald-300/[0.04] p-3 light:border-emerald-300 light:bg-emerald-50 sm:col-span-2"
        >
          <div class="flex items-start justify-between gap-4">
            <div>
              <p
                id="lead-archive-status"
                class="flex items-center gap-2 text-xs font-semibold text-white/65 light:text-slate-700"
              >
                <Archive class="h-3.5 w-3.5" />
                Reopen lead
              </p>
              <p class="mt-1 text-[11px] leading-5 text-white/35 light:text-slate-500">
                Restore the status implied by its current stage while preserving the full lead history.
              </p>
            </div>
            <Button
              type="button"
              size="sm"
              variant="outline"
              :disabled="saving || transitioning || !canWriteLeads"
              aria-describedby="lead-archive-status"
              @click="requestTransition"
            >
              Reopen
            </Button>
          </div>
        </div>
      </form>

      <DialogFooter>
        <Button variant="outline" :disabled="saving || transitioning" @click="open = false">{{
          isArchived ? 'Close' : 'Cancel'
        }}</Button>
        <Button
          v-if="!isArchived && canWriteLeads"
          type="submit"
          form="lead-edit-form"
          class="gap-2 bg-cyan-400 text-black hover:bg-cyan-300"
          :disabled="saving || transitioning || !draft.title.trim()"
        >
          <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
          <Save v-else class="h-4 w-4" />
          Save changes
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <AlertDialog :open="transitionOpen" @update:open="setTransitionOpen">
    <AlertDialogContent>
      <AlertDialogHeader>
        <AlertDialogTitle>Reopen this lead?</AlertDialogTitle>
        <AlertDialogDescription>
          The lead will return to the status defined by its current stage. Its history and previous outcome dates stay
          intact.
        </AlertDialogDescription>
      </AlertDialogHeader>
      <div
        v-if="errorMessage"
        role="alert"
        class="flex items-start gap-2 rounded-xl border border-destructive/25 bg-destructive/10 p-3 text-xs leading-5 text-destructive"
      >
        <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" />
        <span>{{ errorMessage }}</span>
      </div>
      <label class="block">
        <span class="mb-1.5 block text-xs font-medium text-muted-foreground">Reason (optional)</span>
        <textarea
          v-model="transitionReason"
          rows="3"
          maxlength="2000"
          :disabled="transitioning"
          placeholder="Why is this lead returning?"
          class="w-full resize-y rounded-xl border border-border bg-background px-3 py-2.5 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
        />
      </label>
      <AlertDialogFooter>
        <AlertDialogCancel :disabled="transitioning">Keep as is</AlertDialogCancel>
        <Button
          class="bg-emerald-600 text-white hover:bg-emerald-500"
          :disabled="transitioning || !canWriteLeads"
          @click.prevent="transitionLead"
        >
          <Loader2 v-if="transitioning" class="mr-2 h-4 w-4 animate-spin" />
          Reopen lead
        </Button>
      </AlertDialogFooter>
    </AlertDialogContent>
  </AlertDialog>
</template>
