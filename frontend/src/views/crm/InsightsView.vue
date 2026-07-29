<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  ArrowUpRight,
  AlertTriangle,
  CalendarCheck2,
  ChevronRight,
  CircleDollarSign,
  Clock3,
  LineChart,
  Loader2,
  PackageCheck,
  RefreshCw,
  Target,
  UsersRound,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { useAppToast } from '@/composables/useAppToast'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import { useAuthStore } from '@/stores/auth'
import {
  crmInsightsService,
  type CRMInsightMoney,
  type CRMInsights,
  type CRMSystemSegment,
  type CRMSystemSegmentContact,
  type CRMSystemSegmentContacts,
} from '@/services/crmInsights'

const router = useRouter()
const toast = useAppToast()
const authStore = useAuthStore()
const SEGMENT_PAGE_SIZE = 50
const loading = ref(true)
const loadingContacts = ref(false)
const loadingMoreContacts = ref(false)
const insights = ref<CRMInsights | null>(null)
const loadError = ref('')
const segmentsError = ref('')
const segmentContactsError = ref('')
const segments = ref<CRMSystemSegment[]>([])
const selectedSegment = ref<CRMSystemSegment | null>(null)
const segmentContacts = ref<CRMSystemSegmentContact[]>([])
const segmentTotal = ref(0)
const segmentPage = ref(1)
let segmentRequestSequence = 0

