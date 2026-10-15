<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Bold, Code, Italic, Strikethrough } from 'lucide-vue-next'
import type { WhatsAppFormatKind } from '@/lib/whatsappFormat'

withDefaults(defineProps<{ disabled?: boolean }>(), { disabled: false })

const emit = defineEmits<{ format: [kind: WhatsAppFormatKind] }>()

const { t } = useI18n()

const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.userAgent)
const mod = isMac ? '⌘' : 'Ctrl+'

const actions = computed(() => [
  { kind: 'bold' as const, icon: Bold, label: t('chat.composer.bold'), shortcut: `${mod}B` },
  { kind: 'italic' as const, icon: Italic, label: t('chat.composer.italic'), shortcut: `${mod}I` },
  {
    kind: 'strike' as const,
    icon: Strikethrough,
    label: t('chat.composer.strikethrough'),
    shortcut: isMac ? '⇧⌘X' : 'Ctrl+Shift+X',
  },
  { kind: 'mono' as const, icon: Code, label: t('chat.composer.monospace'), shortcut: '' },
])
</script>

<template>
  <div
    role="toolbar"
    :aria-label="t('chat.composer.formatting')"
    class="flex items-center gap-0.5"
    data-testid="composer-format-toolbar"
  >
    <!-- mousedown.prevent keeps the textarea focused with its selection intact. -->
    <button
      v-for="action in actions"
      :key="action.kind"
      type="button"
      class="flex h-7 w-7 items-center justify-center rounded-md text-white/40 transition-colors hover:bg-white/[0.08] hover:text-white/80 disabled:pointer-events-none disabled:opacity-40 light:text-gray-500 light:hover:bg-gray-200 light:hover:text-gray-800"
      :aria-label="action.label"
      :title="action.shortcut ? `${action.label} (${action.shortcut})` : action.label"
      :disabled="disabled"
      :data-format="action.kind"
      @mousedown.prevent
      @click="emit('format', action.kind)"
    >
      <component :is="action.icon" class="h-3.5 w-3.5" aria-hidden="true" />
    </button>
  </div>
</template>
