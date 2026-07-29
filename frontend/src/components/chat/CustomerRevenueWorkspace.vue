<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import {
  AlertCircle,
  ArrowUpRight,
  CalendarClock,
  CheckCircle2,
  CircleDollarSign,
  Clipboard,
  Clock3,
  CreditCard,
  FileSearch,
  History,
  ListChecks,
  Loader2,
  LockKeyhole,
  Package,
  Plus,
  RefreshCw,
  Route,
  Sparkles,
  Wand2,
  X,
} from 'lucide-vue-next'
import { Avatar, AvatarFallback, AvatarImage } from '@/components/ui/avatar'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import type { Contact } from '@/stores/contacts'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import { getAvatarGradient, getInitials } from '@/lib/utils'
import {
  copilotService,
  crmService,
  customerWorkspaceService,
  type Booking,
  type ContactPackage,
  type CopilotRun,
  type CustomerIdentity,
  type CustomerTimelineEvent,
  type CustomerWorkspace,
  type CustomerWorkspaceCapabilities,
  type FollowUpTask,
  type Pipeline,
} from '@/services/productSuite'
import ContactInfoPanel from '@/components/chat/ContactInfoPanel.vue'

type WorkspaceTab = 'overview' | 'timeline' | 'details' | 'copilot'
type CopilotAction = Extract<CopilotRun['task_type'], 'summary' | 'qualify' | 'extract_actions'>
type ContactInfoSessionData = InstanceType<typeof ContactInfoPanel>['$props']['sessionData']

const props = withDefaults(defineProps<{
  contactId: string
  contact?: Partial<Contact> | null
  sessionData?: ContactInfoSessionData
  surface?: 'chat' | 'omnichannel'
}>(), {
  contact: null,
  sessionData: null,
  surface: 'chat',
})

const emit = defineEmits<{
  close: []
  tagsUpdated: [tags: string[]]
}>()

const toast = useAppToast()
const authStore = useAuthStore()
const workspace = ref<CustomerWorkspace | null>(null)
const loading = ref(true)
const refreshing = ref(false)
const error = ref('')
const activeTab = ref<WorkspaceTab>('overview')
let loadSequence = 0

const showJourneyDialog = ref(false)
const showTaskDialog = ref(false)
const savingJourney = ref(false)
const savingTask = ref(false)
const pipelines = ref<Pipeline[]>([])
const pipelinesLoading = ref(false)
const journeyDraft = ref({
  title: '',
  pipeline_id: '',
  value: '',
  currency: 'MYR',
  source: 'whatsapp',
})
const taskDraft = ref({
  title: '',
  description: '',
  priority: 'normal' as FollowUpTask['priority'],
  due_at: '',
  lead_id: '',
})

const copilotRunning = ref<CopilotAction | null>(null)
const copilotRun = ref<CopilotRun | null>(null)
const copilotResult = ref('')

const contactRecord = computed(() => workspace.value?.contact ?? props.contact)
const contactName = computed(() =>
  contactRecord.value?.profile_name ||
  contactRecord.value?.name ||
  contactRecord.value?.phone_number ||
  'Customer',
)
const contactPhone = computed(() => contactRecord.value?.phone_number || '')
const identities = computed(() => workspace.value?.identities ?? [])
const journeys = computed(() => workspace.value?.journeys ?? [])
const tasks = computed(() => workspace.value?.tasks ?? [])
const bookings = computed(() => workspace.value?.bookings ?? [])
const packages = computed(() => workspace.value?.packages ?? [])
const invoices = computed(() => workspace.value?.invoices ?? [])
const timeline = computed(() =>
  [...(workspace.value?.timeline ?? [])].sort(
    (left, right) => new Date(right.occurred_at).getTime() - new Date(left.occurred_at).getTime(),
  ),
)
const openJourneys = computed(() => journeys.value.filter((journey) => journey.status === 'open'))
const openTasks = computed(() => tasks.value.filter((task) => !['completed', 'cancelled'].includes(task.status)))
const selectedPipeline = computed(() =>
  pipelines.value.find((pipeline) => pipeline.id === journeyDraft.value.pipeline_id),
)
const firstOpenStage = computed(() =>
  selectedPipeline.value?.stages
    .filter((stage) => stage.kind === 'open')
    .sort((left, right) => left.display_order - right.display_order)[0],
)

const upcomingBookings = computed(() => {
  const now = Date.now()
  return bookings.value
    .filter((booking) => booking.event?.starts_at && new Date(booking.event.starts_at).getTime() >= now)
    .filter((booking) => !['cancelled', 'no_show'].includes(booking.status))
    .sort((left, right) =>
      new Date(left.event!.starts_at).getTime() - new Date(right.event!.starts_at).getTime(),
    )
    .slice(0, 3)
})

const recentBookings = computed(() => {
  const now = Date.now()
  return bookings.value
    .filter((booking) => !booking.event?.starts_at || new Date(booking.event.starts_at).getTime() < now)
    .sort((left, right) =>
      new Date(right.event?.starts_at ?? right.created_at).getTime() -
      new Date(left.event?.starts_at ?? left.created_at).getTime(),
    )
    .slice(0, 2)
})

const activePackages = computed(() =>
  packages.value.filter((item) => ['active', 'pending'].includes(item.status)).slice(0, 3),
)
const visibleInvoices = computed(() =>
  [...invoices.value]
    .sort((left, right) => Number(right.due_minor > 0) - Number(left.due_minor > 0))
    .slice(0, 4),
)

