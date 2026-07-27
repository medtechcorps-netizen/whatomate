<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  AlertTriangle,
  CheckCircle2,
  Clock3,
  DatabaseBackup,
  FileKey2,
  HeartPulse,
  LifeBuoy,
  Loader2,
  LockKeyhole,
  Plus,
  RefreshCw,
  ShieldCheck,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import {
  privacyService,
  supportService,
  type PrivacyRequest,
  type RetentionPolicy,
  type SupportCase,
  type TenantHealth,
} from '@/services/productSuite'

type TrustPane = 'privacy' | 'support'

interface RecoverySummary {
  checkpoints?: Array<{
    id: string
    kind?: string
    status: string
    created_at: string
    expires_at?: string
  }>
  last_verified_at?: string
  recovery_ready?: boolean
}

interface TrustQueueSummary {
  privacy_visible: boolean
  privacy_open: number
  support_visible: boolean
  support_open: number
}

const route = useRoute()
const router = useRouter()
const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const saving = ref(false)
const loadingMore = ref(false)
const pane = ref<TrustPane>(route.path.includes('/support') ? 'support' : 'privacy')
const policies = ref<RetentionPolicy[]>([])
const requests = ref<PrivacyRequest[]>([])
const cases = ref<SupportCase[]>([])
const health = ref<TenantHealth | null>(null)
const recovery = ref<RecoverySummary | null>(null)
const queueSummary = ref<TrustQueueSummary | null>(null)
const privacyPage = ref(1)
const privacyTotal = ref(0)
const supportPage = ref(1)
const supportTotal = ref(0)
const editingRetentionCategory = ref('')
const privacyDraft = ref({
  request_type: 'access' as PrivacyRequest['request_type'],
  contact_id: '',
})
const supportDraft = ref({
  title: '',
  description: '',
  severity: 'normal' as SupportCase['severity'],
})
const retentionDraft = ref({
  data_category: 'contacts',
  retention_days: 2555,
  grace_period_days: 30,
  action: 'review' as RetentionPolicy['action'],
  legal_basis: 'Documented operational and legal requirement',
  is_enabled: true,
})
const privacyUpdates = reactive<Record<string, { status: string; resolution: string }>>({})
const supportUpdates = reactive<Record<string, { status: SupportCase['status']; resolution: string }>>({})

const canReadPrivacySettings = computed(() => authStore.hasPermission('privacy.settings', 'read'))
const canWritePrivacySettings = computed(() => authStore.hasPermission('privacy.settings', 'write'))
const canReadPrivacyRequests = computed(() => authStore.hasPermission('privacy.requests', 'read'))
const canWritePrivacyRequests = computed(() => authStore.hasPermission('privacy.requests', 'write'))
const canReadSupport = computed(() => authStore.hasPermission('support', 'read'))
const canWriteSupport = computed(() => authStore.hasPermission('support', 'write'))
const availablePanes = computed(() => [
  ...(canReadPrivacySettings.value || canReadPrivacyRequests.value
    ? [{ key: 'privacy' as const, label: 'Privacy & compliance', icon: LockKeyhole }]
    : []),
  ...(canReadSupport.value
    ? [{ key: 'support' as const, label: 'Support & recovery', icon: LifeBuoy }]
    : []),
])

const openPrivacy = computed(() =>
  queueSummary.value?.privacy_visible
    ? queueSummary.value.privacy_open
    : requests.value.filter(
        (item) => !['completed', 'denied', 'canceled', 'expired'].includes(item.status),
      ).length,
)
const openCases = computed(() =>
  queueSummary.value?.support_visible
    ? queueSummary.value.support_open
    : cases.value.filter((item) => !['resolved', 'closed'].includes(item.status)).length,
)
const failingChecks = computed(() => health.value?.checks.filter((check) => ['warn', 'fail', 'not_configured'].includes(check.status)).length ?? 0)

