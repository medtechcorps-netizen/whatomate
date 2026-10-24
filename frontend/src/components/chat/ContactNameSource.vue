<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { BookUser } from 'lucide-vue-next'
import type { Contact } from '@/stores/contacts'

// Shown under a contact's display name: where the name came from when the
// WhatsApp Business app supplied it, and the customer's own WhatsApp profile
// name when it differs from the name shown.
const props = withDefaults(
  defineProps<{
    contact: Pick<
      Contact,
      'name_source' | 'wa_profile_name' | 'wa_business_saved_name_removed'
    > | null
    displayName: string
    align?: 'center' | 'start'
  }>(),
  { align: 'center' },
)

const { t } = useI18n()

const profileName = computed(() => {
  const name = props.contact?.wa_profile_name?.trim() || ''
  return name && name !== props.displayName.trim() ? name : ''
})

const fromWhatsAppBusiness = computed(() => props.contact?.name_source === 'wa_business')
const removed = computed(() => Boolean(props.contact?.wa_business_saved_name_removed))
</script>

<template>
  <div
    v-if="fromWhatsAppBusiness || profileName"
    class="mt-1 flex flex-col gap-1"
    :class="align === 'start' ? 'items-start' : 'items-center'"
    data-testid="contact-name-source"
  >
    <span
      v-if="fromWhatsAppBusiness"
      class="inline-flex h-5 items-center gap-1 rounded-md px-1.5 text-[10px] font-medium bg-emerald-500/15 text-emerald-300 light:bg-emerald-50 light:text-emerald-700"
      :title="removed ? t('contacts.nameSource.removedFromWhatsAppBusinessHint') : t('contacts.nameSource.savedInWhatsAppBusinessHint')"
      data-testid="contact-name-source-wa-business"
    >
      <BookUser class="h-3 w-3 shrink-0" aria-hidden="true" />
      {{ removed ? t('contacts.nameSource.removedFromWhatsAppBusiness') : t('contacts.nameSource.savedInWhatsAppBusiness') }}
    </span>
    <p
      v-if="profileName"
      class="max-w-full truncate text-xs text-muted-foreground"
      data-testid="contact-wa-profile-name"
    >
      {{ t('contacts.nameSource.whatsappProfile', { name: profileName }) }}
    </p>
  </div>
</template>
