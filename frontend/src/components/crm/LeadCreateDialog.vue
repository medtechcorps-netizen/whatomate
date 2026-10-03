<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Check, Loader2, Plus } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import ContactPicker from '@/components/shared/ContactPicker.vue'
import type { Contact } from '@/stores/contacts'
import { contactDisplayName } from '@/lib/contactAddress'
import type { CRMLead, Pipeline } from '@/services/productSuite'
import {
  activePipelines,
  defaultPipelineId as pickDefaultPipelineId,
  followUpPresetDate,
  openStages,
  stageChainLabel,
  type LeadDraft,
} from '@/lib/crmFlow'

type FollowUpChoice = 'none' | 'tomorrow' | '3days' | 'week' | 'custom'

const props = withDefaults(
  defineProps<{
    open: boolean
    saving?: boolean
    contact?: { id: string; name: string } | null
    pipelines: Pipeline[]
    pipelinesLoading?: boolean
    defaultPipelineId?: string
    defaultTitle?: string
    canScheduleFollowUp?: boolean
    existingOpenLeads?: CRMLead[]
  }>(),
  {
    saving: false,
    contact: null,
    pipelinesLoading: false,
    defaultPipelineId: '',
    defaultTitle: '',
    canScheduleFollowUp: false,
    existingOpenLeads: () => [],
  },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  submit: [draft: LeadDraft]
}>()

const FOLLOW_UP_CHOICES: Array<{ value: FollowUpChoice; label: string }> = [
  { value: 'none', label: 'No follow-up' },
  { value: 'tomorrow', label: 'Tomorrow' },
  { value: '3days', label: 'In 3 days' },
  { value: 'week', label: 'Next week' },
  { value: 'custom', label: 'Pick a date' },
]

const CURRENCIES = ['MYR', 'SGD', 'USD']

function emptyDraft(): LeadDraft {
  return {
    contact_id: '',
    contact_name: '',
    pipeline_id: '',
    stage_id: '',
    title: '',
    value: '',
    currency: 'MYR',
    follow_up_at: '',
  }
}

const draft = ref<LeadDraft>(emptyDraft())
const titleEdited = ref(false)
const followUpChoice = ref<FollowUpChoice>('none')

const pipelineOptions = computed(() => activePipelines(props.pipelines ?? []))
const selectedPipeline = computed(
  () => pipelineOptions.value.find((pipeline) => pipeline.id === draft.value.pipeline_id) ?? null,
)
const stageOptions = computed(() => openStages(selectedPipeline.value))
const hasFixedContact = computed(() => Boolean(props.contact?.id))

// The host may pass a raw channel placeholder (e.g. "bsuid:...") as the name.
function fixedContactName() {
  const name = (props.contact?.name ?? '').trim()
  return name ? contactDisplayName({ name }) : ''
}

const description = computed(() =>
  hasFixedContact.value
    ? `Create a lead for ${fixedContactName() || 'this customer'}. You can move it through the stages later.`
    : 'Create a lead and choose where it starts.',
)

function suggestedTitle() {
  if (props.defaultTitle.trim()) return props.defaultTitle.trim()
  return [draft.value.contact_name.trim(), selectedPipeline.value?.name ?? '']
    .filter(Boolean)
    .join(' – ')
}

function refreshSuggestedTitle() {
  if (!titleEdited.value) draft.value.title = suggestedTitle()
}

function ensureValidStage() {
  if (!stageOptions.value.some((stage) => stage.id === draft.value.stage_id)) {
    draft.value.stage_id = stageOptions.value[0]?.id ?? ''
  }
}

function ensureValidPipeline() {
  if (pipelineOptions.value.some((pipeline) => pipeline.id === draft.value.pipeline_id)) return
  draft.value.pipeline_id = pickDefaultPipelineId(props.pipelines ?? [], {
    preferredId: props.defaultPipelineId || null,
  })
}

function resetDraft() {
  draft.value = {
    ...emptyDraft(),
    contact_id: props.contact?.id ?? '',
    contact_name: fixedContactName(),
  }
  titleEdited.value = false
  followUpChoice.value = 'none'
  ensureValidPipeline()
  ensureValidStage()
  refreshSuggestedTitle()
}

watch(
  () => props.open,
  (isOpen) => {
    if (isOpen) resetDraft()
  },
  { immediate: true },
)

// A host may switch the fixed customer while the dialog is open.
watch(
  () => props.contact?.id,
  () => {
    if (props.open) resetDraft()
  },
)

