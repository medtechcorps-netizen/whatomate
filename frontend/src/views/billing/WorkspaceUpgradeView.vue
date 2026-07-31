<script setup lang="ts">
import type { Component } from 'vue'
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import {
  ArrowLeft,
  ArrowUpRight,
  Bot,
  Building2,
  Check,
  CircleDollarSign,
  Clock3,
  Crown,
  Headphones,
  Inbox,
  PhoneCall,
  RefreshCw,
  Sparkles,
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

type OfferKey = 'starter' | 'growth' | 'sprint' | 'enterprise'
type OfferAvailability = 'available' | 'coming_soon' | 'contact_sales'

interface PlanOfferDefinition {
  key: OfferKey
  catalogCode?: string
  name: string
  category: string
  description: string
  priceLead: string
  priceCadence?: string
  availability: OfferAvailability
  badge: string
  icon: Component
  features: string[]
}

interface DisplayPlanOffer extends PlanOfferDefinition {
  plan: PlanSummary | null
  priceOption: PlanPriceOption | null
}

const route = useRoute()
const authStore = useAuthStore()
const organizationsStore = useOrganizationsStore()
const isPlatformOwner = computed(() => authStore.user?.is_super_admin === true)
const backRoute = computed(() =>
  authStore.hasPermission('resellers', 'read') ? '/resellers' : '/',
)
const backLabel = computed(() =>
  backRoute.value === '/resellers'
    ? 'Back to Partner Console'
    : 'Back to dashboard',
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
const explicitPlanSelection = ref(false)

const planOfferDefinitions: PlanOfferDefinition[] = [
  {
    key: 'starter',
    catalogCode: 'rereply-starter',
    name: 'Starter',
    category: 'WhatsApp Chatbot',
    description:
      'Turn WhatsApp into an always-on front desk that answers, guides and hands over to your team.',
    priceLead: 'RM 300',
    priceCadence: '/ month',
    availability: 'available',
    badge: 'Essential',
    icon: Bot,
    features: [
      'Visual WhatsApp chatbot and automated replies',
      'Keyword triggers and always-on FAQ handling',
      'Broadcast campaigns for approved audiences',
      'Meta-approved templates and WhatsApp Flows',
      'Contact profiles, tags and conversation history',
      'Dashboard and agent performance visibility',
    ],
  },
  {
    key: 'growth',
    catalogCode: 'rereply-growth',
    name: 'Growth',
    category: 'Omnichannel',
    description:
      'Bring every conversation, lead and follow-up into one customer journey your whole team can see.',
    priceLead: 'RM 600',
    priceCadence: '/ month',
    availability: 'available',
    badge: 'Most popular',
    icon: Inbox,
    features: [
      'Everything in Starter',
      'Shared inbox for WhatsApp, Instagram, Messenger, email and web chat',
      'Multi-agent assignment and conversation control',
      'CRM lead pipeline, follow-ups and automations',
      'Appointments, packages, invoices and payments',
      'Qwen AI Copilot for faster reviewed responses',
    ],
  },
  {
    key: 'sprint',
    name: 'Sprint',
    category: 'Calling',
    description:
      'The planned commercial launch for a connected voice layer that keeps calls, transfers and customer context together.',
    priceLead: 'RM 900',
    priceCadence: '/ month',
    availability: 'coming_soon',
    badge: 'Coming soon',
    icon: PhoneCall,
    features: [
      'Everything in Growth',
      'Inbound and outbound WhatsApp calling',
      'Visual IVR call flow builder',
      'Browser-based answering, hold and transfers',
      'Call logs, status tracking and recordings',
      'CRM-linked call history for complete context',
    ],
  },
  {
    key: 'enterprise',
    name: 'Enterprise',
    category: 'Tailored operations',
    description:
      'A guided rollout for multi-site, high-volume or custom operations with commercial terms built around you.',
    priceLead: 'Call us',
    availability: 'contact_sales',
    badge: 'Built for scale',
    icon: Crown,
    features: [
      'Everything in Growth and Sprint when available',
      'Tailored multi-site and multi-brand rollout',
      'Custom channels, workflows and integrations',
      'SSO, API, webhooks and audit controls',
      'Priority onboarding and solution design',
      'Custom capacity, support and commercial terms',
    ],
  },
]

const enterpriseContactHref =
  'mailto:medtechcorps@gmail.com?subject=ReReply%20Enterprise%20Plan'

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

const catalogPlanPriceOptions = computed<PlanPriceOption[]>(() =>
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

const planPriceOptions = computed(() =>
  catalogPlanPriceOptions.value.filter(
    (option) => option.price.assignable !== false,
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

const currentCatalogPrice = computed(
  () =>
    catalogPlanPriceOptions.value.find(
      (option) => option.key === currentPlanPriceKey.value,
    ) ?? null,
)

const isCurrentPriceSelected = computed(
  () =>
    Boolean(currentPlanPriceKey.value) &&
    selectedPlanPriceKey.value === currentPlanPriceKey.value,
)

const retiredCurrentPrice = computed(() =>
  currentCatalogPrice.value?.price.assignable === false
    ? currentCatalogPrice.value
    : null,
)

const currentPriceUnresolved = computed(() => {
  const current = subscription.value
  if (!current || current.status === 'unlicensed') return false
  if (!current.plan_id || !current.plan_price_id) return true
  return !currentCatalogPrice.value
})

const unresolvedPriceRequiresSelection = computed(
  () => currentPriceUnresolved.value && !explicitPlanSelection.value,
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
  },
  {
    key: 'crm.enabled',
    label: 'CRM pipeline, tasks, insights and automations',
  },
  {
    key: 'bookings.enabled',
    label: 'Appointment calendar and booking workflows',
  },
  {
    key: 'commerce.enabled',
    label: 'Packages, invoices and payment tracking',
  },
  {
    key: 'copilot.enabled',
    label: 'Qwen AI Copilot workspace',
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

const displayOffers = computed<DisplayPlanOffer[]>(() =>
  planOfferDefinitions.map((offer) => {
    const plan = offer.catalogCode
      ? (plans.value.find((item) => item.code === offer.catalogCode) ?? null)
      : null
    return {
      ...offer,
      plan,
      priceOption: plan ? preferredPlanPrice(plan) : null,
    }
  }),
)

const missingCatalogOffers = computed(() =>
  displayOffers.value.filter(
    (offer) =>
      offer.availability === 'available' && (!offer.plan || !offer.priceOption),
  ),
)

const selectedOffer = computed(
  () =>
    displayOffers.value.find(
      (offer) => offer.plan?.id === selectedPlanPrice.value?.plan.id,
    ) ?? null,
)

const additionalCatalogOptions = computed(() => {
  const merchandisedPlanIds = new Set(
    displayOffers.value.flatMap((offer) => (offer.plan ? [offer.plan.id] : [])),
  )
  return plans.value.flatMap((plan) => {
    if (merchandisedPlanIds.has(plan.id)) return []
    const priceOption = preferredPlanPrice(plan)
    return priceOption ? [{ plan, priceOption }] : []
  })
})

const additionalCatalogPlanId = computed<string>({
  get: () =>
    selectedOffer.value ? '' : selectedPlanPrice.value?.plan.id || '',
  set: (planId) => {
    if (!planId) return
    const option = additionalCatalogOptions.value.find(
      (item) => item.plan.id === planId,
    )
    if (option) selectPlan(option.plan)
  },
})

const selectedReviewFeatures = computed(() => {
  if (selectedOffer.value) return selectedOffer.value.features
  if (!selectedPlanPrice.value) return []
  return planBenefits(selectedPlanPrice.value.plan)
    .filter((benefit) => benefit.enabled)
    .map((benefit) => benefit.label)
})

function selectPlan(plan: PlanSummary) {
  const option = preferredPlanPrice(plan)
  if (!option) {
    toast.error('This plan does not have an active recurring price')
    return
  }
  selectedPlanPriceKey.value = option.key
  explicitPlanSelection.value = true
  trialDays.value = plan.trial_days || 14
  void nextTick(() => {
    const reduceMotion =
      typeof window !== 'undefined' &&
      window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
    upgradeConfirmation.value?.scrollIntoView({
      behavior: reduceMotion ? 'auto' : 'smooth',
      block: 'start',
    })
  })
}

function selectOffer(offer: DisplayPlanOffer) {
  if (offer.availability !== 'available' || !offer.plan || !offer.priceOption) {
    return
  }
  selectPlan(offer.plan)
}

function isOfferSelected(offer: DisplayPlanOffer) {
  return offer.plan?.id === selectedPlanPrice.value?.plan.id
}

function isCurrentOffer(offer: DisplayPlanOffer) {
  return (
    Boolean(offer.plan?.id) && subscription.value?.plan_id === offer.plan?.id
  )
}

function formatMoney(price: PlanPriceSummary) {
  return formatCurrencyMinorUnits(price.currency, price.unit_amount_minor)
}

function formatInterval(price: PlanPriceSummary) {
  const count = price.interval_count || 1
  if (count === 1) return price.interval
  return `${count} ${price.interval}s`
}

function displayOfferPrice(offer: DisplayPlanOffer) {
  if (offer.availability !== 'available' || !offer.priceOption) {
    return offer.priceLead
  }
  return formatMoney(offer.priceOption.price)
}

function displayOfferCadence(offer: DisplayPlanOffer) {
  if (offer.availability !== 'available' || !offer.priceOption) {
    return offer.priceCadence
  }
  return `/ ${formatInterval(offer.priceOption.price)}`
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
  const exactCurrent = planPriceOptions.value.find(
    (option) => option.key === currentPlanPriceKey.value,
  )
  const currentCatalogPlan =
    plans.value.find((plan) => plan.id === subscription.value?.plan_id) ?? null
  const currentPlan = currentCatalogPlan
    ? preferredPlanPrice(currentCatalogPlan)
    : null
  const firstMerchandised =
    displayOffers.value.find(
      (offer) =>
        offer.availability === 'available' && Boolean(offer.priceOption),
    )?.priceOption ?? null
  const fallback = currentPriceUnresolved.value
    ? null
    : (exactCurrent ??
      currentPlan ??
      firstMerchandised ??
      planPriceOptions.value[0] ??
      null)
  selectedPlanPriceKey.value = fallback?.key ?? ''
  explicitPlanSelection.value = false
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
    licenseChangeBlockingReason.value ||
    unresolvedPriceRequiresSelection.value
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
    class="h-full overflow-y-auto bg-[#08090a] text-white light:bg-[#f4f3ef] light:text-gray-950"
  >
    <header
      class="sticky top-0 z-20 border-b border-white/[0.08] bg-[#0b0c0e]/95 backdrop-blur-xl light:border-gray-200 light:bg-white/95"
    >
      <div
        class="mx-auto flex min-h-20 max-w-[1760px] items-center gap-4 px-5 py-4 md:px-8"
      >
        <RouterLink
          :to="backRoute"
          class="flex h-11 w-11 items-center justify-center rounded-xl border border-white/10 text-white/55 transition hover:border-white/20 hover:text-white light:border-gray-200 light:text-gray-500 light:hover:text-gray-900"
          :aria-label="backLabel"
        >
          <ArrowLeft aria-hidden="true" class="h-4 w-4" />
        </RouterLink>
        <div
          class="flex h-11 w-11 items-center justify-center rounded-xl border border-amber-300/20 bg-amber-300/10"
        >
          <CircleDollarSign
            aria-hidden="true"
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
          <p class="mt-0.5 text-sm text-white/50 light:text-gray-600">
            Choose the capability your team needs now, with a clear path to what
            comes next.
          </p>
        </div>
        <Button
          variant="outline"
          size="sm"
          :disabled="loading"
          @click="loadWorkspaceUpgrade"
        >
          <RefreshCw
            aria-hidden="true"
            class="mr-2 h-4 w-4"
            :class="{ 'animate-spin': loading }"
          />
          Refresh
        </Button>
      </div>
    </header>

    <main class="mx-auto max-w-[1760px] space-y-7 px-5 py-7 md:px-8">
      <div v-if="loading" class="space-y-5">
        <div
          class="h-36 animate-pulse rounded-3xl bg-white/[0.04] light:bg-white"
        />
        <div class="grid gap-4 md:grid-cols-2 2xl:grid-cols-4">
          <div
            v-for="index in 4"
            :key="index"
            class="h-[520px] animate-pulse rounded-3xl bg-white/[0.04] light:bg-white"
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
        <p class="mt-2 text-sm text-red-100/65 light:text-red-600">
          {{ loadError }}
        </p>
        <Button class="mt-5" variant="outline" @click="loadWorkspaceUpgrade">
          Try again
        </Button>
      </div>

      <template v-else>
        <section
          class="relative overflow-hidden rounded-3xl border border-white/[0.09] bg-[#111315] p-5 light:border-gray-200 light:bg-white md:p-6"
          aria-label="Workspace plan comparison"
        >
          <div
            class="pointer-events-none absolute -right-20 -top-24 h-72 w-72 rounded-full bg-amber-300/[0.06] blur-3xl"
          />
          <div
            class="relative grid gap-5 xl:grid-cols-[minmax(0,1fr)_minmax(420px,0.8fr)] xl:items-center"
          >
            <div class="flex items-start gap-4">
              <div
                class="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl border border-emerald-300/15 bg-emerald-300/[0.08]"
              >
                <Building2
                  aria-hidden="true"
                  class="h-5 w-5 text-emerald-300 light:text-emerald-700"
                />
              </div>
              <div>
                <p
                  class="text-sm font-medium text-white/55 light:text-gray-600"
                >
                  {{ organizationName }}
                </p>
                <div class="mt-1 flex flex-wrap items-center gap-2">
                  <h2 class="text-xl font-semibold tracking-tight">
                    {{ subscription?.plan_name || 'No active plan' }}
                  </h2>
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
                <div
                  class="mt-3 flex items-center gap-2 text-xs text-white/60 light:text-gray-500"
                >
                  <Clock3 aria-hidden="true" class="h-3.5 w-3.5" />
                  <span>
                    {{
                      subscription?.status === 'trialing'
                        ? 'Trial ends'
                        : 'Current period ends'
                    }}
                    {{
                      formatDate(
                        subscription?.status === 'trialing'
                          ? subscription?.trial_ends_at
                          : subscription?.current_period_end,
                      )
                    }}
                  </span>
                </div>
              </div>
            </div>

            <div
              class="rounded-2xl border border-amber-300/15 bg-amber-300/[0.045] px-4 py-3"
            >
              <p
                class="text-xs font-semibold text-amber-100 light:text-amber-800"
              >
                Product access, not portfolio capacity
              </p>
              <p
                class="mt-1 text-xs leading-5 text-white/55 light:text-gray-600"
              >
                Workspace subscriptions control which ReReply products one
                organization can use. Partner capacity controls how many
                customer workspaces a portfolio can create.
              </p>
            </div>
          </div>
        </section>

        <section>
          <div class="mb-5 grid gap-3 lg:grid-cols-[minmax(0,1fr)_460px]">
            <div>
              <p
                class="text-[11px] font-semibold uppercase tracking-[0.22em] text-amber-200/75 light:text-amber-700"
              >
                3 plans + Enterprise
              </p>
              <h2
                class="mt-2 max-w-3xl text-3xl font-semibold tracking-[-0.035em] md:text-4xl"
              >
                Choose the way your team grows
              </h2>
            </div>
            <p
              class="self-end text-sm leading-6 text-white/50 light:text-gray-600"
            >
              Start with WhatsApp automation, unite every channel, then add
              calling when Sprint launches. Every next tier carries the value of
              the one before it.
            </p>
          </div>

          <div
            v-if="isPlatformOwner && missingCatalogOffers.length"
            data-testid="workspace-plan-catalog-sync"
            class="mb-4 rounded-2xl border border-blue-300/15 bg-blue-300/[0.05] px-4 py-3 text-sm leading-6 text-white/65 light:text-gray-700"
          >
            The
            {{ missingCatalogOffers.map((offer) => offer.name).join(' and ') }}
            catalog
            {{ missingCatalogOffers.length === 1 ? 'entry is' : 'entries are' }}
            still syncing. Pricing remains visible for comparison, but selection
            stays disabled until the approved catalog price is available.
          </div>

          <div
            v-if="currentPriceUnresolved"
            data-testid="upgrade-price-unresolved"
            class="mb-4 rounded-2xl border border-amber-300/20 bg-amber-300/[0.06] px-4 py-3 text-sm leading-6 text-white/75 light:text-amber-900"
            role="status"
          >
            <p class="font-medium">Current billing price needs confirmation</p>
            <p class="mt-1 text-white/65 light:text-amber-800">
              <template v-if="unresolvedPriceRequiresSelection">
                The current license price is not in this workspace's assignable
                catalog. Nothing will change automatically. Choose a plan
                explicitly to continue.
              </template>
              <template v-else>
                You explicitly selected
                {{ selectedPlanPrice?.plan.name }} as the replacement. It will
                change only after an approval reference is entered and the
                update is submitted.
              </template>
            </p>
          </div>

          <div class="grid items-stretch gap-4 md:grid-cols-2 2xl:grid-cols-4">
            <article
              v-for="(offer, index) in displayOffers"
              :id="`workspace-plan-${offer.key}`"
              :key="offer.key"
              :data-testid="`workspace-plan-${offer.key}`"
              :aria-labelledby="`workspace-plan-${offer.key}-title`"
              class="plan-offer-card group relative flex min-h-[545px] flex-col overflow-hidden rounded-3xl border p-5 transition duration-300 md:p-6"
              :class="[
                offer.key === 'growth'
                  ? 'border-amber-300/35 bg-[linear-gradient(155deg,rgba(111,117,70,0.28),rgba(17,19,21,0.98)_48%)] shadow-[0_24px_70px_rgba(71,75,43,0.16)] light:bg-[linear-gradient(155deg,#fffbeb,#ffffff_52%)]'
                  : offer.key === 'sprint'
                    ? 'border-sky-300/15 bg-[linear-gradient(155deg,rgba(69,111,126,0.13),rgba(17,19,21,0.98)_45%)] light:border-sky-200 light:bg-[linear-gradient(155deg,#f0f9ff,#ffffff_52%)]'
                    : offer.key === 'enterprise'
                      ? 'border-white/[0.14] bg-[linear-gradient(155deg,rgba(255,255,255,0.07),rgba(17,19,21,0.98)_42%)] light:border-gray-300 light:bg-[linear-gradient(155deg,#f3f4f6,#ffffff_52%)]'
                      : 'border-white/[0.09] bg-[#111315] light:border-gray-200 light:bg-white',
                isOfferSelected(offer)
                  ? 'ring-2 ring-amber-300/55 ring-offset-2 ring-offset-[#08090a] light:ring-offset-[#f4f3ef]'
                  : 'hover:-translate-y-1 hover:border-white/20 light:hover:border-gray-300',
              ]"
            >
              <div
                class="pointer-events-none absolute right-5 top-2 font-mono text-[72px] font-semibold tracking-tighter text-white/[0.025] light:text-gray-950/[0.035]"
              >
                0{{ index + 1 }}
              </div>
              <div
                class="absolute inset-x-0 top-0 h-1"
                :class="{
                  'bg-emerald-300/70': offer.key === 'starter',
                  'bg-amber-300': offer.key === 'growth',
                  'bg-sky-300/70': offer.key === 'sprint',
                  'bg-white/35 light:bg-gray-700': offer.key === 'enterprise',
                }"
              />

              <div class="relative flex items-start justify-between gap-3">
                <div
                  class="flex h-11 w-11 items-center justify-center rounded-2xl border"
                  :class="{
                    'border-emerald-300/15 bg-emerald-300/[0.08] text-emerald-200 light:text-emerald-700':
                      offer.key === 'starter',
                    'border-amber-300/20 bg-amber-300/[0.1] text-amber-200 light:text-amber-800':
                      offer.key === 'growth',
                    'border-sky-300/15 bg-sky-300/[0.08] text-sky-200 light:text-sky-700':
                      offer.key === 'sprint',
                    'border-white/10 bg-white/[0.05] text-white/70 light:border-gray-200 light:text-gray-700':
                      offer.key === 'enterprise',
                  }"
                >
                  <component
                    :is="offer.icon"
                    aria-hidden="true"
                    class="h-5 w-5"
                  />
                </div>
                <Badge
                  :variant="offer.key === 'growth' ? 'success' : 'outline'"
                  class="relative"
                >
                  {{ offer.badge }}
                </Badge>
              </div>

              <div class="relative mt-5">
                <p
                  class="text-xs font-semibold uppercase tracking-[0.16em] text-white/60 light:text-gray-500"
                >
                  {{ offer.category }}
                </p>
                <div class="mt-2 flex flex-wrap items-center gap-2">
                  <h3
                    :id="`workspace-plan-${offer.key}-title`"
                    class="text-2xl font-semibold tracking-[-0.025em]"
                  >
                    {{ offer.name }}
                  </h3>
                  <Badge v-if="isCurrentOffer(offer)" variant="success">
                    Current
                  </Badge>
                </div>
                <div class="mt-5 flex items-end gap-2">
                  <p
                    class="text-3xl font-semibold tracking-[-0.04em]"
                    :class="{ 'text-2xl': offer.key === 'enterprise' }"
                  >
                    {{ displayOfferPrice(offer) }}
                  </p>
                  <p
                    v-if="displayOfferCadence(offer)"
                    class="pb-1 text-sm text-white/60 light:text-gray-500"
                  >
                    {{ displayOfferCadence(offer) }}
                  </p>
                </div>
                <p
                  class="mt-3 min-h-[72px] text-sm leading-6 text-white/55 light:text-gray-600"
                >
                  {{ offer.description }}
                </p>
              </div>

              <div
                class="my-5 h-px bg-gradient-to-r from-white/10 via-white/[0.04] to-transparent light:from-gray-200 light:via-gray-100"
              />

              <ul class="relative flex-1 space-y-3">
                <li
                  v-for="feature in offer.features"
                  :key="feature"
                  class="flex items-start gap-2.5 text-sm leading-5 text-white/75 light:text-gray-800"
                >
                  <span
                    class="mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-emerald-300/[0.08]"
                  >
                    <Check
                      aria-hidden="true"
                      class="h-3 w-3 text-emerald-300 light:text-emerald-700"
                    />
                  </span>
                  <span>{{ feature }}</span>
                </li>
              </ul>

              <Button
                v-if="offer.availability === 'available'"
                class="relative mt-6 h-11 w-full"
                :variant="offer.key === 'growth' ? 'default' : 'outline'"
                type="button"
                :disabled="!offer.plan || !offer.priceOption"
                :aria-pressed="isOfferSelected(offer)"
                @click="selectOffer(offer)"
              >
                {{
                  !offer.plan || !offer.priceOption
                    ? `${offer.name} catalog syncing`
                    : isOfferSelected(offer)
                      ? isCurrentPriceSelected
                        ? 'Current plan selected'
                        : retiredCurrentPrice && isCurrentOffer(offer)
                          ? 'Replacement price selected'
                          : 'Plan selected'
                      : `Choose ${offer.name}`
                }}
              </Button>
              <Button
                v-else-if="offer.availability === 'coming_soon'"
                class="relative mt-6 h-11 w-full"
                variant="outline"
                type="button"
                disabled
              >
                <Sparkles aria-hidden="true" class="h-4 w-4" />
                Planned launch
              </Button>
              <Button
                v-else
                as="a"
                :href="enterpriseContactHref"
                class="relative mt-6 h-11 w-full"
                variant="outline"
              >
                Email sales
                <ArrowUpRight aria-hidden="true" class="h-4 w-4" />
              </Button>
            </article>
          </div>

          <div
            v-if="isPlatformOwner && additionalCatalogOptions.length"
            data-testid="upgrade-other-catalog-plan"
            class="mt-4 grid gap-3 rounded-2xl border border-white/[0.1] bg-white/[0.035] p-4 light:border-gray-200 light:bg-white md:grid-cols-[minmax(0,1fr)_minmax(280px,420px)] md:items-center"
          >
            <div>
              <p class="text-sm font-medium">Other assignable catalog plans</p>
              <p
                class="mt-1 text-xs leading-5 text-white/60 light:text-gray-600"
              >
                Platform owners can select a private or specialist plan returned
                by this workspace's approved catalog.
              </p>
            </div>
            <div class="space-y-2">
              <Label for="upgrade-other-plan">Catalog plan</Label>
              <select
                id="upgrade-other-plan"
                v-model="additionalCatalogPlanId"
                data-testid="upgrade-other-plan"
                class="upgrade-select"
              >
                <option value="">Choose another plan</option>
                <option
                  v-for="option in additionalCatalogOptions"
                  :key="option.plan.id"
                  :value="option.plan.id"
                >
                  {{ option.plan.name }} —
                  {{ formatMoney(option.priceOption.price) }} /
                  {{ formatInterval(option.priceOption.price) }}
                </option>
              </select>
            </div>
          </div>

          <p
            class="mt-4 text-center text-xs leading-5 text-white/60 light:text-gray-600"
          >
            Monthly prices are shown in Malaysian Ringgit and exclude applicable
            taxes. Sprint is the planned commercial Calling launch and cannot be
            activated yet; any existing Calling beta access is separate and may
            change before launch.
          </p>
        </section>

        <section
          v-if="selectedPlanPrice"
          ref="upgradeConfirmation"
          data-testid="upgrade-plan-review"
          class="scroll-mt-24"
        >
          <Card
            class="overflow-hidden border-amber-300/20 bg-[#111315] light:border-amber-200 light:bg-white"
          >
            <div class="grid lg:grid-cols-[minmax(0,1fr)_minmax(390px,0.72fr)]">
              <CardHeader
                class="border-b border-white/[0.08] lg:border-b-0 lg:border-r light:border-gray-200"
              >
                <p
                  class="text-[11px] font-semibold uppercase tracking-[0.2em] text-amber-200/70 light:text-amber-700"
                >
                  Plan review
                </p>
                <CardTitle
                  aria-live="polite"
                  class="mt-2 text-2xl tracking-tight"
                >
                  {{ selectedOffer?.name || selectedPlanPrice.plan.name }}
                </CardTitle>
                <p
                  class="max-w-xl text-sm leading-6 text-white/55 light:text-gray-600"
                >
                  {{
                    selectedOffer?.description ||
                    selectedPlanPrice.plan.description ||
                    'Review the billing interval and activation details before applying this plan.'
                  }}
                </p>
                <div class="mt-4 grid gap-3 sm:grid-cols-2">
                  <div
                    v-for="feature in selectedReviewFeatures"
                    :key="feature"
                    class="flex items-center gap-2 text-xs text-white/70 light:text-gray-700"
                  >
                    <span
                      class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-emerald-300/10"
                    >
                      <Check
                        aria-hidden="true"
                        class="h-3 w-3 text-emerald-300 light:text-emerald-700"
                      />
                    </span>
                    {{ feature }}
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
                    class="mt-2 text-sm leading-6 text-white/50 light:text-gray-600"
                  >
                    {{ licenseChangeBlockingReason }}
                  </p>
                </div>

                <div v-else-if="isPlatformOwner" class="space-y-4">
                  <div
                    v-if="retiredCurrentPrice"
                    data-testid="upgrade-retired-price"
                    class="rounded-xl border border-blue-300/15 bg-blue-300/[0.05] px-4 py-3 text-xs leading-5 text-white/60 light:text-gray-700"
                  >
                    This workspace keeps its legacy
                    {{ formatMoney(retiredCurrentPrice.price) }} price until an
                    approved plan change is applied. The published replacement
                    is selected below; no billing change happens automatically.
                  </div>
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
                        data-testid="upgrade-trial"
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
                      :placeholder="`${organizationName.toUpperCase().replace(/[^A-Z0-9]+/g, '-')}-PLAN`"
                    />
                    <p
                      class="text-xs leading-5 text-white/60 light:text-gray-600"
                    >
                      Required for the audit trail. This records an approved
                      manual license; it does not claim that payment was
                      collected.
                    </p>
                  </div>
                  <Button
                    class="h-11 w-full"
                    data-testid="upgrade-submit"
                    :loading="submitting"
                    :disabled="
                      submitting ||
                      !approvalReference.trim() ||
                      isCurrentPriceSelected ||
                      unresolvedPriceRequiresSelection
                    "
                    @click="applySelectedPlan"
                  >
                    {{
                      isCurrentPriceSelected
                        ? 'Current license is already active'
                        : subscription?.plan_id
                          ? 'Update workspace license'
                          : 'Activate workspace license'
                    }}
                  </Button>
                </div>

                <div
                  v-else
                  class="rounded-xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-gray-200 light:bg-gray-50"
                >
                  <Headphones
                    aria-hidden="true"
                    class="h-5 w-5 text-amber-200 light:text-amber-700"
                  />
                  <p class="mt-3 font-medium">Plan change requires approval</p>
                  <p
                    class="mt-1 text-sm leading-6 text-white/50 light:text-gray-600"
                  >
                    Your workspace billing owner can apply the selected plan
                    after commercial approval.
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
  min-height: 2.75rem;
  width: 100%;
  border-radius: 0.75rem;
  border: 1px solid rgb(255 255 255 / 0.1);
  background: #090a0b;
  padding: 0 0.875rem;
  color: white;
  font-size: 0.875rem;
  outline: none;
  transition:
    border-color 160ms ease,
    box-shadow 160ms ease;
}

.upgrade-select:focus {
  border-color: rgb(252 211 77 / 0.45);
  box-shadow: 0 0 0 3px rgb(252 211 77 / 0.08);
}

:global(.light) .upgrade-select {
  border-color: rgb(229 231 235);
  background: white;
  color: rgb(17 24 39);
}

@media (prefers-reduced-motion: reduce) {
  .plan-offer-card {
    transition: none;
    transform: none !important;
  }
}
</style>
