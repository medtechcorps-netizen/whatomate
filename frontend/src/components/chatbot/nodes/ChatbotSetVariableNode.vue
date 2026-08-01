<script setup lang="ts">
import { computed } from 'vue'
import { Variable } from 'lucide-vue-next'
import BaseNode from '@/components/calling/nodes/BaseNode.vue'

defineOptions({ inheritAttrs: false })

const props = defineProps<{ data: any }>()

const assignments = computed(() => {
  const set = props.data?.config?.set
  if (!set || typeof set !== 'object' || Array.isArray(set)) return []
  return Object.entries(set)
})

function formatValue(value: unknown): string {
  if (typeof value === 'string') return value || '""'
  try {
    return JSON.stringify(value)
  } catch {
    return String(value)
  }
}

const summary = computed(() => {
  if (assignments.value.length === 0) return 'No assignments'
  const [name, value] = assignments.value[0]
  const first = `${name || '(unnamed)'} = ${formatValue(value)}`
  const suffix = assignments.value.length > 1 ? ` +${assignments.value.length - 1} more` : ''
  const text = first + suffix
  return text.length > 72 ? `${text.slice(0, 69)}...` : text
})
</script>

<template>
  <BaseNode
    :label="data?.label || 'Set variable'"
    header-class="bg-teal-600"
    :has-input="!data?.isEntryNode"
  >
    <template #icon><Variable class="h-4 w-4" /></template>
    <p class="truncate font-mono" :title="summary">{{ summary }}</p>
    <p class="mt-1 text-[10px]">{{ assignments.length }} assignment{{ assignments.length === 1 ? '' : 's' }}</p>
  </BaseNode>
</template>
