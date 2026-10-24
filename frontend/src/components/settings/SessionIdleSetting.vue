<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Loader2 } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { api, readSessionId } from '@/services/api'
import { readSelectedOrganizationId } from '@/lib/browserIdentity'
import {
  DEFAULT_SESSION_IDLE_TIMEOUT_MINUTES,
  SESSION_IDLE_TIMEOUT_OPTIONS,
  currentPolicyIdentity,
  normalizeSessionIdleTimeout,
  rememberSessionIdlePolicy,
} from '@/lib/sessionActivity'
import { formatIdleDuration } from '@/lib/sessionSignout'

// Organization policy "Sign out after inactivity". Shown to roles that can
// write the General settings (admins); stored in the organization settings.
// The current value comes from the settings the page already loaded (null
// while that load is in flight), so this control adds no request.
const props = defineProps<{ initialMinutes: number | null }>()
const emit = defineEmits<{ saved: [] }>()

const { t } = useI18n()
const selected = ref(String(DEFAULT_SESSION_IDLE_TIMEOUT_MINUTES))
const saved = ref(selected.value)
const isLoading = computed(() => props.initialMinutes === null)
const isSaving = ref(false)

watch(
  () => props.initialMinutes,
  (minutes) => {
    if (minutes === null) return
    selected.value = String(normalizeSessionIdleTimeout(minutes))
    saved.value = selected.value
  },
  { immediate: true },
)

function optionLabel(minutes: number) {
  return minutes === 0 ? t('sessionSecurity.idleSettingOff') : formatIdleDuration(minutes, t)
}

async function save() {
  isSaving.value = true
  // The organization and session this save is made for. If either changes
  // before it completes (an org switch, a new login), the result belongs to
  // them and is not applied here.
  const identity = currentPolicyIdentity()
  const session = readSessionId()
  // A super admin viewing another organization saves that organization's
  // policy, not the one their own session follows (keyed by the organization
  // /me names): /me stays the source for theirs.
  const viewed = readSelectedOrganizationId()
  const ownOrganization = !viewed || identity === `${identity.split('|')[0]}|${viewed}`
  const stillCurrent = () => currentPolicyIdentity() === identity &&
    readSessionId() === session && readSelectedOrganizationId() === viewed
  try {
    const minutes = Number(selected.value)
    await api.put('/org/settings', { session_idle_timeout_minutes: minutes })
    if (!stillCurrent()) return
    saved.value = selected.value
    // Apply it in this admin's own tabs straight away.
    if (ownOrganization) rememberSessionIdlePolicy(minutes, identity)
    toast.success(t('sessionSecurity.idleSettingSaved'))
    emit('saved')
  } catch {
    if (stillCurrent()) toast.error(t('sessionSecurity.idleSettingSaveFailed'))
  } finally {
    isSaving.value = false
  }
}
</script>

<template>
  <div
    data-testid="session-idle-setting"
    class="rounded-xl border border-white/[0.08] bg-white/[0.02] light:bg-white light:border-gray-200"
  >
    <div class="p-6 pb-3">
      <h3 class="text-lg font-semibold text-white light:text-gray-900">
        {{ t('sessionSecurity.idleSettingTitle') }}
      </h3>
      <p class="text-sm text-white/40 light:text-gray-500">
        {{ t('sessionSecurity.idleSettingDesc') }}
      </p>
    </div>
    <div class="flex flex-wrap items-center justify-between gap-4 p-6 pt-3">
      <Select v-model="selected" :disabled="isLoading || isSaving">
        <SelectTrigger
          data-testid="session-idle-select"
          :aria-label="t('sessionSecurity.idleSettingTitle')"
          class="w-56 bg-white/[0.04] border-white/[0.1] text-white/70 light:bg-white light:border-gray-200 light:text-gray-700"
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent
          class="bg-[#141414] border-white/[0.08] light:bg-white light:border-gray-200"
        >
          <SelectItem
            v-for="minutes in SESSION_IDLE_TIMEOUT_OPTIONS"
            :key="minutes"
            :value="String(minutes)"
            class="text-white/70 focus:bg-white/[0.08] focus:text-white light:text-gray-700 light:focus:bg-gray-100"
          >
            {{ optionLabel(minutes) }}
          </SelectItem>
        </SelectContent>
      </Select>
      <Button
        type="button"
        variant="outline"
        size="sm"
        data-testid="session-idle-save"
        class="bg-white/[0.04] border-white/[0.1] text-white/70 hover:bg-white/[0.08] hover:text-white light:bg-white light:border-gray-200 light:text-gray-700 light:hover:bg-gray-50"
        :disabled="isLoading || isSaving || selected === saved"
        @click="save"
      >
        <Loader2 v-if="isSaving" class="mr-2 h-4 w-4 animate-spin" />
        {{ t('sessionSecurity.idleSettingSave') }}
      </Button>
    </div>
  </div>
</template>
