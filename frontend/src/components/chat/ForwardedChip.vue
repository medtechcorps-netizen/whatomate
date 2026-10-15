<script setup lang="ts">
import { Forward } from 'lucide-vue-next'

// Internal CRM marker. WhatsApp offers no settable "Forwarded" label, so the
// customer receives an ordinary message; only agents see this chip.
</script>

<template>
  <div
    class="mb-1 flex items-center gap-1 text-[11px] italic opacity-70"
    data-testid="chat-message-forwarded"
    :title="$t('chat.forward.chipHint')"
  >
    <Forward class="h-3 w-3" aria-hidden="true" />
    <span>{{ $t('chat.forward.chip') }}</span>
  </div>
</template>