// Pipelines may arrive after the dialog opens; adopt a valid default then.
watch(pipelineOptions, () => {
  if (!props.open) return
  ensureValidPipeline()
  ensureValidStage()
  refreshSuggestedTitle()
})

function selectPipeline(pipelineId: string) {
  if (props.saving || draft.value.pipeline_id === pipelineId) return
  draft.value.pipeline_id = pipelineId
  ensureValidStage()
  refreshSuggestedTitle()
}

function selectStage(stageId: string) {
  if (props.saving) return
  draft.value.stage_id = stageId
}

function onTitleInput(event: Event) {
  draft.value.title = (event.target as HTMLInputElement).value
  titleEdited.value = true
}

function onContactSelected(contact: Contact) {
  draft.value.contact_id = contact.id
  draft.value.contact_name = contactDisplayName(contact)
  refreshSuggestedTitle()
}

function onContactIdChange(id: string) {
  draft.value.contact_id = id
  if (!id) {
    draft.value.contact_name = ''
    refreshSuggestedTitle()
  }
}

function toLocalInputValue(date: Date) {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 16)
}

function chooseFollowUp(choice: FollowUpChoice) {
  if (props.saving) return
  followUpChoice.value = choice
  if (choice === 'none') draft.value.follow_up_at = ''
  else if (choice === 'custom') draft.value.follow_up_at = ''
  else draft.value.follow_up_at = toLocalInputValue(followUpPresetDate(choice))
}

const minFollowUp = computed(() => toLocalInputValue(new Date()))

const followUpSummary = computed(() => {
  if (followUpChoice.value === 'none' || followUpChoice.value === 'custom' || !draft.value.follow_up_at) return ''
  const date = new Date(draft.value.follow_up_at)
  if (Number.isNaN(date.getTime())) return ''
  return `Follow-up on ${date.toLocaleString(undefined, {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
    hour: 'numeric',
    minute: '2-digit',
  })}`
})

const valueError = computed(() => {
  const raw = String(draft.value.value ?? '').trim()
  if (!raw) return ''
  const amount = Number(raw)
  if (!Number.isFinite(amount) || amount < 0) return 'Enter an amount of zero or more.'
  return ''
})

const duplicateLead = computed(() => {
  if (!draft.value.contact_id || !draft.value.pipeline_id) return null
  return (
    (props.existingOpenLeads ?? []).find(
      (lead) =>
        lead.status === 'open' &&
        lead.contact_id === draft.value.contact_id &&
        lead.pipeline_id === draft.value.pipeline_id,
    ) ?? null
  )
})

const noOpenStage = computed(() => Boolean(selectedPipeline.value) && stageOptions.value.length === 0)

const needsFollowUpDate = computed(() => followUpChoice.value === 'custom' && !draft.value.follow_up_at)

/** One short line explaining why "Add lead" is disabled, or '' when nothing required is missing. */
const missingReason = computed(() => {
  if (props.saving) return ''
  if (!draft.value.contact_id) return 'Choose a customer'
  // The loading line and the "no pipeline" notice already explain these states.
  if (!pipelineOptions.value.length) return ''
  if (!selectedPipeline.value) return 'Choose a pipeline'
  if (!draft.value.stage_id) return 'Choose a starting stage'
  if (!draft.value.title.trim()) return 'Enter a lead title'
  if (needsFollowUpDate.value) return 'Choose a follow-up date'
  return ''
})

const canSubmit = computed(
  () =>
    !props.saving &&
    Boolean(draft.value.contact_id) &&
    Boolean(selectedPipeline.value) &&
    Boolean(draft.value.stage_id) &&
    Boolean(draft.value.title.trim()) &&
    !valueError.value &&
    !needsFollowUpDate.value,
)

function setOpen(value: boolean) {
  if (!value && props.saving) return
  emit('update:open', value)
}

function submit() {
  if (!canSubmit.value) return
  emit('submit', {
    ...draft.value,
    title: draft.value.title.trim(),
    value: String(draft.value.value ?? '').trim(),
    follow_up_at: props.canScheduleFollowUp ? draft.value.follow_up_at : '',
  })
}

const inputClass =
  'h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 disabled:opacity-60 light:border-slate-300 light:bg-white'
const chipBase =
  'inline-flex min-h-9 items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 disabled:cursor-not-allowed disabled:opacity-60'
const chipIdle =
  'border-white/10 bg-white/[0.03] text-white/70 hover:border-cyan-300/30 hover:text-white light:border-slate-200 light:bg-white light:text-slate-700 light:hover:text-slate-950'
