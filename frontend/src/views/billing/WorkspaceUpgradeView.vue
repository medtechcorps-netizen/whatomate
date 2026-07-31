<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import {
  ArrowLeft,
  Building2,
  CalendarDays,
  Check,
  CircleDollarSign,
  Clock3,
  CreditCard,
  Inbox,
  LineChart,
  Package,
  RefreshCw,
  Sparkles,
  Workflow,
  X,
} from 'lucide-vue-next'
import { toast } from 'vue-sonner'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import { organizationsService, type Organization } from '@/services/api'
import {
  organizationSubscriptionService,
  productService,
  type PlanPriceSummary,
  type PlanSummary,
  type SetOrganizationSubscriptionRequest,
  type SubscriptionSummary,
} from '@/services/productSuite'
import {
  getErrorMessage,
  unwrapItemResponse,
  unwrapListResponse,
} from '@/lib/api-utils'
import { formatCurrencyMinorUnits } from '@/lib/currency'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

interface PlanPriceOption {
  key: string
  plan: PlanSummary
  price: PlanPriceSummary
}

interface PlanBenefit {
  key: string
  label: string
  enabled: boolean
}

const route = useRoute()
const authStore = useAuthStore()
const organizationsStore = useOrganizationsStore()
const isPlatformOwner = computed(() => authStore.user?.is_super_admin === true)
const backRoute = computed(() =>
  authStore.hasPermission('resellers', 'read') ? '/resellers' : '/',
)
const activeOrganizationId = computed(() =>
  isPlatformOwner.value
    ? organizationsStore.selectedOrgId || authStore.organizationId
    : authStore.organizationId,
)

const loading = ref(true)
const loadError = ref('')
const plans = ref<PlanSummary[]>([])
const subscription = ref<SubscriptionSummary | null>(null)
const organization = ref<Organization | null>(null)
const selectedPlanPriceKey = ref('')
const licenseStatus = ref<'active' | 'trialing'>('active')
const trialDays = ref(14)
const approvalReference = ref('')
const submitting = ref(false)
const upgradeConfirmation = ref<HTMLElement | null>(null)

const requestedOrganizationId = computed(() => {
  const queryValue = route.query.organization_id
  if (isPlatformOwner.value && typeof queryValue === 'string' && queryValue) {
    return queryValue
  }
  return authStore.organizationId
})

const organizationName = computed(
  () =>
    organization.value?.name ||
    authStore.user?.organization_name ||
    'Current workspace',
)

const planPriceOptions = computed<PlanPriceOption[]>(() =>
  plans.value.flatMap((plan) =>
    plan.prices
      .filter((price) => price.interval !== 'one_time')
      .map((price) => ({
        key: `${plan.id}::${price.id}`,
        plan,
        price,
      })),
  ),
)

const recurringPlans = computed(() =>
  plans.value.filter((plan) =>
    plan.prices.some((price) => price.interval !== 'one_time'),
  ),
)

const selectedPlanPrice = computed(
  () =>
    planPriceOptions.value.find(
      (option) => option.key === selectedPlanPriceKey.value,
    ) ?? null,
)

const selectedPlanPrices = computed(() => {
  if (!selectedPlanPrice.value) return []
  return planPriceOptions.value.filter(
    (option) => option.plan.id === selectedPlanPrice.value?.plan.id,
  )
})

const currentPlanPriceKey = computed(() => {
  if (!subscription.value?.plan_id || !subscription.value?.plan_price_id) {
    return ''
  }
  return `${subscription.value.plan_id}::${subscription.value.plan_price_id}`
})

const isCurrentPriceSelected = computed(
  () =>
    Boolean(currentPlanPriceKey.value) &&
    selectedPlanPriceKey.value === currentPlanPriceKey.value,
)

