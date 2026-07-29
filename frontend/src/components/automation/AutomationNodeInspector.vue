<script setup lang="ts">
import { computed } from 'vue'
import {
  BellRing,
  CheckSquare2,
  Clock3,
  GitBranch,
  Info,
  LockKeyhole,
  Trash2,
} from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import type {
  AutomationPolicyCatalog,
  AutomationPolicyNode,
} from '@/services/automationPolicies'

const props = defineProps<{
  node: AutomationPolicyNode | null
  readonly?: boolean
  catalog?: AutomationPolicyCatalog | null
}>()

const emit = defineEmits<{
  update: [config: Record<string, unknown>]
  delete: []
}>()

const curatedEventGroups = [
  {
    label: 'Appointments',
    options: [
      { value: 'booking.created', label: 'Booking created' },
      { value: 'booking.status_changed', label: 'Booking status changed' },
    ],
  },
  {
    label: 'Customer & inbox',
    options: [
      { value: 'contact.created', label: 'Customer created' },
      { value: 'contact.updated', label: 'Customer updated' },
      { value: 'contact.merged', label: 'Customer merged' },
      { value: 'message.incoming', label: 'Incoming message' },
      { value: 'consent.opted_out', label: 'Marketing opt-out' },
    ],
  },
  {
    label: 'CRM',
    options: [
      { value: 'crm.lead.created', label: 'Lead created' },
      { value: 'crm.lead.updated', label: 'Lead updated' },
      { value: 'crm.lead.stage_moved', label: 'Lead stage moved' },
      { value: 'task.completed', label: 'Follow-up completed' },
    ],
  },
  {
    label: 'Packages & payments',
    options: [
      { value: 'package.sold', label: 'Package sold' },
      { value: 'package.balance_low', label: 'Package balance low' },
      { value: 'package.expiring', label: 'Package expiring' },
      { value: 'invoice.created', label: 'Invoice created' },
      { value: 'invoice.overdue', label: 'Invoice overdue' },
      { value: 'invoice.paid', label: 'Invoice paid' },
      { value: 'payment.recorded', label: 'Payment recorded' },
    ],
  },
]

const curatedConditionFields = [
  { value: 'event.event_type', label: 'Event type' },
  { value: 'event.category', label: 'Event category' },
  { value: 'event.source_type', label: 'Source type' },
  { value: 'event.actor_type', label: 'Actor type' },
  { value: 'event.title', label: 'Event title' },
  { value: 'event.summary', label: 'Event summary' },
  { value: 'event.metadata.to_status', label: 'Booking status (metadata.to_status)' },
  { value: 'contact.marketing_opt_out', label: 'Customer marketing opt-out' },
]

const curatedConditionOperators = [
  { value: 'equals', label: 'Equals' },
  { value: 'not_equals', label: 'Does not equal' },
  { value: 'exists', label: 'Exists' },
  { value: 'in', label: 'Is one of' },
]

function enumLabel(value: string) {
  return value
    .replace(/[._]/g, ' ')
    .replace(/\b\w/g, (letter) => letter.toUpperCase())
}

const eventGroups = computed(() => {
  if (!props.catalog) return curatedEventGroups
  const allowed = new Set(props.catalog.event_types)
  const known = new Set(
    curatedEventGroups.flatMap((group) => group.options.map((option) => option.value)),
  )
  const groups = curatedEventGroups
    .map((group) => ({
      ...group,
      options: group.options.filter((option) => allowed.has(option.value)),
    }))
    .filter((group) => group.options.length)
  const other = props.catalog.event_types
    .filter((value) => !known.has(value))
    .map((value) => ({ value, label: enumLabel(value) }))
  return other.length
    ? [...groups, { label: 'Other lifecycle events', options: other }]
    : groups
})

const conditionFields = computed(() => {
  if (!props.catalog) return curatedConditionFields
  const labels = new Map(
    curatedConditionFields.map((field) => [field.value, field.label]),
  )
  return props.catalog.condition_fields.map((value) => ({
    value,
    label: labels.get(value) ?? enumLabel(value),
  }))
})

const conditionOperators = computed(() => {
  if (!props.catalog) return curatedConditionOperators
  const labels = new Map(
    curatedConditionOperators.map((operator) => [operator.value, operator.label]),
  )
  return props.catalog.condition_operators.map((value) => ({
    value,
    label: labels.get(value) ?? enumLabel(value),
  }))
})