const chipActive = 'border-cyan-300/60 bg-cyan-300/[0.12] text-cyan-100 light:border-cyan-500 light:bg-cyan-50 light:text-cyan-900'
</script>

<template>
  <Dialog :open="open" @update:open="setOpen">
    <DialogContent
      data-testid="lead-create-dialog"
      class="max-h-[92vh] w-[calc(100vw-1.5rem)] max-w-xl overflow-y-auto border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950"
    >
      <DialogHeader>
        <DialogTitle>Add to pipeline</DialogTitle>
        <DialogDescription>{{ description }}</DialogDescription>
      </DialogHeader>

      <form id="lead-create-form" class="space-y-5" @submit.prevent="submit">
        <fieldset :disabled="saving" class="contents">
          <div v-if="!hasFixedContact" role="group" aria-labelledby="lead-create-customer-label" class="space-y-1.5">
            <span id="lead-create-customer-label" class="block text-xs font-medium text-white/60 light:text-slate-700">
              Customer
            </span>
            <ContactPicker
              v-if="open"
              :model-value="draft.contact_id"
              :disabled="saving"
              @update:model-value="onContactIdChange"
              @selected="onContactSelected"
            />
          </div>

          <div class="space-y-1.5">
            <div
              v-if="pipelinesLoading && !pipelineOptions.length"
              class="flex items-center gap-2 rounded-xl border border-white/10 px-3 py-3 text-xs text-white/55 light:border-slate-200 light:text-slate-600"
              aria-live="polite"
            >
              <Loader2 class="h-4 w-4 animate-spin" />
              Loading pipelines
            </div>
            <p
              v-else-if="!pipelineOptions.length"
              role="alert"
              class="rounded-xl border border-amber-300/20 bg-amber-300/[0.06] p-3 text-xs leading-5 text-amber-100 light:border-amber-300 light:bg-amber-50 light:text-amber-900"
            >
              No pipeline is set up yet. Ask an admin to create one.
            </p>
            <p v-else-if="pipelineOptions.length === 1" class="text-sm text-white/75 light:text-slate-700">
              Pipeline: <span class="font-medium text-white light:text-slate-950">{{ pipelineOptions[0].name }}</span>
            </p>
            <fieldset v-else>
              <legend id="lead-create-pipeline-label" class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">
                Pipeline
              </legend>
              <div role="radiogroup" aria-labelledby="lead-create-pipeline-label" class="grid gap-2">
                <label
                  v-for="pipeline in pipelineOptions"
                  :key="pipeline.id"
                  data-testid="lead-pipeline-option"
                  class="block cursor-pointer rounded-xl border p-3 transition focus-within:ring-2 focus-within:ring-cyan-300"
                  :class="
                    draft.pipeline_id === pipeline.id
                      ? 'border-cyan-300/60 bg-cyan-300/[0.08] light:border-cyan-500 light:bg-cyan-50'
                      : 'border-white/10 bg-white/[0.02] hover:border-cyan-300/30 light:border-slate-200 light:bg-slate-50'
                  "
                >
                  <input
                    type="radio"
                    name="lead-create-pipeline"
                    class="sr-only"
                    :value="pipeline.id"
                    :checked="draft.pipeline_id === pipeline.id"
                    @change="selectPipeline(pipeline.id)"
                  />
                  <span class="flex items-start justify-between gap-3">
                    <span class="min-w-0">
                      <span class="block text-sm font-semibold">{{ pipeline.name }}</span>
                      <span
                        v-if="pipeline.description"
                        class="mt-0.5 block text-xs leading-5 text-white/55 light:text-slate-600"
                      >
                        {{ pipeline.description }}
                      </span>
                      <span
                        v-if="stageChainLabel(pipeline)"
                        class="mt-1 block truncate text-[11px] text-white/40 light:text-slate-500"
                      >
                        {{ stageChainLabel(pipeline) }}
                      </span>
                    </span>
                    <Check
                      v-if="draft.pipeline_id === pipeline.id"
                      class="mt-0.5 h-4 w-4 shrink-0 text-cyan-300 light:text-cyan-700"
                      aria-hidden="true"
                    />
                  </span>
                </label>
              </div>
            </fieldset>
          </div>

          <div v-if="selectedPipeline" class="space-y-1.5">
            <span id="lead-create-stage-label" class="block text-xs font-medium text-white/60 light:text-slate-700">
              Starting stage
            </span>
            <p
              v-if="noOpenStage"
              role="alert"
              class="rounded-xl border border-amber-300/20 bg-amber-300/[0.06] p-3 text-xs leading-5 text-amber-100 light:border-amber-300 light:bg-amber-50 light:text-amber-900"
            >
              This pipeline has no active open stage. Ask an admin to configure it.
            </p>
            <div v-else role="group" aria-labelledby="lead-create-stage-label" class="flex flex-wrap gap-2">
              <button
                v-for="stage in stageOptions"
                :key="stage.id"
                type="button"
                data-testid="lead-stage-option"
                :aria-pressed="draft.stage_id === stage.id"
                :class="[chipBase, draft.stage_id === stage.id ? chipActive : chipIdle]"
                @click="selectStage(stage.id)"
              >
                <Check v-if="draft.stage_id === stage.id" class="h-3.5 w-3.5" aria-hidden="true" />
                {{ stage.name }}
              </button>
            </div>
          </div>

          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Lead title</span>
            <input
              :value="draft.title"
              required
              maxlength="255"
              :class="inputClass"
              @input="onTitleInput"
            />
          </label>

          <div class="grid gap-3 sm:grid-cols-[1fr_8rem]">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">
                Estimated value <span class="font-normal text-white/40 light:text-slate-500">(optional)</span>
              </span>
              <input
                v-model="draft.value"
                type="number"
                min="0"
                step="0.01"
                inputmode="decimal"
                placeholder="0.00"
                :aria-invalid="valueError ? 'true' : undefined"
                aria-describedby="lead-create-value-error"
                :class="inputClass"
              />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Currency</span>
              <select v-model="draft.currency" :class="[inputClass, 'bg-[#15191f]']">
                <option v-for="currency in CURRENCIES" :key="currency" :value="currency">{{ currency }}</option>
              </select>
            </label>
            <p
              v-if="valueError"
              id="lead-create-value-error"
              class="text-xs text-rose-300 light:text-rose-700 sm:col-span-2"
            >
              {{ valueError }}
            </p>
          </div>

          <div v-if="canScheduleFollowUp" class="space-y-1.5">
            <span id="lead-create-follow-up-label" class="block text-xs font-medium text-white/60 light:text-slate-700">
              Follow-up
            </span>
            <div role="group" aria-labelledby="lead-create-follow-up-label" class="flex flex-wrap gap-2">
              <button
                v-for="choice in FOLLOW_UP_CHOICES"
                :key="choice.value"
                type="button"
                data-testid="lead-follow-up-preset"
                :aria-pressed="followUpChoice === choice.value"
                :class="[chipBase, followUpChoice === choice.value ? chipActive : chipIdle]"
                @click="chooseFollowUp(choice.value)"
              >
                {{ choice.label }}
              </button>
            </div>
            <p v-if="followUpSummary" class="text-xs text-white/45 light:text-slate-500" aria-live="polite">
              {{ followUpSummary }}
            </p>
            <label v-if="followUpChoice === 'custom'" class="block">
              <span class="sr-only">Follow-up date and time</span>
              <input
                v-model="draft.follow_up_at"
                type="datetime-local"
                :min="minFollowUp"
                required
                :class="[inputClass, 'bg-[#15191f] text-xs']"
              />
            </label>
            <p
              v-if="needsFollowUpDate"
              data-testid="lead-create-follow-up-missing"
              class="text-xs text-white/45 light:text-slate-500"
            >
              Choose a follow-up date
            </p>
          </div>

          <div
            v-if="duplicateLead"
            role="status"
            class="flex items-start gap-2 rounded-xl border border-amber-300/20 bg-amber-300/[0.06] p-3 text-xs leading-5 text-amber-100 light:border-amber-300 light:bg-amber-50 light:text-amber-900"
          >
            <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
            <span>
              This customer already has an open lead in this pipeline ("{{ duplicateLead.title }}"). You can still add
              another one.
            </span>
          </div>
        </fieldset>
      </form>

      <p
        v-if="missingReason"
        id="lead-create-missing-reason"
        data-testid="lead-create-missing-reason"
        class="text-xs text-white/55 light:text-slate-600"
        aria-live="polite"
      >
        {{ missingReason }}
      </p>

      <DialogFooter class="gap-2">
        <Button variant="outline" :disabled="saving" @click="setOpen(false)">Cancel</Button>
        <Button
          type="submit"
          form="lead-create-form"
          data-testid="lead-create-submit"
          class="gap-2 bg-cyan-400 text-black hover:bg-cyan-300"
          :disabled="!canSubmit"
          :aria-describedby="missingReason ? 'lead-create-missing-reason' : undefined"
        >
          <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
          <Plus v-else class="h-4 w-4" />
          Add lead
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