const licenseChangeBlockingReason = computed(() => {
  const current = subscription.value
  if (current?.provider && current.provider !== 'manual') {
    return 'This workspace uses a provider-managed subscription. Change or cancel it through that billing provider before applying a manual plan.'
  }
  return ''
})

const knownBenefits = [
  {
    key: 'omnichannel.enabled',
    label: 'Shared omnichannel inbox',
    icon: Inbox,
  },
  {
    key: 'crm.enabled',
    label: 'CRM pipeline, tasks, insights and automations',
    icon: Workflow,
  },
  {
    key: 'bookings.enabled',
    label: 'Appointment calendar and booking workflows',
    icon: CalendarDays,
  },
  {
    key: 'commerce.enabled',
    label: 'Packages, invoices and payment tracking',
    icon: Package,
  },
  {
    key: 'copilot.enabled',
    label: 'AI copilot workspace',
    icon: Sparkles,
  },
  {
    key: 'threads.public_engagement.enabled',
    label: 'Threads public engagement',
    icon: LineChart,
  },
] as const

function entitlementEnabled(value: unknown): boolean {
  if (typeof value === 'boolean') return value
  if (typeof value === 'number') return Number.isFinite(value) && value > 0
  if (typeof value === 'string') {
    return ['true', 'enabled', 'yes', '1'].includes(value.trim().toLowerCase())
  }
  if (value && typeof value === 'object') {
    const record = value as Record<string, unknown>
    return entitlementEnabled(record.value ?? record.enabled)
  }
  return false
}

function planBenefits(plan: PlanSummary): PlanBenefit[] {
  return knownBenefits.map((benefit) => ({
    key: benefit.key,
    label: benefit.label,
    enabled: entitlementEnabled(plan.entitlements?.[benefit.key]),
  }))
}

function planPriceOptionsFor(plan: PlanSummary): PlanPriceOption[] {
  return planPriceOptions.value.filter((option) => option.plan.id === plan.id)
}

function preferredPlanPrice(plan: PlanSummary): PlanPriceOption | null {
  const options = planPriceOptionsFor(plan)
  if (!options.length) return null
  const current = options.find(
    (option) => option.key === currentPlanPriceKey.value,
  )
  if (current) return current
  return (
    options.find(
      (option) =>
        option.price.interval === 'month' &&
        (option.price.interval_count || 1) === 1,
    ) ?? options[0]
  )
}

function selectPlan(plan: PlanSummary) {
  const option = preferredPlanPrice(plan)
  if (!option) {
    toast.error('This plan does not have an active recurring price')
    return
  }
  selectedPlanPriceKey.value = option.key
  trialDays.value = plan.trial_days || 14
  void nextTick(() => {
    upgradeConfirmation.value?.scrollIntoView({
      behavior: 'smooth',
      block: 'start',
    })
  })
}

function isPlanSelected(plan: PlanSummary) {
  return selectedPlanPrice.value?.plan.id === plan.id
}

function isCurrentPlan(plan: PlanSummary) {
  return subscription.value?.plan_id === plan.id
}

function formatMoney(price: PlanPriceSummary) {
  return formatCurrencyMinorUnits(price.currency, price.unit_amount_minor)
}

function formatInterval(price: PlanPriceSummary) {
  const count = price.interval_count || 1
  if (count === 1) return price.interval
  return `${count} ${price.interval}s`
}

function formatPlanPrice(plan: PlanSummary) {
  const option = preferredPlanPrice(plan)
  if (!option) return 'Price unavailable'
  return `${formatMoney(option.price)} / ${formatInterval(option.price)}`
}

function formatDate(value?: string) {
  if (!value) return 'Not scheduled'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'Not scheduled'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
  }).format(date)
}

function hydrateSelection() {
  const current = planPriceOptions.value.find(
    (option) => option.key === currentPlanPriceKey.value,
  )
  const fallback = current ?? planPriceOptions.value[0] ?? null
  selectedPlanPriceKey.value = fallback?.key ?? ''
  licenseStatus.value =
    subscription.value?.status === 'trialing' ? 'trialing' : 'active'
  trialDays.value = fallback?.plan.trial_days || 14
  approvalReference.value = ''
}