function rawCapability(key: keyof CustomerWorkspaceCapabilities): boolean | undefined {
  const direct = workspace.value?.capabilities?.[key]
  if (typeof direct === 'boolean') return direct
  const permissions = workspace.value?.permissions
  if (!permissions) return undefined
  if (key === 'packages') return permissions.packages ?? permissions.commerce
  if (key === 'payments') return permissions.payments ?? permissions.commerce
  if (key === 'tasks') return permissions.tasks ?? permissions.crm
  return permissions[key as keyof typeof permissions]
}

function canView(key: keyof CustomerWorkspaceCapabilities, permission: string) {
  const serverCapability = rawCapability(key)
  return serverCapability ?? authStore.hasPermission(permission, 'read')
}

const canViewCRM = computed(() => canView('crm', 'crm.leads'))
const canViewTasks = computed(() => canView('tasks', 'tasks'))
const canViewBookings = computed(() => canView('bookings', 'bookings'))
const canViewPackages = computed(() => canView('packages', 'packages'))
const canViewPayments = computed(() => canView('payments', 'payments'))
const canUseCopilot = computed(() =>
  canView('copilot', 'copilot') &&
  authStore.hasPermission('copilot', 'execute'),
)
const canCreateJourney = computed(() =>
  canViewCRM.value &&
  authStore.hasPermission('crm.leads', 'write') &&
  authStore.hasPermission('crm.pipelines', 'read'),
)
const canCreateTask = computed(() =>
  canViewTasks.value && authStore.hasPermission('tasks', 'write'),
)

const detailContact = computed<Contact>(() => ({
  id: props.contactId,
  phone_number: contactPhone.value,
  name: contactName.value,
  profile_name: contactRecord.value?.profile_name,
  avatar_url: contactRecord.value?.avatar_url,
  status: contactRecord.value?.status || 'active',
  tags: contactRecord.value?.tags ?? [],
  metadata: (contactRecord.value?.metadata ?? {}) as Record<string, unknown>,
  unread_count: props.contact?.unread_count ?? 0,
  assigned_user_id: contactRecord.value?.assigned_user_id,
  marketing_opt_out: contactRecord.value?.marketing_opt_out,
  created_at: contactRecord.value?.created_at || '',
  updated_at: contactRecord.value?.updated_at || '',
}))

const summary = computed(() => {
  const source = workspace.value?.summary
  const fallbackOutstanding = invoices.value
    .filter((invoice) => invoice.status === 'open')
    .reduce((sum, invoice) => sum + Math.max(0, invoice.due_minor), 0)
  const currency = source?.currency || invoices.value[0]?.currency || journeys.value[0]?.currency || 'MYR'
  const pipelineValue = source?.pipeline_value?.length
    ? source.pipeline_value
    : openJourneys.value.reduce<Array<{ currency: string; amount_minor: number }>>((totals, journey) => {
        const found = totals.find((item) => item.currency === journey.currency)
        if (found) found.amount_minor += journey.value_minor
        else totals.push({ currency: journey.currency || currency, amount_minor: journey.value_minor })
        return totals
      }, [])
  const outstanding = source
    ? source.outstanding ??
      (typeof source.outstanding_minor === 'number'
        ? [{ currency, amount_minor: source.outstanding_minor }]
        : [])
    : [{ currency, amount_minor: fallbackOutstanding }]
  const credits = source?.available_credits ?? activePackages.value.reduce(
    (total, item) => total + (item.balances ?? []).reduce((sum, balance) => sum + balance.available, 0),
    0,
  )
  return {
    openJourneys: source?.open_journeys ?? source?.journey_count ?? openJourneys.value.length,
    openTasks: source?.open_task_count ?? openTasks.value.length,
    pipelineValue,
    outstanding,
    credits,
  }
})

async function loadWorkspace(silent = false) {
  const sequence = ++loadSequence
  if (silent) refreshing.value = true
  else loading.value = true
  error.value = ''
  try {
    const response = await customerWorkspaceService.get(props.contactId)
    if (sequence !== loadSequence) return
    const result = unwrapItemResponse<CustomerWorkspace>(response)
    workspace.value = {
      ...result,
      identities: result.identities ?? [],
      journeys: result.journeys ?? [],
      tasks: result.tasks ?? [],
      bookings: result.bookings ?? [],
      packages: result.packages ?? [],
      invoices: result.invoices ?? [],
      payments: result.payments ?? [],
      timeline: result.timeline ?? [],
    }
  } catch (cause) {
    if (sequence !== loadSequence) return
    error.value = getErrorMessage(cause)
  } finally {
    if (sequence === loadSequence) {
      loading.value = false
      refreshing.value = false
    }
  }
}

async function ensurePipelines() {
  if (pipelines.value.length || pipelinesLoading.value) return
  pipelinesLoading.value = true
  try {
    const response = await crmService.pipelines()
    pipelines.value = unwrapListResponse<Pipeline>(response, 'pipelines')
    journeyDraft.value.pipeline_id =
      pipelines.value.find((item) => item.is_default)?.id ?? pipelines.value[0]?.id ?? ''
  } catch (cause) {
    toast.error('Pipelines could not be loaded', getErrorMessage(cause))
  } finally {
    pipelinesLoading.value = false
  }
}

async function openJourney() {
  showJourneyDialog.value = true
  journeyDraft.value.title = `${contactName.value} enquiry`
  journeyDraft.value.source = props.surface === 'chat' ? 'whatsapp' : 'other'
  await ensurePipelines()
}

