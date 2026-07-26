<script setup lang="ts">
withDefaults(defineProps<{
  compact?: boolean
  size?: 'sm' | 'md' | 'lg'
  tone?: 'light' | 'dark'
  tagline?: boolean
}>(), {
  compact: false,
  size: 'md',
  tone: 'light',
  tagline: false
})

const markSizes = {
  sm: 'h-7 w-7',
  md: 'h-9 w-9',
  lg: 'h-12 w-12'
}

const wordmarkSizes = {
  sm: 'text-[17px]',
  md: 'text-[22px]',
  lg: 'text-[30px]'
}
</script>

<template>
  <div class="inline-flex items-center gap-2.5" aria-label="ReReply">
    <img
      src="/brand/rereply-mark.png"
      alt=""
      :class="[
        markSizes[size],
        'shrink-0 object-contain',
        tone === 'light' ? 'brand-mark-light' : ''
      ]"
    >
    <div v-if="!compact" class="min-w-0">
      <div
        :class="[
          wordmarkSizes[size],
          'brand-wordmark leading-none tracking-[-0.035em]',
          tone === 'light' ? 'text-[#f2f2e9]' : 'text-[#292d1e]'
        ]"
      >
        ReReply
      </div>
      <div
        v-if="tagline"
        :class="[
          'mt-1 text-[9px] font-semibold uppercase tracking-[0.2em]',
          tone === 'light' ? 'text-[#cbd49a]/70' : 'text-[#697046]'
        ]"
      >
        Conversations, aligned
      </div>
    </div>
  </div>
</template>
