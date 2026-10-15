<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { ChevronDown, MailCheck, MailOpen } from 'lucide-vue-next'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { contactHasUnread } from '@/lib/chatInboxState'
import type { Contact } from '@/stores/contacts'

// Per-row menu for the chat list. It sits absolutely over the row's right
// edge so the row keeps its height. The row selects on click, so the
// trigger's click (mouse or keyboard activation) stops propagation; the menu
// content is portaled out of the row.
const props = withDefaults(
  defineProps<{
    contact: Pick<Contact, 'id' | 'unread_count' | 'manual_unread'>
    // Mark as unread changes team-shared state and needs chat:write.
    canMarkUnread?: boolean
  }>(),
  { canMarkUnread: true },
)

const emit = defineEmits<{
  'mark-read': [contactId: string]
  'mark-unread': [contactId: string]
}>()

const { t } = useI18n()

const showMarkRead = computed(() => contactHasUnread(props.contact))
const showMarkUnread = computed(() => !showMarkRead.value && props.canMarkUnread)
</script>

<template>
  <DropdownMenu v-if="showMarkRead || showMarkUnread">
    <DropdownMenuTrigger as-child>
      <button
        type="button"
        data-testid="chat-row-actions"
        :data-contact-id="contact.id"
        :aria-label="t('chat.readState.actions')"
        :title="t('chat.readState.actions')"
        class="absolute right-1.5 top-1/2 flex h-6 w-6 -translate-y-1/2 items-center justify-center rounded-md bg-[#151517] text-white/60 opacity-0 shadow-sm transition-opacity hover:text-white focus-visible:opacity-100 group-hover:opacity-100 data-[state=open]:opacity-100 [@media(hover:none)]:opacity-100 light:bg-white light:text-gray-500 light:hover:text-gray-900"
        @click.stop
      >
        <ChevronDown class="h-3.5 w-3.5" aria-hidden="true" />
      </button>
    </DropdownMenuTrigger>
    <DropdownMenuContent align="end" class="w-44" data-testid="chat-row-actions-menu">
      <DropdownMenuItem
        v-if="showMarkRead"
        data-testid="chat-row-mark-read"
        @select="emit('mark-read', contact.id)"
      >
        <MailCheck class="mr-2 h-4 w-4" aria-hidden="true" />
        <span>{{ t('chat.readState.markRead') }}</span>
      </DropdownMenuItem>
      <DropdownMenuItem
        v-if="showMarkUnread"
        data-testid="chat-row-mark-unread"
        @select="emit('mark-unread', contact.id)"
      >
        <MailOpen class="mr-2 h-4 w-4" aria-hidden="true" />
        <span>{{ t('chat.readState.markUnread') }}</span>
      </DropdownMenuItem>
    </DropdownMenuContent>
  </DropdownMenu>
</template>