async function createJourney() {
  if (!journeyDraft.value.title.trim() || !firstOpenStage.value) return
  savingJourney.value = true
  try {
    await crmService.createLead({
      contact_id: props.contactId,
      pipeline_id: journeyDraft.value.pipeline_id,
      stage_id: firstOpenStage.value.id,
      title: journeyDraft.value.title.trim(),
      status: 'open',
      source: journeyDraft.value.source,
      value_minor: Math.max(0, Math.round(Number(journeyDraft.value.value || 0) * 100)),
      currency: journeyDraft.value.currency,
      idempotency_key: crypto.randomUUID(),
    })
    showJourneyDialog.value = false
    journeyDraft.value.value = ''
    toast.success('Journey created', 'It is now visible in the revenue pipeline.')
    await loadWorkspace(true)
  } catch (cause) {
    toast.error('Journey was not created', getErrorMessage(cause))
  } finally {
    savingJourney.value = false
  }
}

function openFollowUp() {
  taskDraft.value.title = `Follow up with ${contactName.value}`
  taskDraft.value.lead_id = openJourneys.value[0]?.id ?? ''
  showTaskDialog.value = true
}

async function createFollowUp() {
  if (!taskDraft.value.title.trim()) return
  savingTask.value = true
  try {
    await crmService.createTask({
      contact_id: props.contactId,
      lead_id: taskDraft.value.lead_id || undefined,
      title: taskDraft.value.title.trim(),
      description: taskDraft.value.description.trim(),
      priority: taskDraft.value.priority,
      due_at: taskDraft.value.due_at
        ? new Date(taskDraft.value.due_at).toISOString()
        : undefined,
      source: `${props.surface}_workspace`,
      idempotency_key: crypto.randomUUID(),
    })
    showTaskDialog.value = false
    taskDraft.value.description = ''
    taskDraft.value.due_at = ''
    toast.success('Follow-up scheduled')
    await loadWorkspace(true)
  } catch (cause) {
    toast.error('Follow-up was not created', getErrorMessage(cause))
  } finally {
    savingTask.value = false
  }
}

function copilotLabel(action: CopilotAction) {
  return {
    summary: 'Summary',
    qualify: 'Qualify',
    extract_actions: 'Next actions',
  }[action]
}

async function runCopilot(action: CopilotAction) {
  if (!canUseCopilot.value || copilotRunning.value) return
  copilotRunning.value = action
  copilotRun.value = null
  copilotResult.value = ''
  try {
    const response = await copilotService.run(props.contactId, action, {
      message_limit: 30,
      idempotency_key: crypto.randomUUID(),
    })
    copilotRun.value = unwrapItemResponse<CopilotRun>(response)
    copilotResult.value =
      copilotRun.value.result_text ||
      (copilotRun.value.structured_result
        ? JSON.stringify(copilotRun.value.structured_result, null, 2)
        : 'No suggestion was returned.')
  } catch (cause) {
    toast.error('Copilot could not complete this review', getErrorMessage(cause))
  } finally {
    copilotRunning.value = null
  }
}

async function copyCopilotResult() {
  if (!copilotResult.value) return
  try {
    await navigator.clipboard.writeText(copilotResult.value)
    toast.success('Copilot result copied')
  } catch {
    toast.error('Copilot result could not be copied')
  }
}

function money(amountMinor: number, currency = 'MYR') {
  return new Intl.NumberFormat('en-MY', {
    style: 'currency',
    currency,
    maximumFractionDigits: 2,
  }).format(amountMinor / 100)
}

function moneyList(values: Array<{ currency: string; amount_minor: number }>) {
  const visible = values.filter((item) => item.amount_minor !== 0)
  if (!visible.length) return money(0)
  return visible.map((item) => money(item.amount_minor, item.currency)).join(' · ')
}

function shortDate(value?: string) {
  if (!value) return 'Not scheduled'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    year: new Date(value).getFullYear() === new Date().getFullYear() ? undefined : 'numeric',
  }).format(new Date(value))
}

function dateTime(value?: string) {
  if (!value) return 'Not scheduled'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(value))
}

function packageCredits(item: ContactPackage) {
  const balances = item.balances ?? []
  if (!balances.length) return 'Credits unavailable'
  return `${balances.reduce((sum, balance) => sum + balance.available, 0)} credits available`
}

function bookingName(booking: Booking) {
  return booking.event?.service?.name || 'Appointment'
}

function timelineIcon(event: CustomerTimelineEvent) {
  if (event.category === 'booking') return CalendarClock
  if (['payment', 'invoice', 'commerce'].includes(event.category)) return CircleDollarSign
  if (event.category === 'package') return Package
  if (['journey', 'crm'].includes(event.category)) return Route
  if (event.category === 'task') return ListChecks
  return History
}

function identityLabel(identity: CustomerIdentity) {
  return identity.display_name || identity.address || identity.normalized_address || identity.channel
}

watch(
  () => props.contactId,
  () => {
    activeTab.value = 'overview'
    copilotRun.value = null
    copilotResult.value = ''
    void loadWorkspace()
  },
)

onMounted(() => void loadWorkspace())
</script>

