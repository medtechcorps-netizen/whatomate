<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import {
  exchangeSsoCode,
  isTabSessionCurrent,
  keepSsoCode,
  ssoExchangeInterrupted,
  unsentSsoCode,
  SESSION_BUSY_CODE,
  SESSION_UNSUPPORTED_CODE,
} from '@/services/api'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Loader2, AlertCircle, CheckCircle } from 'lucide-vue-next'
import { toast } from 'vue-sonner'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()

const status = ref<'loading' | 'success' | 'error'>('loading')
const errorMessage = ref('')

const homeFor = (user: { role?: { name?: string } } | null | undefined) =>
  user?.role?.name === 'agent' ? '/analytics/agents' : '/'

// The session this page signed in was replaced (another tab signed in): it
// is not this tab's to enter.
const toLogin = () => {
  const basePath = ((window as any).__BASE_PATH__ ?? '').replace(/\/$/, '')
  window.location.assign(basePath + '/login')
}

onMounted(async () => {
  // The SSO redirect set no cookie: it handed over a one-time code, bound to
  // this browser. Take it off the URL (it is single-use; kept in this tab
  // until its exchange is sent, so a reload while it waits retries it), then
  // exchange it for the session, ordered like a password sign-in.
  const fromUrl = typeof route.query.code === 'string' ? route.query.code : ''
  if (fromUrl) {
    keepSsoCode(fromUrl)
    // Off the URL before the exchange is sent (history, Referer).
    await router.replace({ query: { ...route.query, code: undefined } }).catch(() => undefined)
  }
  const code = fromUrl || unsentSsoCode()
  if (!code) {
    // A reload while the exchange was out: its answer was never seen here,
    // so neither the session from before nor a "used link" can be assumed.
    if (ssoExchangeInterrupted()) {
      status.value = 'error'
      errorMessage.value = t('auth.ssoInterrupted')
      return
    }
    // Back after a sign-in, or a reload once it finished: a live session
    // this tab shows goes on into the app; otherwise the link is spent.
    if ((authStore.isAuthenticated && isTabSessionCurrent()) || authStore.restoreSession()) {
      void router.replace(homeFor(authStore.user))
      return
    }
    status.value = 'error'
    errorMessage.value = t('auth.ssoLinkUsed')
    return
  }
  try {
    // The answer is a sign-in's ({expires_in, user}): its user is applied at
    // once, as a password sign-in does, with no request in between.
    const response = await exchangeSsoCode(code)
    const user = response.data.data.user

    // Set auth in store (only user — tokens are in httpOnly cookies). The
    // tab shows the session its exchange just began: nothing ran in between.
    authStore.setAuth({ user })

    status.value = 'success'
    toast.success(t('auth.ssoLoginSuccess'))

    // Redirect based on role, unless the session was replaced meanwhile.
    setTimeout(() => {
      if (!isTabSessionCurrent()) {
        toLogin()
        return
      }
      router.push(homeFor(user))
    }, 1000)
  } catch (error: any) {
    status.value = 'error'
    if (error?.code === SESSION_UNSUPPORTED_CODE) {
      errorMessage.value = t('errors.unsupportedBrowser')
    } else if (
      error?.code === SESSION_BUSY_CODE ||
      error?.code === 'ERR_CANCELED' ||
      error?.code === 'ECONNABORTED' ||
      error?.code === 'ETIMEDOUT'
    ) {
      errorMessage.value = t('errors.timeout')
    } else {
      errorMessage.value = error.response?.data?.message || t('auth.ssoLoginFailed')
    }
  }
})
</script>

<template>
  <div class="min-h-screen flex items-center justify-center bg-gradient-to-br from-gray-900 to-gray-800 light:from-violet-50 light:to-violet-100 p-4">
    <Card class="w-full max-w-md">
      <CardHeader class="text-center">
        <div class="flex justify-center mb-4">
          <div v-if="status === 'loading'" class="h-12 w-12 rounded-xl bg-primary/10 flex items-center justify-center">
            <Loader2 class="h-7 w-7 text-primary animate-spin" />
          </div>
          <div v-else-if="status === 'success'" class="h-12 w-12 rounded-xl bg-green-900/30 light:bg-green-100 flex items-center justify-center">
            <CheckCircle class="h-7 w-7 text-green-400 light:text-green-600" />
          </div>
          <div v-else class="h-12 w-12 rounded-xl bg-red-900/30 light:bg-red-100 flex items-center justify-center">
            <AlertCircle class="h-7 w-7 text-red-400 light:text-red-600" />
          </div>
        </div>
        <CardTitle class="text-xl">
          <template v-if="status === 'loading'">{{ $t('auth.ssoLoading') }}</template>
          <template v-else-if="status === 'success'">{{ $t('auth.ssoSuccess') }}</template>
          <template v-else>{{ $t('auth.ssoFailed') }}</template>
        </CardTitle>
        <CardDescription>
          <template v-if="status === 'loading'">{{ $t('auth.ssoLoadingDesc') }}</template>
          <template v-else-if="status === 'success'">{{ $t('auth.ssoSuccessDesc') }}</template>
          <template v-else>{{ errorMessage }}</template>
        </CardDescription>
      </CardHeader>
      <CardContent v-if="status === 'error'" class="text-center">
        <RouterLink to="/login" class="text-primary hover:underline">
          {{ $t('auth.returnToLogin') }}
        </RouterLink>
      </CardContent>
    </Card>
  </div>
</template>
