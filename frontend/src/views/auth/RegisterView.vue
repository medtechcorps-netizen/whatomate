<script setup lang="ts">
import { ref, computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { toast } from 'vue-sonner'
import { Loader2 } from 'lucide-vue-next'
import ReReplyLogo from '@/components/brand/ReReplyLogo.vue'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()

const fullName = ref('')
const email = ref('')
const password = ref('')
const confirmPassword = ref('')
const isLoading = ref(false)

const invitationToken = computed(() => (route.query.invite as string) || '')

const handleRegister = async () => {
  if (!invitationToken.value) {
    toast.error(t('auth.invitationRequired'))
    return
  }

  if (!fullName.value || !email.value || !password.value) {
    toast.error(t('auth.fillAllFields'))
    return
  }

  if (password.value !== confirmPassword.value) {
    toast.error(t('auth.passwordsMismatch'))
    return
  }

  if (password.value.length < 12) {
    toast.error(t('auth.passwordTooShort'))
    return
  }

  isLoading.value = true

  try {
    await authStore.register({
      full_name: fullName.value,
      email: email.value,
      password: password.value,
      invitation_token: invitationToken.value
    })
    toast.success(t('auth.registrationSuccess'))
    router.push('/')
  } catch (error: any) {
    const message = error.response?.data?.message || t('auth.registrationFailed')
    toast.error(message)
  } finally {
    isLoading.value = false
  }
}
</script>

<template>
  <div class="auth-shell relative min-h-screen flex items-center justify-center overflow-hidden bg-[#10120d] light:bg-[#f3f2e9] p-4">
    <div class="auth-grid absolute inset-0 opacity-50 light:opacity-30" aria-hidden="true" />
    <div class="auth-glow auth-glow-top absolute -top-44 right-[-8rem] h-[34rem] w-[34rem] rounded-full" aria-hidden="true" />
    <Card class="relative z-10 w-full max-w-md rounded-[1.4rem] border-white/[0.09] bg-[#171912]/90 shadow-2xl shadow-black/25 backdrop-blur-xl light:border-[#697046]/15 light:bg-white/80 light:shadow-[#545a37]/10">
      <CardHeader class="space-y-1 text-center">
        <div class="flex justify-center mb-4">
          <ReReplyLogo size="lg" tone="light" tagline class="light:hidden" />
          <ReReplyLogo size="lg" tone="dark" tagline class="hidden light:inline-flex" />
        </div>
        <CardTitle class="auth-title text-3xl tracking-[-0.035em] text-[#f4f4eb] light:text-[#24271b]">{{ $t('auth.createAccount') }}</CardTitle>
        <CardDescription class="text-white/45 light:text-[#5f634d]">
          {{ $t('auth.createAccountDesc') }}
        </CardDescription>
      </CardHeader>

      <!-- No invitation token in URL - show invitation required message -->
      <template v-if="!invitationToken">
        <CardContent>
          <div class="text-center py-4">
            <p class="text-sm text-white/45 light:text-[#5f634d]">
              {{ $t('auth.invitationRequired') }}
            </p>
          </div>
        </CardContent>
        <CardFooter class="flex flex-col space-y-4">
          <RouterLink to="/login" class="w-full">
            <Button variant="outline" class="w-full border-white/[0.1] bg-white/[0.03] text-white hover:bg-white/[0.07] light:border-[#697046]/20 light:bg-white light:text-[#323624] light:hover:bg-[#697046]/5">
              {{ $t('auth.signIn') }}
            </Button>
          </RouterLink>
        </CardFooter>
      </template>

      <!-- Has a signed invitation token — show registration form -->
      <form v-else @submit.prevent="handleRegister">
        <CardContent class="space-y-4">
          <div class="space-y-2">
            <Label for="fullName">{{ $t('auth.fullName') }}</Label>
            <Input
              id="fullName"
              v-model="fullName"
              type="text"
              :placeholder="$t('auth.fullNamePlaceholder')"
              :disabled="isLoading"
              autocomplete="name"
            />
          </div>
          <div class="space-y-2">
            <Label for="email">{{ $t('common.email') }}</Label>
            <Input
              id="email"
              v-model="email"
              type="email"
              :placeholder="$t('auth.emailPlaceholder')"
              :disabled="isLoading"
              autocomplete="email"
            />
          </div>
          <div class="space-y-2">
            <Label for="password">{{ $t('auth.password') }}</Label>
            <Input
              id="password"
              v-model="password"
              type="password"
              :placeholder="$t('auth.passwordMinLength')"
              :disabled="isLoading"
              autocomplete="new-password"
            />
          </div>
          <div class="space-y-2">
            <Label for="confirmPassword">{{ $t('auth.confirmPassword') }}</Label>
            <Input
              id="confirmPassword"
              v-model="confirmPassword"
              type="password"
              :placeholder="$t('auth.confirmPasswordPlaceholder')"
              :disabled="isLoading"
              autocomplete="new-password"
            />
          </div>
        </CardContent>
        <CardFooter class="flex flex-col space-y-4">
          <Button type="submit" class="w-full bg-[#697046] text-white hover:bg-[#77804f]" :disabled="isLoading">
            <Loader2 v-if="isLoading" class="mr-2 h-4 w-4 animate-spin" />
            {{ $t('auth.createAccountBtn') }}
          </Button>
          <p class="text-sm text-center text-white/40 light:text-[#5f634d]">
            {{ $t('auth.alreadyHaveAccount') }}
            <RouterLink to="/login" class="text-[#cbd49a] hover:underline light:text-[#697046]">
              {{ $t('auth.signIn') }}
            </RouterLink>
          </p>
        </CardFooter>
      </form>
    </Card>
  </div>
</template>