function privacyNextStatuses(status: string) {
  const transitions: Record<string, string[]> = {
    received: ['awaiting_verification', 'verified', 'in_progress', 'denied', 'canceled'],
    awaiting_verification: ['verified', 'denied', 'canceled', 'expired'],
    verified: ['in_progress', 'completed', 'denied', 'canceled'],
    in_progress: ['completed', 'denied', 'canceled'],
  }
  return transitions[status] ?? []
}

function supportNextStatuses(status: SupportCase['status']): SupportCase['status'][] {
  if (status === 'closed') return []
  if (status === 'resolved') return ['open', 'investigating', 'closed']
  return ['open', 'investigating', 'waiting', 'waiting_customer', 'waiting_internal', 'resolved', 'closed']
    .filter((item) => item !== status) as SupportCase['status'][]
}

function setPane(next: TrustPane) {
  pane.value = next
  router.replace(next === 'support' ? '/settings/support' : '/settings/privacy')
}

function formatDate(value?: string) {
  if (!value) return 'Not scheduled'
  return new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short', year: 'numeric' }).format(new Date(value))
}

function totalFromResponse(response: any) {
  const payload = response?.data?.data ?? response?.data
  return typeof payload?.total === 'number' ? payload.total : 0
}

async function load() {
  loading.value = true
  try {
    const [settingsResponse, requestsResponse, casesResponse, healthResponse, recoveryResponse, summaryResponse] = await Promise.all([
      canReadPrivacySettings.value ? privacyService.settings() : Promise.resolve(null),
      canReadPrivacyRequests.value ? privacyService.requests({ limit: 100 }) : Promise.resolve(null),
      canReadSupport.value ? supportService.cases({ limit: 100 }) : Promise.resolve(null),
      canReadSupport.value ? supportService.health() : Promise.resolve(null),
      canReadSupport.value ? supportService.recovery() : Promise.resolve(null),
      canReadPrivacyRequests.value || canReadSupport.value
        ? supportService.summary()
        : Promise.resolve(null),
    ])
    const settings = settingsResponse
      ? unwrapItemResponse<{ retention_policies?: RetentionPolicy[] }>(settingsResponse)
      : null
    policies.value = settingsResponse
      ? settings?.retention_policies ?? unwrapListResponse<RetentionPolicy>(settingsResponse, 'retention_policies')
      : []
    requests.value = requestsResponse
      ? unwrapListResponse<PrivacyRequest>(requestsResponse, 'requests')
      : []
    privacyTotal.value = requestsResponse ? totalFromResponse(requestsResponse) : 0
    privacyPage.value = 1
    cases.value = casesResponse
      ? unwrapListResponse<SupportCase>(casesResponse, 'cases')
      : []
    supportTotal.value = casesResponse ? totalFromResponse(casesResponse) : 0
    supportPage.value = 1
    for (const request of requests.value) {
      privacyUpdates[request.id] = {
        status: privacyNextStatuses(request.status)[0] ?? request.status,
        resolution: request.resolution ?? '',
      }
    }
    for (const supportCase of cases.value) {
      supportUpdates[supportCase.id] = {
        status: supportNextStatuses(supportCase.status)[0] ?? supportCase.status,
        resolution: '',
      }
    }
    health.value = healthResponse ? unwrapItemResponse<TenantHealth>(healthResponse) : null
    recovery.value = recoveryResponse ? unwrapItemResponse<RecoverySummary>(recoveryResponse) : null
    queueSummary.value = summaryResponse
      ? unwrapItemResponse<TrustQueueSummary>(summaryResponse)
      : null
  } catch (error) {
    toast.error('Trust center could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function loadMoreTrust(kind: 'privacy' | 'support') {
  if (loadingMore.value) return
  loadingMore.value = true
  try {
    if (kind === 'privacy') {
      const nextPage = privacyPage.value + 1
      const response = await privacyService.requests({ page: nextPage, limit: 100 })
      const incoming = unwrapListResponse<PrivacyRequest>(response, 'requests')
      const existing = new Set(requests.value.map((item) => item.id))
      for (const request of incoming) {
        if (existing.has(request.id)) continue
        requests.value.push(request)
        privacyUpdates[request.id] = {
          status: privacyNextStatuses(request.status)[0] ?? request.status,
          resolution: request.resolution ?? '',
        }
      }
      privacyPage.value = nextPage
      privacyTotal.value = totalFromResponse(response)
      return
    }
    const nextPage = supportPage.value + 1
    const response = await supportService.cases({ page: nextPage, limit: 100 })
    const incoming = unwrapListResponse<SupportCase>(response, 'cases')
    const existing = new Set(cases.value.map((item) => item.id))
    for (const supportCase of incoming) {
      if (existing.has(supportCase.id)) continue
      cases.value.push(supportCase)
      supportUpdates[supportCase.id] = {
        status: supportNextStatuses(supportCase.status)[0] ?? supportCase.status,
        resolution: '',
      }
    }
    supportPage.value = nextPage
    supportTotal.value = totalFromResponse(response)
  } catch (error) {
    toast.error('Older trust records could not be loaded', getErrorMessage(error))
  } finally {
    loadingMore.value = false
  }
}

async function createPrivacyRequest() {
  saving.value = true
  try {
    await privacyService.createRequest({
      request_type: privacyDraft.value.request_type,
      contact_id: privacyDraft.value.contact_id.trim() || undefined,
    })
    privacyDraft.value.contact_id = ''
    toast.success('Privacy request opened')
    await load()
  } catch (error) {
    toast.error('Privacy request was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function saveRetentionPolicy() {
  if (
    !retentionDraft.value.data_category.trim() ||
    retentionDraft.value.retention_days < 1 ||
    retentionDraft.value.grace_period_days < 0
  ) {
    toast.warning('Enter a category and a valid retention schedule')
    return
  }
  saving.value = true
  try {
    await privacyService.updateSettings({
      retention_policies: [{
        data_category: retentionDraft.value.data_category.trim().toLowerCase().replace(/\s+/g, '_'),
        retention_days: retentionDraft.value.retention_days,
        grace_period_days: retentionDraft.value.grace_period_days,
        action: retentionDraft.value.action,
        legal_basis: retentionDraft.value.legal_basis.trim(),
        is_enabled: retentionDraft.value.is_enabled,
      }],
    })
    editingRetentionCategory.value = ''
    toast.success('Retention baseline saved')
    await load()
  } catch (error) {
    toast.error('Retention policy was not saved', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

function editRetentionPolicy(policy: RetentionPolicy) {
  editingRetentionCategory.value = policy.data_category
  retentionDraft.value = {
    data_category: policy.data_category,
    retention_days: policy.retention_days,
    grace_period_days: policy.grace_period_days ?? 0,
    action: policy.action,
    legal_basis: policy.legal_basis ?? '',
    is_enabled: policy.is_enabled,
  }
}

function privacyCompletionNeedsEvidence(request: PrivacyRequest) {
  const draft = privacyUpdates[request.id]
  return (
    draft?.status === 'completed' &&
    !draft.resolution.trim() &&
    !(request.resolution ?? '').trim()
  )
}

async function updatePrivacyWorkflow(request: PrivacyRequest) {
  const draft = privacyUpdates[request.id]
  if (!draft || draft.status === request.status) return
  if (privacyCompletionNeedsEvidence(request)) {
    toast.warning('Completion evidence is required', 'Add a fulfillment note before completing this request.')
    return
  }
  saving.value = true
  try {
    await privacyService.updateRequest(request.id, {
      status: draft.status,
      resolution: draft.resolution.trim() || undefined,
    })
    toast.success('Privacy request updated')
    await load()
  } catch (error) {
    toast.error('Privacy request was not updated', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function updateSupportWorkflow(supportCase: SupportCase) {
  const draft = supportUpdates[supportCase.id]
  if (!draft || draft.status === supportCase.status) return
  saving.value = true
  try {
    await supportService.updateCase(supportCase.id, {
      status: draft.status,
      resolution: draft.resolution.trim() || undefined,
    })
    toast.success('Support case updated')
    await load()
  } catch (error) {
    toast.error('Support case was not updated', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function createSupportCase() {
  if (!supportDraft.value.title.trim()) return
  saving.value = true
  try {
    await supportService.createCase({
      title: supportDraft.value.title.trim(),
      description: supportDraft.value.description.trim(),
      severity: supportDraft.value.severity,
    })
    supportDraft.value = { title: '', description: '', severity: 'normal' }
    toast.success('Support case opened')
    await load()
  } catch (error) {
    toast.error('Support case was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

watch(() => route.path, (path) => {
  pane.value = path.includes('/support') ? 'support' : 'privacy'
})
onMounted(load)
</script>

<template>
  <div class="h-full overflow-y-auto bg-[#090b0d] light:bg-[#f5f5f2]">
    <PageHeader
      title="Trust center"
      description="Privacy operations, tenant health and time-bound recovery controls."
      :icon="ShieldCheck"
      icon-gradient="bg-gradient-to-br from-sky-400 to-indigo-700 shadow-sky-500/20"
    >
      <template #actions>
        <Button variant="outline" size="sm" class="gap-2" :disabled="loading" @click="load">
          <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
          Refresh evidence
        </Button>
      </template>
    </PageHeader>

    <main class="mx-auto max-w-[1480px] space-y-6 p-5 md:p-7">
      <section class="relative overflow-hidden rounded-[28px] border border-sky-300/15 bg-[#101923] px-6 py-6 light:border-sky-200 light:bg-[#12314b]">
        <div class="absolute -right-16 -top-24 h-64 w-64 rounded-full bg-sky-400/15 blur-3xl" />
        <div class="relative grid gap-5 lg:grid-cols-[1.2fr_repeat(3,.55fr)] lg:items-end">
          <div>
            <p class="text-[10px] font-semibold uppercase tracking-[0.25em] text-sky-200/70">Operational assurance</p>
            <h2 class="mt-2 text-2xl font-semibold tracking-tight text-white md:text-3xl">Proof, not promises.</h2>
            <p class="mt-2 max-w-lg text-sm leading-6 text-sky-50/50">
              Every request and support intervention is tenant-bound, permissioned and auditable.
            </p>
          </div>
          <div v-for="metric in [
            { label: 'Visible privacy queue', value: openPrivacy },
            { label: 'Visible support cases', value: openCases },
            { label: 'Health attention', value: failingChecks },
          ]" :key="metric.label" class="rounded-2xl border border-white/10 bg-black/15 p-4">
            <p class="text-[10px] uppercase tracking-[0.16em] text-white/40">{{ metric.label }}</p>
            <p class="mt-2 text-2xl font-semibold text-white">{{ metric.value }}</p>
          </div>
        </div>
      </section>

      <div class="flex w-fit rounded-xl border border-white/[0.07] bg-black/20 p-1 light:border-black/10 light:bg-gray-100">
        <button
          v-for="item in availablePanes"
          :key="item.key"
          class="flex items-center gap-2 rounded-lg px-4 py-2 text-sm font-medium transition"
          :class="pane === item.key ? 'bg-white text-gray-950 shadow-sm' : 'text-white/45 hover:text-white light:text-gray-500'"
          @click="setPane(item.key as TrustPane)"
        >
          <component :is="item.icon" class="h-4 w-4" />
          {{ item.label }}
        </button>
      </div>

      <div v-if="loading" class="flex h-80 items-center justify-center">
        <Loader2 class="h-6 w-6 animate-spin text-sky-300" />
      </div>

      <div
        v-else-if="pane === 'privacy'"
        class="grid gap-6"
        :class="canWritePrivacyRequests ? 'xl:grid-cols-[minmax(0,1fr)_390px]' : 'xl:grid-cols-1'"
      >
        <div class="space-y-6">
          <section v-if="canReadPrivacyRequests" class="overflow-hidden rounded-2xl border border-white/[0.08] bg-white/[0.025] light:border-black/10 light:bg-white">
            <div class="flex items-center justify-between border-b border-white/[0.07] px-5 py-4 light:border-black/10">
              <div>
                <p class="font-semibold text-white light:text-gray-900">Data-subject requests</p>
                <p class="mt-0.5 text-xs text-white/40 light:text-gray-500">Identity must be verified before fulfillment.</p>
              </div>
              <FileKey2 class="h-5 w-5 text-sky-300" />
            </div>
            <div class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
              <article v-for="request in requests" :key="request.id" class="px-5 py-4">
                <div class="flex items-center gap-4">
                <div class="rounded-xl bg-sky-400/10 p-2.5 text-sky-300"><LockKeyhole class="h-4 w-4" /></div>
                <div class="min-w-0 flex-1">
                  <p class="text-sm font-medium capitalize text-white light:text-gray-900">{{ request.request_type }} request</p>
                  <p class="mt-0.5 text-xs text-white/35 light:text-gray-500">
                    {{ request.request_number || 'Privacy request' }} · Opened {{ formatDate(request.created_at) }} · Due {{ formatDate(request.due_at) }}
                  </p>
                </div>
                <Badge variant="outline" class="capitalize">{{ request.status }}</Badge>
                </div>
                <div
                  v-if="canWritePrivacyRequests && privacyNextStatuses(request.status).length"
                  class="mt-3 grid gap-2 pl-12 md:grid-cols-[180px_1fr_auto]"
                >
                  <select
                    v-model="privacyUpdates[request.id].status"
                    class="h-9 rounded-md border border-white/10 bg-[#151d25] px-2 text-xs text-white light:border-gray-200 light:bg-white light:text-gray-900"
                  >
                    <option v-for="status in privacyNextStatuses(request.status)" :key="status" :value="status">
                      {{ status.replace(/_/g, ' ') }}
                    </option>
                  </select>
                  <input
                    v-model="privacyUpdates[request.id].resolution"
                    maxlength="4000"
                    placeholder="Fulfillment evidence or decision note"
                    class="h-9 rounded-md border border-white/10 bg-black/20 px-3 text-xs text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900"
                  />
                  <Button
                    size="sm"
                    variant="outline"
                    :disabled="saving || privacyCompletionNeedsEvidence(request)"
                    @click="updatePrivacyWorkflow(request)"
                  >
                    Update
                  </Button>
                  <p
                    v-if="privacyCompletionNeedsEvidence(request)"
                    class="text-[10px] text-amber-200 md:col-start-2"
                  >
                    Fulfillment evidence is required before completion.
                  </p>
                </div>
              </article>
              <p v-if="!requests.length" class="py-20 text-center text-sm text-white/35 light:text-gray-500">No privacy requests recorded.</p>
              <div v-if="requests.length < privacyTotal" class="p-4 text-center">
                <Button variant="outline" :disabled="loadingMore" @click="loadMoreTrust('privacy')">
                  <Loader2 v-if="loadingMore" class="mr-2 h-4 w-4 animate-spin" />
                  Load older privacy requests
                </Button>
              </div>
            </div>
          </section>

          <section v-if="canReadPrivacySettings" class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white">
            <div class="mb-4 flex items-center gap-3">
              <DatabaseBackup class="h-5 w-5 text-violet-300" />
              <div>
                <p class="font-semibold text-white light:text-gray-900">Retention policy</p>
                <p class="text-xs text-white/40 light:text-gray-500">Documented schedules for operator review and execution.</p>
              </div>
            </div>
            <div class="mb-4 rounded-xl border border-amber-300/15 bg-amber-400/[0.06] p-3 text-xs leading-5 text-amber-200 light:border-amber-200 light:bg-amber-50 light:text-amber-800">
              Automated deletion and anonymization are not enabled yet. Execute each due policy through an approved operator process and retain the evidence.
            </div>
            <div class="grid gap-3 md:grid-cols-2">
              <div v-for="policy in policies" :key="policy.id" class="rounded-xl border border-white/[0.07] bg-black/10 p-4 light:border-black/10 light:bg-gray-50">
                <div class="flex items-start justify-between gap-3">
                  <p class="text-sm font-medium capitalize text-white light:text-gray-900">{{ policy.data_category.split('_').join(' ') }}</p>
                  <div class="flex items-center gap-2">
                    <Badge variant="outline">{{ policy.retention_days }} days</Badge>
                    <Button v-if="canWritePrivacySettings" size="sm" variant="ghost" @click="editRetentionPolicy(policy)">Edit</Button>
                  </div>
                </div>
                <p class="mt-2 text-xs capitalize text-white/40 light:text-gray-500">{{ policy.action }} · {{ policy.legal_basis || 'Basis to be documented' }}</p>
              </div>
              <p v-if="!policies.length" class="col-span-full rounded-xl bg-amber-400/[0.06] p-4 text-xs text-amber-300">
                No retention policy is configured. Add policies before onboarding regulated customers.
              </p>
            </div>
            <form
              v-if="canWritePrivacySettings"
              class="mt-5 grid gap-3 rounded-xl border border-violet-300/15 bg-violet-300/[0.035] p-4 md:grid-cols-2 xl:grid-cols-3"
              @submit.prevent="saveRetentionPolicy"
            >
              <input
                v-model="retentionDraft.data_category"
                required
                maxlength="100"
                :disabled="Boolean(editingRetentionCategory)"
                placeholder="Data category, e.g. contacts"
                class="h-10 rounded-md border border-white/10 bg-black/20 px-3 text-sm text-white outline-none disabled:cursor-not-allowed disabled:opacity-55 light:border-gray-200 light:bg-white light:text-gray-900"
              />
              <input
                v-model.number="retentionDraft.retention_days"
                required
                type="number"
                min="1"
                max="36500"
                placeholder="Retention days"
                class="h-10 rounded-md border border-white/10 bg-black/20 px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900"
              />
              <input
                v-model.number="retentionDraft.grace_period_days"
                required
                type="number"
                min="0"
                max="3650"
                placeholder="Grace days"
                class="h-10 rounded-md border border-white/10 bg-black/20 px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900"
              />
              <select
                v-model="retentionDraft.action"
                class="h-10 rounded-md border border-white/10 bg-[#151d25] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
              >
                <option value="review">Operator review</option>
                <option value="archive">Archive</option>
                <option value="anonymize">Anonymize</option>
                <option value="delete">Delete</option>
              </select>
              <input
                v-model="retentionDraft.legal_basis"
                maxlength="2000"
                placeholder="Legal basis / operational rationale"
                class="h-10 rounded-md border border-white/10 bg-black/20 px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900 xl:col-span-2"
              />
              <label class="flex items-center gap-2 text-xs text-white/60 light:text-gray-700">
                <input v-model="retentionDraft.is_enabled" type="checkbox" class="accent-violet-400" />
                Active policy
              </label>
              <Button type="submit" class="bg-violet-500 text-white hover:bg-violet-400 md:col-span-1 xl:col-span-2" :disabled="saving">
                {{ editingRetentionCategory ? 'Update retention rule' : 'Save retention rule' }}
              </Button>
              <p v-if="editingRetentionCategory" class="text-[10px] leading-4 text-violet-100/60 md:col-span-2 xl:col-span-3">
                Category is the immutable policy key. Create a separate rule instead of renaming an existing category.
              </p>
            </form>
          </section>
        </div>

        <aside v-if="canWritePrivacyRequests" class="h-fit rounded-[24px] border border-sky-300/15 bg-sky-300/[0.045] p-5 light:border-sky-200 light:bg-sky-50">
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-sky-300 light:text-sky-700">Intake</p>
          <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-950">Open a privacy request</h3>
          <p class="mt-2 text-xs leading-5 text-white/40 light:text-gray-600">Use the CRM contact ID when the request belongs to an existing customer.</p>
          <form class="mt-6 space-y-4" @submit.prevent="createPrivacyRequest">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Request type</span>
              <select v-model="privacyDraft.request_type" class="h-11 w-full rounded-xl border border-white/10 bg-[#151d25] px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="access">Access</option>
                <option value="portability">Portability</option>
                <option value="correction">Correction</option>
                <option value="restriction">Restriction</option>
                <option value="erasure">Erasure</option>
              </select>
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Contact ID (optional)</span>
              <input v-model="privacyDraft.contact_id" placeholder="UUID" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none placeholder:text-white/25 focus:border-sky-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <Button type="submit" class="h-11 w-full gap-2 bg-sky-500 text-white hover:bg-sky-400" :disabled="saving">
              <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
              <Plus v-else class="h-4 w-4" />
              Open request
            </Button>
          </form>
        </aside>
      </div>

      <div
        v-else
        class="grid gap-6"
        :class="canWriteSupport ? 'xl:grid-cols-[minmax(0,1fr)_390px]' : 'xl:grid-cols-1'"
      >
        <div class="space-y-6">
          <section class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white">
            <div class="mb-4 flex items-center justify-between">
              <div>
                <p class="font-semibold text-white light:text-gray-900">Tenant health</p>
                <p class="mt-0.5 text-xs text-white/40 light:text-gray-500">Read-only operational checks; secrets are never returned.</p>
              </div>
              <Badge variant="outline" class="capitalize" :class="health?.status === 'healthy' ? 'border-emerald-400/30 text-emerald-300' : 'border-amber-400/30 text-amber-300'">
                <HeartPulse class="mr-1.5 h-3.5 w-3.5" />
                {{ health?.status || 'unknown' }}
              </Badge>
            </div>
            <div class="grid gap-3 md:grid-cols-2">
              <div v-for="check in health?.checks ?? []" :key="check.key" class="flex gap-3 rounded-xl border border-white/[0.07] bg-black/10 p-4 light:border-black/10 light:bg-gray-50">
                <CheckCircle2 v-if="check.status === 'pass'" class="mt-0.5 h-4 w-4 shrink-0 text-emerald-300" />
                <AlertTriangle v-else class="mt-0.5 h-4 w-4 shrink-0 text-amber-300" />
                <div>
                  <p class="text-sm font-medium text-white light:text-gray-900">{{ check.label }}</p>
                  <p class="mt-1 text-xs leading-5 text-white/40 light:text-gray-500">{{ check.detail }}</p>
                </div>
              </div>
            </div>
          </section>

          <section class="overflow-hidden rounded-2xl border border-white/[0.08] bg-white/[0.025] light:border-black/10 light:bg-white">
            <div class="flex items-center justify-between border-b border-white/[0.07] px-5 py-4 light:border-black/10">
              <div>
                <p class="font-semibold text-white light:text-gray-900">Support cases</p>
                <p class="mt-0.5 text-xs text-white/40 light:text-gray-500">Support access is separate and must be explicitly granted.</p>
              </div>
              <LifeBuoy class="h-5 w-5 text-violet-300" />
            </div>
            <div class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
              <article v-for="supportCase in cases" :key="supportCase.id" class="px-5 py-4">
                <div class="flex gap-4">
                  <div class="min-w-0 flex-1">
                    <div class="flex flex-wrap items-center gap-2">
                      <p class="text-sm font-medium text-white light:text-gray-900">{{ supportCase.title }}</p>
                      <Badge variant="outline" class="capitalize">{{ supportCase.severity }}</Badge>
                    </div>
                    <p class="mt-1 line-clamp-2 text-xs leading-5 text-white/40 light:text-gray-500">{{ supportCase.description }}</p>
                    <p class="mt-1 text-[10px] text-white/30 light:text-gray-400">{{ supportCase.case_number || supportCase.id }}</p>
                  </div>
                  <span class="text-xs capitalize text-white/40 light:text-gray-500">{{ supportCase.status.replace(/_/g, ' ') }}</span>
                </div>
                <div
                  v-if="canWriteSupport && supportNextStatuses(supportCase.status).length"
                  class="mt-3 grid gap-2 md:grid-cols-[180px_1fr_auto]"
                >
                  <select
                    v-model="supportUpdates[supportCase.id].status"
                    class="h-9 rounded-md border border-white/10 bg-[#191721] px-2 text-xs text-white light:border-gray-200 light:bg-white light:text-gray-900"
                  >
                    <option v-for="status in supportNextStatuses(supportCase.status)" :key="status" :value="status">
                      {{ status.replace(/_/g, ' ') }}
                    </option>
                  </select>
                  <input
                    v-model="supportUpdates[supportCase.id].resolution"
                    maxlength="4000"
                    placeholder="Resolution / operator note"
                    class="h-9 rounded-md border border-white/10 bg-black/20 px-3 text-xs text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900"
                  />
                  <Button size="sm" variant="outline" :disabled="saving" @click="updateSupportWorkflow(supportCase)">
                    Update
                  </Button>
                </div>
              </article>
              <p v-if="!cases.length" class="py-20 text-center text-sm text-white/35 light:text-gray-500">No support cases recorded.</p>
              <div v-if="cases.length < supportTotal" class="p-4 text-center">
                <Button variant="outline" :disabled="loadingMore" @click="loadMoreTrust('support')">
                  <Loader2 v-if="loadingMore" class="mr-2 h-4 w-4 animate-spin" />
                  Load older support cases
                </Button>
              </div>
            </div>
          </section>

          <section class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-black/10 light:bg-white">
            <div class="flex items-center gap-3">
              <div class="rounded-xl bg-emerald-400/10 p-2.5 text-emerald-300"><DatabaseBackup class="h-5 w-5" /></div>
              <div class="flex-1">
                <p class="font-semibold text-white light:text-gray-900">Recovery readiness</p>
                <p class="mt-0.5 text-xs text-white/40 light:text-gray-500">Last verified {{ formatDate(recovery?.last_verified_at) }}</p>
              </div>
              <Badge variant="outline" :class="recovery?.recovery_ready ? 'border-emerald-400/30 text-emerald-300' : 'border-amber-400/30 text-amber-300'">
                {{ recovery?.recovery_ready ? 'Ready' : 'Review' }}
              </Badge>
            </div>
            <div v-if="recovery?.checkpoints?.length" class="mt-4 grid gap-3 md:grid-cols-2">
              <div v-for="checkpoint in recovery.checkpoints" :key="checkpoint.id" class="flex items-center gap-3 rounded-xl bg-black/10 p-3 light:bg-gray-50">
                <Clock3 class="h-4 w-4 text-sky-300" />
                <div>
                  <p class="text-xs font-medium capitalize text-white light:text-gray-900">{{ checkpoint.kind || 'Recovery checkpoint' }}</p>
                  <p class="mt-0.5 text-[11px] capitalize text-white/35 light:text-gray-500">{{ checkpoint.status }} · {{ formatDate(checkpoint.created_at) }}</p>
                </div>
              </div>
            </div>
          </section>
        </div>

        <aside v-if="canWriteSupport" class="h-fit rounded-[24px] border border-violet-300/15 bg-violet-300/[0.045] p-5 light:border-violet-200 light:bg-violet-50">
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-violet-300 light:text-violet-700">Escalation</p>
          <h3 class="mt-2 text-xl font-semibold text-white light:text-gray-950">Open a support case</h3>
          <p class="mt-2 text-xs leading-5 text-white/40 light:text-gray-600">A case does not grant data access. Any support grant remains separate and time-limited.</p>
          <form class="mt-6 space-y-4" @submit.prevent="createSupportCase">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Issue</span>
              <input v-model="supportDraft.title" required maxlength="255" placeholder="Inbound delivery delayed" class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none placeholder:text-white/25 focus:border-violet-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Evidence and impact</span>
              <textarea v-model="supportDraft.description" required rows="4" maxlength="4000" placeholder="What happened, when, and who is affected…" class="w-full resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none placeholder:text-white/25 focus:border-violet-300/50 light:border-gray-200 light:bg-white light:text-gray-900" />
            </label>
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Severity</span>
              <select v-model="supportDraft.severity" class="h-11 w-full rounded-xl border border-white/10 bg-[#191721] px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="low">Low</option>
                <option value="normal">Normal</option>
                <option value="high">High</option>
                <option value="critical">Critical</option>
              </select>
            </label>
            <Button type="submit" class="h-11 w-full gap-2 bg-violet-500 text-white hover:bg-violet-400" :disabled="saving || !supportDraft.title.trim()">
              <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
              <Plus v-else class="h-4 w-4" />
              Open support case
            </Button>
          </form>
        </aside>
      </div>
    </main>
  </div>
</template>
