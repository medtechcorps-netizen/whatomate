<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { User } from 'lucide-vue-next'
import type { Contact } from '@/stores/contacts'

// Compact owner marker for a chat. An active transfer is the current human
// handover, so its agent wins over the stored assignee when both are set.
const props = withDefaults(
  defineProps<{
    contact: Pick<Contact, 'assigned_user_id' | 'assigned_user_name' | 'transfer_agent_id' | 'transfer_agent_name'> | null
    currentUserId?: string | null
    variant?: 'row' | 'header'
  }>(),
  { currentUserId: null, variant: 'row' },
)

const { t } = useI18n()

const owner = computed(() => {
  const contact = props.contact
  if (!contact) return null
  if (contact.transfer_agent_id) {
    return { id: contact.transfer_agent_id, name: contact.transfer_agent_name || '', viaTransfer: true }
  }
  if (contact.assigned_user_id) {
    return { id: contact.assigned_user_id, name: contact.assigned_user_name || '', viaTransfer: false }
  }
  return null
})

const isMine = computed(() => Boolean(owner.value && props.currentUserId && owner.value.id === props.currentUserId))

const label = computed(() => {
  if (!owner.value) return ''
  if (isMine.value) return t('chat.assignment.you')
  const firstName = owner.value.name.trim().split(/\s+/)[0]
  return firstName || t('chat.assignment.teammate')
})

const description = computed(() => {
  if (!owner.value) return ''
  if (isMine.value) return t('chat.assignment.assignedToYou')
  const name = owner.value.name.trim() || t('chat.assignment.teammate')
  return owner.value.viaTransfer
    ? t('chat.assignment.handledBy', { name })
    : t('chat.assignment.assignedTo', { name })
})

const badgeClass = computed(() => {
  const tone = isMine.value
    ? 'bg-sky-500/15 text-sky-300 light:bg-sky-100 light:text-sky-800'
    : 'bg-white/[0.06] text-white/60 light:bg-gray-100 light:text-gray-600'
  return props.variant === 'header'
    ? `h-5 px-1.5 text-[10px] rounded-md ${tone}`
    : `h-4 max-w-[84px] px-1 text-[10px] leading-4 rounded ${tone}`
})
</script>

<template>
  <span
    v-if="owner"
    data-testid="chat-assignee-badge"
    :data-assignee-kind="isMine ? 'you' : 'other'"
    :class="['inline-flex shrink-0 items-center gap-0.5 font-medium', badgeClass]"
    :title="description"
    :aria-label="description"
  >
    <User class="h-2.5 w-2.5 shrink-0" aria-hidden="true" />
    <span class="truncate">{{ label }}</span>
  </span>
</template>