function localDate(daysFromToday = 0) {
  const date = new Date()
  date.setDate(date.getDate() + daysFromToday)
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

const dateFrom = ref(localDate(-29))
const dateTo = ref(localDate())
const invalidRange = computed(
  () => !dateFrom.value || !dateTo.value || dateFrom.value > dateTo.value,
)
const canViewPipeline = computed(
  () =>
    authStore.hasProductEntitlement('crm.enabled') &&
    authStore.hasPermission('crm.leads', 'read'),
)
const canViewBookings = computed(
  () =>
    authStore.hasProductEntitlement('bookings.enabled') &&
    authStore.hasPermission('bookings', 'read'),
)
const canViewRevenue = computed(
  () =>
    authStore.hasProductEntitlement('commerce.enabled') &&
    authStore.hasPermission('payments', 'read'),
)
const canViewPackages = computed(
  () =>
    authStore.hasProductEntitlement('commerce.enabled') &&
    authStore.hasPermission('packages', 'read'),
)
const canViewTasks = computed(
  () =>
    authStore.hasProductEntitlement('crm.enabled') &&
    authStore.hasPermission('tasks', 'read'),
)
const canViewSegments = computed(
  () =>
    authStore.hasProductEntitlement('crm.enabled') &&
    authStore.hasPermission('contacts', 'read'),
)
const canOpenChat = computed(() => authStore.hasPermission('chat', 'read'))
const hasContinuityAccess = computed(
  () => canViewTasks.value || canViewRevenue.value || canViewPackages.value,
)
const conversionRate = computed(() => clampPercent(insights.value?.pipeline.conversion_rate ?? 0))
const attendanceRate = computed(() => clampPercent(insights.value?.bookings.attendance_rate ?? 0))
const noShowRate = computed(() => clampPercent(insights.value?.bookings.no_show_rate ?? 0))
const riskCount = computed(
  () =>
    (canViewTasks.value ? (insights.value?.tasks.overdue ?? 0) : 0) +
    (canViewRevenue.value ? (insights.value?.revenue.overdue_invoices ?? 0) : 0) +
    (canViewPackages.value
      ? (insights.value?.packages.low_balance ?? 0) +
        (insights.value?.packages.expiring_soon ?? 0)
      : 0),
)
const hasMoreSegmentContacts = computed(
  () => segmentContacts.value.length < segmentTotal.value,
)

function clampPercent(value: number) {
  return Math.min(100, Math.max(0, Number.isFinite(value) ? value : 0))
}

function formatPercent(value: number) {
  return `${clampPercent(value).toFixed(value % 1 === 0 ? 0 : 1)}%`
}

function formatMoney(item: CRMInsightMoney) {
  try {
    return new Intl.NumberFormat(undefined, {
      style: 'currency',
      currency: item.currency || 'MYR',
      maximumFractionDigits: 2,
    }).format((item.amount_minor ?? 0) / 100)
  } catch {
    return `${item.currency || 'MYR'} ${((item.amount_minor ?? 0) / 100).toFixed(2)}`
  }
}

function moneySummary(items: CRMInsightMoney[] | undefined) {
  if (!items?.length) return '—'
  return items.map(formatMoney).join(' · ')
}

function formatWhen(value?: string) {
  if (!value) return 'No recent message'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'No recent message'
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(date)
}

function segmentTone(key: string) {
  if (['overdue_invoice', 'no_show_recovery'].includes(key)) {
    return 'border-red-400/20 bg-red-400/[0.05] text-red-200 light:text-red-700'
  }
  if (['package_low', 'package_expiring', 'needs_follow_up'].includes(key)) {
    return 'border-amber-300/20 bg-amber-300/[0.05] text-amber-100 light:text-amber-700'
  }
  return 'border-sky-300/20 bg-sky-300/[0.05] text-sky-100 light:text-sky-700'
}

async function load() {
  if (invalidRange.value) {
    toast.warning('Choose a valid reporting period')
    return
  }

  loading.value = true
  loadError.value = ''
  let segmentFailure: unknown = null
  try {
    const segmentRequest = canViewSegments.value
      ? crmInsightsService.segments().catch((error: unknown) => {
          segmentFailure = error
          return null
        })
      : Promise.resolve(null)
    const [insightResponse, segmentResponse] = await Promise.all([
      crmInsightsService.get({ from: dateFrom.value, to: dateTo.value }),
      segmentRequest,
    ])
    insights.value = unwrapItemResponse<CRMInsights>(insightResponse)

    if (canViewSegments.value) {
      const segmentPayload = segmentResponse
        ? unwrapItemResponse<{ segments: CRMSystemSegment[] }>(segmentResponse)
        : null
      segments.value = segmentPayload?.segments ?? []
      segmentsError.value = segmentFailure
        ? getErrorMessage(segmentFailure, 'Customer segments are temporarily unavailable')
        : ''
    } else {
      segmentRequestSequence += 1
      segments.value = []
      segmentsError.value = ''
      selectedSegment.value = null
      segmentContacts.value = []
      segmentTotal.value = 0
    }

    if (selectedSegment.value) {
      selectedSegment.value =
        segments.value.find((segment) => segment.key === selectedSegment.value?.key) ?? null
      if (selectedSegment.value) await selectSegment(selectedSegment.value)
      else {
        segmentContacts.value = []
        segmentTotal.value = 0
      }
    }
  } catch (error) {
    loadError.value = getErrorMessage(error, 'CRM insights could not be loaded')
    toast.error('CRM insights could not be loaded', loadError.value)
  } finally {
    loading.value = false
  }
}

async function selectSegment(segment: CRMSystemSegment) {
  selectedSegment.value = segment
  segmentContacts.value = []
  segmentTotal.value = segment.count
  segmentPage.value = 1
  await loadSegmentPage(segment, 1, false)
}

async function loadSegmentPage(
  segment: CRMSystemSegment,
  page: number,
  append: boolean,
) {
  const requestSequence = ++segmentRequestSequence
  if (append) loadingMoreContacts.value = true
  else loadingContacts.value = true
  segmentContactsError.value = ''
  try {
    const response = await crmInsightsService.segmentContacts(segment.key, {
      page,
      limit: SEGMENT_PAGE_SIZE,
    })
    if (
      requestSequence !== segmentRequestSequence ||
      selectedSegment.value?.key !== segment.key
    ) {
      return
    }
    const result = unwrapItemResponse<CRMSystemSegmentContacts>(response)
    const incoming = result.contacts ?? []
    if (append) {
      const contactsByID = new Map(
        [...segmentContacts.value, ...incoming].map((contact) => [contact.id, contact]),
      )
      segmentContacts.value = [...contactsByID.values()]
    } else {
      segmentContacts.value = incoming
    }
    segmentTotal.value = result?.total ?? segment.count
    segmentPage.value = result?.page ?? page
    if (result?.segment) selectedSegment.value = result.segment
  } catch (error) {
    if (requestSequence !== segmentRequestSequence) return
    segmentContactsError.value = getErrorMessage(
      error,
      'Segment contacts could not be loaded',
    )
    if (!append) segmentContacts.value = []
    toast.error('Segment contacts could not be loaded', segmentContactsError.value)
  } finally {
    if (requestSequence === segmentRequestSequence) {
      loadingContacts.value = false
      loadingMoreContacts.value = false
    }
  }
}

async function loadMoreSegmentContacts() {
  if (
    !selectedSegment.value ||
    loadingContacts.value ||
    loadingMoreContacts.value ||
    !hasMoreSegmentContacts.value
  ) {
    return
  }
  await loadSegmentPage(selectedSegment.value, segmentPage.value + 1, true)
}

function contactName(contact: CRMSystemSegmentContact) {
  return contact.profile_name || contact.phone_number || 'Unnamed customer'
}

function openContact(contact: CRMSystemSegmentContact) {
  if (!canOpenChat.value) return
  void router.push(`/chat/${contact.id}`)
}

onMounted(load)
</script>

<template>
  <div class="flex h-full flex-col overflow-hidden bg-[#08090a] light:bg-[#f5f6f7]">
    <PageHeader
      title="CRM insights"
      description="Revenue and care performance."
      :icon="LineChart"
      icon-gradient="bg-cyan-500/15 text-cyan-300 ring-1 ring-cyan-300/20"
    >
      <template #actions>
        <div class="hidden items-end gap-2 2xl:flex">
          <label class="grid gap-1 text-[10px] font-medium uppercase tracking-[0.14em] text-muted-foreground">
            From
            <Input
              v-model="dateFrom"
              name="crm-insights-from"
              type="date"
              class="h-10 w-[148px]"
              :max="dateTo"
              :aria-invalid="invalidRange"
            />
          </label>
          <label class="grid gap-1 text-[10px] font-medium uppercase tracking-[0.14em] text-muted-foreground">
            To
            <Input
              v-model="dateTo"
              name="crm-insights-to"
              type="date"
              class="h-10 w-[148px]"
              :min="dateFrom"
              :aria-invalid="invalidRange"
            />
          </label>
          <Button
            variant="outline"
            class="h-10"
            :disabled="loading || invalidRange"
            aria-label="Refresh CRM insights"
            @click="load"
          >
            <RefreshCw :class="['mr-2 h-4 w-4', loading && 'animate-spin']" />
            Refresh
          </Button>
        </div>
      </template>
    </PageHeader>

    <section
      class="shrink-0 border-b border-white/[0.08] bg-[#0b0c0d] px-4 py-3 light:border-gray-200 light:bg-white 2xl:hidden"
      aria-label="CRM insights reporting period"
    >
      <div class="grid grid-cols-2 gap-2">
        <label class="grid min-w-0 gap-1 text-[10px] font-medium uppercase tracking-[0.14em] text-muted-foreground">
          From
          <Input
            v-model="dateFrom"
            name="crm-insights-from-compact"
            type="date"
            class="h-11 w-full min-w-0"
            :max="dateTo"
            :aria-invalid="invalidRange"
          />
        </label>
        <label class="grid min-w-0 gap-1 text-[10px] font-medium uppercase tracking-[0.14em] text-muted-foreground">
          To
          <Input
            v-model="dateTo"
            name="crm-insights-to-compact"
            type="date"
            class="h-11 w-full min-w-0"
            :min="dateFrom"
            :aria-invalid="invalidRange"
          />
        </label>
        <Button
          variant="outline"
          class="col-span-2 h-11"
          :disabled="loading || invalidRange"
          aria-label="Refresh CRM insights"
          @click="load"
        >
          <RefreshCw :class="['mr-2 h-4 w-4', loading && 'animate-spin']" />
          Refresh insights
        </Button>
      </div>
      <p v-if="invalidRange" class="mt-2 text-xs text-red-300 light:text-red-700" role="alert">
        The start date must be on or before the end date.
      </p>
    </section>

    <div
      v-if="loading && !insights"
      class="flex flex-1 items-center justify-center"
      role="status"
      aria-live="polite"
    >
      <div class="text-center">
        <Loader2 class="mx-auto h-7 w-7 animate-spin text-cyan-300" />
        <p class="mt-3 text-sm text-muted-foreground">Building the customer revenue picture…</p>
      </div>
    </div>

    <div
      v-else-if="loadError && !insights"
      class="flex flex-1 items-center justify-center p-6"
      role="alert"
    >
      <div class="max-w-md text-center">
        <AlertTriangle class="mx-auto h-8 w-8 text-red-300 light:text-red-600" />
        <h2 class="mt-3 text-base font-semibold">CRM insights are unavailable</h2>
        <p class="mt-1 text-sm text-muted-foreground">{{ loadError }}</p>
        <Button variant="outline" class="mt-4 h-11" @click="load">
          <RefreshCw class="mr-2 h-4 w-4" />
          Try again
        </Button>
      </div>
    </div>

    <main v-else class="flex-1 overflow-y-auto">
      <div class="mx-auto max-w-[1500px] space-y-5 p-4 md:p-6">
        <div
          v-if="loadError"
          class="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-red-400/20 bg-red-400/[0.05] px-4 py-3 text-sm text-red-100 light:text-red-800"
          role="alert"
        >
          <span>{{ loadError }} Existing figures remain visible.</span>
          <Button variant="outline" size="sm" :disabled="loading" @click="load">Retry</Button>
        </div>
        <section class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-5" aria-label="CRM summary">
          <Card v-if="canViewPipeline" class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardHeader class="flex-row items-start justify-between space-y-0 pb-2">
              <div>
                <CardDescription>Open pipeline</CardDescription>
                <CardTitle class="mt-2 break-words text-xl">{{ moneySummary(insights?.pipeline.open_value) }}</CardTitle>
              </div>
              <Target class="h-5 w-5 text-cyan-300" />
            </CardHeader>
            <CardContent class="text-xs text-muted-foreground">
              {{ insights?.pipeline.open_count ?? 0 }} active journeys
            </CardContent>
          </Card>

          <Card v-if="canViewPipeline" class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardHeader class="flex-row items-start justify-between space-y-0 pb-2">
              <div>
                <CardDescription>Conversion</CardDescription>
                <CardTitle class="mt-2 text-2xl">{{ formatPercent(conversionRate) }}</CardTitle>
              </div>
              <ArrowUpRight class="h-5 w-5 text-emerald-300" />
            </CardHeader>
            <CardContent>
              <div
                class="h-1.5 overflow-hidden rounded-full bg-white/[0.07] light:bg-gray-100"
                role="progressbar"
                aria-label="Lead conversion rate"
                aria-valuemin="0"
                aria-valuemax="100"
                :aria-valuenow="conversionRate"
              >
                <div class="h-full rounded-full bg-emerald-400" :style="{ width: `${conversionRate}%` }" />
              </div>
            </CardContent>
          </Card>

          <Card v-if="canViewBookings" class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardHeader class="flex-row items-start justify-between space-y-0 pb-2">
              <div>
                <CardDescription>Attendance</CardDescription>
                <CardTitle class="mt-2 text-2xl">{{ formatPercent(attendanceRate) }}</CardTitle>
              </div>
              <CalendarCheck2 class="h-5 w-5 text-sky-300" />
            </CardHeader>
            <CardContent class="text-xs text-muted-foreground">
              {{ insights?.bookings.completed ?? 0 }} completed · {{ formatPercent(noShowRate) }} no-show
            </CardContent>
          </Card>

          <Card v-if="canViewRevenue" class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardHeader class="flex-row items-start justify-between space-y-0 pb-2">
              <div>
                <CardDescription>Collected</CardDescription>
                <CardTitle class="mt-2 break-words text-xl">{{ moneySummary(insights?.revenue.collected) }}</CardTitle>
              </div>
              <CircleDollarSign class="h-5 w-5 text-emerald-300" />
            </CardHeader>
            <CardContent class="text-xs text-muted-foreground">
              {{ moneySummary(insights?.revenue.outstanding) }} outstanding
            </CardContent>
          </Card>

          <Card v-if="hasContinuityAccess" class="border-amber-300/15 bg-amber-300/[0.035] light:border-amber-200 light:bg-amber-50">
            <CardHeader class="flex-row items-start justify-between space-y-0 pb-2">
              <div>
                <CardDescription>Needs attention</CardDescription>
                <CardTitle class="mt-2 text-2xl">{{ riskCount }}</CardTitle>
              </div>
              <AlertTriangle class="h-5 w-5 text-amber-300 light:text-amber-600" />
            </CardHeader>
            <CardContent class="text-xs text-muted-foreground">
              Tasks, invoices and package retention signals
            </CardContent>
          </Card>
        </section>

        <section
          class="grid gap-5"
          :class="hasContinuityAccess ? 'xl:grid-cols-[1.35fr_.65fr]' : 'grid-cols-1'"
        >
          <Card class="border-white/[0.08] bg-white/[0.02] light:border-gray-200 light:bg-white">
            <CardHeader>
              <div class="flex flex-wrap items-start justify-between gap-3">
                <div>
                  <CardTitle class="text-base">Dynamic customer segments</CardTitle>
                  <CardDescription class="mt-1">
                    Live cohorts derived from journeys, visits, balances and follow-up state.
                  </CardDescription>
                </div>
                <Badge variant="outline">{{ segments.length }} live segments</Badge>
              </div>
            </CardHeader>
            <CardContent>
              <div v-if="segmentsError" class="rounded-xl border border-red-400/20 bg-red-400/[0.04] p-5 text-sm text-red-100 light:text-red-800" role="alert">
                <p class="font-medium">Customer segments are unavailable</p>
                <p class="mt-1 text-xs opacity-70">{{ segmentsError }}</p>
              </div>
              <div v-else-if="segments.length" class="grid gap-2 md:grid-cols-2">
                <button
                  v-for="segment in segments"
                  :key="segment.key"
                  type="button"
                  class="group flex min-h-24 items-center gap-3 rounded-xl border p-3 text-left transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300"
                  :class="[
                    segmentTone(segment.key),
                    selectedSegment?.key === segment.key && 'ring-1 ring-current',
                  ]"
                  :aria-pressed="selectedSegment?.key === segment.key"
                  @click="selectSegment(segment)"
                >
                  <span class="flex h-11 min-w-11 items-center justify-center rounded-lg bg-black/10 text-lg font-semibold light:bg-white/70">
                    {{ segment.count }}
                  </span>
                  <span class="min-w-0 flex-1">
                    <span class="block text-sm font-semibold">{{ segment.label }}</span>
                    <span class="mt-1 line-clamp-2 block text-xs opacity-65">{{ segment.description }}</span>
                  </span>
                  <ChevronRight class="h-4 w-4 shrink-0 opacity-40 transition group-hover:translate-x-0.5 group-hover:opacity-80" />
                </button>
              </div>
              <div v-else class="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
                {{
                  canViewSegments
                    ? 'No retention or recovery segments match right now.'
                    : 'Contact segments require Contacts read access.'
                }}
              </div>
            </CardContent>
          </Card>

          <Card v-if="hasContinuityAccess" class="border-white/[0.08] bg-white/[0.02] light:border-gray-200 light:bg-white">
            <CardHeader>
              <CardTitle class="text-base">Care continuity</CardTitle>
              <CardDescription>Signals converted into internal follow-up work.</CardDescription>
            </CardHeader>
            <CardContent class="space-y-3">
              <div v-if="canViewTasks" class="flex items-center justify-between rounded-lg border border-white/[0.07] p-3 light:border-gray-200">
                <span class="flex items-center gap-2 text-sm"><Clock3 class="h-4 w-4 text-amber-300" /> Overdue tasks</span>
                <strong>{{ insights?.tasks.overdue ?? 0 }}</strong>
              </div>
              <div v-if="canViewRevenue" class="flex items-center justify-between rounded-lg border border-white/[0.07] p-3 light:border-gray-200">
                <span class="flex items-center gap-2 text-sm"><CircleDollarSign class="h-4 w-4 text-red-300" /> Overdue invoices</span>
                <strong>{{ insights?.revenue.overdue_invoices ?? 0 }}</strong>
              </div>
              <div v-if="canViewPackages" class="flex items-center justify-between rounded-lg border border-white/[0.07] p-3 light:border-gray-200">
                <span class="flex items-center gap-2 text-sm"><PackageCheck class="h-4 w-4 text-cyan-300" /> Low package balance</span>
                <strong>{{ insights?.packages.low_balance ?? 0 }}</strong>
              </div>
              <div v-if="canViewPackages" class="flex items-center justify-between rounded-lg border border-white/[0.07] p-3 light:border-gray-200">
                <span class="flex items-center gap-2 text-sm"><AlertTriangle class="h-4 w-4 text-amber-300" /> Expiring soon</span>
                <strong>{{ insights?.packages.expiring_soon ?? 0 }}</strong>
              </div>
              <p class="pt-1 text-xs leading-5 text-muted-foreground">
                Automation creates auditable team tasks. Customer messages remain human-approved.
              </p>
            </CardContent>
          </Card>
        </section>

        <Card
          v-if="selectedSegment"
          class="border-cyan-300/15 bg-cyan-300/[0.025] light:border-cyan-100 light:bg-white"
        >
          <CardHeader>
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <CardTitle class="flex items-center gap-2 text-base">
                  <UsersRound class="h-4 w-4 text-cyan-300 light:text-cyan-700" />
                  {{ selectedSegment.label }}
                </CardTitle>
                <CardDescription class="mt-1">{{ selectedSegment.description }}</CardDescription>
              </div>
              <Badge variant="outline">{{ segmentTotal }} customers</Badge>
            </div>
          </CardHeader>
          <CardContent>
            <div
              v-if="loadingContacts"
              class="flex min-h-32 items-center justify-center"
              role="status"
              aria-label="Loading segment contacts"
            >
              <Loader2 class="h-5 w-5 animate-spin text-cyan-300" />
            </div>
            <div
              v-else-if="segmentContactsError && !segmentContacts.length"
              class="rounded-xl border border-red-400/20 bg-red-400/[0.04] p-6 text-center text-sm text-red-100 light:text-red-800"
              role="alert"
            >
              <p class="font-medium">Segment contacts are unavailable</p>
              <p class="mt-1 text-xs opacity-70">{{ segmentContactsError }}</p>
              <Button variant="outline" size="sm" class="mt-3" @click="selectSegment(selectedSegment)">
                Try again
              </Button>
            </div>
            <div v-else-if="segmentContacts.length" class="space-y-3">
              <p v-if="!canOpenChat" class="text-xs text-muted-foreground">
                Chat access is required to open these customer conversations.
              </p>
              <div class="overflow-hidden rounded-xl border border-white/[0.07] light:border-gray-200">
              <button
                v-for="contact in segmentContacts"
                :key="contact.id"
                type="button"
                class="flex min-h-16 w-full items-center gap-3 border-b border-white/[0.06] px-4 py-3 text-left transition last:border-0 hover:bg-white/[0.035] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-cyan-300 disabled:cursor-default disabled:opacity-75 disabled:hover:bg-transparent light:border-gray-100 light:hover:bg-gray-50"
                :disabled="!canOpenChat"
                :aria-label="canOpenChat ? `Open ${contactName(contact)} in chat` : `${contactName(contact)}; chat access required`"
                @click="openContact(contact)"
              >
                <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-cyan-300/10 font-semibold text-cyan-200 light:text-cyan-700">
                  {{ contactName(contact).slice(0, 1).toUpperCase() }}
                </span>
                <span class="min-w-0 flex-1">
                  <span class="block truncate text-sm font-medium">
                    {{ contactName(contact) }}
                  </span>
                  <span class="mt-0.5 block truncate text-xs text-muted-foreground">
                    {{ contact.phone_number || 'No phone number' }} · {{ formatWhen(contact.last_message_at) }}
                  </span>
                </span>
                <ArrowUpRight v-if="canOpenChat" class="h-4 w-4 shrink-0 text-muted-foreground" />
                <span v-else class="shrink-0 text-[10px] text-muted-foreground">Read only</span>
              </button>
              </div>
              <div v-if="hasMoreSegmentContacts" class="text-center">
                <Button
                  variant="outline"
                  class="h-11"
                  :disabled="loadingMoreContacts"
                  @click="loadMoreSegmentContacts"
                >
                  <Loader2 v-if="loadingMoreContacts" class="mr-2 h-4 w-4 animate-spin" />
                  Load more customers
                </Button>
              </div>
            </div>
            <div v-else class="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
              This segment has no matching customers right now.
            </div>
          </CardContent>
        </Card>
      </div>
    </main>
  </div>
</template>