const config = computed(() => props.node?.config ?? {})
const conditionIsBooleanField = computed(
  () => String(config.value.field ?? '') === 'contact.marketing_opt_out',
)
const availableConditionOperators = computed(() =>
  conditionIsBooleanField.value
    ? conditionOperators.value.filter((operator) => operator.value !== 'in')
    : conditionOperators.value,
)
const selectedEvents = computed<string[]>(() => {
  if (Array.isArray(config.value.event_types)) {
    return config.value.event_types.map(String)
  }
  return config.value.event_type ? [String(config.value.event_type)] : []
})
const operatorNeedsValue = computed(
  () => String(config.value.operator ?? '') !== 'exists',
)

const nodePresentation = computed(() => {
  switch (props.node?.type) {
    case 'trigger':
      return { label: 'Customer event', helper: 'Choose one or more lifecycle events.', icon: BellRing }
    case 'condition':
      return { label: 'Guard condition', helper: 'Continue only when the event matches.', icon: GitBranch }
    case 'delay':
      return { label: 'Wait window', helper: 'Delay the next step without blocking a worker.', icon: Clock3 }
    case 'create_task':
      return { label: 'Create follow-up', helper: 'Put accountable work into the CRM task queue.', icon: CheckSquare2 }
    default:
      return { label: 'Step settings', helper: 'Select a step on the canvas.', icon: Info }
  }
})

function patch(values: Record<string, unknown>) {
  emit('update', { ...config.value, ...values })
}

function toggleEvent(eventType: string, enabled: boolean) {
  const events = new Set(selectedEvents.value)
  if (enabled) events.add(eventType)
  else events.delete(eventType)
  patch({ event_types: [...events], event_type: undefined })
}

function updateConditionField(field: string) {
  if (field === 'contact.marketing_opt_out') {
    const operator = config.value.operator === 'in' ? 'equals' : config.value.operator
    patch({
      field,
      operator,
      value:
        operator === 'exists'
          ? undefined
          : typeof config.value.value === 'boolean'
            ? config.value.value
            : false,
    })
    return
  }
  patch({
    field,
    value:
      typeof config.value.value === 'boolean'
        ? String(config.value.value)
        : config.value.value,
  })
}

function updateConditionOperator(operator: string) {
  if (operator === 'exists') {
    patch({ operator, value: undefined })
    return
  }
  if (conditionIsBooleanField.value) {
    patch({
      operator,
      value: typeof config.value.value === 'boolean' ? config.value.value : false,
    })
    return
  }
  if (operator === 'in') {
    const current = config.value.value
    patch({
      operator,
      value: Array.isArray(current)
        ? current
        : String(current ?? '').trim()
          ? [String(current).trim()]
          : [],
    })
    return
  }
  const current = config.value.value
  patch({
    operator,
    value: Array.isArray(current) ? String(current[0] ?? '') : current,
  })
}

function updateConditionValue(value: string) {
  if (config.value.operator === 'in') {
    patch({
      value: value
        .split(',')
        .map((item) => item.trim())
        .filter(Boolean),
    })
    return
  }
  patch({ value })
}

function numberValue(value: string, fallback = 0) {
  const parsed = Number.parseInt(value, 10)
  return Number.isFinite(parsed) ? parsed : fallback
}

function optionalNumberValue(value: string) {
  if (!value.trim()) return undefined
  return numberValue(value)
}
</script>

