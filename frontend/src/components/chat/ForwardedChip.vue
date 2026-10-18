<script setup lang="ts">
import { Forward } from 'lucide-vue-next'
import type { ForwardDeliveryState } from './messageForward'

// Internal CRM marker. WhatsApp offers no settable "Forwarded" label, so the
// customer receives an ordinary message; only agents see this chip. A stalled
// forward also says that its status is unknown and it won't be resent.
defineProps<{
  state?: ForwardDeliveryState
}>()
</script>

<template>
  <div class="mb-1" data-testid="chat-message-forwarded">
    <div class="flex items-center gap-1 text-[11px] italic opacity-70" :title="$t('chat.forward.chipHint')">
      <Forward class="h-3 w-3" aria-hidden="true" />
      <span>{{ $t('chat.forward.chip') }}</span>
    </div>
    <p
      v-if="state"
      class="mt-0.5 text-[11px] text-amber-600 light:text-amber-700"
      :data-forward-delivery-state="state"
    >
      {{ state === 'delayed' ? $t('chat.forward.delayedNote') : $t('chat.forward.uncertainDeliveryNote') }}
    </p>
  </div>
</template>
