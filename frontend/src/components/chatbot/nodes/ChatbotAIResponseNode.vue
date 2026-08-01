<script setup lang="ts">
import { computed } from 'vue'
import { Sparkles } from 'lucide-vue-next'
import BaseNode from '@/components/calling/nodes/BaseNode.vue'

defineOptions({ inheritAttrs: false })

const props = defineProps<{ data: any }>()

const summary = computed(() => {
  const template = props.data?.config?.prompt_template ?? props.data?.config?.prompt
  if (typeof template !== 'string' || !template.trim()) return 'Latest inbound message'
  const text = template.trim().replace(/\s+/g, ' ')
  return text.length > 72 ? `${text.slice(0, 69)}...` : text
})
</script>

<template>
  <BaseNode
    :label="data?.label || 'AI response'"
    header-class="bg-purple-600"
    :has-input="!data?.isEntryNode"
  >
    <template #icon><Sparkles class="h-4 w-4" /></template>
    <p class="truncate" :title="summary">{{ summary }}</p>
    <p class="mt-1 text-[10px]">Uses workspace AI settings</p>
  </BaseNode>
</template>
