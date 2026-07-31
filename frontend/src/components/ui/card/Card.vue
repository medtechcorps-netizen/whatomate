<script setup lang="ts">
import { cn } from '@/lib/utils'

const props = defineProps<{
  class?: string
}>()
</script>

<template>
  <div
    :class="cn(
      'rounded-xl border border-white/[0.06] bg-white/[0.02] text-card-foreground backdrop-blur-sm shadow-xl shadow-black/20 transition-all duration-300 hover:border-white/[0.15] hover:bg-white/[0.04] hover:shadow-2xl hover:shadow-emerald-500/5 light:bg-slate-50 light:border-slate-300 light:shadow-slate-300/40 light:hover:border-slate-400 light:hover:shadow-lg light:hover:shadow-slate-300/60',
      props.class
    )"
  >
    <slot />
  </div>
</template>
