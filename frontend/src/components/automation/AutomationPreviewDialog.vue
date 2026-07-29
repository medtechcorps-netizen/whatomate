<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Clock3,
  FlaskConical,
  Loader2,
  Play,
  ShieldCheck,
  XCircle,
} from 'lucide-vue-next'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import type {
  AutomationPreviewIssue,
  AutomationPreviewResult,
  AutomationPreviewSimulationInput,
} from '@/services/automationPolicies'

const props = defineProps<{
  open: boolean
  loading?: boolean
  eventTypes: string[]
  suggestedMetadata?: Record<string, unknown>
  showContactOptOut?: boolean
  result?: AutomationPreviewResult | null
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  run: [input: AutomationPreviewSimulationInput]
  'select-node': [nodeId: string]
}>()

const eventType = ref('')
const actorType = ref('system')
const sourceType = ref('preview')
const title = ref('Simulated customer activity')
const summary = ref('A safe synthetic event for policy preview.')
const metadataText = ref('{}')
const metadataError = ref('')
const marketingOptOut = ref(false)

watch(
  () => [props.open, props.eventTypes, props.suggestedMetadata] as const,
  () => {
    if (!props.open) return
    eventType.value = props.eventTypes[0] ?? ''
    actorType.value = 'system'
    sourceType.value = 'preview'
    title.value = 'Simulated customer activity'
    summary.value = 'A safe synthetic event for policy preview.'
    metadataText.value = JSON.stringify(props.suggestedMetadata ?? {}, null, 2)
    metadataError.value = ''
    marketingOptOut.value = false
  },
  { immediate: true, deep: true },
)

const category = computed(() => {
  const prefix = eventType.value.split('.')[0]
  if (prefix === 'crm') return 'crm'
  if (prefix === 'booking') return 'booking'
  if (prefix === 'package') return 'package'
  if (prefix === 'invoice') return 'invoice'
  if (prefix === 'payment') return 'payment'
  if (prefix === 'message') return 'message'
  if (prefix === 'consent') return 'consent'
  if (prefix === 'task') return 'task'
  return 'contact'
})

function issueMessage(issue: string | AutomationPreviewIssue) {
  return typeof issue === 'string' ? issue : issue.message
}

function issueNode(issue: string | AutomationPreviewIssue) {
  return typeof issue === 'string' ? '' : issue.node_id ?? ''
}

function runPreview() {
  metadataError.value = ''
  let metadata: Record<string, unknown>
  try {
    const parsed = JSON.parse(metadataText.value || '{}')
    if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') {
      throw new Error('Metadata must be a JSON object.')
    }
    metadata = parsed
  } catch (error) {
    metadataError.value = error instanceof Error ? error.message : 'Metadata is not valid JSON.'
    return
  }

  emit('run', {
    event: {
      event_type: eventType.value,
      category: category.value,
      source_type: sourceType.value.trim() || 'preview',
      actor_type: actorType.value,
      title: title.value.trim() || 'Simulated customer activity',
      summary: summary.value.trim(),
      metadata,
      contact: {
        marketing_opt_out: marketingOptOut.value,
      },
    },
  })
}

