<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  ArrowRight,
  Check,
  AlertCircle,
  HeartPulse,
  LifeBuoy,
  Loader2,
  LockKeyhole,
  Rocket,
  ShieldCheck,
  Sparkles,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { Progress } from '@/components/ui/progress'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import {
  onboardingService,
  privacyService,
  productService,
  supportService,
  type OnboardingSummary,
  type PrivacyRequest,
  type SubscriptionSummary,
  type SupportCase,
  type TenantHealth,
  type WorkspaceTemplateSummary,
} from '@/services/productSuite'

const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const applyingTemplate = ref('')
const onboarding = ref<OnboardingSummary | null>(null)
const subscription = ref<SubscriptionSummary | null>(null)
const health = ref<TenantHealth | null>(null)
const templates = ref<WorkspaceTemplateSummary[]>([])
const privacyRequests = ref<PrivacyRequest[]>([])
const supportCases = ref<SupportCase[]>([])
const launchChannelOptions = [
  { value: 'whatsapp', label: 'WhatsApp' },
  { value: 'instagram', label: 'Instagram' },
  { value: 'messenger', label: 'Messenger' },
  { value: 'threads', label: 'Threads' },
  { value: 'email', label: 'Email' },
  { value: 'webchat', label: 'Web chat' },
]
const profileDraft = ref({
  business_name: '',
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Kuala_Lumpur',
  vertical: 'general',
  intended_channels: [] as string[],
})
const canManageOnboarding = computed(() => authStore.hasPermission('onboarding', 'write'))
const canReadBilling = computed(() => authStore.hasPermission('billing', 'read'))
const canReadTemplates = computed(() => authStore.hasPermission('workspace_templates', 'read'))
const canApplyTemplates = computed(() => authStore.hasPermission('workspace_templates', 'write'))
const canReadPrivacyRequests = computed(() => authStore.hasPermission('privacy.requests', 'read'))
const canReadSupport = computed(() => authStore.hasPermission('support', 'read'))

const completedSteps = computed(() => onboarding.value?.steps.filter((step) => step.completed).length ?? 0)
const totalSteps = computed(() => onboarding.value?.steps.length ?? 0)
const healthTone = computed(() => {
  if (health.value?.status === 'healthy') return 'text-emerald-300 bg-emerald-400/10 border-emerald-400/20'
  if (health.value?.status === 'degraded') return 'text-red-300 bg-red-400/10 border-red-400/20'
  return 'text-amber-300 bg-amber-400/10 border-amber-400/20'
})

