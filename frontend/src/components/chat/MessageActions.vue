<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { Loader2, Reply, RotateCw, SmilePlus } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { Message } from '@/stores/contacts'

/**
 * Per-bubble action strip (react, reply, retry). It keeps the established
 * footprint beside the bubble: a 24px-wide column, visible but muted at rest
 * and at full strength when the row is hovered or focused. Touch screens have
 * no hover, so the strip is always at full strength there. Reply stays a real
 * <button> with the lucide-reply icon inside the message list; the
 * chat-composer E2E spec relies on that.
 */
const props = defineProps<{
  message: Message
  quickReactions: readonly string[]
  reactionOpen: boolean
  retrying?: boolean
}>()

const emit = defineEmits<{
  reply: [message: Message]
  react: [messageId: string, emoji: string]
  retry: [message: Message]
  'update:reactionOpen': [open: boolean]
}>()

const { t } = useI18n()

const reactLabel = computed(() => t('chat.messageActions.react'))
const replyLabel = computed(() => t('chat.reply'))
const retryLabel = computed(() => t('chat.retry'))

const canRetry = computed(() =>
  props.message.direction === 'outgoing' &&
  props.message.status === 'failed' &&
  props.message.message_type !== 'template',
)
</script>

<template>
  <div
    data-testid="chat-message-actions"
    :class="[
      'message-actions flex flex-col gap-0.5 transition-opacity self-center ml-1',
      reactionOpen && 'is-open',
    ]"
  >
    <Tooltip>
      <TooltipTrigger as-child>
        <!-- The span anchors the tooltip; the button anchors the popover.
             Nesting both triggers on one element breaks the popover anchor. -->
        <span class="inline-flex">
          <Popover :open="reactionOpen" @update:open="(open: boolean) => emit('update:reactionOpen', open)">
            <PopoverTrigger as-child>
              <Button
                variant="ghost"
                size="icon"
                class="h-6 w-6"
                :aria-label="reactLabel"
              >
                <SmilePlus class="h-3 w-3" aria-hidden="true" />
              </Button>
            </PopoverTrigger>
            <PopoverContent side="top" class="w-auto p-2">
              <div class="flex gap-1" role="group" :aria-label="reactLabel">
                <button
                  v-for="emoji in quickReactions"
                  :key="emoji"
                  type="button"
                  class="text-lg hover:bg-muted p-1 rounded cursor-pointer"
                  @click="emit('react', message.id, emoji)"
                >
                  {{ emoji }}
                </button>
              </div>
            </PopoverContent>
          </Popover>
        </span>
      </TooltipTrigger>
      <TooltipContent>{{ reactLabel }}</TooltipContent>
    </Tooltip>
    <Tooltip>
      <TooltipTrigger as-child>
        <Button
          variant="ghost"
          size="icon"
          class="h-6 w-6"
          data-testid="chat-message-reply"
          :aria-label="replyLabel"
          @click="emit('reply', message)"
        >
          <Reply class="h-3 w-3" aria-hidden="true" />
        </Button>
      </TooltipTrigger>
      <TooltipContent>{{ replyLabel }}</TooltipContent>
    </Tooltip>
    <Tooltip v-if="canRetry">
      <TooltipTrigger as-child>
        <Button
          variant="ghost"
          size="icon"
          class="h-6 w-6 text-destructive hover:text-destructive"
          :disabled="retrying"
          :aria-label="retryLabel"
          @click="emit('retry', message)"
        >
          <Loader2 v-if="retrying" class="h-3 w-3 animate-spin" aria-hidden="true" />
          <RotateCw v-else class="h-3 w-3" aria-hidden="true" />
        </Button>
      </TooltipTrigger>
      <TooltipContent>{{ retryLabel }}</TooltipContent>
    </Tooltip>
  </div>
</template>

<style scoped>
/*
 * Reply and React stay visible at rest, muted, so agents can find them; they
 * come to full strength on row hover, keyboard focus or an open picker. These
 * are scoped rules rather than Tailwind's group-hover: with the locked
 * postcss-selector-parser 6.1.3, Tailwind 3.4 emits no group-* or peer-* rules.
 */
.message-actions {
  opacity: 0.6;
}

.group:hover .message-actions,
.message-actions:focus-within,
.message-actions.is-open {
  opacity: 1;
}

/* Touch screens cannot hover, so the actions are always at full strength. */
@media (hover: none) {
  .message-actions {
    opacity: 1;
  }
}
</style>
