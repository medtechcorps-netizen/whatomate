<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { ContactListFilter } from '@/stores/contacts'

// Chip bar under the chat search box. "All" is the default list; the other
// views are server-side filters, so totals and paging stay correct.
const props = withDefaults(
  defineProps<{
    modelValue?: ContactListFilter
    // Users limited to their own chats have nothing unassigned to see.
    showUnassigned?: boolean
  }>(),
  { modelValue: 'all', showUnassigned: true },
)

const emit = defineEmits<{
  'update:modelValue': [filter: ContactListFilter]
}>()

const { t } = useI18n()

interface FilterOption {
  value: ContactListFilter
  label: string
  testId: string
}

const options = computed<FilterOption[]>(() => {
  const list: FilterOption[] = [
    { value: 'all', label: t('chat.filters.all'), testId: 'chat-filter-all' },
    { value: 'unread', label: t('chat.filters.unread'), testId: 'chat-filter-unread' },
    { value: 'mine', label: t('chat.filters.mine'), testId: 'chat-filter-mine' },
  ]
  if (props.showUnassigned) {
    list.push({ value: 'unassigned', label: t('chat.filters.unassigned'), testId: 'chat-filter-unassigned' })
  }
  return list
})

const activeFilter = computed(() => props.modelValue ?? 'all')

function select(filter: ContactListFilter) {
  if (filter !== activeFilter.value) emit('update:modelValue', filter)
}

function chipClass(filter: ContactListFilter) {
  return filter === activeFilter.value
    ? 'bg-emerald-500/15 text-emerald-300 light:bg-emerald-100 light:text-emerald-800'
    : 'text-white/50 hover:bg-white/[0.06] hover:text-white light:text-gray-500 light:hover:bg-gray-100 light:hover:text-gray-900'
}
</script>

<template>
  <div
    class="mt-2 flex items-center gap-1 overflow-x-auto"
    role="group"
    :aria-label="t('chat.filters.label')"
    data-testid="chat-filters"
  >
    <template v-for="option in options" :key="option.value">
      <Tooltip v-if="option.value === 'unread'">
        <TooltipTrigger as-child>
          <button
            type="button"
            :data-testid="option.testId"
            :aria-pressed="option.value === activeFilter"
            :class="['h-6 shrink-0 rounded-full px-2.5 text-[11px] font-medium transition-colors', chipClass(option.value)]"
            @click="select(option.value)"
          >
            {{ option.label }}
          </button>
        </TooltipTrigger>
        <TooltipContent class="max-w-64 text-xs">{{ t('chat.filters.unreadHint') }}</TooltipContent>
      </Tooltip>
      <button
        v-else
        type="button"
        :data-testid="option.testId"
        :aria-pressed="option.value === activeFilter"
        :class="['h-6 shrink-0 rounded-full px-2.5 text-[11px] font-medium transition-colors', chipClass(option.value)]"
        @click="select(option.value)"
      >
        {{ option.label }}
      </button>
    </template>
  </div>
</template>
