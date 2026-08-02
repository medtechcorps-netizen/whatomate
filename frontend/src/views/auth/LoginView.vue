<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useAuthStore } from '@/stores/auth'
import { api } from '@/services/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Separator } from '@/components/ui/separator'
import { toast } from 'vue-sonner'
import { ArrowRight, Building2, CheckCircle2, Loader2, Search } from 'lucide-vue-next'
import ReReplyLogo from '@/components/brand/ReReplyLogo.vue'

const { t } = useI18n()

interface SSOProvider {
  provider: string
  name: string
}

const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()

const email = ref('')
const password = ref('')
const isLoading = ref(false)
const ssoProviders = ref<SSOProvider[]>([])
const organization = ref('')
const isSSODiscovering = ref(false)
const hasDiscoveredSSO = ref(false)

// SSO provider icons (using simple SVG paths)
const providerIcons: Record<string, string> = {
  google: 'M12.545,10.239v3.821h5.445c-0.712,2.315-2.647,3.972-5.445,3.972c-3.332,0-6.033-2.701-6.033-6.032s2.701-6.032,6.033-6.032c1.498,0,2.866,0.549,3.921,1.453l2.814-2.814C17.503,2.988,15.139,2,12.545,2C7.021,2,2.543,6.477,2.543,12s4.478,10,10.002,10c8.396,0,10.249-7.85,9.426-11.748L12.545,10.239z',
  microsoft: 'M11 11H3V3h8v8zm10 0h-8V3h8v8zM11 21H3v-8h8v8zm10 0h-8v-8h8v8z',
  github: 'M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z',
  facebook: 'M24 12.073c0-6.627-5.373-12-12-12s-12 5.373-12 12c0 5.99 4.388 10.954 10.125 11.854v-8.385H7.078v-3.47h3.047V9.43c0-3.007 1.792-4.669 4.533-4.669 1.312 0 2.686.235 2.686.235v2.953H15.83c-1.491 0-1.956.925-1.956 1.874v2.25h3.328l-.532 3.47h-2.796v8.385C19.612 23.027 24 18.062 24 12.073z',
  custom: 'M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8zm-1-13h2v6h-2zm0 8h2v2h-2z'
}

// Dark-first: default is dark mode, light: prefix for light mode
const providerColors: Record<string, string> = {
  google: 'hover:bg-red-950 border-red-800 light:hover:bg-red-50 light:border-red-200',
  microsoft: 'hover:bg-blue-950 border-blue-800 light:hover:bg-blue-50 light:border-blue-200',
  github: 'hover:bg-gray-800 border-gray-600 light:hover:bg-gray-100 light:border-gray-300',
  facebook: 'hover:bg-blue-950 border-blue-800 light:hover:bg-blue-50 light:border-blue-200',
  custom: 'hover:bg-purple-950 border-purple-800 light:hover:bg-purple-50 light:border-purple-200'
}

const discoverSSOProviders = async () => {
  const selector = organization.value.trim().toLowerCase()
  if (!selector) {
    ssoProviders.value = []
    hasDiscoveredSSO.value = false
    toast.error(t('auth.workspaceCodeRequired'))
    return
  }

  isSSODiscovering.value = true
  try {
    const response = await api.get('/auth/sso/providers', {
      params: { organization: selector }
    })
    ssoProviders.value = response.data.data || []
    hasDiscoveredSSO.value = true
  } catch {
    ssoProviders.value = []
    hasDiscoveredSSO.value = false
    toast.error(t('auth.ssoDiscoveryFailed'))
  } finally {
    isSSODiscovering.value = false
  }
}

watch(organization, () => {
  if (!isSSODiscovering.value) {
    ssoProviders.value = []
    hasDiscoveredSSO.value = false
  }
})

onMounted(async () => {
  // Check for SSO error in query params
  const ssoError = route.query.sso_error as string
  if (ssoError) {
    toast.error(decodeURIComponent(ssoError))
    // Clear the error from URL
    router.replace({ query: { ...route.query, sso_error: undefined } })
  }

  const queryOrganization = Array.isArray(route.query.organization)
    ? route.query.organization[0]
    : route.query.organization
  if (typeof queryOrganization === 'string' && queryOrganization.trim()) {
    organization.value = queryOrganization
    await discoverSSOProviders()
  }
})

