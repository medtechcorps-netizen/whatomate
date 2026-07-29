<script setup lang="ts">
import { computed } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import {
  BellRing,
  CheckSquare2,
  Clock3,
  GitBranch,
} from 'lucide-vue-next'
import type { AutomationNodeType } from '@/services/automationPolicies'

const props = defineProps<{
  type: AutomationNodeType
  data: {
    config?: Record<string, unknown>
    selected?: boolean
  }
  selected?: boolean
}>()

const presentation = computed(() => {
  switch (props.type) {
    case 'trigger':
      return {
        label: 'Customer event',
        eyebrow: 'WHEN',
        icon: BellRing,
        accent: 'automation-node-cyan',
      }
    case 'condition':
      return {
        label: 'Guard',
        eyebrow: 'ONLY IF',
        icon: GitBranch,
        accent: 'automation-node-amber',
      }
    case 'delay':
      return {
        label: 'Wait',
        eyebrow: 'THEN',
        icon: Clock3,
        accent: 'automation-node-violet',
      }
    default:
      return {
        label: 'Create follow-up',
        eyebrow: 'DO',
        icon: CheckSquare2,
        accent: 'automation-node-emerald',
      }
  }
})

const summary = computed(() => {
  const config = props.data?.config ?? {}
  if (props.type === 'trigger') {
    if (Array.isArray(config.event_types)) {
      return config.event_types.map(String).join(' or ')
    }
    return String(config.event_type || 'Choose an event')
  }
  if (props.type === 'condition') {
    const field = String(config.field || 'Choose a field').replace('event.', '')
    const operator = String(config.operator || 'operator').replace(/_/g, ' ')
    const value = config.value === undefined ? '' : ` ${String(config.value)}`
    return `${field} ${operator}${value}`
  }
  if (props.type === 'delay') {
    const minutes = Number(config.minutes ?? 0)
    if (minutes >= 1440 && minutes % 1440 === 0) {
      return `${minutes / 1440} ${minutes === 1440 ? 'day' : 'days'}`
    }
    if (minutes >= 60 && minutes % 60 === 0) {
      return `${minutes / 60} ${minutes === 60 ? 'hour' : 'hours'}`
    }
    return `${minutes} ${minutes === 1 ? 'minute' : 'minutes'}`
  }
  return String(config.title || 'Name the follow-up task')
})

const hasInput = computed(() => props.type !== 'trigger')
const hasOutput = computed(() => props.type !== 'create_task')
</script>

<template>
  <article
    class="automation-policy-node group w-[238px] overflow-visible rounded-2xl border bg-[#101419]/95 shadow-2xl shadow-black/30 backdrop-blur transition duration-200 light:bg-white"
    :class="[
      presentation.accent,
      { 'automation-policy-node-selected': selected || data?.selected },
    ]"
    :aria-label="`${presentation.label}: ${summary}`"
  >
    <Handle
      v-if="hasInput"
      id="input"
      type="target"
      :position="Position.Left"
      class="automation-handle !h-3.5 !w-3.5 !border-[3px] !border-[#101419] !bg-slate-400 light:!border-white"
    />

    <div class="flex items-center gap-3 border-b border-white/[0.07] px-4 py-3 light:border-black/[0.07]">
      <span class="automation-node-icon flex h-8 w-8 shrink-0 items-center justify-center rounded-xl">
        <component :is="presentation.icon" class="h-4 w-4" />
      </span>
      <div class="min-w-0">
        <p class="text-[9px] font-bold tracking-[0.2em] text-white/35 light:text-gray-500">
          {{ presentation.eyebrow }}
        </p>
        <p class="mt-0.5 truncate text-xs font-semibold text-white light:text-gray-900">
          {{ presentation.label }}
        </p>
      </div>
    </div>

    <p class="min-h-[52px] break-words px-4 py-3 text-xs leading-5 text-white/60 light:text-gray-600">
      {{ summary }}
    </p>

    <template v-if="type === 'condition'">
      <Handle
        id="true"
        type="source"
        :position="Position.Right"
        :style="{ top: '38%' }"
        class="automation-handle !h-3.5 !w-3.5 !border-[3px] !border-[#101419] !bg-emerald-400 light:!border-white"
      />
      <Handle
        id="false"
        type="source"
        :position="Position.Right"
        :style="{ top: '76%' }"
        class="automation-handle !h-3.5 !w-3.5 !border-[3px] !border-[#101419] !bg-slate-500 light:!border-white"
      />
      <span class="absolute -right-12 top-[29%] text-[9px] font-bold uppercase tracking-wider text-emerald-300">
        Match
      </span>
      <span class="absolute -right-14 top-[68%] text-[9px] font-bold uppercase tracking-wider text-white/35 light:text-gray-500">
        No match
      </span>
    </template>
    <Handle
      v-else-if="hasOutput"
      id="always"
      type="source"
      :position="Position.Right"
      class="automation-handle !h-3.5 !w-3.5 !border-[3px] !border-[#101419] !bg-lime-300 light:!border-white"
    />
  </article>
</template>

<style scoped>
.automation-policy-node {
  border-color: rgba(255, 255, 255, 0.11);
}

.automation-policy-node:hover,
.automation-policy-node-selected {
  border-color: var(--node-accent);
  box-shadow:
    0 0 0 1px var(--node-accent-soft),
    0 22px 50px rgba(0, 0, 0, 0.42),
    0 0 32px var(--node-glow);
}

.automation-node-icon {
  color: var(--node-accent);
  background: var(--node-accent-soft);
  box-shadow: inset 0 0 0 1px var(--node-accent-soft);
}

.automation-node-cyan {
  --node-accent: #67e8f9;
  --node-accent-soft: rgba(34, 211, 238, 0.13);
  --node-glow: rgba(34, 211, 238, 0.12);
}

.automation-node-amber {
  --node-accent: #fcd34d;
  --node-accent-soft: rgba(251, 191, 36, 0.13);
  --node-glow: rgba(251, 191, 36, 0.1);
}

.automation-node-violet {
  --node-accent: #c4b5fd;
  --node-accent-soft: rgba(167, 139, 250, 0.14);
  --node-glow: rgba(139, 92, 246, 0.12);
}

.automation-node-emerald {
  --node-accent: #86efac;
  --node-accent-soft: rgba(74, 222, 128, 0.13);
  --node-glow: rgba(34, 197, 94, 0.11);
}

@media (prefers-reduced-motion: reduce) {
  .automation-policy-node {
    transition: none;
  }
}
</style>
