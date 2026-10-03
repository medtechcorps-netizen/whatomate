<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import {
  AlertCircle,
  ArrowUpRight,
  CalendarClock,
  Check,
  CheckCircle2,
  CircleDollarSign,
  Clipboard,
  Clock3,
  FileSearch,
  History,
  ListChecks,
  Loader2,
  LockKeyhole,
  Package,
  Plus,
  Receipt,
  RefreshCw,
  RotateCcw,
  Route,
  Sparkles,
  ThumbsDown,
  Trophy,
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
import { formatCurrencyMinorUnits } from '@/lib/currency'
import { contactAddressDisplay, contactDisplayName } from '@/lib/contactAddress'
import {
  buildCreateLeadPayload,
  buildFollowUpTaskPayload,
  defaultPipelineId,
  firstOpenStage,
  followUpPresetDate,
  followUpUrgency,
  invoiceForLead,
  leadFlowState,
  leadSourceForChannel,
  lostStage,
  nextFollowUpForLead,
  openStages,
  pickFocusLead,
  rememberPipelineId,
  rememberedPipelineId,
  wonStage,
  type LeadDraft,
  type WorkspaceRequestedAction,
} from '@/lib/crmFlow'
import { getAvatarGradient, getInitials } from '@/lib/utils'
import {
  copilotService,
  crmService,
  customerWorkspaceService,
  type Booking,
  type CommerceInvoice,
  type ContactPackage,
  type CopilotRun,
  type CRMLead,
  type CustomerIdentity,
  type CustomerTimelineEvent,
  type CustomerWorkspace,
  type CustomerWorkspaceCapabilities,
  type FollowUpTask,
  type Pipeline,
  type PipelineStage,
} from '@/services/productSuite'
import ContactInfoPanel from '@/components/chat/ContactInfoPanel.vue'
import ContactBookingDialog from '@/components/booking/ContactBookingDialog.vue'
import LeadCreateDialog from '@/components/crm/LeadCreateDialog.vue'
import LeadOutcomeDialog from '@/components/crm/LeadOutcomeDialog.vue'
import InvoiceQuickDialog from '@/components/commerce/InvoiceQuickDialog.vue'

type WorkspaceTab = 'overview' | 'timeline' | 'details' | 'copilot'
type CopilotAction = Extract<CopilotRun['task_type'], 'summary' | 'qualify' | 'extract_actions'>
type ContactInfoSessionData = InstanceType<typeof ContactInfoPanel>['$props']['sessionData']
type WorkspaceMutationContext = {
  generation: number
  selectedContactId: string
  canonicalContactId: string
}
type FollowUpPreset = 'tomorrow' | '3days' | 'week'

// completingTaskId marker while several follow-ups are being marked done together.
const BATCH_COMPLETE_ID = '__lead-follow-ups__'

const props = withDefaults(defineProps<{
  contactId: string
  contact?: Partial<Contact> | null
  sessionData?: ContactInfoSessionData
  surface?: 'chat' | 'omnichannel'
  channel?: string | null
  conversationId?: string | null
  requestedAction?: WorkspaceRequestedAction | null
}>(), {
  contact: null,
  sessionData: null,
  surface: 'chat',
  channel: null,
  conversationId: null,
  requestedAction: null,
})

const emit = defineEmits<{
  close: []
  tagsUpdated: [tags: string[]]
  'action-consumed': [nonce: number]
}>()

const FOLLOW_UP_PRESETS: Array<{ value: FollowUpPreset; label: string }> = [
  { value: 'tomorrow', label: 'Tomorrow' },
  { value: '3days', label: 'In 3 days' },
  { value: 'week', label: 'Next week' },
]

const CHANNEL_LABELS: Record<string, string> = {
  whatsapp: 'WhatsApp',
  instagram: 'Instagram',
  messenger: 'Messenger',
  facebook: 'Facebook',
  threads: 'Threads',
  email: 'Email',
  webchat: 'Website chat',
  tiktok: 'TikTok',
}

const toast = useAppToast()
const authStore = useAuthStore()
const workspace = ref<CustomerWorkspace | null>(null)
const loading = ref(true)
const refreshing = ref(false)
const error = ref('')
const activeTab = ref<WorkspaceTab>('overview')
const workspaceSelectionContactId = ref('')
let loadSequence = 0
let copilotSequence = 0
let mutationContextGeneration = 0
let handledActionNonce: number | null = null

const showJourneyDialog = ref(false)
const showTaskDialog = ref(false)
const showBookingDialog = ref(false)
const showOutcomeDialog = ref(false)
const showInvoiceDialog = ref(false)
const savingJourney = ref(false)
const savingTask = ref(false)
const journeyMutationContext = ref<WorkspaceMutationContext | null>(null)
const taskMutationContext = ref<WorkspaceMutationContext | null>(null)
const moveMutationContext = ref<WorkspaceMutationContext | null>(null)
const completeMutationContext = ref<WorkspaceMutationContext | null>(null)
const invoiceMutationContext = ref<WorkspaceMutationContext | null>(null)
const pipelines = ref<Pipeline[]>([])
const pipelinesLoading = ref(false)
const pipelinesError = ref('')
// Set when adding a lead failed because pipelines could not be loaded, so a
// successful retry takes the user straight back to the add-lead form.
const retryOpensLeadDialog = ref(false)
let pipelinesRequest: Promise<boolean> | null = null
// Each full (non-silent) workspace load gets a new id; the automatic pipeline
// fetch for the stage stepper runs at most once per id.
let workspaceLoadId = 0
let autoPipelinesLoadId = -1
const journeyIdempotencyKey = ref('')
const followUpIdempotencyKey = ref('')
const taskIdempotencyKey = ref('')
const journeyDraft = ref<LeadDraft>(emptyLeadDraft())
const taskDraft = ref({
  title: '',
  description: '',
  priority: 'normal' as FollowUpTask['priority'],
  due_at: '',
  lead_id: '',
})
const movingLeadId = ref('')
const movingStageId = ref('')
const completingTaskId = ref('')
const outcome = ref<'won' | 'lost'>('won')
const outcomeLeadId = ref('')
const invoiceLead = ref<CRMLead | null>(null)

const copilotRunning = ref<CopilotAction | null>(null)
const copilotRun = ref<CopilotRun | null>(null)
const copilotResult = ref('')

const contactRecord = computed(() => workspace.value?.contact ?? props.contact)
const canonicalContactId = computed(() => contactRecord.value?.id || props.contactId)
const contactAddressInput = computed(() => ({
  phone_number: contactRecord.value?.phone_number ?? props.contact?.phone_number ?? '',
  whatsapp_account: props.contact?.whatsapp_account ?? null,
  metadata: (contactRecord.value?.metadata ?? props.contact?.metadata ?? null) as Record<string, unknown> | null,
}))
// Never a raw placeholder such as "bsuid:..."; used for the header, avatar,
// follow-up title, lead form, booking toast and the Details tab.
const contactName = computed(() =>
  contactDisplayName(
    {
      ...contactAddressInput.value,
      profile_name: contactRecord.value?.profile_name,
      name: contactRecord.value?.name,
    },
    { channel: props.channel },
  ),
)
const contactPhone = computed(() => contactRecord.value?.phone_number || '')
const contactAddress = computed(() =>
  contactAddressDisplay(contactAddressInput.value, { channel: props.channel }),
)
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

const leadStatusRank: Record<CRMLead['status'], number> = { open: 0, won: 1, lost: 2, archived: 3 }
const visibleLeads = computed(() =>
  journeys.value
    .filter((journey) => journey.status !== 'archived')
    .sort((left, right) =>
      leadStatusRank[left.status] - leadStatusRank[right.status] ||
      timeValue(right.last_activity_at ?? right.updated_at ?? right.created_at) -
        timeValue(left.last_activity_at ?? left.updated_at ?? left.created_at),
    ),
)

const focusLead = computed(() => pickFocusLead(journeys.value))
// The focus lead is shown in the Next step card, so the list only shows the rest.
const otherLeads = computed(() => visibleLeads.value.filter((lead) => lead.id !== focusLead.value?.id))
const flowState = computed(() => leadFlowState(focusLead.value))
const focusPipeline = computed(() => pipelineForLead(focusLead.value))
const focusStages = computed(() => openStages(focusPipeline.value))
const focusStageIndex = computed(() =>
  focusStages.value.findIndex((stage) => stage.id === focusLead.value?.stage_id),
)
const focusWonStage = computed(() => wonStage(focusPipeline.value))
const focusLostStage = computed(() => lostStage(focusPipeline.value))
const focusReopenStage = computed(() => firstOpenStage(focusPipeline.value))
const focusInvoice = computed(() =>
  focusLead.value ? invoiceForLead(invoices.value, focusLead.value.id) : null,
)
const focusFollowUp = computed(() => nextFollowUpForLead(tasks.value, focusLead.value))
const focusFollowUpUrgency = computed(() => followUpUrgency(focusFollowUp.value?.dueAt))
const focusFollowUpTask = computed(() => focusFollowUp.value?.task ?? null)
// An overdue or undated follow-up task can be ticked off straight from the card.
const canFinishFocusFollowUp = computed(() =>
  canCreateTask.value &&
  Boolean(focusFollowUpTask.value) &&
  (focusFollowUpUrgency.value === 'overdue' || focusFollowUpUrgency.value === 'none'),
)
const focusLeadOpenTasks = computed(() => {
  const leadId = focusLead.value?.id
  return leadId ? openTasks.value.filter((task) => task.lead_id === leadId) : []
})
const outcomeUnavailableReason = computed(() => {
  if (!focusPipeline.value) return ''
  const missingWon = !focusWonStage.value
  const missingLost = !focusLostStage.value
  if (missingWon && missingLost) return 'This pipeline has no Won or Lost stage. Ask an admin to add them.'
  if (missingWon) return 'This pipeline has no Won stage. Ask an admin to add one.'
  if (missingLost) return 'This pipeline has no Lost stage. Ask an admin to add one.'
  return ''
})
// The stepper and Won/Lost/Reopen need the lead's pipeline; without it (no
// permission to read pipelines, a failed load, or a pipeline that is no longer
// listed) staff are sent to the pipeline page instead.
const focusMoveUnavailable = computed(() =>
  canMoveLead.value && !focusPipeline.value && !pipelinesLoading.value,
)
const moreOpenLeads = computed(() =>
  flowState.value === 'open' ? Math.max(0, openJourneys.value.length - 1) : 0,
)
const outcomeLead = computed(() =>
  journeys.value.find((journey) => journey.id === outcomeLeadId.value) ?? null,
)
const outcomeStage = computed(() => {
  const pipeline = pipelineForLead(outcomeLead.value)
  return outcome.value === 'won' ? wonStage(pipeline) : lostStage(pipeline)
})

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

function hasEntitlement(key: string) {
  return typeof authStore.hasProductEntitlement === 'function'
    ? authStore.hasProductEntitlement(key)
    : false
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
const canReadPipelines = computed(() =>
  canViewCRM.value && authStore.hasPermission('crm.pipelines', 'read'),
)
const canCreateJourney = computed(() =>
  canViewCRM.value &&
  authStore.hasPermission('crm.leads', 'write') &&
  authStore.hasPermission('crm.pipelines', 'read'),
)
const canMoveLead = computed(() => canViewCRM.value && authStore.hasPermission('crm.leads', 'write'))
const canCreateTask = computed(() =>
  canViewTasks.value && authStore.hasPermission('tasks', 'write'),
)
const canCreateBooking = computed(() =>
  canViewBookings.value && authStore.hasPermission('bookings', 'write'),
)
const canInvoice = computed(() =>
  authStore.hasPermission('payments', 'write') &&
  authStore.hasPermission('contacts', 'read') &&
  hasEntitlement('commerce.enabled'),
)
const canSellPackages = computed(() => canInvoice.value && authStore.hasPermission('packages', 'write'))
const showCommerceSection = computed(() =>
  (canViewPackages.value || canViewPayments.value) &&
  (activePackages.value.length > 0 || visibleInvoices.value.length > 0),
)

const leadSource = computed(() =>
  props.surface === 'chat' || leadSourceForChannel(props.channel) === 'whatsapp' ? 'whatsapp' : 'other',
)
const leadSourceReference = computed(() => {
  const conversationId = props.conversationId?.trim()
  return conversationId ? `conversation:${conversationId}`.slice(0, 255) : undefined
})
// Storage is not reactive, so the default is recomputed whenever the dialog
// opens or the pipelines arrive.
const leadDefaultPipelineId = ref('')
const leadDialogContact = computed(() => ({
  id: journeyMutationContext.value?.canonicalContactId || canonicalContactId.value,
  name: contactName.value,
}))
const invoiceContact = computed(() =>
  invoiceMutationContext.value
    ? { id: invoiceMutationContext.value.canonicalContactId, name: contactName.value }
    : null,
)

const summaryLine = computed(() => {
  const parts: string[] = []
  const pipelineValue = summary.value.pipelineValue.filter((item) => item.amount_minor > 0)
  if (pipelineValue.length) parts.push(`Open pipeline ${moneyList(pipelineValue)}`)
  const outstanding = summary.value.outstanding.filter((item) => item.amount_minor > 0)
  if (outstanding.length) parts.push(`Unpaid ${moneyList(outstanding)}`)
  return parts.join(' · ')
})

function emptyLeadDraft(): LeadDraft {
  return {
    contact_id: '',
    contact_name: '',
    pipeline_id: '',
    stage_id: '',
    title: '',
    value: '',
    currency: 'MYR',
    follow_up_at: '',
  }
}

function refreshLeadDefaultPipelineId() {
  leadDefaultPipelineId.value = defaultPipelineId(pipelines.value, {
    rememberedId: rememberedPipelineId(authStore.organizationId || undefined),
  })
}

function resetJourneyDraft() {
  journeyDraft.value = {
    ...emptyLeadDraft(),
    pipeline_id: leadDefaultPipelineId.value,
  }
}

function resetTaskDraft() {
  taskDraft.value = {
    title: '',
    description: '',
    priority: 'normal',
    due_at: '',
    lead_id: '',
  }
}

function invalidateWorkspaceMutationDialogs() {
  mutationContextGeneration += 1
  journeyMutationContext.value = null
  taskMutationContext.value = null
  moveMutationContext.value = null
  completeMutationContext.value = null
  invoiceMutationContext.value = null
  showJourneyDialog.value = false
  showTaskDialog.value = false
  showBookingDialog.value = false
  showOutcomeDialog.value = false
  showInvoiceDialog.value = false
  savingJourney.value = false
  savingTask.value = false
  movingLeadId.value = ''
  movingStageId.value = ''
  completingTaskId.value = ''
  outcomeLeadId.value = ''
  invoiceLead.value = null
  retryOpensLeadDialog.value = false
  resetJourneyDraft()
  resetTaskDraft()
}

function captureWorkspaceMutationContext(): WorkspaceMutationContext | null {
  const selectedContactId = props.contactId.trim()
  const resolvedContactId = workspace.value?.contact?.id?.trim()
  if (
    !selectedContactId
    || !resolvedContactId
    || loading.value
    || workspaceSelectionContactId.value !== selectedContactId
  ) return null

  return {
    generation: ++mutationContextGeneration,
    selectedContactId,
    canonicalContactId: resolvedContactId,
  }
}

function mutationContextIsCurrent(
  context: WorkspaceMutationContext,
  active: WorkspaceMutationContext | null,
) {
  return (
    active?.generation === context.generation
    && active.selectedContactId === context.selectedContactId
    && active.canonicalContactId === context.canonicalContactId
    && props.contactId === context.selectedContactId
    && workspaceSelectionContactId.value === context.selectedContactId
    && canonicalContactId.value === context.canonicalContactId
  )
}

const detailContact = computed<Contact>(() => ({
  id: canonicalContactId.value,
  phone_number: contactPhone.value,
  name: contactName.value,
  profile_name: contactRecord.value?.profile_name,
  avatar_url: contactRecord.value?.avatar_url,
  status: contactRecord.value?.status || 'active',
  tags: contactRecord.value?.tags ?? [],
  metadata: (contactRecord.value?.metadata ?? {}) as Record<string, unknown>,
  unread_count: props.contact?.unread_count ?? 0,
  assigned_user_id: contactRecord.value?.assigned_user_id,
  whatsapp_account: props.contact?.whatsapp_account,
  marketing_opt_out: contactRecord.value?.marketing_opt_out,
  identity_review_ai_state: contactRecord.value?.identity_review_ai_state ?? {
    known: false,
    ai_allowed: false,
    blocked: true,
    open_hold_count: 0,
    reason: 'identity_review_state_unavailable',
  },
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
  return {
    pipelineValue,
    outstanding,
  }
})

async function loadWorkspace(silent = false) {
  const sequence = ++loadSequence
  const selectedContactId = props.contactId
  if (silent) refreshing.value = true
  else {
    workspaceLoadId += 1
    loading.value = true
    workspaceSelectionContactId.value = ''
  }
  error.value = ''
  try {
    const response = await customerWorkspaceService.get(selectedContactId)
    if (sequence !== loadSequence || props.contactId !== selectedContactId) return
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
    workspaceSelectionContactId.value = selectedContactId
  } catch (cause) {
    if (sequence !== loadSequence || props.contactId !== selectedContactId) return
    error.value = getErrorMessage(cause)
  } finally {
    if (sequence === loadSequence) {
      loading.value = false
      refreshing.value = false
    }
  }
}

/** Loads the pipelines once; concurrent callers share one request. Resolves true when they are available. */
function ensurePipelines(): Promise<boolean> {
  if (pipelines.value.length) return Promise.resolve(true)
  if (pipelinesRequest) return pipelinesRequest
  pipelinesLoading.value = true
  pipelinesError.value = ''
  let request!: Promise<boolean>
  request = (async () => {
    try {
      const response = await crmService.pipelines()
      pipelines.value = unwrapListResponse<Pipeline>(response, 'pipelines')
      refreshLeadDefaultPipelineId()
      if (!journeyDraft.value.pipeline_id) journeyDraft.value.pipeline_id = leadDefaultPipelineId.value
      return true
    } catch (cause) {
      pipelinesError.value = getErrorMessage(cause) || 'Pipelines could not be loaded.'
      return false
    } finally {
      pipelinesLoading.value = false
      if (pipelinesRequest === request) pipelinesRequest = null
    }
  })()
  // A request that already settled (a synchronous failure) is not kept.
  if (pipelinesLoading.value) pipelinesRequest = request
  return request
}

async function retryPipelines() {
  const loaded = await ensurePipelines()
  if (!loaded || !retryOpensLeadDialog.value) return
  retryOpensLeadDialog.value = false
  if (canCreateJourney.value) await openJourney()
}

function pipelineForLead(lead: CRMLead | null | undefined): Pipeline | null {
  if (!lead) return null
  return pipelines.value.find((pipeline) => pipeline.id === lead.pipeline_id) ?? null
}

function isConflict(cause: unknown) {
  const status = (cause as { response?: { status?: number } } | null)?.response?.status
  return status === 409
}

async function openJourney() {
  const context = captureWorkspaceMutationContext()
  if (!context) return
  journeyMutationContext.value = context
  journeyIdempotencyKey.value = crypto.randomUUID()
  followUpIdempotencyKey.value = crypto.randomUUID()
  refreshLeadDefaultPipelineId()
  resetJourneyDraft()
  journeyDraft.value.contact_id = context.canonicalContactId
  journeyDraft.value.contact_name = contactName.value
  retryOpensLeadDialog.value = false
  showJourneyDialog.value = true
  const loaded = await ensurePipelines()
  if (
    !loaded
    && pipelinesError.value
    && showJourneyDialog.value
    && journeyMutationContext.value?.generation === context.generation
  ) {
    // The lead form has no error state of its own, so close it and offer the
    // retry in the Next step card (it reopens the form once pipelines load).
    showJourneyDialog.value = false
    journeyMutationContext.value = null
    retryOpensLeadDialog.value = true
  }
}

async function submitLeadDraft(draft: LeadDraft) {
  journeyDraft.value = { ...draft }
  await createJourney()
}

async function createJourney() {
  const context = journeyMutationContext.value
  if (
    !context
    || !showJourneyDialog.value
    || !mutationContextIsCurrent(context, journeyMutationContext.value)
    || savingJourney.value
  ) return
  const draft = journeyDraft.value
  const pipeline = pipelines.value.find((item) => item.id === draft.pipeline_id) ?? null
  const stages = openStages(pipeline)
  const stage = stages.find((item) => item.id === draft.stage_id) ?? stages[0] ?? null
  if (!draft.title.trim() || !pipeline || !stage) return

  const followUpAt = canCreateTask.value ? draft.follow_up_at : ''
  savingJourney.value = true
  try {
    const response = await crmService.createLead(buildCreateLeadPayload(
      {
        ...draft,
        contact_id: context.canonicalContactId,
        pipeline_id: pipeline.id,
        stage_id: stage.id,
        follow_up_at: followUpAt,
      },
      {
        source: leadSource.value,
        sourceReference: leadSourceReference.value,
        idempotencyKey: journeyIdempotencyKey.value || crypto.randomUUID(),
      },
    ) as Partial<CRMLead>)
    if (!mutationContextIsCurrent(context, journeyMutationContext.value)) return
    rememberPipelineId(authStore.organizationId || undefined, pipeline.id)

    let followUpFailed = false
    if (followUpAt) {
      const created = unwrapItemResponse<Partial<CRMLead>>(response)
      try {
        if (!created?.id) throw new Error('The new lead was not returned')
        await crmService.createTask(buildFollowUpTaskPayload({
          contactId: context.canonicalContactId,
          leadId: created.id,
          leadTitle: draft.title,
          dueAt: followUpAt,
          source: `${props.surface}_workspace`,
          idempotencyKey: followUpIdempotencyKey.value || crypto.randomUUID(),
        }) as Partial<FollowUpTask>)
      } catch {
        followUpFailed = true
      }
      if (!mutationContextIsCurrent(context, journeyMutationContext.value)) return
    }

    savingJourney.value = false
    showJourneyDialog.value = false
    journeyMutationContext.value = null
    resetJourneyDraft()
    if (followUpFailed) {
      toast.warning('Lead added, but the follow-up was not scheduled', 'Use Set follow-up to try again.')
    } else {
      toast.success('Lead added', `Added to ${pipeline.name || 'the pipeline'} - ${stage.name || 'first stage'}.`)
    }
    await loadWorkspace(true)
  } catch (cause) {
    if (mutationContextIsCurrent(context, journeyMutationContext.value)) {
      toast.error('Lead was not added', getErrorMessage(cause))
    }
  } finally {
    if (mutationContextIsCurrent(context, journeyMutationContext.value)) {
      savingJourney.value = false
    }
  }
}

function openFollowUp(leadId?: string) {
  const context = captureWorkspaceMutationContext()
  if (!context) return
  taskMutationContext.value = context
  taskIdempotencyKey.value = crypto.randomUUID()
  const preferredLeadId = typeof leadId === 'string' && openJourneys.value.some((journey) => journey.id === leadId)
    ? leadId
    : flowState.value === 'open'
      ? focusLead.value?.id ?? ''
      : openJourneys.value[0]?.id ?? ''
  taskDraft.value = {
    title: `Follow up with ${contactName.value}`,
    description: '',
    priority: 'normal',
    due_at: '',
    lead_id: preferredLeadId,
  }
  showTaskDialog.value = true
}

function toLocalInputValue(date: Date) {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 16)
}

function applyTaskPreset(preset: FollowUpPreset) {
  taskDraft.value.due_at = toLocalInputValue(followUpPresetDate(preset))
}

function taskPresetActive(preset: FollowUpPreset) {
  return Boolean(taskDraft.value.due_at) && taskDraft.value.due_at === toLocalInputValue(followUpPresetDate(preset))
}

async function createFollowUp() {
  const context = taskMutationContext.value
  if (
    !context
    || !showTaskDialog.value
    || !mutationContextIsCurrent(context, taskMutationContext.value)
    || !taskDraft.value.title.trim()
    || savingTask.value
  ) return
  savingTask.value = true
  try {
    await crmService.createTask({
      contact_id: context.canonicalContactId,
      lead_id: taskDraft.value.lead_id || undefined,
      title: taskDraft.value.title.trim(),
      description: taskDraft.value.description.trim(),
      priority: taskDraft.value.priority,
      due_at: taskDraft.value.due_at
        ? new Date(taskDraft.value.due_at).toISOString()
        : undefined,
      source: `${props.surface}_workspace`,
      idempotency_key: taskIdempotencyKey.value || crypto.randomUUID(),
    })
    if (!mutationContextIsCurrent(context, taskMutationContext.value)) return
    savingTask.value = false
    showTaskDialog.value = false
    taskMutationContext.value = null
    taskDraft.value.description = ''
    taskDraft.value.due_at = ''
    toast.success('Follow-up scheduled')
    await loadWorkspace(true)
  } catch (cause) {
    if (mutationContextIsCurrent(context, taskMutationContext.value)) {
      toast.error('Follow-up was not created', getErrorMessage(cause))
    }
  } finally {
    if (mutationContextIsCurrent(context, taskMutationContext.value)) {
      savingTask.value = false
    }
  }
}

function completeTask(task: FollowUpTask | null | undefined) {
  if (!task) return Promise.resolve()
  return completeTasks([task])
}

/** Marks follow-ups done one after another, stopping if the selected customer changes. */
async function completeTasks(list: FollowUpTask[]) {
  if (!canCreateTask.value || completingTaskId.value || !list.length) return
  const context = captureWorkspaceMutationContext()
  if (!context) return
  completeMutationContext.value = context
  completingTaskId.value = list.length === 1 ? list[0].id : BATCH_COMPLETE_ID
  let completed = 0
  try {
    for (const task of list) {
      await crmService.completeTask(task.id, task.version)
      if (!mutationContextIsCurrent(context, completeMutationContext.value)) return
      completed += 1
    }
    if (list.length === 1) toast.success('Follow-up done', list[0].title)
    else toast.success('Follow-ups done', `${completed} follow-ups marked done.`)
    await loadWorkspace(true)
  } catch (cause) {
    if (!mutationContextIsCurrent(context, completeMutationContext.value)) return
    if (isConflict(cause)) {
      toast.info('This follow-up changed elsewhere. Refreshed.')
      await loadWorkspace(true)
    } else {
      toast.error('Follow-up was not updated', getErrorMessage(cause))
      if (completed) await loadWorkspace(true)
    }
  } finally {
    if (mutationContextIsCurrent(context, completeMutationContext.value)) {
      completingTaskId.value = ''
      completeMutationContext.value = null
    }
  }
}

async function moveLeadToStage(
  lead: CRMLead,
  stage: PipelineStage,
  options: { reason?: string; successTitle?: string } = {},
): Promise<boolean> {
  if (!canMoveLead.value || movingLeadId.value || lead.stage_id === stage.id) return false
  const context = captureWorkspaceMutationContext()
  if (!context) return false
  moveMutationContext.value = context
  movingLeadId.value = lead.id
  movingStageId.value = stage.id
  try {
    await crmService.moveLead(lead.id, stage.id, lead.version, options.reason)
    if (!mutationContextIsCurrent(context, moveMutationContext.value)) return false
    if (options.successTitle) toast.success(options.successTitle, `Moved to ${stage.name}.`)
    else toast.success(`Moved to ${stage.name}`)
    await loadWorkspace(true)
    return mutationContextIsCurrent(context, moveMutationContext.value)
  } catch (cause) {
    if (!mutationContextIsCurrent(context, moveMutationContext.value)) return false
    if (isConflict(cause)) {
      toast.info('This lead changed elsewhere. Refreshed.')
      await loadWorkspace(true)
    } else {
      toast.error('The lead was not moved', getErrorMessage(cause))
    }
    return false
  } finally {
    if (mutationContextIsCurrent(context, moveMutationContext.value)) {
      movingLeadId.value = ''
      movingStageId.value = ''
      moveMutationContext.value = null
    }
  }
}

function moveFocusLead(stage: PipelineStage) {
  const lead = focusLead.value
  if (!lead || lead.status !== 'open') return
  void moveLeadToStage(lead, stage)
}

function openOutcome(kind: 'won' | 'lost') {
  const lead = focusLead.value
  if (!lead || !canMoveLead.value) return
  outcome.value = kind
  outcomeLeadId.value = lead.id
  showOutcomeDialog.value = true
}

async function confirmOutcome(payload: { reason?: string; createInvoice?: boolean }) {
  const lead = outcomeLead.value
  const stage = outcomeStage.value
  const kind = outcome.value
  if (!lead) return
  if (!stage) {
    toast.error(
      kind === 'won' ? 'This pipeline has no Won stage' : 'This pipeline has no Lost stage',
      'Ask an admin to add one in the pipeline settings.',
    )
    return
  }
  const moved = await moveLeadToStage(lead, stage, {
    reason: kind === 'lost' ? payload.reason : undefined,
    successTitle: kind === 'won' ? 'Marked as won' : 'Marked as lost',
  })
  if (!moved) {
    // A conflicting edit refreshes the lead; close so the latest state is shown.
    if (!outcomeLead.value || outcomeLead.value.status !== 'open') showOutcomeDialog.value = false
    return
  }
  showOutcomeDialog.value = false
  if (kind === 'won' && payload.createInvoice && canInvoice.value) {
    openInvoice({ ...lead, status: 'won', stage_id: stage.id })
  }
}

function reopenFocusLead() {
  const lead = focusLead.value
  const stage = focusReopenStage.value
  if (!lead || !stage) return
  void moveLeadToStage(lead, stage, { successTitle: 'Lead reopened' })
}

function openInvoice(lead: CRMLead | null) {
  if (!canInvoice.value) return
  const context = captureWorkspaceMutationContext()
  if (!context) return
  invoiceMutationContext.value = context
  invoiceLead.value = lead
  showInvoiceDialog.value = true
}

async function invoiceCreated() {
  const context = invoiceMutationContext.value
  if (!context || !mutationContextIsCurrent(context, invoiceMutationContext.value)) return
  await loadWorkspace(true)
}

function openBooking() {
  if (!canCreateBooking.value) return
  showBookingDialog.value = true
}

async function bookingCreated() {
  toast.success('Appointment reserved', `${contactName.value}'s booking is now visible in the care timeline.`)
  await loadWorkspace(true)
}

function handleRequestedAction() {
  const action = props.requestedAction
  if (!action || handledActionNonce === action.nonce) return
  if (
    loading.value
    || !workspace.value
    || workspaceSelectionContactId.value !== props.contactId
  ) return
  handledActionNonce = action.nonce
  activeTab.value = 'overview'
  if (action.kind === 'lead') {
    if (canCreateJourney.value) void openJourney()
    else toast.info('You do not have access to add leads.')
  } else if (action.kind === 'follow-up') {
    if (canCreateTask.value) openFollowUp()
    else toast.info('You do not have access to schedule follow-ups.')
  } else if (action.kind === 'booking') {
    if (canCreateBooking.value) openBooking()
    else toast.info('You do not have access to book appointments.')
  }
  emit('action-consumed', action.nonce)
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
  const sequence = ++copilotSequence
  const contactId = canonicalContactId.value
  copilotRunning.value = action
  copilotRun.value = null
  copilotResult.value = ''
  try {
    const response = await copilotService.run(contactId, action, {
      message_limit: 30,
      idempotency_key: crypto.randomUUID(),
    })
    if (sequence !== copilotSequence || canonicalContactId.value !== contactId) return
    copilotRun.value = unwrapItemResponse<CopilotRun>(response)
    copilotResult.value =
      copilotRun.value.result_text ||
      (copilotRun.value.structured_result
        ? JSON.stringify(copilotRun.value.structured_result, null, 2)
        : 'No suggestion was returned.')
  } catch (cause) {
    if (sequence === copilotSequence && canonicalContactId.value === contactId) {
      toast.error('Copilot could not complete this review', getErrorMessage(cause))
    }
  } finally {
    if (sequence === copilotSequence) copilotRunning.value = null
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

function timeValue(value?: string) {
  if (!value) return 0
  const time = new Date(value).getTime()
  return Number.isNaN(time) ? 0 : time
}

function money(amountMinor: number, currency = 'MYR') {
  return formatCurrencyMinorUnits(currency || 'MYR', amountMinor)
}

function moneyList(values: Array<{ currency: string; amount_minor: number }>) {
  const visible = values.filter((item) => item.amount_minor !== 0)
  if (!visible.length) return money(0)
  return visible.map((item) => money(item.amount_minor, item.currency)).join(' + ')
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

function timeOnly(value?: string) {
  if (!value) return ''
  return new Intl.DateTimeFormat('en-MY', { hour: '2-digit', minute: '2-digit' }).format(new Date(value))
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

function channelLabel(channel?: string) {
  const key = (channel ?? '').trim().toLowerCase()
  if (!key) return 'Channel'
  return CHANNEL_LABELS[key] ?? key.charAt(0).toUpperCase() + key.slice(1)
}

function identityExtra(identity: CustomerIdentity) {
  const displayName = identity.display_name?.trim()
  if (!displayName) return ''
  return displayName.toLowerCase() === contactName.value.trim().toLowerCase() ? '' : displayName
}

function identityTitle(identity: CustomerIdentity) {
  return [channelLabel(identity.channel), identity.display_name || identity.address || identity.normalized_address]
    .filter(Boolean)
    .join(' - ')
}

function leadBadgeLabel(lead: CRMLead) {
  if (lead.status === 'won') return 'Won'
  if (lead.status === 'lost') return 'Lost'
  return lead.stage?.name || 'Open'
}

function leadBadgeClass(lead: CRMLead) {
  if (lead.status === 'won') return 'border-emerald-400/30 text-emerald-200 light:text-emerald-800'
  if (lead.status === 'lost') return 'border-rose-400/25 text-rose-200 light:text-rose-700'
  return ''
}

function pipelineLink(lead: CRMLead) {
  return { path: '/crm/pipeline', query: { pipeline: lead.pipeline_id, lead: lead.id } }
}

function invoicePaid(invoice: CommerceInvoice) {
  return invoice.due_minor <= 0 || invoice.status === 'paid'
}

function stageStepState(index: number) {
  const current = focusStageIndex.value
  if (index === current) return 'current'
  if (current >= 0 && index < current) return 'done'
  return 'upcoming'
}

watch(
  () => props.contactId,
  () => {
    copilotSequence += 1
    invalidateWorkspaceMutationDialogs()
    pipelinesError.value = ''
    activeTab.value = 'overview'
    copilotRunning.value = null
    copilotRun.value = null
    copilotResult.value = ''
    void loadWorkspace()
  },
)

// The stage stepper and the Won/Lost/Reopen actions need the lead's pipeline,
// so an open or lost focus lead fetches GET /api/crm/pipelines automatically -
// only for users who can read pipelines, and at most once per workspace load
// (a failure shows a Try again button instead of refetching).
// NOTE: e2e specs that seed journeys in the workspace response must also mock
// GET /api/crm/pipelines, or this request reaches the real API.
watch(
  () => [focusLead.value?.id, flowState.value, canReadPipelines.value] as const,
  ([leadId, state, canRead]) => {
    if (!leadId || !canRead || (state !== 'open' && state !== 'lost')) return
    if (autoPipelinesLoadId === workspaceLoadId) return
    autoPipelinesLoadId = workspaceLoadId
    void ensurePipelines()
  },
  { immediate: true },
)

watch(
  () => [props.requestedAction?.nonce, loading.value, workspace.value, workspaceSelectionContactId.value] as const,
  () => handleRequestedAction(),
  { immediate: true },
)

watch(canUseCopilot, (allowed) => {
  if (!allowed && activeTab.value === 'copilot') activeTab.value = 'overview'
})

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
              class="border-rose-400/25 px-1.5 text-[10px] text-rose-300 light:text-rose-700"
            >
              Opted out
            </Badge>
          </div>
          <p
            v-if="contactAddress.kind === 'phone'"
            class="mt-0.5 truncate text-xs text-white/50 light:text-gray-600"
            data-testid="workspace-contact-phone"
          >
            {{ contactAddress.text }}
          </p>
          <Badge
            v-else
            variant="secondary"
            class="mt-1 max-w-full px-1.5 text-[10px] font-normal text-white/55 light:text-gray-600"
            data-testid="workspace-contact-address"
            :title="contactAddress.hint || undefined"
          >
            <span class="truncate">{{ contactAddress.text }}</span>
          </Badge>
          <p
            v-if="contactAddress.kind !== 'phone' && contactAddress.hint"
            class="mt-1 text-[11px] leading-4 text-white/45 light:text-gray-500"
            data-testid="workspace-contact-address-hint"
          >
            {{ contactAddress.hint }}
          </p>
          <div v-if="identities.length" class="mt-2 flex flex-wrap gap-1">
            <Badge
              v-for="identity in identities.slice(0, 4)"
              :key="identity.id"
              variant="secondary"
              class="max-w-full gap-1 px-1.5 text-[10px] font-normal"
              :title="identityTitle(identity)"
            >
              <CheckCircle2
                v-if="identity.verified ?? identity.is_verified"
                class="h-2.5 w-2.5 text-emerald-400"
                aria-label="Verified"
              />
              {{ channelLabel(identity.channel) }}
              <span v-if="identityExtra(identity)" class="max-w-28 truncate opacity-65">{{ identityExtra(identity) }}</span>
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
      <p class="mt-3 text-sm font-medium">Loading customer details</p>
      <p class="mt-1 text-xs text-white/40 light:text-gray-500">Leads, follow-ups, appointments and invoices.</p>
    </div>

    <div v-else-if="error" class="flex flex-1 flex-col items-center justify-center px-6 text-center" role="alert">
      <AlertCircle class="h-7 w-7 text-rose-300" />
      <h3 class="mt-3 text-sm font-semibold">Customer details are unavailable</h3>
      <p class="mt-1 max-w-xs text-xs leading-5 text-white/40 light:text-gray-500">{{ error }}</p>
      <Button variant="outline" class="mt-4 h-11" @click="loadWorkspace()">
        <RefreshCw class="mr-2 h-4 w-4" />
        Try again
      </Button>
    </div>

    <Tabs v-else v-model="activeTab" class="flex min-h-0 flex-1 flex-col">
      <div class="shrink-0 border-b border-white/[0.07] px-3 py-2 light:border-gray-100">
        <TabsList
          class="grid h-9 w-full bg-white/[0.035] light:bg-gray-100"
          :class="canUseCopilot ? 'grid-cols-4' : 'grid-cols-3'"
        >
          <TabsTrigger value="overview" class="text-xs">Overview</TabsTrigger>
          <TabsTrigger value="timeline" class="text-xs">Timeline</TabsTrigger>
          <TabsTrigger value="details" class="text-xs">Details</TabsTrigger>
          <TabsTrigger v-if="canUseCopilot" value="copilot" class="text-xs">Copilot</TabsTrigger>
        </TabsList>
      </div>

      <TabsContent value="overview" class="mt-0 min-h-0 flex-1">
        <ScrollArea class="h-full">
          <div class="space-y-5 p-3.5">
            <!-- Next step: one guided card driven by the customer's most relevant lead -->
            <p v-if="!canViewCRM" class="text-xs text-white/45 light:text-gray-500">
              <LockKeyhole class="mr-1.5 inline h-3.5 w-3.5" />
              Leads are hidden by your permissions.
            </p>
            <section
              v-else
              class="rounded-xl border p-3.5"
              :class="{
                'border-cyan-300/20 bg-cyan-300/[0.04] light:border-cyan-200 light:bg-cyan-50/60': flowState === 'none' || flowState === 'open',
                'border-emerald-300/20 bg-emerald-300/[0.04] light:border-emerald-200 light:bg-emerald-50/60': flowState === 'won',
                'border-white/[0.08] bg-white/[0.02] light:border-gray-200 light:bg-gray-50': flowState === 'lost',
              }"
              aria-labelledby="workspace-next-step-title"
              data-testid="lead-next-step"
              :data-state="flowState"
            >
              <p class="text-[11px] font-semibold uppercase tracking-wider text-white/45 light:text-gray-500">Next step</p>

              <!-- No lead yet -->
              <template v-if="flowState === 'none' || !focusLead">
                <h3 id="workspace-next-step-title" class="mt-1 text-sm font-semibold">Not in the pipeline yet</h3>
                <p class="mt-1 text-xs leading-5 text-white/55 light:text-gray-600">
                  Add this customer to a pipeline so the team can follow up until the lead is won.
                </p>
                <Button
                  v-if="canCreateJourney"
                  class="mt-3 h-11 w-full"
                  data-testid="add-to-pipeline"
                  @click="openJourney()"
                >
                  <Plus class="h-4 w-4" />
                  Add to pipeline
                </Button>
                <p v-else class="mt-3 text-xs text-white/40 light:text-gray-500">You do not have access to add leads.</p>
              </template>

              <!-- Open lead -->
              <template v-else-if="flowState === 'open'">
                <div class="mt-1 flex items-start justify-between gap-3">
                  <div class="min-w-0">
                    <h3 id="workspace-next-step-title" class="truncate text-sm font-semibold">{{ focusLead.title }}</h3>
                    <p class="mt-0.5 truncate text-xs text-white/50 light:text-gray-600">
                      {{ focusLead.pipeline?.name || focusPipeline?.name || 'Pipeline' }}
                    </p>
                  </div>
                  <span
                    v-if="focusLead.value_minor > 0"
                    class="shrink-0 text-xs font-semibold text-emerald-200 light:text-emerald-800"
                  >
                    {{ money(focusLead.value_minor, focusLead.currency) }}
                  </span>
                </div>

                <div class="mt-3">
                  <p id="workspace-stage-label" class="mb-1.5 text-[11px] font-medium text-white/50 light:text-gray-600">Stage</p>
                  <ol
                    v-if="focusStages.length"
                    class="flex flex-wrap gap-1.5"
                    aria-labelledby="workspace-stage-label"
                    data-testid="lead-stage-stepper"
                  >
                    <li v-for="(stage, index) in focusStages" :key="stage.id">
                      <button
                        type="button"
                        data-testid="lead-stage-step"
                        class="inline-flex min-h-9 items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 disabled:cursor-not-allowed"
                        :class="{
                          'border-cyan-300/60 bg-cyan-300/[0.14] text-cyan-100 light:border-cyan-500 light:bg-cyan-100 light:text-cyan-900': stageStepState(index) === 'current',
                          'border-white/10 bg-white/[0.03] text-white/60 hover:border-cyan-300/30 hover:text-white light:border-gray-200 light:bg-white light:text-gray-600 light:hover:text-gray-900': stageStepState(index) !== 'current',
                          'opacity-60': Boolean(movingLeadId) && movingStageId !== stage.id,
                        }"
                        :aria-current="stageStepState(index) === 'current' ? 'step' : undefined"
                        :aria-label="stageStepState(index) === 'current' ? `${stage.name} (current stage)` : `Move to ${stage.name}`"
                        :disabled="!canMoveLead || Boolean(movingLeadId) || stageStepState(index) === 'current'"
                        @click="moveFocusLead(stage)"
                      >
                        <Loader2 v-if="movingStageId === stage.id" class="h-3 w-3 animate-spin" />
                        <Check v-else-if="stageStepState(index) === 'done'" class="h-3 w-3 text-cyan-300 light:text-cyan-700" />
                        {{ stage.name }}
                      </button>
                    </li>
                  </ol>
                  <div v-else class="flex items-center gap-2">
                    <Badge variant="outline" class="h-6 gap-1 px-2 text-[11px]">
                      <span class="h-1.5 w-1.5 rounded-full" :style="{ backgroundColor: focusLead.stage?.color || '#67e8f9' }" />
                      {{ focusLead.stage?.name || 'Open' }}
                    </Badge>
                    <Loader2 v-if="pipelinesLoading" class="h-3.5 w-3.5 animate-spin text-white/40" />
                  </div>
                </div>

                <template v-if="canMoveLead && focusPipeline">
                  <div class="mt-3 grid grid-cols-2 gap-2">
                    <Button
                      class="h-10 bg-none bg-emerald-500 text-black shadow-none hover:bg-emerald-400"
                      data-testid="lead-mark-won"
                      :disabled="Boolean(movingLeadId) || !focusWonStage"
                      :aria-describedby="outcomeUnavailableReason ? 'workspace-outcome-unavailable' : undefined"
                      @click="openOutcome('won')"
                    >
                      <Trophy class="h-4 w-4" />
                      Won
                    </Button>
                    <Button
                      variant="outline"
                      class="h-10"
                      data-testid="lead-mark-lost"
                      :disabled="Boolean(movingLeadId) || !focusLostStage"
                      :aria-describedby="outcomeUnavailableReason ? 'workspace-outcome-unavailable' : undefined"
                      @click="openOutcome('lost')"
                    >
                      <ThumbsDown class="h-4 w-4" />
                      Lost
                    </Button>
                  </div>
                  <p
                    v-if="outcomeUnavailableReason"
                    id="workspace-outcome-unavailable"
                    class="mt-1.5 text-[11px] leading-4 text-white/45 light:text-gray-500"
                    data-testid="lead-outcome-unavailable"
                  >
                    {{ outcomeUnavailableReason }}
                  </p>
                </template>
                <p
                  v-else-if="focusMoveUnavailable"
                  class="mt-3 text-[11px] leading-4 text-white/45 light:text-gray-500"
                  data-testid="lead-move-unavailable"
                >
                  Open the pipeline to move this lead.
                </p>

                <div
                  class="mt-3 flex items-center justify-between gap-2 rounded-lg px-2.5 py-2 text-xs"
                  :class="{
                    'bg-rose-400/[0.08] text-rose-200 light:bg-rose-50 light:text-rose-800': focusFollowUpUrgency === 'overdue',
                    'bg-amber-300/[0.08] text-amber-100 light:bg-amber-50 light:text-amber-900': focusFollowUpUrgency === 'today' || focusFollowUpUrgency === 'none',
                    'bg-white/[0.03] text-white/65 light:bg-white light:text-gray-700': focusFollowUpUrgency === 'upcoming',
                  }"
                  data-testid="lead-follow-up"
                  :data-urgency="focusFollowUpUrgency"
                >
                  <span class="flex min-w-0 items-center gap-1.5">
                    <Clock3 class="h-3.5 w-3.5 shrink-0" />
                    <span class="truncate">
                      <template v-if="focusFollowUpUrgency === 'overdue'">Follow-up overdue · {{ dateTime(focusFollowUp?.dueAt) }}</template>
                      <template v-else-if="focusFollowUpUrgency === 'today'">Follow up today · {{ timeOnly(focusFollowUp?.dueAt) }}</template>
                      <template v-else-if="focusFollowUpUrgency === 'upcoming'">Next follow-up: {{ dateTime(focusFollowUp?.dueAt) }}</template>
                      <template v-else-if="focusFollowUp?.task">Follow-up has no date: {{ focusFollowUp.task.title }}</template>
                      <template v-else>No follow-up scheduled</template>
                    </span>
                  </span>
                  <div
                    v-if="canCreateTask && (focusFollowUpUrgency === 'none' || focusFollowUpUrgency === 'overdue')"
                    class="flex shrink-0 items-center gap-1.5"
                  >
                    <Button
                      v-if="canFinishFocusFollowUp && focusFollowUpTask"
                      variant="outline"
                      size="xs"
                      class="text-[11px]"
                      data-testid="lead-follow-up-done"
                      :aria-label="`Done: ${focusFollowUpTask.title}`"
                      :disabled="Boolean(completingTaskId)"
                      :loading="completingTaskId === focusFollowUpTask.id"
                      @click="completeTask(focusFollowUpTask)"
                    >
                      <Check class="h-3 w-3" />
                      Done
                    </Button>
                    <Button
                      variant="outline"
                      size="xs"
                      class="text-[11px]"
                      data-testid="lead-set-follow-up"
                      @click="openFollowUp(focusLead.id)"
                    >
                      {{ canFinishFocusFollowUp ? 'Set a new date' : 'Set follow-up' }}
                    </Button>
                  </div>
                </div>

                <p v-if="moreOpenLeads" class="mt-2 text-[11px] text-white/45 light:text-gray-500">
                  +{{ moreOpenLeads }} more open {{ moreOpenLeads === 1 ? 'lead' : 'leads' }}
                </p>
              </template>

              <!-- Won lead -->
              <template v-else-if="flowState === 'won'">
                <h3 id="workspace-next-step-title" class="mt-1 flex items-center gap-1.5 text-sm font-semibold text-emerald-200 light:text-emerald-800">
                  <CheckCircle2 class="h-4 w-4" />
                  Won
                </h3>
                <p class="mt-1 truncate text-xs text-white/65 light:text-gray-700">
                  {{ focusLead.title }}<template v-if="focusLead.value_minor > 0"> · {{ money(focusLead.value_minor, focusLead.currency) }}</template>
                </p>
                <div
                  v-if="canViewTasks && focusLeadOpenTasks.length"
                  class="mt-2 flex items-center justify-between gap-2 text-[11px] text-white/50 light:text-gray-600"
                  data-testid="lead-open-follow-ups"
                >
                  <span>
                    {{ focusLeadOpenTasks.length }} open {{ focusLeadOpenTasks.length === 1 ? 'follow-up' : 'follow-ups' }} for this lead
                  </span>
                  <Button
                    v-if="canCreateTask"
                    variant="ghost"
                    size="xs"
                    class="shrink-0 text-[11px]"
                    data-testid="lead-open-follow-ups-done"
                    :disabled="Boolean(completingTaskId)"
                    :loading="completingTaskId === BATCH_COMPLETE_ID || focusLeadOpenTasks.some((task) => task.id === completingTaskId)"
                    @click="completeTasks([...focusLeadOpenTasks])"
                  >
                    Mark done
                  </Button>
                </div>
                <div
                  v-if="focusInvoice"
                  class="mt-3 rounded-lg border border-white/[0.08] bg-black/10 px-3 py-2.5 text-xs light:border-gray-200 light:bg-white"
                  data-testid="lead-invoice"
                >
                  <p class="font-medium">
                    Invoice {{ focusInvoice.invoice_number }} · {{ money(focusInvoice.total_minor, focusInvoice.currency) }}
                  </p>
                  <p
                    class="mt-0.5"
                    :class="invoicePaid(focusInvoice) ? 'text-emerald-200 light:text-emerald-800' : 'text-amber-200 light:text-amber-800'"
                  >
                    <template v-if="invoicePaid(focusInvoice)">Paid</template>
                    <template v-else>
                      Unpaid · {{ money(focusInvoice.due_minor, focusInvoice.currency) }} due<template v-if="focusInvoice.due_at"> {{ shortDate(focusInvoice.due_at) }}</template>
                    </template>
                  </p>
                  <RouterLink
                    to="/commerce?tab=invoices"
                    class="mt-1.5 inline-flex items-center gap-1 text-[11px] font-medium text-cyan-200 hover:underline light:text-cyan-800"
                  >
                    Open invoices
                    <ArrowUpRight class="h-3 w-3" />
                  </RouterLink>
                </div>
                <template v-else>
                  <Button
                    v-if="canInvoice"
                    class="mt-3 h-11 w-full"
                    data-testid="create-invoice"
                    @click="openInvoice(focusLead)"
                  >
                    <Receipt class="h-4 w-4" />
                    Create invoice
                  </Button>
                  <p v-else class="mt-3 text-xs text-white/45 light:text-gray-500">
                    Ask a manager with billing access to create the invoice.
                  </p>
                </template>
                <Button
                  v-if="canCreateJourney"
                  variant="ghost"
                  size="sm"
                  class="mt-2 w-full"
                  @click="openJourney()"
                >
                  <Plus class="h-3.5 w-3.5" />
                  New lead
                </Button>
              </template>

              <!-- Lost lead -->
              <template v-else>
                <h3 id="workspace-next-step-title" class="mt-1 text-sm font-semibold">Lost</h3>
                <p class="mt-1 truncate text-xs text-white/65 light:text-gray-700">{{ focusLead.title }}</p>
                <p v-if="focusLead.lost_reason" class="mt-1 text-xs leading-5 text-white/45 light:text-gray-500">
                  Reason: {{ focusLead.lost_reason }}
                </p>
                <p
                  v-if="focusMoveUnavailable"
                  class="mt-2 text-[11px] leading-4 text-white/45 light:text-gray-500"
                  data-testid="lead-move-unavailable"
                >
                  Open the pipeline to move this lead.
                </p>
                <p
                  v-else-if="canMoveLead && focusPipeline && !focusReopenStage"
                  class="mt-2 text-[11px] leading-4 text-white/45 light:text-gray-500"
                  data-testid="lead-reopen-unavailable"
                >
                  This pipeline has no open stage. Ask an admin to add one.
                </p>
                <div class="mt-3 grid grid-cols-2 gap-2">
                  <Button
                    v-if="canMoveLead && focusReopenStage"
                    variant="outline"
                    class="h-10"
                    data-testid="lead-reopen"
                    :loading="Boolean(movingLeadId)"
                    @click="reopenFocusLead"
                  >
                    <RotateCcw class="h-4 w-4" />
                    Reopen lead
                  </Button>
                  <Button
                    v-if="canCreateJourney"
                    variant="ghost"
                    class="h-10"
                    @click="openJourney()"
                  >
                    <Plus class="h-4 w-4" />
                    New lead
                  </Button>
                </div>
              </template>

              <div
                v-if="pipelinesError && !pipelinesLoading"
                class="mt-3 flex items-center justify-between gap-2 rounded-lg border border-rose-400/20 bg-rose-400/[0.06] px-2.5 py-2 text-xs text-rose-200 light:border-rose-200 light:bg-rose-50 light:text-rose-800"
                role="alert"
                data-testid="workspace-pipelines-error"
              >
                <span class="flex min-w-0 items-center gap-1.5">
                  <AlertCircle class="h-3.5 w-3.5 shrink-0" />
                  Pipelines could not be loaded.
                </span>
                <Button
                  variant="outline"
                  size="xs"
                  class="shrink-0 text-[11px]"
                  data-testid="workspace-pipelines-retry"
                  @click="retryPipelines"
                >
                  <RefreshCw class="h-3 w-3" />
                  Try again
                </Button>
              </div>

              <RouterLink
                v-if="focusLead && flowState !== 'none'"
                :to="pipelineLink(focusLead)"
                class="mt-2 inline-flex items-center gap-1 rounded-md py-1 text-[11px] font-medium text-cyan-200 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-800"
                data-testid="lead-view-in-pipeline"
              >
                View in pipeline
                <ArrowUpRight class="h-3 w-3" />
              </RouterLink>
            </section>

            <p v-if="summaryLine" class="text-xs text-white/45 light:text-gray-500" data-testid="workspace-money-summary">
              {{ summaryLine }}
            </p>

            <!-- Leads -->
            <section v-if="canViewCRM && otherLeads.length" aria-labelledby="workspace-leads-title">
              <h3 id="workspace-leads-title" class="mb-2 flex items-center gap-2 text-xs font-semibold">
                <Route class="h-3.5 w-3.5 text-cyan-300" />
                Other leads
              </h3>
              <ul class="space-y-1.5">
                <li
                  v-for="lead in otherLeads.slice(0, 3)"
                  :key="lead.id"
                  class="flex items-center justify-between gap-2 rounded-lg border border-white/[0.07] px-3 py-2 light:border-gray-200"
                  data-testid="workspace-lead"
                >
                  <div class="min-w-0">
                    <p class="truncate text-xs font-medium">{{ lead.title }}</p>
                    <div class="mt-1 flex flex-wrap items-center gap-1.5">
                      <Badge variant="outline" class="h-5 px-1.5 text-[10px]" :class="leadBadgeClass(lead)">{{ leadBadgeLabel(lead) }}</Badge>
                      <span v-if="lead.pipeline?.name" class="truncate text-[11px] text-white/40 light:text-gray-500">{{ lead.pipeline.name }}</span>
                    </div>
                  </div>
                  <RouterLink
                    :to="pipelineLink(lead)"
                    class="shrink-0 rounded-md px-1.5 py-1 text-[11px] font-medium text-cyan-200 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-800"
                  >
                    View in pipeline
                  </RouterLink>
                </li>
              </ul>
              <p v-if="otherLeads.length > 3" class="mt-1.5 text-[11px] text-white/40 light:text-gray-500">
                +{{ otherLeads.length - 3 }} more
              </p>
            </section>

            <!-- Follow-ups -->
            <section v-if="canViewTasks" aria-labelledby="workspace-tasks-title">
              <div class="mb-2 flex items-center justify-between gap-2">
                <h3 id="workspace-tasks-title" class="flex items-center gap-2 text-xs font-semibold">
                  <ListChecks class="h-3.5 w-3.5 text-violet-300" />
                  Follow-ups
                </h3>
                <Button
                  v-if="canCreateTask"
                  variant="ghost"
                  size="xs"
                  class="text-[11px]"
                  data-testid="workspace-set-follow-up"
                  @click="openFollowUp()"
                >
                  <Plus class="h-3 w-3" />
                  Set follow-up
                </Button>
              </div>
              <ul v-if="openTasks.length" class="space-y-1.5">
                <li
                  v-for="task in openTasks.slice(0, 3)"
                  :key="task.id"
                  class="flex items-center gap-2 rounded-lg border border-white/[0.07] px-2 py-1.5 light:border-gray-200"
                  data-testid="workspace-task"
                >
                  <button
                    v-if="canCreateTask"
                    type="button"
                    class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full text-white/40 hover:bg-emerald-400/10 hover:text-emerald-300 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-300 disabled:opacity-50 light:text-gray-400 light:hover:text-emerald-700"
                    :aria-label="`Mark follow-up done: ${task.title}`"
                    :disabled="Boolean(completingTaskId)"
                    data-testid="workspace-task-complete"
                    @click="completeTask(task)"
                  >
                    <Loader2 v-if="completingTaskId === task.id" class="h-4 w-4 animate-spin" />
                    <span v-else class="flex h-4 w-4 items-center justify-center rounded-full border border-current">
                      <Check class="h-2.5 w-2.5" />
                    </span>
                  </button>
                  <div class="min-w-0 flex-1 py-0.5" :class="canCreateTask ? '' : 'pl-1'">
                    <p class="truncate text-xs font-medium">{{ task.title }}</p>
                    <p
                      class="mt-0.5 text-[11px]"
                      :class="followUpUrgency(task.due_at) === 'overdue' ? 'text-rose-300 light:text-rose-700' : 'text-white/40 light:text-gray-500'"
                    >
                      {{ task.due_at ? dateTime(task.due_at) : 'No date' }}<template v-if="followUpUrgency(task.due_at) === 'overdue'"> · overdue</template>
                    </p>
                  </div>
                </li>
              </ul>
              <p v-else class="text-xs text-white/40 light:text-gray-500">No open follow-ups.</p>
            </section>

            <!-- Appointments -->
            <section v-if="canViewBookings" aria-labelledby="workspace-bookings-title">
              <div class="mb-2 flex items-center justify-between gap-2">
                <h3 id="workspace-bookings-title" class="flex items-center gap-2 text-xs font-semibold">
                  <CalendarClock class="h-3.5 w-3.5 text-fuchsia-300" />
                  Appointments
                </h3>
                <RouterLink
                  to="/calendar"
                  class="rounded-md px-1.5 py-1 text-[11px] font-medium text-white/45 hover:text-fuchsia-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-fuchsia-300 light:text-gray-500"
                >
                  Calendar
                </RouterLink>
              </div>
              <div class="space-y-2">
                <Button
                  v-if="canCreateBooking"
                  type="button"
                  variant="outline"
                  class="h-10 w-full justify-center border-fuchsia-300/20 text-fuchsia-100 hover:bg-fuchsia-300/[0.08] light:text-fuchsia-800"
                  @click="openBooking"
                >
                  <Plus class="mr-2 h-4 w-4" />
                  Book appointment
                </Button>
                <article v-for="booking in upcomingBookings" :key="booking.id" class="rounded-lg border border-fuchsia-300/12 bg-fuchsia-300/[0.03] px-3 py-2">
                  <div class="flex items-start justify-between gap-2">
                    <div class="min-w-0">
                      <p class="truncate text-xs font-medium">{{ bookingName(booking) }}</p>
                      <p class="mt-0.5 text-[11px] text-white/45 light:text-gray-500">
                        {{ dateTime(booking.event?.starts_at) }}<template v-if="booking.event?.resource?.name"> · {{ booking.event.resource.name }}</template>
                      </p>
                    </div>
                    <Badge variant="outline" class="shrink-0 capitalize text-[10px]">{{ booking.status.replace('_', ' ') }}</Badge>
                  </div>
                </article>
                <p v-if="!upcomingBookings.length" class="text-xs text-white/40 light:text-gray-500">No upcoming appointments.</p>
                <details v-if="recentBookings.length" class="rounded-lg border border-white/[0.06] px-3 py-2 light:border-gray-200">
                  <summary class="cursor-pointer text-[11px] font-medium text-white/50 light:text-gray-600">Past appointments</summary>
                  <div class="mt-2 space-y-2">
                    <div v-for="booking in recentBookings" :key="booking.id" class="flex items-center justify-between gap-2 text-[11px]">
                      <span class="truncate">{{ bookingName(booking) }} · {{ shortDate(booking.event?.starts_at) }}</span>
                      <Badge variant="secondary" class="capitalize text-[10px]">{{ booking.status.replace('_', ' ') }}</Badge>
                    </div>
                  </div>
                </details>
              </div>
            </section>

            <!-- Invoices & packages (only when there is something to show) -->
            <section v-if="showCommerceSection" aria-labelledby="workspace-revenue-title" data-testid="workspace-invoices">
              <div class="mb-2 flex items-center justify-between gap-2">
                <h3 id="workspace-revenue-title" class="flex items-center gap-2 text-xs font-semibold">
                  <Receipt class="h-3.5 w-3.5 text-emerald-300" />
                  Invoices &amp; packages
                </h3>
                <RouterLink
                  to="/commerce?tab=invoices"
                  class="rounded-md px-1.5 py-1 text-[11px] font-medium text-white/45 hover:text-emerald-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-300 light:text-gray-500"
                >
                  Open invoices
                </RouterLink>
              </div>
              <div class="space-y-1.5">
                <article v-for="item in activePackages" :key="item.id" class="flex items-start justify-between gap-2 rounded-lg border border-emerald-300/12 bg-emerald-300/[0.03] px-3 py-2">
                  <div class="min-w-0">
                    <p class="truncate text-xs font-medium">{{ item.package_definition?.name || 'Customer package' }}</p>
                    <p class="mt-0.5 text-[11px] text-emerald-200/70 light:text-emerald-800">{{ packageCredits(item) }}</p>
                  </div>
                  <span class="shrink-0 text-[11px] text-white/40 light:text-gray-500">Expires {{ shortDate(item.expires_at) }}</span>
                </article>
                <article v-for="invoice in visibleInvoices" :key="invoice.id" class="flex items-center justify-between gap-3 rounded-lg border border-white/[0.07] px-3 py-2 light:border-gray-200">
                  <div class="min-w-0">
                    <p class="truncate text-xs font-medium">{{ invoice.invoice_number }}</p>
                    <p class="mt-0.5 text-[11px] text-white/40 light:text-gray-500">{{ shortDate(invoice.due_at || invoice.issued_at) }}</p>
                  </div>
                  <div class="text-right">
                    <p class="text-xs font-semibold">{{ money(invoice.total_minor, invoice.currency) }}</p>
                    <p v-if="invoice.due_minor > 0" class="mt-0.5 text-[10px] text-amber-200 light:text-amber-800">{{ money(invoice.due_minor, invoice.currency) }} unpaid</p>
                    <p v-else class="mt-0.5 text-[10px] text-emerald-200 light:text-emerald-800">Paid</p>
                  </div>
                </article>
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
                <p class="mt-1 text-[11px] text-white/40 light:text-gray-500">Messages, leads, appointments and payments in one place.</p>
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
                    <time class="shrink-0 text-[10px] text-white/35 light:text-gray-400" :datetime="event.occurred_at">{{ dateTime(event.occurred_at) }}</time>
                  </div>
                  <p v-if="event.summary" class="mt-1 text-[11px] leading-5 text-white/45 light:text-gray-500">{{ event.summary }}</p>
                  <p v-if="event.actor?.name" class="mt-1 text-[10px] text-white/30 light:text-gray-400">{{ event.actor.name }}</p>
                </div>
              </li>
            </ol>
            <p v-else class="py-6 text-center text-xs text-white/40 light:text-gray-500">
              No activity yet. New messages, leads and appointments will appear here.
            </p>
          </div>
        </ScrollArea>
      </TabsContent>

      <TabsContent value="details" class="mt-0 min-h-0 flex-1">
        <ContactInfoPanel
          :contact="detailContact"
          :session-data="sessionData"
          :channel="channel"
          embedded
          @tags-updated="emit('tagsUpdated', $event)"
        />
      </TabsContent>

      <TabsContent v-if="canUseCopilot" value="copilot" class="mt-0 min-h-0 flex-1">
        <ScrollArea class="h-full">
          <div class="space-y-4 p-4">
            <div class="rounded-xl border border-emerald-300/15 bg-emerald-300/[0.035] p-3">
              <div class="flex gap-2.5">
                <Sparkles class="mt-0.5 h-4 w-4 shrink-0 text-emerald-200" />
                <div>
                  <h3 class="text-xs font-semibold">Human-reviewed Copilot</h3>
                  <p class="mt-1 text-[11px] leading-4 text-white/45 light:text-gray-600">Copilot can review this conversation, but it cannot send a message or update leads.</p>
                </div>
              </div>
            </div>
            <div class="grid grid-cols-3 gap-2">
              <Button class="h-auto min-h-16 flex-col gap-1.5 px-2 py-2 text-[11px]" variant="outline" :disabled="Boolean(copilotRunning)" @click="runCopilot('summary')">
                <Loader2 v-if="copilotRunning === 'summary'" class="h-4 w-4 animate-spin" />
                <FileSearch v-else class="h-4 w-4 text-emerald-200" />
                Summary
              </Button>
              <Button class="h-auto min-h-16 flex-col gap-1.5 px-2 py-2 text-[11px]" variant="outline" :disabled="Boolean(copilotRunning)" @click="runCopilot('qualify')">
                <Loader2 v-if="copilotRunning === 'qualify'" class="h-4 w-4 animate-spin" />
                <Wand2 v-else class="h-4 w-4 text-cyan-200" />
                Qualify
              </Button>
              <Button class="h-auto min-h-16 flex-col gap-1.5 px-2 py-2 text-[11px]" variant="outline" :disabled="Boolean(copilotRunning)" @click="runCopilot('extract_actions')">
                <Loader2 v-if="copilotRunning === 'extract_actions'" class="h-4 w-4 animate-spin" />
                <ListChecks v-else class="h-4 w-4 text-violet-200" />
                Next actions
              </Button>
            </div>
            <div v-if="copilotRun" class="rounded-xl border border-white/[0.08] bg-white/[0.02] p-3 light:border-gray-200 light:bg-gray-50" aria-live="polite">
              <div class="flex items-center justify-between gap-3">
                <div>
                  <Badge variant="outline" class="text-[10px]">{{ copilotLabel(copilotRun.task_type as CopilotAction) }}</Badge>
                  <p class="mt-1 text-[10px] text-white/35 light:text-gray-400">AI-assisted · review before use</p>
                </div>
                <Button variant="ghost" size="icon" class="h-11 w-11" aria-label="Copy Copilot result" @click="copyCopilotResult">
                  <Clipboard class="h-4 w-4" />
                </Button>
              </div>
              <pre class="mt-3 whitespace-pre-wrap break-words font-sans text-xs leading-5 text-white/70 light:text-gray-700">{{ copilotResult }}</pre>
              <div v-if="copilotRun.safety_warnings?.length" class="mt-3 rounded-lg border border-amber-300/15 bg-amber-300/[0.04] p-2.5">
                <p class="text-[10px] font-semibold uppercase tracking-wider text-amber-200 light:text-amber-800">Review warnings</p>
                <p v-for="warning in copilotRun.safety_warnings" :key="warning" class="mt-1 text-[11px] text-amber-100/60 light:text-amber-900">{{ warning }}</p>
              </div>
            </div>
          </div>
        </ScrollArea>
      </TabsContent>
    </Tabs>

    <LeadCreateDialog
      v-model:open="showJourneyDialog"
      :saving="savingJourney"
      :contact="leadDialogContact"
      :pipelines="pipelines"
      :pipelines-loading="pipelinesLoading"
      :default-pipeline-id="leadDefaultPipelineId"
      :can-schedule-follow-up="canCreateTask"
      :existing-open-leads="openJourneys"
      @submit="submitLeadDraft"
    />

    <Dialog v-model:open="showTaskDialog">
      <DialogContent class="max-w-md">
        <DialogHeader>
          <DialogTitle>Set follow-up</DialogTitle>
          <DialogDescription>Remind the team to get back to {{ contactName }}.</DialogDescription>
        </DialogHeader>
        <form id="workspace-task-form" class="space-y-4" @submit.prevent="createFollowUp">
          <div class="space-y-1.5">
            <Label for="workspace-task-title">What needs to happen?</Label>
            <Input id="workspace-task-title" v-model="taskDraft.title" name="task_title" required maxlength="255" />
          </div>
          <div class="space-y-1.5">
            <Label for="workspace-task-due">When</Label>
            <div class="flex flex-wrap gap-2" role="group" aria-label="Quick dates">
              <button
                v-for="preset in FOLLOW_UP_PRESETS"
                :key="preset.value"
                type="button"
                class="inline-flex min-h-9 items-center rounded-full border px-3 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300"
                :class="taskPresetActive(preset.value)
                  ? 'border-violet-300/60 bg-violet-300/[0.12] text-violet-100 light:border-violet-500 light:bg-violet-50 light:text-violet-900'
                  : 'border-white/10 bg-white/[0.03] text-white/70 hover:text-white light:border-gray-200 light:bg-white light:text-gray-700'"
                :aria-pressed="taskPresetActive(preset.value)"
                @click="applyTaskPreset(preset.value)"
              >
                {{ preset.label }}
              </button>
            </div>
            <Input id="workspace-task-due" v-model="taskDraft.due_at" name="task_due_at" type="datetime-local" />
          </div>
          <div v-if="openJourneys.length" class="space-y-1.5">
            <Label for="workspace-task-journey">Related lead</Label>
            <select id="workspace-task-journey" v-model="taskDraft.lead_id" name="task_lead_id" class="h-10 w-full rounded-lg border border-white/10 bg-[#111416] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900">
              <option value="">No lead</option>
              <option v-for="journey in openJourneys" :key="journey.id" :value="journey.id">{{ journey.title }}</option>
            </select>
          </div>
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
            <Label for="workspace-task-notes">Notes (optional)</Label>
            <Textarea id="workspace-task-notes" v-model="taskDraft.description" name="task_description" :rows="3" maxlength="5000" />
          </div>
        </form>
        <DialogFooter>
          <Button variant="outline" @click="showTaskDialog = false">Cancel</Button>
          <Button form="workspace-task-form" type="submit" class="bg-violet-400 text-black hover:bg-violet-300" :loading="savingTask" :disabled="!taskDraft.title.trim()">
            Save follow-up
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <LeadOutcomeDialog
      v-model:open="showOutcomeDialog"
      :outcome="outcome"
      :lead="outcomeLead"
      :stage-name="outcomeStage?.name"
      :saving="Boolean(movingLeadId)"
      :can-invoice="canInvoice"
      @confirm="confirmOutcome"
    />

    <InvoiceQuickDialog
      v-model:open="showInvoiceDialog"
      :contact="invoiceContact"
      :lead="invoiceLead"
      :can-sell-packages="canSellPackages"
      source="customer_workspace"
      @created="invoiceCreated"
    />

    <ContactBookingDialog
      v-model:open="showBookingDialog"
      :contact-id="canonicalContactId"
      :contact-name="contactName"
      :surface="props.surface"
      @booked="bookingCreated"
    />
  </section>
</template>