const handleLogin = async () => {
  if (!email.value || !password.value) {
    toast.error(t('auth.enterEmailPassword'))
    return
  }

  isLoading.value = true

  try {
    await authStore.login(email.value, password.value)
    toast.success(t('auth.loginSuccess'))

    const redirect = route.query.redirect as string
    router.push(redirect || '/')
  } catch (error: any) {
    const message = error.response?.data?.message || t('auth.invalidCredentials')
    toast.error(message)
  } finally {
    isLoading.value = false
  }
}

const initiateSSO = (provider: string) => {
  const selector = organization.value.trim().toLowerCase()
  if (!selector) {
    toast.error(t('auth.workspaceCodeRequired'))
    return
  }
  const basePath = ((window as any).__BASE_PATH__ ?? '').replace(/\/$/, '')
  window.location.href = `${basePath}/api/auth/sso/${encodeURIComponent(provider)}/init?organization=${encodeURIComponent(selector)}`
}
</script>

<template>
  <div class="auth-shell relative min-h-screen overflow-hidden bg-[#10120d] light:bg-[#f3f2e9]">
    <div class="auth-grid absolute inset-0 opacity-50 light:opacity-30" aria-hidden="true" />
    <div class="auth-glow auth-glow-top absolute -top-44 right-[-8rem] h-[34rem] w-[34rem] rounded-full" aria-hidden="true" />
    <div class="auth-glow auth-glow-bottom absolute -bottom-52 left-[-12rem] h-[36rem] w-[36rem] rounded-full" aria-hidden="true" />

    <div class="relative z-10 mx-auto grid min-h-screen w-full max-w-[1440px] lg:grid-cols-[minmax(0,0.92fr)_minmax(540px,1.08fr)]">
      <section class="flex min-h-screen flex-col px-6 py-7 sm:px-10 lg:px-14 xl:px-20">
        <div class="flex items-center justify-between">
          <ReReplyLogo size="md" tone="light" tagline class="light:hidden" />
          <ReReplyLogo size="md" tone="dark" tagline class="hidden light:inline-flex" />
          <div class="rounded-full border border-[#cbd49a]/20 bg-[#cbd49a]/[0.06] px-3 py-1 text-[10px] font-semibold uppercase tracking-[0.16em] text-[#cbd49a]/80 light:border-[#697046]/20 light:bg-[#697046]/[0.05] light:text-[#596039]">
            Secure workspace
          </div>
        </div>

        <div class="my-auto w-full max-w-[440px] py-16">
          <div class="mb-9">
            <p class="mb-3 flex items-center gap-2 text-[11px] font-semibold uppercase tracking-[0.2em] text-[#cbd49a] light:text-[#697046]">
              <span class="h-px w-8 bg-current opacity-60" />
              Welcome back
            </p>
            <h1 class="auth-title max-w-sm text-[2.55rem] leading-[1.04] tracking-[-0.045em] text-[#f4f4eb] light:text-[#24271b] sm:text-[3.25rem]">
              Pick up every conversation with context.
            </h1>
            <p class="mt-5 max-w-sm text-[15px] leading-6 text-white/48 light:text-[#535646]">
              Sign in to your shared WhatsApp inbox, customer records, and automated journeys.
            </p>
          </div>

          <div class="rounded-[1.4rem] border border-white/[0.09] bg-white/[0.035] p-5 shadow-2xl shadow-black/20 backdrop-blur-xl light:border-[#697046]/15 light:bg-white/70 light:shadow-[#545a37]/10 sm:p-6">
            <form @submit.prevent="handleLogin">
              <div class="space-y-5">
                <div class="space-y-2">
                  <Label for="email" class="text-[11px] font-semibold uppercase tracking-[0.13em] text-white/55 light:text-[#565a43]">
                    {{ $t('common.email') }}
                  </Label>
                  <Input
                    id="email"
                    v-model="email"
                    type="email"
                    :placeholder="$t('auth.emailPlaceholder')"
                    :disabled="isLoading"
                    autocomplete="email"
                    class="h-12 rounded-xl border-white/[0.1] bg-black/20 px-4 text-white placeholder:text-white/25 focus-visible:border-[#cbd49a]/55 focus-visible:ring-[#cbd49a]/20 light:border-[#697046]/20 light:bg-white/80 light:text-[#25281b] light:placeholder:text-[#697046]/40"
                  />
                </div>
                <div class="space-y-2">
                  <Label for="password" class="text-[11px] font-semibold uppercase tracking-[0.13em] text-white/55 light:text-[#565a43]">
                    {{ $t('auth.password') }}
                  </Label>
                  <Input
                    id="password"
                    v-model="password"
                    type="password"
                    :placeholder="$t('auth.passwordPlaceholder')"
                    :disabled="isLoading"
                    autocomplete="current-password"
                    class="h-12 rounded-xl border-white/[0.1] bg-black/20 px-4 text-white placeholder:text-white/25 focus-visible:border-[#cbd49a]/55 focus-visible:ring-[#cbd49a]/20 light:border-[#697046]/20 light:bg-white/80 light:text-[#25281b] light:placeholder:text-[#697046]/40"
                  />
                </div>
                <Button
                  type="submit"
                  class="group h-12 w-full rounded-xl bg-[#697046] text-white shadow-lg shadow-[#697046]/20 transition-all hover:-translate-y-0.5 hover:bg-[#77804f] hover:shadow-xl hover:shadow-[#697046]/25"
                  :disabled="isLoading"
                >
                  <Loader2 v-if="isLoading" class="mr-2 h-4 w-4 animate-spin" />
                  <template v-else>
                    {{ $t('auth.signIn') }}
                    <ArrowRight class="ml-2 h-4 w-4 transition-transform group-hover:translate-x-0.5" />
                  </template>
                </Button>
              </div>
            </form>

            <div class="mt-5 space-y-3">
              <div class="relative my-5">
                <Separator class="bg-white/[0.08] light:bg-[#697046]/15" />
                <span class="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 bg-[#171912] px-2 text-xs text-white/35 light:bg-[#f8f7f0] light:text-[#697046]/60">
                  {{ $t('auth.orContinueWith') }}
                </span>
              </div>

              <form class="space-y-3" data-testid="sso-workspace-discovery" @submit.prevent="discoverSSOProviders">
                <div class="space-y-2">
                  <Label for="sso-organization" class="flex items-center gap-2 text-[11px] font-semibold uppercase tracking-[0.13em] text-white/55 light:text-[#565a43]">
                    <Building2 class="h-3.5 w-3.5 text-[#cbd49a] light:text-[#697046]" />
                    {{ $t('auth.workspaceCode') }}
                  </Label>
                  <div class="flex gap-2">
                    <Input
                      id="sso-organization"
                      v-model="organization"
                      name="organization"
                      type="text"
                      :placeholder="$t('auth.workspaceCodePlaceholder')"
                      :disabled="isSSODiscovering"
                      autocomplete="off"
                      autocapitalize="none"
                      spellcheck="false"
                      class="h-11 min-w-0 flex-1 rounded-xl border-white/[0.1] bg-black/20 px-4 text-white placeholder:text-white/25 focus-visible:border-[#cbd49a]/55 focus-visible:ring-[#cbd49a]/20 light:border-[#697046]/20 light:bg-white/80 light:text-[#25281b] light:placeholder:text-[#697046]/40"
                      data-testid="sso-workspace-code"
                    />
                    <Button
                      type="submit"
                      variant="outline"
                      class="h-11 shrink-0 rounded-xl border-[#cbd49a]/25 bg-[#cbd49a]/[0.06] px-3 text-[#dce3ae] hover:bg-[#cbd49a]/[0.12] hover:text-[#f2f5d8] light:border-[#697046]/25 light:bg-[#697046]/[0.05] light:text-[#596039] light:hover:bg-[#697046]/10"
                      :disabled="isSSODiscovering || !organization.trim()"
                      data-testid="sso-discover-button"
                    >
                      <Loader2 v-if="isSSODiscovering" class="h-4 w-4 animate-spin" />
                      <Search v-else class="h-4 w-4" />
                      <span class="sr-only">{{ $t('auth.findSignInMethods') }}</span>
                    </Button>
                  </div>
                  <p class="text-xs leading-5 text-white/35 light:text-[#666a53]">
                    {{ $t('auth.workspaceCodeHint') }}
                  </p>
                </div>
              </form>

              <p
                v-if="hasDiscoveredSSO && ssoProviders.length === 0"
                class="rounded-xl border border-white/[0.08] bg-black/10 px-3 py-2.5 text-xs leading-5 text-white/45 light:border-[#697046]/15 light:bg-[#697046]/[0.04] light:text-[#666a53]"
                data-testid="sso-no-providers"
              >
                {{ $t('auth.noWorkspaceSSO') }}
              </p>

              <Button
                v-for="provider in ssoProviders"
                :key="provider.provider"
                variant="outline"
                class="w-full justify-start gap-3 transition-colors bg-white/[0.04] border-white/[0.1] text-white/70 hover:bg-white/[0.08] hover:text-white light:bg-white light:border-[#697046]/15 light:text-[#4e5233] light:hover:bg-[#697046]/5"
                :class="providerColors[provider.provider] || providerColors.custom"
                type="button"
                :data-testid="`sso-provider-${provider.provider}`"
                @click="initiateSSO(provider.provider)"
              >
                <svg class="h-5 w-5" viewBox="0 0 24 24" fill="currentColor">
                  <path :d="providerIcons[provider.provider] || providerIcons.custom" />
                </svg>
                {{ provider.name }}
              </Button>
            </div>
          </div>

          <p class="mt-6 text-center text-sm text-white/35 light:text-[#666a53]">
            {{ $t('auth.noAccount') }}
            <RouterLink to="/register" class="text-[#cbd49a] hover:text-[#e0e6ba] hover:underline light:text-[#697046]">
              {{ $t('auth.signUp') }}
            </RouterLink>
          </p>
        </div>

        <div class="flex items-center gap-2 text-[11px] text-white/30 light:text-[#697046]/65">
          <span class="h-1.5 w-1.5 rounded-full bg-[#aeba69]" />
          Encrypted sessions · Role-based access · Tenant isolation
        </div>
      </section>

      <aside class="relative hidden min-h-screen overflow-hidden border-l border-white/[0.07] bg-[#171912] lg:flex lg:flex-col">
        <div class="absolute inset-0 bg-[radial-gradient(circle_at_65%_20%,rgba(203,212,154,0.13),transparent_36%),linear-gradient(145deg,transparent_20%,rgba(105,112,70,0.08)_80%)]" />
        <div class="auth-orbit absolute right-[-18%] top-[8%] h-[38rem] w-[38rem] rounded-full border border-[#cbd49a]/10" aria-hidden="true" />
        <div class="auth-orbit auth-orbit-inner absolute right-[-5%] top-[21%] h-[24rem] w-[24rem] rounded-full border border-[#cbd49a]/10" aria-hidden="true" />
        <img src="/brand/rereply-mark.png" alt="" class="absolute -right-14 top-20 h-[31rem] w-[31rem] object-contain opacity-[0.055]" aria-hidden="true">

        <div class="relative z-10 mt-auto p-14 xl:p-20">
          <p class="mb-5 text-[11px] font-semibold uppercase tracking-[0.24em] text-[#cbd49a]/70">
            Customer conversations, aligned
          </p>
          <h2 class="auth-title max-w-[590px] text-5xl leading-[1.06] tracking-[-0.045em] text-[#f0f0e6] xl:text-[4.35rem]">
            Reply with the whole relationship in view.
          </h2>
          <p class="mt-7 max-w-lg text-base leading-7 text-white/45">
            ReReply brings WhatsApp conversations, customer context, automation, and human handover into one focused workspace.
          </p>

          <div class="mt-12 grid max-w-xl grid-cols-3 gap-3">
            <div v-for="item in ['Shared inbox', 'Smart journeys', 'Clear ownership']" :key="item" class="rounded-2xl border border-white/[0.07] bg-white/[0.025] p-4">
              <CheckCircle2 class="mb-7 h-4 w-4 text-[#cbd49a]" />
              <p class="text-xs font-medium leading-5 text-white/58">{{ item }}</p>
            </div>
          </div>
        </div>
      </aside>
    </div>
  </div>
</template>