async function load() {
  loading.value = true
  try {
    const [onboardingResponse, subscriptionResponse, healthResponse, templatesResponse, privacyResponse, supportResponse] =
      await Promise.all([
        onboardingService.get(),
        canReadBilling.value ? productService.subscription() : Promise.resolve(null),
        canReadSupport.value ? supportService.health() : Promise.resolve(null),
        canReadTemplates.value ? onboardingService.templates() : Promise.resolve(null),
        canReadPrivacyRequests.value ? privacyService.requests({ limit: 5 }) : Promise.resolve(null),
        canReadSupport.value ? supportService.cases({ limit: 5 }) : Promise.resolve(null),
      ])

    onboarding.value = unwrapItemResponse<OnboardingSummary>(onboardingResponse)
    subscription.value = subscriptionResponse
      ? unwrapItemResponse<SubscriptionSummary>(subscriptionResponse)
      : null
    health.value = healthResponse ? unwrapItemResponse<TenantHealth>(healthResponse) : null
    templates.value = templatesResponse
      ? unwrapListResponse<WorkspaceTemplateSummary>(templatesResponse, 'templates')
      : []
    privacyRequests.value = privacyResponse
      ? unwrapListResponse<PrivacyRequest>(privacyResponse, 'requests')
      : []
    supportCases.value = supportResponse
      ? unwrapListResponse<SupportCase>(supportResponse, 'cases')
      : []
    const profile = onboarding.value?.profile ?? {}
    const business = profile.business && typeof profile.business === 'object'
      ? profile.business as Record<string, unknown>
      : {}
    const intendedChannels = Array.isArray(profile.intended_channels)
      ? profile.intended_channels.filter(
          (channel): channel is string =>
            typeof channel === 'string' && launchChannelOptions.some((option) => option.value === channel),
        )
      : ['whatsapp']
    profileDraft.value = {
      business_name:
        typeof profile.business_name === 'string'
          ? profile.business_name
          : typeof business.name === 'string'
            ? business.name
            : '',
      timezone:
        typeof profile.timezone === 'string'
          ? profile.timezone
          : Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Kuala_Lumpur',
      vertical:
        typeof profile.vertical === 'string'
          ? profile.vertical
            : onboarding.value?.vertical || 'general',
      intended_channels: intendedChannels,
    }
  } catch (error) {
    toast.error('Launchpad could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function completeStep(key: string) {
  try {
    await onboardingService.completeStep(key)
    toast.success('Milestone completed')
    await load()
  } catch (error) {
    toast.error('Could not complete milestone', getErrorMessage(error))
  }
}

async function saveWorkspaceProfile() {
  if (!profileDraft.value.business_name.trim() || !profileDraft.value.timezone.trim()) {
    toast.warning('Business name and operating timezone are required')
    return
  }
  if (profileDraft.value.intended_channels.length === 0) {
    toast.warning('Select at least one channel required at launch')
    return
  }
  try {
    const existing = onboarding.value?.profile ?? {}
    const existingBusiness =
      existing.business && typeof existing.business === 'object'
        ? existing.business as Record<string, unknown>
        : {}
    await onboardingService.updateProfile({
      ...existing,
      business_name: profileDraft.value.business_name.trim(),
      business: {
        ...existingBusiness,
        name: profileDraft.value.business_name.trim(),
      },
      timezone: profileDraft.value.timezone.trim(),
      vertical: profileDraft.value.vertical,
      intended_channels: [...profileDraft.value.intended_channels],
    })
    toast.success('Workspace profile saved')
    await load()
  } catch (error) {
    toast.error('Workspace profile was not saved', getErrorMessage(error))
  }
}

async function applyTemplate(template: WorkspaceTemplateSummary) {
  applyingTemplate.value = template.key
  try {
    await onboardingService.applyTemplate(template.key)
    toast.success(`${template.name} workspace applied`)
    await load()
  } catch (error) {
    toast.error('Template was not applied', getErrorMessage(error))
  } finally {
    applyingTemplate.value = ''
  }
}

onMounted(load)
</script>

<template>
  <div class="h-full overflow-y-auto bg-[#08090a] light:bg-[#f5f3ee]">
    <PageHeader
      title="Workspace launchpad"
      description="Commercial readiness, privacy controls and go-live milestones in one place."
      :icon="Rocket"
      icon-gradient="bg-gradient-to-br from-amber-400 to-orange-600 shadow-amber-500/20"
    >
      <template #actions>
        <Badge
          v-if="health"
          variant="outline"
          class="gap-1.5 border px-3 py-1.5 capitalize"
          :class="healthTone"
        >
          <HeartPulse class="h-3.5 w-3.5" />
          {{ health.status }}
        </Badge>
      </template>
    </PageHeader>

    <div v-if="loading" class="flex h-[calc(100%-4rem)] items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-amber-400" />
    </div>

    <div v-else class="mx-auto max-w-[1500px] space-y-6 p-5 md:p-7">
      <section
        class="relative overflow-hidden rounded-[28px] border border-white/10 bg-[#111315] px-6 py-7 shadow-2xl shadow-black/30 light:border-black/10 light:bg-[#17201d]"
      >
        <div class="absolute -right-20 -top-24 h-72 w-72 rounded-full bg-amber-400/10 blur-3xl" />
        <div class="absolute bottom-0 right-[22%] h-36 w-36 rounded-full bg-emerald-400/10 blur-3xl" />
        <div class="relative grid gap-8 lg:grid-cols-[1.35fr_.65fr] lg:items-end">
          <div>
            <p class="mb-3 text-[11px] font-semibold uppercase tracking-[0.28em] text-amber-300">
              Your route to revenue
            </p>
            <h2 class="max-w-3xl text-3xl font-semibold tracking-[-0.04em] text-white md:text-5xl">
              Turn this workspace into a business-ready operating system.
            </h2>
            <p class="mt-4 max-w-2xl text-sm leading-6 text-white/55 md:text-base">
              Finish the essentials, select a vertical playbook, connect approved channels, then invite the team.
              ReReply keeps each customer organization isolated throughout.
            </p>
          </div>
          <div class="rounded-2xl border border-white/10 bg-black/20 p-5 backdrop-blur">
            <div class="mb-3 flex items-end justify-between">
              <div>
                <p class="text-xs uppercase tracking-[0.18em] text-white/40">Go-live progress</p>
                <p class="mt-1 text-3xl font-semibold text-white">{{ onboarding?.progress_percent ?? 0 }}%</p>
              </div>
              <span class="text-sm text-white/50">{{ completedSteps }}/{{ totalSteps }} milestones</span>
            </div>
            <Progress :model-value="onboarding?.progress_percent ?? 0" class="h-2 bg-white/10" />
            <div class="mt-4 flex items-center justify-between text-xs text-white/45">
              <span class="capitalize">{{ onboarding?.vertical || 'Select a vertical' }}</span>
              <span class="capitalize">{{ (onboarding?.provisioning_state || onboarding?.status || 'not_started').replace(/_/g, ' ') }}</span>
            </div>
          </div>
        </div>
      </section>

      <section
        id="workspace-profile"
        class="grid gap-5 rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white lg:grid-cols-[.8fr_1.2fr] lg:items-end"
      >
        <div>
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-amber-300">Workspace identity</p>
          <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-900">Name the business and its operating clock</h3>
          <p class="mt-2 text-xs leading-5 text-white/45 light:text-gray-500">
            This profile drives onboarding and local scheduling. It contains no channel credentials or clinical records.
          </p>
        </div>
        <form class="grid gap-3 md:grid-cols-[1fr_1fr_.7fr_auto]" @submit.prevent="saveWorkspaceProfile">
          <Input
            v-model="profileDraft.business_name"
            required
            maxlength="255"
            placeholder="Business name"
            :disabled="!canManageOnboarding"
          />
          <Input
            v-model="profileDraft.timezone"
            required
            maxlength="100"
            placeholder="Asia/Kuala_Lumpur"
            :disabled="!canManageOnboarding"
          />
          <select
            v-model="profileDraft.vertical"
            class="h-10 rounded-md border border-white/10 bg-[#17191a] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
            :disabled="!canManageOnboarding"
          >
            <option value="clinic">Clinic</option>
            <option value="pharmacy">Pharmacy</option>
            <option value="wellness">Wellness</option>
            <option value="general">General</option>
          </select>
          <fieldset class="space-y-2 md:col-span-4">
            <legend class="text-xs font-medium text-white/70 light:text-gray-700">
              Channels required at launch
            </legend>
            <p class="text-[11px] leading-4 text-white/40 light:text-gray-500">
              Onboarding stays incomplete until every selected channel has its own authorization and a passing health test.
            </p>
            <div class="flex flex-wrap gap-2">
              <label
                v-for="option in launchChannelOptions"
                :key="option.value"
                class="flex cursor-pointer items-center gap-2 rounded-lg border border-white/10 bg-white/[0.03] px-3 py-2 text-xs text-white/70 light:border-gray-200 light:bg-gray-50 light:text-gray-700"
              >
                <input
                  v-model="profileDraft.intended_channels"
                  type="checkbox"
                  :value="option.value"
                  :disabled="!canManageOnboarding"
                  class="h-4 w-4 accent-amber-300"
                />
                {{ option.label }}
              </label>
            </div>
          </fieldset>
          <Button
            v-if="canManageOnboarding"
            type="submit"
            class="bg-amber-300 text-black hover:bg-amber-200 md:col-span-4 md:justify-self-end"
          >
            Save profile
          </Button>
        </form>
      </section>

      <div class="grid gap-6 xl:grid-cols-[1.15fr_.85fr]">
        <section class="rounded-2xl border border-white/[0.08] bg-white/[0.025] light:border-black/10 light:bg-white">
          <div class="flex items-center justify-between border-b border-white/[0.07] px-5 py-4 light:border-black/10">
            <div>
              <h3 class="font-semibold text-white light:text-gray-900">Go-live milestones</h3>
              <p class="mt-0.5 text-xs text-white/45 light:text-gray-500">Inferred checks update automatically.</p>
            </div>
            <Sparkles class="h-5 w-5 text-amber-300" />
          </div>
          <div class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
            <div
              v-for="(step, index) in onboarding?.steps ?? []"
              :key="step.key"
              class="group flex items-center gap-4 px-5 py-4"
            >
              <div
                class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border text-xs font-semibold"
                :class="
                  step.completed
                    ? 'border-emerald-400/30 bg-emerald-400/10 text-emerald-300'
                    : 'border-white/10 bg-white/[0.03] text-white/45 light:border-black/10 light:text-gray-500'
                "
              >
                <Check v-if="step.completed" class="h-4 w-4" />
                <span v-else>{{ index + 1 }}</span>
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <p class="truncate text-sm font-medium text-white light:text-gray-900">{{ step.label }}</p>
                  <span
                    v-if="step.inferred"
                    class="rounded-full bg-white/[0.05] px-2 py-0.5 text-[10px] uppercase tracking-wider text-white/35 light:bg-gray-100 light:text-gray-500"
                  >
                    automatic
                  </span>
                </div>
                <p class="mt-1 text-xs leading-5 text-white/40 light:text-gray-500">{{ step.description }}</p>
              </div>
              <RouterLink v-if="step.action_path && !step.completed" :to="step.action_path">
                <Button variant="ghost" size="sm" class="text-white/55 hover:text-white light:text-gray-600">
                  Open
                  <ArrowRight class="ml-1.5 h-3.5 w-3.5" />
                </Button>
              </RouterLink>
              <Button
                v-else-if="!step.completed && !step.inferred && canManageOnboarding"
                variant="ghost"
                size="sm"
                class="text-white/55 hover:text-white light:text-gray-600"
                @click="completeStep(step.key)"
              >
                Mark done
              </Button>
            </div>
          </div>
        </section>

        <div class="space-y-6">
          <section v-if="canReadBilling" class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white">
            <div class="flex items-start justify-between">
              <div class="flex gap-3">
                <div class="rounded-xl bg-emerald-400/10 p-2.5 text-emerald-300">
                  <ShieldCheck class="h-5 w-5" />
                </div>
                <div>
                  <p class="text-sm font-semibold text-white light:text-gray-900">License</p>
                  <p class="mt-1 text-xs text-white/45 light:text-gray-500">Provider-neutral subscription control</p>
                </div>
              </div>
              <Badge variant="outline" class="capitalize">{{ subscription?.status || 'unlicensed' }}</Badge>
            </div>
            <div class="mt-5 grid grid-cols-2 gap-3">
              <div class="rounded-xl bg-white/[0.035] p-3 light:bg-gray-50">
                <p class="text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">Plan</p>
                <p class="mt-1 text-sm font-medium capitalize text-white light:text-gray-900">
                  {{ subscription?.plan_name || subscription?.plan_code || 'No active plan' }}
                </p>
              </div>
              <div class="rounded-xl bg-white/[0.035] p-3 light:bg-gray-50">
                <p class="text-[10px] uppercase tracking-wider text-white/35 light:text-gray-500">Billing</p>
                <p class="mt-1 text-sm font-medium capitalize text-white light:text-gray-900">
                  {{ subscription?.provider || 'Manual' }}
                </p>
              </div>
            </div>
          </section>

          <section v-if="canReadPrivacyRequests" class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white">
            <div class="mb-4 flex items-center justify-between">
              <div class="flex items-center gap-3">
                <LockKeyhole class="h-5 w-5 text-sky-300" />
                <div>
                  <p class="text-sm font-semibold text-white light:text-gray-900">Privacy desk</p>
                  <p class="text-xs text-white/40 light:text-gray-500">Requests requiring attention</p>
                </div>
              </div>
              <RouterLink to="/settings/privacy" class="text-xs text-sky-300 hover:text-sky-200">Open center</RouterLink>
            </div>
            <div v-if="privacyRequests.length" class="space-y-2">
              <div
                v-for="request in privacyRequests.slice(0, 3)"
                :key="request.id"
                class="flex items-center justify-between rounded-xl bg-white/[0.035] px-3 py-2.5 light:bg-gray-50"
              >
                <div>
                  <p class="text-xs font-medium capitalize text-white light:text-gray-900">
                    {{ request.request_type }} request
                  </p>
                  <p class="mt-0.5 text-[11px] capitalize text-white/35 light:text-gray-500">{{ request.status }}</p>
                </div>
                <AlertCircle class="h-4 w-4 text-amber-300" />
              </div>
            </div>
            <p v-else class="rounded-xl bg-emerald-400/[0.06] px-3 py-4 text-center text-xs text-emerald-300">
              No open privacy requests
            </p>
          </section>

          <section v-if="canReadSupport" class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white">
            <div class="flex items-center gap-3">
              <LifeBuoy class="h-5 w-5 text-violet-300" />
              <div class="flex-1">
                <p class="text-sm font-semibold text-white light:text-gray-900">Support & recovery</p>
                <p class="text-xs text-white/40 light:text-gray-500">
                  {{ supportCases.filter((item) => !['resolved', 'closed'].includes(item.status)).length }} active cases
                </p>
              </div>
              <RouterLink to="/settings/support">
                <Button variant="outline" size="sm">Review</Button>
              </RouterLink>
            </div>
          </section>
        </div>
      </div>

      <section v-if="canReadTemplates">
        <div class="mb-4 flex items-end justify-between">
          <div>
            <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-amber-300">Vertical editions</p>
            <h3 class="mt-1 text-xl font-semibold text-white light:text-gray-900">Start from a proven operating playbook</h3>
          </div>
          <p class="hidden max-w-lg text-right text-xs leading-5 text-white/40 light:text-gray-500 md:block">
            Templates create safe defaults, not clinical records. You remain in control of every workflow.
          </p>
        </div>
        <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
          <article
            v-for="template in templates"
            :key="template.key"
            class="group flex min-h-60 flex-col rounded-2xl border border-white/[0.08] bg-gradient-to-b from-white/[0.045] to-white/[0.015] p-5 transition hover:-translate-y-0.5 hover:border-amber-300/25 light:border-black/10 light:from-white light:to-[#faf9f6]"
          >
            <div class="flex items-center justify-between">
              <span class="text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">
                v{{ template.version }} · {{ template.vertical }}
              </span>
              <div class="h-2 w-2 rounded-full bg-amber-300 shadow-[0_0_14px_rgba(252,211,77,.6)]" />
            </div>
            <h4 class="mt-5 text-lg font-semibold text-white light:text-gray-900">{{ template.name }}</h4>
            <p class="mt-2 text-xs leading-5 text-white/45 light:text-gray-500">{{ template.description }}</p>
            <ul class="mt-4 flex-1 space-y-2">
              <li v-for="highlight in template.highlights.slice(0, 3)" :key="highlight" class="flex gap-2 text-xs text-white/55 light:text-gray-600">
                <Check class="mt-0.5 h-3.5 w-3.5 shrink-0 text-emerald-300" />
                {{ highlight }}
              </li>
            </ul>
            <Button
              class="mt-5 w-full bg-amber-300 text-black hover:bg-amber-200"
              :disabled="Boolean(applyingTemplate) || !canApplyTemplates"
              @click="applyTemplate(template)"
            >
              <Loader2 v-if="applyingTemplate === template.key" class="mr-2 h-4 w-4 animate-spin" />
              Apply playbook
            </Button>
          </article>
        </div>
      </section>
    </div>
  </div>
</template>