function formatMoment(value?: string) {
  if (!value) return 'Immediately'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(value))
}
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent class="max-h-[92vh] max-w-5xl overflow-hidden border-white/10 bg-[#0b0f10] p-0 light:border-gray-200 light:bg-white">
      <DialogHeader class="border-b border-white/[0.08] px-6 py-5 light:border-gray-200">
        <div class="flex items-start gap-3">
          <span class="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-cyan-300/10 text-cyan-200 light:bg-cyan-100 light:text-cyan-800">
            <FlaskConical class="h-5 w-5" />
          </span>
          <div>
            <DialogTitle>Preview with a synthetic event</DialogTitle>
            <DialogDescription class="mt-1 max-w-2xl">
              This write-free simulation validates the draft and shows the path it would take. It does not prove that a future production event will contain identical data.
            </DialogDescription>
          </div>
        </div>
      </DialogHeader>

      <div class="grid min-h-0 overflow-y-auto lg:grid-cols-[340px_minmax(0,1fr)]">
        <form class="space-y-4 border-b border-white/[0.08] p-5 light:border-gray-200 lg:border-b-0 lg:border-r" @submit.prevent="runPreview">
          <div class="rounded-xl border border-cyan-300/10 bg-cyan-300/[0.05] p-3 text-[11px] leading-5 text-cyan-100/60 light:text-cyan-900">
            <p class="flex items-center gap-2 font-semibold text-cyan-100 light:text-cyan-900">
              <ShieldCheck class="h-3.5 w-3.5" />
              No records will be written
            </p>
            <p class="mt-1">No task, message, customer activity or scheduled job is created by preview.</p>
          </div>

          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Event type</span>
            <select v-model="eventType" required class="automation-preview-input">
              <option v-for="value in eventTypes" :key="value" :value="value">{{ value }}</option>
            </select>
          </label>

          <div class="grid grid-cols-2 gap-3">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Category</span>
              <input
                :value="category"
                readonly
                class="automation-preview-input cursor-default opacity-70"
              />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Actor</span>
              <select v-model="actorType" class="automation-preview-input">
                <option value="system">System</option>
                <option value="user">Teammate</option>
                <option value="contact">Customer</option>
                <option value="provider">Provider</option>
                <option value="import">Import</option>
              </select>
            </label>
          </div>

          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Source type</span>
            <input
              v-model.trim="sourceType"
              maxlength="100"
              class="automation-preview-input"
              placeholder="e.g. booking"
            />
          </label>

          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Event title</span>
            <input v-model.trim="title" maxlength="255" class="automation-preview-input" />
          </label>

          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Summary</span>
            <input v-model.trim="summary" maxlength="500" class="automation-preview-input" />
          </label>

          <label class="block">
            <span class="mb-1.5 flex items-center justify-between text-xs font-medium text-white/60 light:text-gray-700">
              Event metadata
              <span class="font-mono text-[9px] text-white/25 light:text-gray-400">JSON object</span>
            </span>
            <textarea
              v-model="metadataText"
              rows="6"
              spellcheck="false"
              class="automation-preview-input h-auto resize-y py-2.5 font-mono text-[11px]"
              :aria-invalid="Boolean(metadataError)"
              aria-describedby="preview-metadata-error"
            />
            <p v-if="metadataError" id="preview-metadata-error" class="mt-1.5 text-[11px] text-rose-300">
              {{ metadataError }}
            </p>
          </label>

          <label
            v-if="showContactOptOut"
            class="flex cursor-pointer items-start gap-3 rounded-xl border border-white/[0.08] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
          >
            <input
              v-model="marketingOptOut"
              type="checkbox"
              class="mt-0.5 h-4 w-4 rounded border-white/20 bg-transparent accent-cyan-300 focus-visible:ring-2 focus-visible:ring-cyan-200"
            />
            <span>
              <span class="block text-xs font-medium text-white/65 light:text-gray-800">
                Customer has opted out of marketing
              </span>
              <span class="mt-1 block text-[10px] leading-4 text-white/30 light:text-gray-500">
                Synthetic contact context used only for this write-free preview.
              </span>
            </span>
          </label>

          <Button
            type="submit"
            class="h-11 w-full gap-2 bg-cyan-300 text-[#071114] hover:bg-cyan-200"
            :disabled="loading || !eventType"
          >
            <Loader2 v-if="loading" class="h-4 w-4 animate-spin" />
            <Play v-else class="h-4 w-4" />
            {{ loading ? 'Running preview…' : 'Run write-free preview' }}
          </Button>
        </form>

        <section class="min-h-[420px] p-5 sm:p-6" aria-live="polite">
          <div v-if="loading" class="flex h-full min-h-[360px] flex-col items-center justify-center text-center">
            <span class="relative flex h-16 w-16 items-center justify-center rounded-2xl bg-cyan-300/[0.07] text-cyan-200">
              <Loader2 class="h-7 w-7 animate-spin" />
            </span>
            <p class="mt-4 text-sm font-medium text-white light:text-gray-900">Walking the draft graph</p>
            <p class="mt-1 text-xs text-white/35 light:text-gray-600">Server validation remains authoritative.</p>
          </div>

          <div v-else-if="!result" class="flex h-full min-h-[360px] flex-col items-center justify-center text-center">
            <span class="flex h-14 w-14 items-center justify-center rounded-2xl border border-dashed border-white/10 text-white/25 light:border-gray-300 light:text-gray-400">
              <Activity class="h-6 w-6" />
            </span>
            <p class="mt-4 text-sm font-medium text-white light:text-gray-900">Your execution path will appear here</p>
            <p class="mt-1 max-w-sm text-xs leading-5 text-white/35 light:text-gray-600">
              Match a realistic event payload before activating the draft.
            </p>
          </div>

          <div v-else class="space-y-5">
            <div
              class="flex items-start gap-3 rounded-2xl border p-4"
              :class="result.valid && result.actions.length
                ? 'border-emerald-300/15 bg-emerald-300/[0.055]'
                : result.valid
                  ? 'border-amber-300/15 bg-amber-300/[0.055]'
                  : 'border-rose-300/15 bg-rose-300/[0.055]'"
            >
              <CheckCircle2 v-if="result.valid && result.actions.length" class="mt-0.5 h-5 w-5 shrink-0 text-emerald-300 light:text-emerald-700" />
              <AlertTriangle v-else-if="result.valid" class="mt-0.5 h-5 w-5 shrink-0 text-amber-300 light:text-amber-700" />
              <XCircle v-else class="mt-0.5 h-5 w-5 shrink-0 text-rose-300 light:text-rose-700" />
              <div>
                <p class="text-sm font-semibold text-white light:text-gray-900">
                  {{ result.valid && result.actions.length
                    ? 'Draft is valid and reaches a follow-up'
                    : result.valid
                      ? 'Draft is valid, but this event reaches no follow-up'
                      : 'Draft needs attention' }}
                </p>
                <p class="mt-1 text-[11px] leading-5 text-white/40 light:text-gray-600">
                  <template v-if="result.checksum">Checked snapshot {{ result.checksum.slice(0, 10) }} · </template>
                  {{ result.actions.length }} follow-up {{ result.actions.length === 1 ? 'action' : 'actions' }} projected
                </p>
                <p
                  v-if="result.valid && !result.actions.length"
                  class="mt-2 text-[11px] leading-5 text-amber-100/65 light:text-amber-800"
                >
                  Adjust the event metadata, actor, source type or customer opt-out value until the preview exercises a projected task.
                </p>
              </div>
            </div>

            <div v-if="result.errors?.length" class="space-y-2">
              <p class="text-[10px] font-bold uppercase tracking-[0.18em] text-rose-300 light:text-rose-700">Blocking issues</p>
              <button
                v-for="(issue, index) in result.errors"
                :key="index"
                type="button"
                class="flex w-full items-start gap-2 rounded-xl border border-rose-300/10 bg-rose-300/[0.045] p-3 text-left text-[11px] leading-5 text-rose-100/70 outline-none focus-visible:ring-2 focus-visible:ring-rose-300 light:text-rose-900"
                :class="{ 'cursor-pointer hover:border-rose-300/25': issueNode(issue) }"
                :disabled="!issueNode(issue)"
                @click="issueNode(issue) && emit('select-node', issueNode(issue))"
              >
                <XCircle class="mt-0.5 h-3.5 w-3.5 shrink-0" />
                {{ issueMessage(issue) }}
              </button>
            </div>

            <div v-if="result.warnings?.length" class="space-y-2">
              <p class="text-[10px] font-bold uppercase tracking-[0.18em] text-amber-300 light:text-amber-700">Review warnings</p>
              <button
                v-for="(issue, index) in result.warnings"
                :key="index"
                type="button"
                class="flex w-full items-start gap-2 rounded-xl border border-amber-300/10 bg-amber-300/[0.045] p-3 text-left text-[11px] leading-5 text-amber-100/70 outline-none focus-visible:ring-2 focus-visible:ring-amber-300 light:text-amber-900"
                :class="{ 'cursor-pointer hover:border-amber-300/25': issueNode(issue) }"
                :disabled="!issueNode(issue)"
                @click="issueNode(issue) && emit('select-node', issueNode(issue))"
              >
                <AlertTriangle class="mt-0.5 h-3.5 w-3.5 shrink-0" />
                {{ issueMessage(issue) }}
              </button>
            </div>

            <div>
              <div class="mb-3 flex items-center justify-between">
                <p class="text-[10px] font-bold uppercase tracking-[0.18em] text-white/35 light:text-gray-500">Projected path</p>
                <span class="text-[10px] text-white/25 light:text-gray-500">{{ result.steps.length }} steps</span>
              </div>
              <ol class="relative ml-2 border-l border-white/10 pl-5 light:border-gray-200">
                <li v-for="(step, index) in result.steps" :key="`${step.node_id}-${index}`" class="relative pb-5 last:pb-0">
                  <span
                    class="absolute -left-[25px] top-0.5 h-2.5 w-2.5 rounded-full border-2 border-[#0b0f10] light:border-white"
                    :class="step.status === 'skipped' ? 'bg-slate-500' : step.status === 'failed' ? 'bg-rose-400' : 'bg-lime-300'"
                  />
                  <button
                    type="button"
                    class="block w-full cursor-pointer rounded-lg text-left outline-none focus-visible:ring-2 focus-visible:ring-lime-200"
                    @click="emit('select-node', step.node_id)"
                  >
                    <span class="flex items-center justify-between gap-2">
                      <span class="text-xs font-medium capitalize text-white/75 light:text-gray-800">
                        {{ step.node_type.replace('_', ' ') }}
                      </span>
                      <span class="text-[9px] font-semibold uppercase tracking-wider text-white/25 light:text-gray-500">{{ step.status }}</span>
                    </span>
                    <span v-if="step.detail" class="mt-1 block text-[11px] leading-4 text-white/35 light:text-gray-600">{{ step.detail }}</span>
                    <span v-if="step.reason_code" class="mt-1 block font-mono text-[9px] text-white/25 light:text-gray-500">{{ step.reason_code }}</span>
                    <span v-if="step.scheduled_at" class="mt-1.5 flex items-center gap-1 text-[10px] text-violet-300 light:text-violet-700">
                      <Clock3 class="h-3 w-3" />
                      {{ formatMoment(step.scheduled_at) }}
                    </span>
                  </button>
                </li>
              </ol>
            </div>

            <div v-if="result.actions?.length">
              <p class="mb-3 text-[10px] font-bold uppercase tracking-[0.18em] text-white/35 light:text-gray-500">Projected follow-ups</p>
              <div class="space-y-2">
                <article v-for="action in result.actions" :key="action.node_id" class="rounded-xl border border-emerald-300/10 bg-emerald-300/[0.04] p-3">
                  <div class="flex items-start justify-between gap-3">
                    <div>
                      <p class="text-xs font-medium text-white light:text-gray-900">{{ action.title }}</p>
                      <p class="mt-1 text-[10px] capitalize text-white/35 light:text-gray-600">
                        {{ action.priority }} priority · {{ action.owner.replace('_', ' ') }}
                      </p>
                    </div>
                    <span class="rounded-full bg-emerald-300/10 px-2 py-1 text-[9px] font-bold uppercase tracking-wider text-emerald-200 light:text-emerald-800">Task</span>
                  </div>
                  <p class="mt-2 flex items-center gap-1.5 text-[10px] text-white/35 light:text-gray-600">
                    <Clock3 class="h-3 w-3" />
                    {{ formatMoment(action.scheduled_at) }}
                    <template v-if="action.due_at"> · due {{ formatMoment(action.due_at) }}</template>
                  </p>
                </article>
              </div>
            </div>
          </div>
        </section>
      </div>
    </DialogContent>
  </Dialog>
</template>

<style scoped>
.automation-preview-input {
  width: 100%;
  min-height: 2.65rem;
  border-radius: 0.75rem;
  border: 1px solid rgba(255, 255, 255, 0.1);
  background: rgba(0, 0, 0, 0.2);
  padding-left: 0.75rem;
  padding-right: 0.75rem;
  color: rgba(255, 255, 255, 0.88);
  font-size: 0.8rem;
  outline: none;
}

.automation-preview-input:focus-visible {
  border-color: rgba(103, 232, 249, 0.5);
  box-shadow: 0 0 0 3px rgba(103, 232, 249, 0.08);
}

:global(.light) .automation-preview-input {
  border-color: rgb(229 231 235);
  background: white;
  color: rgb(17 24 39);
}
</style>
