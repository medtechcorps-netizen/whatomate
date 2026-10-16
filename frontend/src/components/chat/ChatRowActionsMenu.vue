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

// Per-row menu for the chat list. The trigger is an ordinary flex item at the
// row's right edge, always visible at low emphasis: it never sits over the
// timestamp or unread badge, and it never relies on Tailwind group-/peer-
// variants, which the production CSS build drops. It is smaller than the
// avatar, so the row keeps its height. The row selects on click, so the
// trigger's click (mouse or keyboard activation) stops propagation; the menu
// content is portaled out of the row.
const props = withDefaults(
  defineProps<{
    contact: Pick<Contact, 'id' | 'unread_count' | 'manual_unread'>
    // Read state is shared by the whole team and needs chat:write.
    canChangeReadState?: boolean
  }>(),
  { canChangeReadState: true },
)

const emit = defineEmits<{
  'mark-read': [contactId: string]
  'mark-unread': [contactId: string]
}>()

const { t } = useI18n()

const showMarkRead = computed(() => contactHasUnread(props.contact))
</script>

<template>
  <DropdownMenu v-if="canChangeReadState">
    <DropdownMenuTrigger as-child>
      <button
        type="button"
        data-testid="chat-row-actions"
        :data-contact-id="contact.id"
        :aria-label="t('chat.readState.actions')"
        :title="t('chat.readState.actions')"
        class="flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-white/30 transition-colors hover:bg-white/[0.08] hover:text-white focus-visible:text-white data-[state=open]:bg-white/[0.08] data-[state=open]:text-white light:text-gray-400 light:hover:bg-gray-100 light:hover:text-gray-900 light:data-[state=open]:bg-gray-100 light:data-[state=open]:text-gray-900"
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
        v-else
        data-testid="chat-row-mark-unread"
        @select="emit('mark-unread', contact.id)"
      >
        <MailOpen class="mr-2 h-4 w-4" aria-hidden="true" />
        <span>{{ t('chat.readState.markUnread') }}</span>
      </DropdownMenuItem>
    </DropdownMenuContent>
  </DropdownMenu>
</template>
