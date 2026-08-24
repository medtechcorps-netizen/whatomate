<script setup lang="ts">
import { computed } from 'vue'

const props = withDefaults(defineProps<{
  count: number
  compact?: boolean
}>(), {
  compact: false,
})

const displayCount = computed(() => (props.count > 99 ? '99+' : String(props.count)))
</script>

<template>
  <span
    v-if="count > 0"
    :class="[
      'ml-auto inline-flex h-5 min-w-5 shrink-0 items-center justify-center rounded-full bg-sky-300 px-1.5 text-[10px] font-bold tabular-nums text-slate-950 shadow-[0_0_0_1px_rgba(125,211,252,0.2)] motion-safe:animate-in motion-safe:zoom-in-75 motion-safe:duration-200',
      compact && 'lg:absolute lg:right-1 lg:top-1 lg:ml-0 lg:h-2 lg:min-w-2 lg:p-0 lg:text-[0px] lg:shadow-[0_0_0_2px_#0a0a0b] light:lg:shadow-[0_0_0_2px_#f8fafc]',
    ]"
    aria-hidden="true"
  >
    {{ displayCount }}
  </span>
</template>
