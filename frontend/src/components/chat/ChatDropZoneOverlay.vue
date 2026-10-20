<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { Upload } from 'lucide-vue-next'
import { useChatDropZone } from '@/composables/useChatDropZone'

// Lives inside the chat panel (which must be position: relative) and turns
// the whole panel into a file drop target.
const props = defineProps<{
  target: HTMLElement | null
  recipientName?: string
}>()

const emit = defineEmits<{ files: [files: File[]] }>()

const { t } = useI18n()

const { isActive } = useChatDropZone(
  () => props.target,
  files => emit('files', files),
)
</script>

<template>
  <div
    v-if="isActive"
    class="pointer-events-none absolute inset-2 z-40 flex flex-col items-center justify-center gap-2 rounded-xl border-2 border-dashed border-emerald-500/70 bg-[#0f0f10]/85 text-emerald-300 light:bg-white/90 light:text-emerald-700"
    data-testid="composer-drop-overlay"
    aria-hidden="true"
  >
    <Upload class="h-8 w-8" />
    <p class="text-sm font-medium">{{ t('chat.composer.dropFiles') }}</p>
    <p v-if="recipientName" class="max-w-[80%] truncate text-xs opacity-80">
      {{ t('chat.composer.dropRecipient', { name: recipientName }) }}
    </p>
  </div>
</template>