async function loadWorkspaceUpgrade() {
  const organizationId = requestedOrganizationId.value
  if (!organizationId) {
    loadError.value = 'No workspace is selected'
    loading.value = false
    return
  }

  loading.value = true
  loadError.value = ''
  try {
    if (isPlatformOwner.value) {
      const [organizationsResponse, plansResponse, subscriptionResponse] =
        await Promise.all([
          organizationsService.list(),
          organizationSubscriptionService.plans(organizationId),
          organizationSubscriptionService.get(organizationId),
        ])
      const organizations = unwrapListResponse<Organization>(
        organizationsResponse,
        'organizations',
      )
      organization.value =
        organizations.find((item) => item.id === organizationId) ?? null
      plans.value = unwrapListResponse<PlanSummary>(plansResponse, 'plans')
      subscription.value =
        unwrapItemResponse<SubscriptionSummary>(subscriptionResponse)
    } else {
      const [plansResponse, subscriptionResponse] = await Promise.all([
        productService.plans(),
        productService.subscription(),
      ])
      plans.value = unwrapListResponse<PlanSummary>(plansResponse, 'plans')
      subscription.value =
        unwrapItemResponse<SubscriptionSummary>(subscriptionResponse)
    }
    hydrateSelection()
  } catch (error) {
    loadError.value = getErrorMessage(
      error,
      'Unable to load workspace upgrade options',
    )
  } finally {
    loading.value = false
  }
}

async function applySelectedPlan() {
  const organizationId = requestedOrganizationId.value
  const option = selectedPlanPrice.value
  if (
    !isPlatformOwner.value ||
    !organizationId ||
    !option ||
    licenseChangeBlockingReason.value
  )
    return

  const reference = approvalReference.value.trim()
  if (!reference) {
    toast.error('Enter an approval or contract reference')
    return
  }

  const payload: SetOrganizationSubscriptionRequest = {
    plan_id: option.plan.id,
    plan_price_id: option.price.id,
    status: licenseStatus.value,
    manual_reference: reference,
  }
  if (licenseStatus.value === 'trialing') {
    const days = Number(trialDays.value)
    if (!Number.isInteger(days) || days < 1 || days > 365) {
      toast.error('Trial days must be between 1 and 365')
      return
    }
    payload.trial_days = days
  }

  submitting.value = true
  try {
    const response = await organizationSubscriptionService.set(
      organizationId,
      payload,
    )
    subscription.value = unwrapItemResponse<SubscriptionSummary>(response)
    if (organizationId === activeOrganizationId.value) {
      await authStore.fetchProductEntitlements()
    }
    hydrateSelection()
    toast.success(
      `${organizationName.value} is now on ${option.plan.name}. Navigation access has been refreshed.`,
    )
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to update workspace plan'))
  } finally {
    submitting.value = false
  }
}

onMounted(loadWorkspaceUpgrade)
watch(requestedOrganizationId, () => {
  void loadWorkspaceUpgrade()
})
</script>