<template>
  <section
    class="flex h-full min-h-0 w-full flex-col border-l border-white/[0.08] bg-[#0b0d0e] text-white light:border-gray-200 light:bg-white light:text-gray-900"
    aria-label="Customer revenue workspace"
    data-testid="customer-revenue-workspace"
  >
    <header class="shrink-0 border-b border-white/[0.08] px-4 py-3 light:border-gray-200">
      <div class="flex items-start gap-3">
        <Avatar class="mt-0.5 h-10 w-10 shrink-0 ring-1 ring-white/10 light:ring-gray-200">
          <AvatarImage :src="contactRecord?.avatar_url" />
          <AvatarFallback :class="'text-xs bg-gradient-to-br text-white ' + getAvatarGradient(contactName)">
            {{ getInitials(contactName) }}
          </AvatarFallback>
        </Avatar>
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <h2 class="truncate text-sm font-semibold">{{ contactName }}</h2>
            <Badge
              v-if="contactRecord?.marketing_opt_out"
              variant="outline"
              class="border-rose-400/25 px-1.5 text-[9px] text-rose-300 light:text-rose-700"
            >
              Opted out
            </Badge>
          </div>
          <p class="mt-0.5 truncate text-[11px] text-white/40 light:text-gray-500">{{ contactPhone }}</p>
          <div v-if="identities.length" class="mt-2 flex flex-wrap gap-1">
            <Badge
              v-for="identity in identities.slice(0, 4)"
              :key="identity.id"
              variant="secondary"
              class="max-w-full gap-1 px-1.5 text-[9px] font-normal capitalize"
              :title="identityLabel(identity)"
            >
              <CheckCircle2 v-if="identity.verified ?? identity.is_verified" class="h-2.5 w-2.5 text-emerald-400" />
              {{ identity.channel }}
              <span class="max-w-28 truncate opacity-65">{{ identityLabel(identity) }}</span>
            </Badge>
          </div>
        </div>
        <Button
          variant="ghost"
          size="icon"
          class="h-11 w-11 shrink-0 text-white/45 hover:text-white light:text-gray-500 light:hover:text-gray-900"
          aria-label="Close customer revenue workspace"
          @click="emit('close')"
        >
          <X class="h-4 w-4" />
        </Button>
      </div>
    </header>

    <div v-if="loading" class="flex flex-1 flex-col items-center justify-center px-6 text-center" aria-live="polite">
      <Loader2 class="h-6 w-6 animate-spin text-cyan-300" />
      <p class="mt-3 text-sm font-medium">Loading customer workspace</p>
      <p class="mt-1 text-xs text-white/35 light:text-gray-500">Connecting journeys, care and revenue.</p>
    </div>

    <div v-else-if="error" class="flex flex-1 flex-col items-center justify-center px-6 text-center" role="alert">
      <AlertCircle class="h-7 w-7 text-rose-300" />
      <h3 class="mt-3 text-sm font-semibold">Customer workspace is unavailable</h3>
      <p class="mt-1 max-w-xs text-xs leading-5 text-white/40 light:text-gray-500">{{ error }}</p>
      <Button variant="outline" class="mt-4 h-11" @click="loadWorkspace()">
        <RefreshCw class="mr-2 h-4 w-4" />
        Try again
      </Button>
    </div>

    <Tabs v-else v-model="activeTab" class="flex min-h-0 flex-1 flex-col">
      <div class="shrink-0 border-b border-white/[0.07] px-3 py-2 light:border-gray-100">
        <TabsList class="grid h-9 w-full grid-cols-4 bg-white/[0.035] light:bg-gray-100">
          <TabsTrigger value="overview" class="text-[10px]">Overview</TabsTrigger>
          <TabsTrigger value="timeline" class="text-[10px]">Timeline</TabsTrigger>
          <TabsTrigger value="details" class="text-[10px]">Details</TabsTrigger>
          <TabsTrigger value="copilot" class="text-[10px]">Copilot</TabsTrigger>
        </TabsList>
      </div>

      <TabsContent value="overview" class="mt-0 min-h-0 flex-1">
        <ScrollArea class="h-full">
          <div class="space-y-4 p-3.5">
            <div class="grid grid-cols-2 gap-2">
              <div class="rounded-xl border border-cyan-300/10 bg-cyan-300/[0.035] p-3">
                <p class="text-[9px] font-semibold uppercase tracking-[0.16em] text-cyan-200/65 light:text-cyan-800">Open journeys</p>
                <p class="mt-1 text-lg font-semibold">{{ summary.openJourneys }}</p>
              </div>
              <div class="rounded-xl border border-emerald-300/10 bg-emerald-300/[0.035] p-3">
                <p class="text-[9px] font-semibold uppercase tracking-[0.16em] text-emerald-200/65 light:text-emerald-800">Pipeline value</p>
                <p class="mt-1 truncate text-sm font-semibold" :title="moneyList(summary.pipelineValue)">
                  {{ moneyList(summary.pipelineValue) }}
                </p>
              </div>
              <div class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50">
                <p class="text-[9px] font-semibold uppercase tracking-[0.16em] text-white/35 light:text-gray-500">Open tasks</p>
                <p class="mt-1 text-lg font-semibold">{{ summary.openTasks }}</p>
              </div>
              <div class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50">
                <p class="text-[9px] font-semibold uppercase tracking-[0.16em] text-white/35 light:text-gray-500">Outstanding</p>
                <p class="mt-1 truncate text-sm font-semibold text-amber-200 light:text-amber-800" :title="moneyList(summary.outstanding)">
                  {{ moneyList(summary.outstanding) }}
                </p>
              </div>
            </div>

            <div class="grid grid-cols-2 gap-2">
              <Button
                v-if="canCreateJourney"
                variant="outline"
                class="h-11 justify-start border-cyan-300/20 text-xs"
                @click="openJourney"
              >
                <Plus class="mr-2 h-3.5 w-3.5 text-cyan-300" />
                New journey
              </Button>
              <Button
                v-if="canCreateTask"
                variant="outline"
                class="h-11 justify-start border-violet-300/20 text-xs"
                @click="openFollowUp"
              >
                <Clock3 class="mr-2 h-3.5 w-3.5 text-violet-300" />
                Follow-up
              </Button>
            </div>

            <section aria-labelledby="workspace-journeys-title">
              <div class="mb-2 flex items-center justify-between">
                <h3 id="workspace-journeys-title" class="flex items-center gap-2 text-xs font-semibold">
                  <Route class="h-3.5 w-3.5 text-cyan-300" />
                  Active journeys
                </h3>
                <RouterLink v-if="canViewCRM" to="/crm/pipeline" class="rounded-md p-2 text-white/35 hover:text-cyan-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-gray-500" aria-label="Open full revenue pipeline">
                  <ArrowUpRight class="h-3.5 w-3.5" />
                </RouterLink>
              </div>
              <div v-if="!canViewCRM" class="rounded-xl border border-dashed border-white/[0.08] p-3 text-xs text-white/35 light:border-gray-200 light:text-gray-500">
                <LockKeyhole class="mr-2 inline h-3.5 w-3.5" />
                CRM details are hidden by your permissions.
              </div>
              <div v-else-if="openJourneys.length" class="space-y-2">
                <article v-for="journey in openJourneys.slice(0, 3)" :key="journey.id" class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50">
                  <div class="flex items-start justify-between gap-3">
                    <div class="min-w-0">
                      <p class="truncate text-xs font-medium">{{ journey.title }}</p>
                      <div class="mt-1.5 flex flex-wrap items-center gap-1.5">
                        <Badge variant="outline" class="h-5 gap-1 px-1.5 text-[9px]">
                          <span class="h-1.5 w-1.5 rounded-full" :style="{ backgroundColor: journey.stage?.color || '#67e8f9' }" />
                          {{ journey.stage?.name || 'Open' }}
                        </Badge>
                        <span class="text-[10px] text-white/35 light:text-gray-500">{{ journey.pipeline?.name }}</span>
                      </div>
                    </div>
                    <span class="shrink-0 text-[11px] font-semibold text-emerald-200 light:text-emerald-800">
                      {{ money(journey.value_minor, journey.currency) }}
                    </span>
                  </div>
                  <p class="mt-2 text-[10px] text-white/35 light:text-gray-500">
                    Next action: {{ dateTime(journey.next_action_at) }}
                  </p>
                </article>
              </div>
              <div v-else class="rounded-xl border border-dashed border-white/[0.08] p-4 text-center light:border-gray-200">
                <p class="text-xs font-medium">No active journey</p>
                <p class="mt-1 text-[10px] text-white/35 light:text-gray-500">Create one when this conversation becomes a qualified opportunity.</p>
              </div>
            </section>

            <section aria-labelledby="workspace-tasks-title">
              <div class="mb-2 flex items-center justify-between">
                <h3 id="workspace-tasks-title" class="flex items-center gap-2 text-xs font-semibold">
                  <ListChecks class="h-3.5 w-3.5 text-violet-300" />
                  Next actions
                </h3>
                <RouterLink v-if="canViewTasks" to="/crm/tasks" class="rounded-md p-2 text-white/35 hover:text-violet-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:text-gray-500" aria-label="Open all follow-up tasks">
                  <ArrowUpRight class="h-3.5 w-3.5" />
                </RouterLink>
              </div>
              <div v-if="!canViewTasks" class="rounded-xl border border-dashed border-white/[0.08] p-3 text-xs text-white/35 light:border-gray-200 light:text-gray-500">
                <LockKeyhole class="mr-2 inline h-3.5 w-3.5" />
                Follow-up tasks are hidden by your permissions.
              </div>
              <div v-else-if="openTasks.length" class="space-y-1.5">
                <article v-for="task in openTasks.slice(0, 3)" :key="task.id" class="flex items-start gap-2.5 rounded-xl border border-white/[0.07] px-3 py-2.5 light:border-gray-200">
                  <span class="mt-1 h-2 w-2 shrink-0 rounded-full" :class="task.priority === 'urgent' ? 'bg-rose-400' : task.priority === 'high' ? 'bg-amber-300' : 'bg-violet-300'" />
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-xs font-medium">{{ task.title }}</p>
                    <p class="mt-1 text-[10px] text-white/35 light:text-gray-500">{{ dateTime(task.due_at) }} · {{ task.priority }}</p>
                  </div>
                </article>
              </div>
              <p v-else class="rounded-xl bg-emerald-300/[0.035] p-3 text-xs text-emerald-200/70 light:text-emerald-800">No open follow-ups.</p>
            </section>

            <section aria-labelledby="workspace-bookings-title">
              <div class="mb-2 flex items-center justify-between">
                <h3 id="workspace-bookings-title" class="flex items-center gap-2 text-xs font-semibold">
                  <CalendarClock class="h-3.5 w-3.5 text-fuchsia-300" />
                  Care and bookings
                </h3>
                <RouterLink v-if="canViewBookings" to="/calendar" class="rounded-md p-2 text-white/35 hover:text-fuchsia-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fuchsia-300 light:text-gray-500" aria-label="Open booking calendar">
                  <ArrowUpRight class="h-3.5 w-3.5" />
                </RouterLink>
              </div>
              <div v-if="!canViewBookings" class="rounded-xl border border-dashed border-white/[0.08] p-3 text-xs text-white/35 light:border-gray-200 light:text-gray-500">
                <LockKeyhole class="mr-2 inline h-3.5 w-3.5" />
                Booking details are hidden by your permissions.
              </div>
              <div v-else-if="upcomingBookings.length || recentBookings.length" class="space-y-2">
                <article v-for="booking in upcomingBookings" :key="booking.id" class="rounded-xl border border-fuchsia-300/12 bg-fuchsia-300/[0.03] p-3">
                  <div class="flex items-start justify-between gap-2">
                    <div class="min-w-0">
                      <p class="truncate text-xs font-medium">{{ bookingName(booking) }}</p>
                      <p class="mt-1 text-[10px] text-white/40 light:text-gray-500">{{ dateTime(booking.event?.starts_at) }}</p>
                      <p v-if="booking.event?.resource?.name" class="mt-1 truncate text-[10px] text-white/30 light:text-gray-400">{{ booking.event.resource.name }}</p>
                    </div>
                    <Badge variant="outline" class="shrink-0 capitalize text-[9px]">{{ booking.status.replace('_', ' ') }}</Badge>
                  </div>
                </article>
                <details v-if="recentBookings.length" class="rounded-xl border border-white/[0.06] px-3 py-2 light:border-gray-200">
                  <summary class="cursor-pointer text-[10px] font-medium text-white/45 light:text-gray-600">Recent attendance</summary>
                  <div class="mt-2 space-y-2">
                    <div v-for="booking in recentBookings" :key="booking.id" class="flex items-center justify-between gap-2 text-[10px]">
                      <span class="truncate">{{ bookingName(booking) }} · {{ shortDate(booking.event?.starts_at) }}</span>
                      <Badge variant="secondary" class="capitalize text-[9px]">{{ booking.status.replace('_', ' ') }}</Badge>
                    </div>
                  </div>
                </details>
              </div>
              <p v-else class="rounded-xl border border-dashed border-white/[0.08] p-3 text-xs text-white/35 light:border-gray-200 light:text-gray-500">No bookings found.</p>
            </section>

            <section aria-labelledby="workspace-revenue-title">
              <div class="mb-2 flex items-center justify-between">
                <h3 id="workspace-revenue-title" class="flex items-center gap-2 text-xs font-semibold">
                  <CreditCard class="h-3.5 w-3.5 text-emerald-300" />
                  Packages and revenue
                </h3>
                <RouterLink v-if="canViewPackages || canViewPayments" to="/commerce" class="rounded-md p-2 text-white/35 hover:text-emerald-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-300 light:text-gray-500" aria-label="Open commerce desk">
                  <ArrowUpRight class="h-3.5 w-3.5" />
                </RouterLink>
              </div>
              <div v-if="!canViewPackages && !canViewPayments" class="rounded-xl border border-dashed border-white/[0.08] p-3 text-xs text-white/35 light:border-gray-200 light:text-gray-500">
                <LockKeyhole class="mr-2 inline h-3.5 w-3.5" />
                Commercial details are hidden by your permissions.
              </div>
              <div v-else class="space-y-2">
                <article v-for="item in activePackages" :key="item.id" class="rounded-xl border border-emerald-300/12 bg-emerald-300/[0.03] p-3">
                  <div class="flex items-start justify-between gap-2">
                    <div class="min-w-0">
                      <p class="truncate text-xs font-medium">{{ item.package_definition?.name || 'Customer package' }}</p>
                      <p class="mt-1 text-[10px] text-emerald-200/65 light:text-emerald-800">{{ packageCredits(item) }}</p>
                    </div>
                    <span class="shrink-0 text-[10px] text-white/35 light:text-gray-500">Expires {{ shortDate(item.expires_at) }}</span>
                  </div>
                </article>
                <article v-for="invoice in visibleInvoices" :key="invoice.id" class="flex items-center justify-between gap-3 rounded-xl border border-white/[0.07] px-3 py-2.5 light:border-gray-200">
                  <div class="min-w-0">
                    <p class="truncate text-xs font-medium">{{ invoice.invoice_number }}</p>
                    <p class="mt-1 text-[10px] capitalize text-white/35 light:text-gray-500">{{ invoice.status }} · {{ shortDate(invoice.due_at || invoice.issued_at) }}</p>
                  </div>
                  <div class="text-right">
                    <p class="text-xs font-semibold">{{ money(invoice.total_minor, invoice.currency) }}</p>
                    <p v-if="invoice.due_minor > 0" class="mt-0.5 text-[9px] text-amber-200 light:text-amber-800">{{ money(invoice.due_minor, invoice.currency) }} due</p>
                    <p v-else class="mt-0.5 text-[9px] text-emerald-200 light:text-emerald-800">Paid</p>
                  </div>
                </article>
                <p v-if="!activePackages.length && !visibleInvoices.length" class="rounded-xl border border-dashed border-white/[0.08] p-3 text-xs text-white/35 light:border-gray-200 light:text-gray-500">No package or invoice history.</p>
              </div>
            </section>
          </div>
        </ScrollArea>
      </TabsContent>

      <TabsContent value="timeline" class="mt-0 min-h-0 flex-1">
        <ScrollArea class="h-full">
          <div class="p-4">
            <div class="mb-4 flex items-center justify-between">
              <div>
                <h3 class="text-sm font-semibold">Customer timeline</h3>
                <p class="mt-1 text-[10px] text-white/35 light:text-gray-500">Messages, journeys, bookings and revenue in one chronology.</p>
              </div>
              <Button variant="ghost" size="icon" class="h-11 w-11" :loading="refreshing" aria-label="Refresh customer timeline" @click="loadWorkspace(true)">
                <RefreshCw class="h-4 w-4" />
              </Button>
            </div>
            <ol v-if="timeline.length" class="space-y-0">
              <li v-for="(event, index) in timeline" :key="event.id" class="relative flex gap-3 pb-5">
                <span v-if="index < timeline.length - 1" class="absolute bottom-0 left-[15px] top-8 w-px bg-white/[0.08] light:bg-gray-200" aria-hidden="true" />
                <span class="relative flex h-8 w-8 shrink-0 items-center justify-center rounded-full border border-white/[0.08] bg-[#111416] text-cyan-200 light:border-gray-200 light:bg-gray-50 light:text-cyan-700">
                  <component :is="timelineIcon(event)" class="h-3.5 w-3.5" />
                </span>
                <div class="min-w-0 flex-1 pt-0.5">
                  <div class="flex items-start justify-between gap-3">
                    <p class="text-xs font-medium">{{ event.title }}</p>
                    <time class="shrink-0 text-[9px] text-white/30 light:text-gray-400" :datetime="event.occurred_at">{{ dateTime(event.occurred_at) }}</time>
                  </div>
                  <p v-if="event.summary" class="mt-1 text-[11px] leading-5 text-white/40 light:text-gray-500">{{ event.summary }}</p>
                  <p v-if="event.actor?.name" class="mt-1 text-[9px] text-white/25 light:text-gray-400">{{ event.actor.name }}</p>
                </div>
              </li>
            </ol>
            <div v-else class="rounded-xl border border-dashed border-white/[0.08] px-5 py-10 text-center light:border-gray-200">
              <History class="mx-auto h-6 w-6 text-white/20 light:text-gray-300" />
              <p class="mt-3 text-xs font-medium">No timeline events yet</p>
              <p class="mt-1 text-[10px] text-white/35 light:text-gray-500">New customer activity will appear here automatically.</p>
            </div>
          </div>
        </ScrollArea>
      </TabsContent>

      <TabsContent value="details" class="mt-0 min-h-0 flex-1">
        <ContactInfoPanel
          :contact="detailContact"
          :session-data="sessionData"
          embedded
          @tags-updated="emit('tagsUpdated', $event)"
        />
      </TabsContent>

      <TabsContent value="copilot" class="mt-0 min-h-0 flex-1">
        <ScrollArea class="h-full">
          <div class="space-y-4 p-4">
            <div class="rounded-xl border border-emerald-300/15 bg-emerald-300/[0.035] p-3">
              <div class="flex gap-2.5">
                <Sparkles class="mt-0.5 h-4 w-4 shrink-0 text-emerald-200" />
                <div>
                  <h3 class="text-xs font-semibold">Human-reviewed Copilot</h3>
                  <p class="mt-1 text-[10px] leading-4 text-white/40 light:text-gray-600">Copilot can review this conversation, but it cannot send a message or update CRM records.</p>
                </div>
              </div>
            </div>
            <div v-if="canUseCopilot" class="grid grid-cols-3 gap-2">
              <Button class="h-auto min-h-16 flex-col gap-1.5 px-2 py-2 text-[10px]" variant="outline" :disabled="Boolean(copilotRunning)" @click="runCopilot('summary')">
                <Loader2 v-if="copilotRunning === 'summary'" class="h-4 w-4 animate-spin" />
                <FileSearch v-else class="h-4 w-4 text-emerald-200" />
                Summary
              </Button>
              <Button class="h-auto min-h-16 flex-col gap-1.5 px-2 py-2 text-[10px]" variant="outline" :disabled="Boolean(copilotRunning)" @click="runCopilot('qualify')">
                <Loader2 v-if="copilotRunning === 'qualify'" class="h-4 w-4 animate-spin" />
                <Wand2 v-else class="h-4 w-4 text-cyan-200" />
                Qualify
              </Button>
              <Button class="h-auto min-h-16 flex-col gap-1.5 px-2 py-2 text-[10px]" variant="outline" :disabled="Boolean(copilotRunning)" @click="runCopilot('extract_actions')">
                <Loader2 v-if="copilotRunning === 'extract_actions'" class="h-4 w-4 animate-spin" />
                <ListChecks v-else class="h-4 w-4 text-violet-200" />
                Next actions
              </Button>
            </div>
            <div v-else class="rounded-xl border border-dashed border-white/[0.08] p-4 text-xs text-white/35 light:border-gray-200 light:text-gray-500">
              <LockKeyhole class="mr-2 inline h-3.5 w-3.5" />
              Copilot execution is not available with your current permissions.
            </div>
            <div v-if="copilotRun" class="rounded-xl border border-white/[0.08] bg-white/[0.02] p-3 light:border-gray-200 light:bg-gray-50" aria-live="polite">
              <div class="flex items-center justify-between gap-3">
                <div>
                  <Badge variant="outline" class="text-[9px]">{{ copilotLabel(copilotRun.task_type as CopilotAction) }}</Badge>
                  <p class="mt-1 text-[9px] text-white/30 light:text-gray-400">{{ copilotRun.model }} · review before use</p>
                </div>
                <Button variant="ghost" size="icon" class="h-11 w-11" aria-label="Copy Copilot result" @click="copyCopilotResult">
                  <Clipboard class="h-4 w-4" />
                </Button>
              </div>
              <pre class="mt-3 whitespace-pre-wrap break-words font-sans text-xs leading-5 text-white/70 light:text-gray-700">{{ copilotResult }}</pre>
              <div v-if="copilotRun.safety_warnings?.length" class="mt-3 rounded-lg border border-amber-300/15 bg-amber-300/[0.04] p-2.5">
                <p class="text-[9px] font-semibold uppercase tracking-wider text-amber-200 light:text-amber-800">Review warnings</p>
                <p v-for="warning in copilotRun.safety_warnings" :key="warning" class="mt-1 text-[10px] text-amber-100/60 light:text-amber-900">{{ warning }}</p>
              </div>
            </div>
          </div>
        </ScrollArea>
      </TabsContent>
    </Tabs>

    <Dialog v-model:open="showJourneyDialog">
      <DialogContent class="max-w-md">
        <DialogHeader>
          <DialogTitle>Create customer journey</DialogTitle>
          <DialogDescription>Track this opportunity independently from the customer’s other enquiries.</DialogDescription>
        </DialogHeader>
        <form id="workspace-journey-form" class="space-y-4" @submit.prevent="createJourney">
          <div class="space-y-1.5">
            <Label for="workspace-journey-title">Journey title</Label>
            <Input id="workspace-journey-title" v-model="journeyDraft.title" name="journey_title" required maxlength="255" />
          </div>
          <div class="space-y-1.5">
            <Label for="workspace-journey-pipeline">Pipeline</Label>
            <select id="workspace-journey-pipeline" v-model="journeyDraft.pipeline_id" name="pipeline_id" required class="h-11 w-full rounded-lg border border-white/10 bg-[#111416] px-3 text-sm text-white outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-gray-200 light:bg-white light:text-gray-900">
              <option v-for="pipeline in pipelines" :key="pipeline.id" :value="pipeline.id">{{ pipeline.name }}</option>
            </select>
          </div>
          <div class="grid grid-cols-[1fr_105px] gap-3">
            <div class="space-y-1.5">
              <Label for="workspace-journey-value">Estimated value</Label>
              <Input id="workspace-journey-value" v-model="journeyDraft.value" name="journey_value" type="number" min="0" step="0.01" />
            </div>
            <div class="space-y-1.5">
              <Label for="workspace-journey-currency">Currency</Label>
              <select id="workspace-journey-currency" v-model="journeyDraft.currency" name="journey_currency" class="h-10 w-full rounded-lg border border-white/10 bg-[#111416] px-2 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="MYR">MYR</option>
                <option value="SGD">SGD</option>
                <option value="USD">USD</option>
              </select>
            </div>
          </div>
        </form>
        <DialogFooter>
          <Button variant="outline" @click="showJourneyDialog = false">Cancel</Button>
          <Button form="workspace-journey-form" type="submit" class="bg-cyan-300 text-black hover:bg-cyan-200" :loading="savingJourney" :disabled="!journeyDraft.title.trim() || !firstOpenStage">
            Create journey
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="showTaskDialog">
      <DialogContent class="max-w-md">
        <DialogHeader>
          <DialogTitle>Schedule follow-up</DialogTitle>
          <DialogDescription>Make the next customer promise visible to the team.</DialogDescription>
        </DialogHeader>
        <form id="workspace-task-form" class="space-y-4" @submit.prevent="createFollowUp">
          <div class="space-y-1.5">
            <Label for="workspace-task-title">What needs to happen?</Label>
            <Input id="workspace-task-title" v-model="taskDraft.title" name="task_title" required maxlength="255" />
          </div>
          <div class="space-y-1.5">
            <Label for="workspace-task-notes">Context</Label>
            <Textarea id="workspace-task-notes" v-model="taskDraft.description" name="task_description" :rows="3" maxlength="5000" />
          </div>
          <div class="grid grid-cols-2 gap-3">
            <div class="space-y-1.5">
              <Label for="workspace-task-priority">Priority</Label>
              <select id="workspace-task-priority" v-model="taskDraft.priority" name="task_priority" class="h-10 w-full rounded-lg border border-white/10 bg-[#111416] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
                <option value="low">Low</option>
                <option value="normal">Normal</option>
                <option value="high">High</option>
                <option value="urgent">Urgent</option>
              </select>
            </div>
            <div class="space-y-1.5">
              <Label for="workspace-task-due">Due</Label>
              <Input id="workspace-task-due" v-model="taskDraft.due_at" name="task_due_at" type="datetime-local" />
            </div>
          </div>
          <div v-if="openJourneys.length" class="space-y-1.5">
            <Label for="workspace-task-journey">Related journey</Label>
            <select id="workspace-task-journey" v-model="taskDraft.lead_id" name="task_lead_id" class="h-10 w-full rounded-lg border border-white/10 bg-[#111416] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
              <option value="">No journey</option>
              <option v-for="journey in openJourneys" :key="journey.id" :value="journey.id">{{ journey.title }}</option>
            </select>
          </div>
        </form>
        <DialogFooter>
          <Button variant="outline" @click="showTaskDialog = false">Cancel</Button>
          <Button form="workspace-task-form" type="submit" class="bg-violet-400 text-black hover:bg-violet-300" :loading="savingTask" :disabled="!taskDraft.title.trim()">
            Schedule
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </section>
</template>
