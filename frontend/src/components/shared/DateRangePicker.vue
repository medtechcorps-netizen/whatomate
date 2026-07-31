<script setup lang="ts">
import { useMediaQuery } from '@vueuse/core'
import { useI18n } from 'vue-i18n'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { RangeCalendar } from '@/components/ui/range-calendar'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { CalendarIcon } from 'lucide-vue-next'
import type { TimeRangePreset } from '@/composables/useDateRange'

defineProps<{
  selectedRange: TimeRangePreset
  customDateRange: any
  isDatePickerOpen: boolean
  formatDateRangeDisplay: string
}>()

const emit = defineEmits<{
  'update:selectedRange': [value: TimeRangePreset]
  'update:customDateRange': [value: any]
  'update:isDatePickerOpen': [value: boolean]
  'apply-custom': []
}>()

const { t } = useI18n()
const isCompactCalendar = useMediaQuery('(max-width: 639px)')
</script>

<template>
  <Select
    :model-value="selectedRange"
    @update:model-value="emit('update:selectedRange', $event as TimeRangePreset)"
  >
    <SelectTrigger class="w-full min-w-[132px] sm:w-[140px]">
      <SelectValue :placeholder="t('dateRange.selectRange', 'Date Range')" />
    </SelectTrigger>
    <SelectContent>
      <SelectItem value="today">{{ t('dateRange.today', 'Today') }}</SelectItem>
      <SelectItem value="7days">{{ t('dateRange.last7Days', 'Last 7 Days') }}</SelectItem>
      <SelectItem value="30days">{{ t('dateRange.last30Days', 'Last 30 Days') }}</SelectItem>
      <SelectItem value="this_month">{{ t('dateRange.thisMonth', 'This Month') }}</SelectItem>
      <SelectItem value="custom">{{ t('dateRange.customRange', 'Custom Range') }}</SelectItem>
    </SelectContent>
  </Select>
  <Popover
    v-if="selectedRange === 'custom'"
    :open="isDatePickerOpen"
    @update:open="emit('update:isDatePickerOpen', $event)"
  >
    <PopoverTrigger as-child>
      <Button variant="outline" size="sm">
        <CalendarIcon class="h-4 w-4 mr-1" />
        {{ formatDateRangeDisplay || t('common.select') }}
      </Button>
    </PopoverTrigger>
    <PopoverContent class="max-w-[calc(100vw-2rem)] overflow-x-auto p-3 sm:w-auto sm:p-4" align="end">
      <div class="space-y-4">
        <RangeCalendar
          :model-value="customDateRange"
          @update:model-value="emit('update:customDateRange', $event)"
          :number-of-months="isCompactCalendar ? 1 : 2"
        />
        <Button
          class="w-full"
          size="sm"
          @click="emit('apply-custom')"
          :disabled="!customDateRange?.start || !customDateRange?.end"
        >
          {{ t('dateRange.apply', 'Apply') }}
        </Button>
      </div>
    </PopoverContent>
  </Popover>
</template>