<template>
  <div
    class="min-h-full bg-[#08090a] text-white light:bg-[#f4f3ef] light:text-gray-950"
  >
    <header
      class="border-b border-white/[0.08] bg-[#0b0c0e]/95 light:border-gray-200 light:bg-white/95"
    >
      <div
        class="mx-auto flex min-h-20 max-w-[1500px] items-center gap-4 px-5 py-4 md:px-8"
      >
        <RouterLink
          :to="backRoute"
          class="flex h-10 w-10 items-center justify-center rounded-xl border border-white/10 text-white/55 transition hover:border-white/20 hover:text-white light:border-gray-200 light:text-gray-500 light:hover:text-gray-900"
          aria-label="Back to Partner Console"
        >
          <ArrowLeft class="h-4 w-4" />
        </RouterLink>
        <div
          class="flex h-11 w-11 items-center justify-center rounded-xl border border-amber-300/20 bg-amber-300/10"
        >
          <CircleDollarSign
            class="h-5 w-5 text-amber-200 light:text-amber-700"
          />
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex flex-wrap items-center gap-2">
            <h1 class="text-xl font-semibold tracking-tight">
              Upgrade workspace
            </h1>
            <Badge variant="outline">{{ organizationName }}</Badge>
          </div>
          <p class="mt-0.5 text-sm text-white/45 light:text-gray-500">
            Compare exactly what each available product plan unlocks before
            changing access.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          :disabled="loading"
          @click="loadWorkspaceUpgrade"
        >
          <RefreshCw
            class="mr-2 h-4 w-4"
            :class="{ 'animate-spin': loading }"
          />
          Refresh
        </Button>
      </div>
    </header>

    <main class="mx-auto max-w-[1500px] space-y-6 px-5 py-7 md:px-8">
      <div v-if="loading" class="grid gap-4 lg:grid-cols-[320px_minmax(0,1fr)]">
        <div
          class="h-48 animate-pulse rounded-2xl bg-white/[0.04] light:bg-white"
        />
        <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          <div
            v-for="index in 3"
            :key="index"
            class="h-96 animate-pulse rounded-2xl bg-white/[0.04] light:bg-white"
          />
        </div>
      </div>

      <div
        v-else-if="loadError"
        class="mx-auto max-w-xl rounded-2xl border border-red-400/20 bg-red-400/[0.06] p-6 text-center"
      >
        <p class="text-base font-semibold text-red-100 light:text-red-700">
          Upgrade options unavailable
        </p>
        <p class="mt-2 text-sm text-red-100/60 light:text-red-600">
          {{ loadError }}
        </p>
        <Button class="mt-5" variant="outline" @click="loadWorkspaceUpgrade">
          Try again
        </Button>
      </div>

      <template v-else>
        <section
          class="grid gap-4 lg:grid-cols-[320px_minmax(0,1fr)]"
          aria-label="Workspace plan comparison"
        >
          <Card
            class="h-fit border-white/[0.09] bg-[#111315] light:border-gray-200 light:bg-white"
          >
            <CardHeader>
              <CardTitle class="flex items-center gap-2 text-base">
                <Building2
                  class="h-4 w-4 text-emerald-300 light:text-emerald-700"
                />
                {{ organizationName }}
              </CardTitle>
              <p class="text-xs text-white/40 light:text-gray-500">
                Current workspace license
              </p>
            </CardHeader>
            <CardContent class="space-y-4">
              <div
                class="rounded-xl border border-white/[0.08] bg-white/[0.025] p-4 light:border-gray-200 light:bg-gray-50"
              >
                <div class="flex items-center justify-between gap-2">
                  <p class="font-medium">
                    {{ subscription?.plan_name || 'No active plan' }}
                  </p>
                  <Badge
                    :variant="
                      subscription?.status === 'active'
                        ? 'success'
                        : subscription?.status === 'trialing'
                          ? 'info'
                          : 'warning'
                    "
                  >
                    {{ subscription?.status || 'Unlicensed' }}
                  </Badge>
                </div>
                <div class="mt-4 flex items-start gap-2 text-xs">
                  <Clock3
                    class="mt-0.5 h-3.5 w-3.5 text-white/30 light:text-gray-400"
                  />
                  <div>
                    <p class="text-white/35 light:text-gray-500">
                      {{
                        subscription?.status === 'trialing'
                          ? 'Trial ends'
                          : 'Current period ends'
                      }}
                    </p>
                    <p class="mt-1 font-medium">
                      {{
                        formatDate(
                          subscription?.status === 'trialing'
                            ? subscription?.trial_ends_at
                            : subscription?.current_period_end,
                        )
                      }}
                    </p>
                  </div>
                </div>
              </div>

              <div
                class="rounded-xl border border-amber-300/15 bg-amber-300/[0.05] p-4"
              >
                <p
                  class="text-xs font-medium text-amber-100 light:text-amber-800"
                >
                  Workspace plan vs partner tier
                </p>
                <p
                  class="mt-1 text-xs leading-5 text-white/45 light:text-gray-600"
                >
                  This page changes feature access for one organization.
                  Starter, Growth and Enterprise under Partner Console → Brand
                  &amp; portfolio capacity control partner workspace limits
                  instead.
                </p>
              </div>
            </CardContent>
          </Card>

          <div>
            <div class="mb-4 flex flex-wrap items-end justify-between gap-3">
              <div>
                <p
                  class="text-[11px] font-semibold uppercase tracking-[0.18em] text-amber-200/65 light:text-amber-700"
                >
                  Available workspace plans
                </p>
                <h2 class="mt-1 text-2xl font-semibold tracking-tight">
                  Choose by capability, not just by name
                </h2>
              </div>
              <p
                class="max-w-md text-xs leading-5 text-white/40 light:text-gray-500"
              >
                Core chat, campaigns, templates and chatbot tools remain
                available. The comparison below shows the additional licensed
                modules.
              </p>
            </div>

            <div
              v-if="!recurringPlans.length"
              class="rounded-2xl border border-amber-300/20 bg-amber-300/[0.05] p-8 text-center"
            >
              <CreditCard
                class="mx-auto h-8 w-8 text-amber-200/55 light:text-amber-700"
              />
              <p class="mt-3 font-medium">No upgrade plan is configured</p>
              <p
                class="mx-auto mt-1 max-w-lg text-sm text-white/45 light:text-gray-600"
              >
                A platform owner must publish an active recurring workspace plan
                before it can be selected here.
              </p>
            </div>

            <template v-else>
              <div
                v-if="isPlatformOwner && recurringPlans.length === 1"
                class="mb-4 rounded-xl border border-blue-300/15 bg-blue-300/[0.05] px-4 py-3 text-xs leading-5 text-white/50 light:text-gray-600"
              >
                Only {{ recurringPlans[0].name }} is currently published in the
                workspace product catalog. Additional plans will appear here
                only after their prices and entitlements are configured; the
                partner portfolio tiers are not substitutes.
              </div>

              <div
                class="grid items-stretch gap-4 md:grid-cols-2 2xl:grid-cols-3"
              >
                <article
                  v-for="plan in recurringPlans"
                  :key="plan.id"
                  class="relative flex min-h-[390px] flex-col overflow-hidden rounded-2xl border p-5 transition"
                  :class="
                    isPlanSelected(plan)
                      ? 'border-amber-300/55 bg-amber-300/[0.08] shadow-[0_18px_55px_rgba(217,170,75,0.10)] light:bg-amber-50'
                      : 'border-white/[0.09] bg-[#111315] hover:border-white/20 light:border-gray-200 light:bg-white light:hover:border-gray-300'
                  "
                >
                  <div
                    v-if="isPlanSelected(plan)"
                    class="absolute inset-x-0 top-0 h-0.5 bg-amber-300"
                  />
                  <div class="flex items-start justify-between gap-3">
                    <div>
                      <div class="flex flex-wrap items-center gap-2">
                        <h3 class="text-lg font-semibold">{{ plan.name }}</h3>
                        <Badge v-if="isCurrentPlan(plan)" variant="success">
                          Current
                        </Badge>
                      </div>
                      <p
                        class="mt-1 font-mono text-[10px] uppercase tracking-wider text-white/30 light:text-gray-400"
                      >
                        {{ plan.code }}
                      </p>
                    </div>
                    <div
                      class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border"
                      :class="
                        isPlanSelected(plan)
                          ? 'border-amber-300/35 bg-amber-300/15 text-amber-200 light:text-amber-800'
                          : 'border-white/10 bg-white/[0.04] text-white/35 light:border-gray-200 light:text-gray-500'
                      "
                    >
                      <Check v-if="isPlanSelected(plan)" class="h-4 w-4" />
                      <CreditCard v-else class="h-4 w-4" />
                    </div>
                  </div>

                  <p class="mt-4 text-2xl font-semibold tracking-tight">
                    {{ formatPlanPrice(plan) }}
                  </p>
                  <p
                    class="mt-2 min-h-10 text-xs leading-5 text-white/45 light:text-gray-600"
                  >
                    {{
                      plan.description ||
                      'A licensed ReReply workspace plan with the modules shown below.'
                    }}
                  </p>

                  <div
                    class="my-4 h-px bg-gradient-to-r from-transparent via-white/10 to-transparent light:via-gray-200"
                  />

                  <ul class="flex-1 space-y-2.5">
                    <li
                      v-for="benefit in planBenefits(plan)"
                      :key="benefit.key"
                      class="flex items-start gap-2 text-xs leading-5"
                      :class="
                        benefit.enabled
                          ? 'text-white/75 light:text-gray-800'
                          : 'text-white/25 light:text-gray-400'
                      "
                    >
                      <Check
                        v-if="benefit.enabled"
                        class="mt-0.5 h-3.5 w-3.5 shrink-0 text-emerald-300 light:text-emerald-700"
                      />
                      <X v-else class="mt-0.5 h-3.5 w-3.5 shrink-0" />
                      <span>{{ benefit.label }}</span>
                    </li>
                  </ul>

                  <Button
                    class="mt-5 w-full"
                    :variant="isPlanSelected(plan) ? 'default' : 'outline'"
                    type="button"
                    @click="selectPlan(plan)"
                  >
                    {{
                      isPlanSelected(plan)
                        ? isCurrentPlan(plan)
                          ? 'Current plan selected'
                          : 'Plan selected'
                        : `Choose ${plan.name}`
                    }}
                  </Button>
                </article>
              </div>
            </template>
          </div>
        </section>

        <section
          v-if="selectedPlanPrice"
          ref="upgradeConfirmation"
          class="scroll-mt-24"
        >
          <Card
            class="overflow-hidden border-amber-300/20 bg-[#111315] light:border-amber-200 light:bg-white"
          >
            <div class="grid lg:grid-cols-[minmax(0,1fr)_minmax(360px,0.72fr)]">
              <CardHeader
                class="border-b border-white/[0.08] lg:border-b-0 lg:border-r light:border-gray-200"
              >
                <p
                  class="text-[11px] font-semibold uppercase tracking-[0.18em] text-amber-200/65 light:text-amber-700"
                >
                  Selected upgrade
                </p>
                <CardTitle class="mt-2 text-2xl">
                  {{ selectedPlanPrice.plan.name }}
                </CardTitle>
                <p
                  class="max-w-xl text-sm leading-6 text-white/45 light:text-gray-600"
                >
                  {{
                    selectedPlanPrice.plan.description ||
                    'Review the billing interval and activation details before applying this plan.'
                  }}
                </p>
                <div class="mt-4 grid gap-3 sm:grid-cols-2">
                  <div
                    v-for="benefit in planBenefits(
                      selectedPlanPrice.plan,
                    ).filter((item) => item.enabled)"
                    :key="benefit.key"
                    class="flex items-center gap-2 text-xs text-white/65 light:text-gray-700"
                  >
                    <span
                      class="flex h-5 w-5 items-center justify-center rounded-full bg-emerald-300/10"
                    >
                      <Check
                        class="h-3 w-3 text-emerald-300 light:text-emerald-700"
                      />
                    </span>
                    {{ benefit.label }}
                  </div>
                </div>
              </CardHeader>

              <CardContent class="p-6">
                <div
                  v-if="isPlatformOwner && licenseChangeBlockingReason"
                  class="rounded-xl border border-amber-300/15 bg-amber-300/[0.05] p-5"
                >
                  <p class="font-medium text-amber-100 light:text-amber-800">
                    Manual plan change unavailable
                  </p>
                  <p
                    class="mt-2 text-sm leading-6 text-white/45 light:text-gray-600"
                  >
                    {{ licenseChangeBlockingReason }}
                  </p>
                </div>

                <div v-else-if="isPlatformOwner" class="space-y-4">
                  <div class="space-y-2">
                    <Label for="upgrade-price">Billing interval</Label>
                    <select
                      id="upgrade-price"
                      v-model="selectedPlanPriceKey"
                      data-testid="upgrade-plan-price"
                      class="upgrade-select"
                    >
                      <option
                        v-for="option in selectedPlanPrices"
                        :key="option.key"
                        :value="option.key"
                      >
                        {{ formatMoney(option.price) }} /
                        {{ formatInterval(option.price) }}
                      </option>
                    </select>
                  </div>
                  <div class="grid grid-cols-2 gap-3">
                    <div class="space-y-2">
                      <Label for="upgrade-status">Activation</Label>
                      <select
                        id="upgrade-status"
                        v-model="licenseStatus"
                        data-testid="upgrade-status"
                        class="upgrade-select"
                      >
                        <option value="active">Activate now</option>
                        <option value="trialing">Start trial</option>
                      </select>
                    </div>
                    <div v-if="licenseStatus === 'trialing'" class="space-y-2">
                      <Label for="upgrade-trial-days">Trial days</Label>
                      <Input
                        id="upgrade-trial-days"
                        v-model.number="trialDays"
                        data-testid="upgrade-trial-days"
                        type="number"
                        min="1"
                        max="365"
                      />
                    </div>
                  </div>
                  <div class="space-y-2">
                    <Label for="upgrade-reference">
                      Approval / contract reference
                    </Label>
                    <Input
                      id="upgrade-reference"
                      v-model="approvalReference"
                      data-testid="upgrade-reference"
                      maxlength="255"
                      placeholder="RELIVE-2026-UPGRADE"
                    />
                    <p
                      class="text-[11px] leading-5 text-white/35 light:text-gray-500"
                    >
                      Required for the audit trail. This records an approved
                      manual license; it does not claim that payment was
                      collected.
                    </p>
                  </div>
                  <Button
                    class="w-full"
                    data-testid="upgrade-submit"
                    :loading="submitting"
                    :disabled="submitting || !approvalReference.trim()"
                    @click="applySelectedPlan"
                  >
                    <Check class="mr-2 h-4 w-4" />
                    {{
                      isCurrentPriceSelected
                        ? 'Update current license'
                        : `Apply ${selectedPlanPrice.plan.name}`
                    }}
                  </Button>
                </div>

                <div
                  v-else
                  class="rounded-xl border border-amber-300/15 bg-amber-300/[0.05] p-5"
                >
                  <p class="font-medium">Ready to upgrade?</p>
                  <p
                    class="mt-2 text-sm leading-6 text-white/45 light:text-gray-600"
                  >
                    Ask your ReReply account manager or platform owner to apply
                    this plan. Workspace administrators can compare benefits
                    here, while license changes remain approval-controlled.
                  </p>
                </div>
              </CardContent>
            </div>
          </Card>
        </section>
      </template>
    </main>
  </div>
</template>

<style scoped>
.upgrade-select {
  @apply flex h-10 w-full rounded-md border border-white/10 bg-[#0d0f11] px-3 py-2 text-sm text-white outline-none transition focus:border-amber-300/50 focus:ring-2 focus:ring-amber-300/15;
}

:global(.light) .upgrade-select {
  @apply border-gray-200 bg-white text-gray-900;
}
</style>