<template>
  <aside class="flex h-full min-h-0 flex-col bg-[#0d1012] light:bg-white" aria-label="Step configuration">
    <div class="border-b border-white/[0.08] px-4 py-4 light:border-gray-200">
      <div class="flex items-start justify-between gap-3">
        <div class="flex min-w-0 items-start gap-3">
          <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-xl bg-lime-200/[0.08] text-lime-200 light:bg-lime-100 light:text-lime-800">
            <component :is="nodePresentation.icon" class="h-4 w-4" />
          </span>
          <div class="min-w-0">
            <h2 class="text-sm font-semibold text-white light:text-gray-900">{{ nodePresentation.label }}</h2>
            <p class="mt-1 text-[11px] leading-4 text-white/35 light:text-gray-600">{{ nodePresentation.helper }}</p>
          </div>
        </div>
        <Button
          v-if="node && node.type !== 'trigger' && !readonly"
          variant="ghost"
          size="icon"
          class="h-8 w-8 shrink-0 text-white/35 hover:text-rose-300 light:text-gray-500"
          :aria-label="`Delete ${nodePresentation.label} step`"
          @click="emit('delete')"
        >
          <Trash2 class="h-4 w-4" />
        </Button>
      </div>
    </div>

    <div v-if="!node" class="flex flex-1 flex-col items-center justify-center px-7 text-center">
      <span class="flex h-12 w-12 items-center justify-center rounded-2xl border border-dashed border-white/10 text-white/25 light:border-gray-300 light:text-gray-400">
        <GitBranch class="h-5 w-5" />
      </span>
      <p class="mt-4 text-sm font-medium text-white light:text-gray-900">Select a step</p>
      <p class="mt-1 text-xs leading-5 text-white/35 light:text-gray-600">
        Its business rule and safety details will appear here.
      </p>
    </div>

    <div v-else class="min-h-0 flex-1 overflow-y-auto p-4">
      <fieldset v-if="node.type === 'trigger'" :disabled="readonly" class="space-y-5">
        <div
          v-for="group in eventGroups"
          :key="group.label"
          class="rounded-xl border border-white/[0.07] bg-white/[0.02] p-3 light:border-gray-200 light:bg-gray-50"
        >
          <legend class="px-1 text-[10px] font-bold uppercase tracking-[0.16em] text-white/35 light:text-gray-500">
            {{ group.label }}
          </legend>
          <label
            v-for="option in group.options"
            :key="option.value"
            class="mt-2 flex min-h-9 cursor-pointer items-center gap-2.5 rounded-lg px-2 text-xs text-white/60 transition hover:bg-white/[0.04] light:text-gray-700 light:hover:bg-white"
          >
            <input
              type="checkbox"
              class="h-4 w-4 rounded border-white/20 bg-transparent accent-lime-300 focus-visible:ring-2 focus-visible:ring-lime-200"
              :checked="selectedEvents.includes(option.value)"
              @change="toggleEvent(option.value, ($event.target as HTMLInputElement).checked)"
            />
            <span>{{ option.label }}</span>
          </label>
        </div>
        <p class="flex gap-2 rounded-xl border border-cyan-300/10 bg-cyan-300/[0.05] p-3 text-[11px] leading-5 text-cyan-100/60 light:text-cyan-900">
          <Info class="mt-0.5 h-3.5 w-3.5 shrink-0" />
          Task-created events are intentionally unavailable to prevent automation loops.
        </p>
      </fieldset>

      <fieldset v-else-if="node.type === 'condition'" :disabled="readonly" class="space-y-4">
        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Event field</span>
          <select
            :value="config.field"
            class="automation-inspector-input"
            @change="updateConditionField(($event.target as HTMLSelectElement).value)"
          >
            <option value="" disabled>Choose a field</option>
            <option v-for="field in conditionFields" :key="field.value" :value="field.value">
              {{ field.label }}
            </option>
          </select>
        </label>
        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Operator</span>
          <select
            :value="config.operator"
            class="automation-inspector-input"
            @change="updateConditionOperator(($event.target as HTMLSelectElement).value)"
          >
            <option value="" disabled>Choose an operator</option>
            <option v-for="operator in availableConditionOperators" :key="operator.value" :value="operator.value">
              {{ operator.label }}
            </option>
          </select>
        </label>
        <label v-if="operatorNeedsValue" class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Expected value</span>
          <select
            v-if="conditionIsBooleanField"
            :value="String(config.value ?? false)"
            class="automation-inspector-input"
            @change="patch({ value: ($event.target as HTMLSelectElement).value === 'true' })"
          >
            <option value="false">No — customer can receive marketing</option>
            <option value="true">Yes — customer opted out</option>
          </select>
          <input
            v-else
            :value="Array.isArray(config.value) ? config.value.join(', ') : config.value"
            class="automation-inspector-input"
            :placeholder="config.operator === 'in' ? 'completed, no_show' : 'e.g. completed'"
            @input="updateConditionValue(($event.target as HTMLInputElement).value)"
          />
          <span v-if="config.operator === 'in'" class="mt-1.5 block text-[10px] text-white/30 light:text-gray-500">
            Separate allowed values with commas.
          </span>
        </label>
        <div class="rounded-xl border border-amber-300/10 bg-amber-300/[0.05] p-3">
          <p class="text-[10px] font-bold uppercase tracking-[0.15em] text-amber-200 light:text-amber-800">Branch behavior</p>
          <p class="mt-1.5 text-[11px] leading-5 text-white/40 light:text-gray-600">
            Match follows the green path. An unconnected “No match” path ends safely with no action.
          </p>
        </div>
      </fieldset>

      <fieldset v-else-if="node.type === 'delay'" :disabled="readonly" class="space-y-4">
        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Wait time in minutes</span>
          <input
            type="number"
            min="0"
            max="43200"
            step="1"
            :value="config.minutes ?? 0"
            class="automation-inspector-input"
            @input="patch({ minutes: numberValue(($event.target as HTMLInputElement).value) })"
          />
        </label>
        <div class="grid grid-cols-3 gap-2">
          <button
            v-for="preset in [{ label: '1 hour', value: 60 }, { label: '1 day', value: 1440 }, { label: '3 days', value: 4320 }]"
            :key="preset.value"
            type="button"
            class="cursor-pointer rounded-xl border border-white/[0.08] bg-white/[0.025] px-2 py-2 text-[10px] font-medium text-white/45 outline-none transition hover:border-violet-300/30 hover:text-violet-200 focus-visible:ring-2 focus-visible:ring-violet-300 light:border-gray-200 light:bg-gray-50 light:text-gray-600"
            @click="patch({ minutes: preset.value })"
          >
            {{ preset.label }}
          </button>
        </div>
        <p class="flex gap-2 rounded-xl bg-violet-300/[0.05] p-3 text-[11px] leading-5 text-white/40 light:text-gray-600">
          <Clock3 class="mt-0.5 h-3.5 w-3.5 shrink-0 text-violet-300 light:text-violet-700" />
          Delays are durable. A restart will not lose or duplicate scheduled work.
        </p>
      </fieldset>

      <fieldset v-else-if="node.type === 'create_task'" :disabled="readonly" class="space-y-4">
        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Follow-up title</span>
          <input
            :value="config.title"
            maxlength="255"
            class="automation-inspector-input"
            placeholder="Review customer follow-up"
            @input="patch({ title: ($event.target as HTMLInputElement).value })"
          />
        </label>
        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Instructions</span>
          <textarea
            :value="String(config.description ?? '')"
            maxlength="4000"
            rows="4"
            class="automation-inspector-input h-auto resize-y py-2.5"
            placeholder="Give the teammate enough context to act safely."
            @input="patch({ description: ($event.target as HTMLTextAreaElement).value })"
          />
        </label>
        <div class="grid grid-cols-2 gap-3">
          <label>
            <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Priority</span>
            <select
              :value="config.priority ?? 'normal'"
              class="automation-inspector-input"
              @change="patch({ priority: ($event.target as HTMLSelectElement).value })"
            >
              <option value="low">Low</option>
              <option value="normal">Normal</option>
              <option value="high">High</option>
              <option value="urgent">Urgent</option>
            </select>
          </label>
          <label>
            <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Owner</span>
            <select
              :value="config.owner ?? 'unassigned'"
              class="automation-inspector-input"
              @change="patch({ owner: ($event.target as HTMLSelectElement).value })"
            >
              <option value="unassigned">Unassigned queue</option>
              <option value="contact_owner">Contact owner</option>
            </select>
          </label>
        </div>
        <div class="grid grid-cols-2 gap-3">
          <label>
            <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Due after (min)</span>
            <input
              type="number"
              min="0"
              max="525600"
              :value="config.due_in_minutes ?? ''"
              class="automation-inspector-input"
              placeholder="Optional"
              @input="patch({ due_in_minutes: optionalNumberValue(($event.target as HTMLInputElement).value) })"
            />
          </label>
          <label>
            <span class="mb-1.5 block text-xs font-medium text-white/55 light:text-gray-700">Remind after (min)</span>
            <input
              type="number"
              min="0"
              max="525600"
              :value="config.remind_in_minutes ?? ''"
              class="automation-inspector-input"
              placeholder="Optional"
              @input="patch({ remind_in_minutes: optionalNumberValue(($event.target as HTMLInputElement).value) })"
            />
          </label>
        </div>
        <div class="rounded-xl border border-emerald-300/10 bg-emerald-300/[0.05] p-3">
          <p class="flex items-center gap-2 text-[10px] font-bold uppercase tracking-[0.15em] text-emerald-200 light:text-emerald-800">
            <LockKeyhole class="h-3.5 w-3.5" />
            Internal action only
          </p>
          <p class="mt-1.5 text-[11px] leading-5 text-white/40 light:text-gray-600">
            This creates a CRM follow-up. It does not draft or send a customer message.
          </p>
        </div>
      </fieldset>
    </div>
  </aside>
</template>

<style scoped>
.automation-inspector-input {
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
  transition: border-color 180ms ease, box-shadow 180ms ease;
}

.automation-inspector-input:focus-visible {
  border-color: rgba(217, 249, 157, 0.5);
  box-shadow: 0 0 0 3px rgba(217, 249, 157, 0.09);
}

.automation-inspector-input:disabled {
  cursor: not-allowed;
  opacity: 0.55;
}

:global(.light) .automation-inspector-input {
  border-color: rgb(229 231 235);
  background: white;
  color: rgb(17 24 39);
}
</style>
