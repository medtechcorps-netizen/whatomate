<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import draggable from 'vuedraggable'
import {
  Archive,
  ArrowRight,
  CalendarClock,
  Check,
  ChevronLeft,
  ChevronRight,
  CircleDollarSign,
  GripVertical,
  Loader2,
  MessageCircle,
  MoreHorizontal,
  Pencil,
  Plus,
  Receipt,
  RefreshCw,
  RotateCcw,
  Route,
  Settings2,
  UserRound,
  X,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import LeadEditDialog from '@/components/crm/LeadEditDialog.vue'
import LeadCreateDialog from '@/components/crm/LeadCreateDialog.vue'
import LeadOutcomeDialog from '@/components/crm/LeadOutcomeDialog.vue'
import InvoiceQuickDialog from '@/components/commerce/InvoiceQuickDialog.vue'
import PipelineSettingsDialog from '@/components/crm/PipelineSettingsDialog.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapListResponse } from '@/lib/api-utils'
import { contactDisplayName } from '@/lib/contactAddress'
import {
  activePipelines,
  buildCreateLeadPayload,
  buildFollowUpTaskPayload,
  defaultPipelineId,
  followUpPresetDate,
  followUpUrgency,
  isFollowUpOverdue,
  nextFollowUpForLead,
  rememberPipelineId,
  rememberedPipelineId,
  type FollowUpUrgency,
  type LeadDraft,
} from '@/lib/crmFlow'
import {
  crmService,
  type CommerceInvoice,
  type CRMLead,
  type FollowUpTask,
  type Pipeline,
  type PipelineStage,
} from '@/services/productSuite'

type BoardColumn = PipelineStage & { leads: CRMLead[] }
type OutcomeKind = 'won' | 'lost'
type FollowUpPreset = 'tomorrow' | '3days' | 'week' | 'custom'

const FOLLOW_UP_PRESETS: Array<{ value: FollowUpPreset; label: string }> = [
  { value: 'tomorrow', label: 'Tomorrow' },
  { value: '3days', label: 'In 3 days' },
  { value: 'week', label: 'Next week' },
  { value: 'custom', label: 'Pick a date' },
]

const toast = useAppToast()
const authStore = useAuthStore()
// The page also renders outside a router in unit tests; every query read is guarded.
const route = useRoute()

const loading = ref(true)
const pipelines = ref<Pipeline[]>([])
const selectedPipelineId = ref('')
const columns = ref<BoardColumn[]>([])
const tasks = ref<FollowUpTask[]>([])
const pipelineSettingsOpen = ref(false)
const leadEditorOpen = ref(false)
const editingLead = ref<CRMLead | null>(null)
const openLeadMenuId = ref('')
const movingLeadId = ref('')
const showArchived = ref(false)
const archiveOpen = ref(false)
const archiving = ref(false)
const archivingLead = ref<CRMLead | null>(null)
const archiveReason = ref('')
const archiveError = ref('')
const archiveIdempotencyKey = ref('')
let archiveReturnFocus: HTMLButtonElement | null = null
let initialised = false

// Follow-up sheet
const followUpsOpen = ref(false)
const showAllFollowUps = ref(false)
const completingTaskId = ref('')

// Lead highlight (from ?lead=, "Show lead" and newly added leads)
const highlightedLeadId = ref('')
let highlightTimer: ReturnType<typeof setTimeout> | null = null

// New lead
const leadCreateOpen = ref(false)
const leadCreateSaving = ref(false)
const leadCreateKey = ref('')

// Won / Lost
const outcomeOpen = ref(false)
const outcomeKind = ref<OutcomeKind>('won')
const outcomeLead = ref<CRMLead | null>(null)
const outcomeStage = ref<BoardColumn | null>(null)
const outcomeSaving = ref(false)
let outcomeFromDrag = false

// Invoice after a win
const invoiceOpen = ref(false)
const invoiceLead = ref<CRMLead | null>(null)
// Leads invoiced from this board in this visit; their card swaps "Create invoice" for a link.
const invoicedLeadIds = ref(new Set<string>())

// Quick follow-up
const followUpDialogOpen = ref(false)
const followUpLead = ref<CRMLead | null>(null)
const followUpPreset = ref<FollowUpPreset>('tomorrow')
const followUpCustomAt = ref('')
const followUpTaskTitle = ref('')
const followUpSaving = ref(false)
const followUpKey = ref('')
const followUpError = ref('')

const now = ref(new Date())
let clockTimer: ReturnType<typeof setInterval> | null = null

const canReadTasks = computed(() => authStore.hasPermission('tasks', 'read'))
const canWriteTasks = computed(() => authStore.hasPermission('tasks', 'write'))
const canWriteLeads = computed(() => authStore.hasPermission('crm.leads', 'write'))
const canReadLeads = computed(() => authStore.hasPermission('crm.leads', 'read'))
const canArchiveLeads = computed(() => authStore.hasPermission('crm.leads', 'delete'))
const canWritePipelines = computed(() => authStore.hasPermission('crm.pipelines', 'write'))
const canDeletePipelineStages = computed(() => authStore.hasPermission('crm.pipelines', 'delete'))
const canReadContacts = computed(() => authStore.hasPermission('contacts', 'read'))
const canCreateLeads = computed(() => canWriteLeads.value && canReadContacts.value)
const canOpenChat = computed(() => authStore.hasPermission('chat', 'read'))
const canInvoice = computed(
  () =>
    authStore.hasPermission('payments', 'write') &&
    authStore.hasPermission('contacts', 'read') &&
    authStore.hasProductEntitlement('commerce.enabled'),
)
const canSellPackages = computed(() => canInvoice.value && authStore.hasPermission('packages', 'write'))

const activePipelineList = computed(() => activePipelines(pipelines.value))
const selectedPipeline = computed(() => pipelines.value.find((pipeline) => pipeline.id === selectedPipelineId.value))
const pipelineChoices = computed(() => {
  const list = activePipelineList.value
  // Keep an inactive pipeline that is still on screen selectable.
  if (selectedPipeline.value && !list.some((pipeline) => pipeline.id === selectedPipeline.value?.id)) {
    return [...list, selectedPipeline.value]
  }
  return list
})

const boardLeads = computed(() => columns.value.flatMap((column) => column.leads))
const boardLeadById = computed(() => new Map(boardLeads.value.map((lead) => [lead.id, lead])))
const openColumns = computed(() => columns.value.filter((column) => column.kind === 'open'))
const wonColumn = computed(() => columns.value.find((column) => column.kind === 'won' && column.is_active !== false))
const lostColumn = computed(() => columns.value.find((column) => column.kind === 'lost' && column.is_active !== false))
const firstOpenColumn = computed(() => openColumns.value.find((column) => column.is_active !== false))

const openLeads = computed(() => boardLeads.value.filter((lead) => lead.status === 'open'))
const openValues = computed(() => currencyTotals(openLeads.value))
const visibleValues = computed(() => currencyTotals(boardLeads.value))
const wonThisMonth = computed(() => {
  const current = now.value
  return boardLeads.value.filter((lead) => {
    if (lead.status !== 'won' || !lead.won_at) return false
    const wonAt = new Date(lead.won_at)
    return wonAt.getFullYear() === current.getFullYear() && wonAt.getMonth() === current.getMonth()
  })
})
const wonThisMonthValues = computed(() => currencyTotals(wonThisMonth.value))

const openTasks = computed(() =>
  tasks.value.filter((task) => task.status === 'open' || task.status === 'in_progress'),
)
const tasksByLead = computed(() => {
  const grouped = new Map<string, FollowUpTask[]>()
  for (const task of openTasks.value) {
    if (!task.lead_id) continue
    const list = grouped.get(task.lead_id) ?? []
    list.push(task)
    grouped.set(task.lead_id, list)
  }
  return grouped
})
const followUpByLead = computed(() => {
  const result = new Map<string, ReturnType<typeof nextFollowUpForLead>>()
  for (const lead of boardLeads.value) {
    result.set(lead.id, nextFollowUpForLead(tasksByLead.value.get(lead.id) ?? [], lead))
  }
  return result
})

function taskInPipeline(task: FollowUpTask) {
  if (!task.lead_id) return false
  if (boardLeadById.value.has(task.lead_id)) return true
  return Boolean(selectedPipelineId.value) && task.lead?.pipeline_id === selectedPipelineId.value
}

const pipelineTasks = computed(() => openTasks.value.filter(taskInPipeline))
const pipelineOverdueCount = computed(
  () => pipelineTasks.value.filter((task) => isFollowUpOverdue(task.due_at, now.value)).length,
)
const sheetTasks = computed(() => {
  const list = showAllFollowUps.value ? openTasks.value : pipelineTasks.value
  return [...list].sort((a, b) => dueTime(a.due_at) - dueTime(b.due_at))
})
const followUpsButtonLabel = computed(() => {
  const count = pipelineTasks.value.length
  const overdue = pipelineOverdueCount.value
  return `Follow-ups, ${count} open, ${overdue} overdue`
})

function dueTime(value?: string) {
  if (!value) return Number.POSITIVE_INFINITY
  const time = new Date(value).getTime()
  return Number.isNaN(time) ? Number.POSITIVE_INFINITY : time
}

function queryParam(key: string): string {
  const value = route?.query?.[key]
  const first = Array.isArray(value) ? value[0] : value
  return typeof first === 'string' ? first : ''
}

function organizationId() {
  return authStore.organizationId || undefined
}

function pickBoardPipeline(preferredId: string) {
  return (
    defaultPipelineId(pipelines.value, {
      preferredId: preferredId || null,
      rememberedId: rememberedPipelineId(organizationId()),
    }) ||
    pipelines.value[0]?.id ||
    ''
  )
}

function rebuildBoard(leads: CRMLead[]) {
  const stages = selectedPipeline.value?.stages ?? []
  columns.value = [...stages]
    .sort((a, b) => a.display_order - b.display_order)
    .map((stage) => ({
      ...stage,
      leads: leads.filter((lead) => lead.stage_id === stage.id),
    }))
    // Hide switched-off stages unless leads still sit in them.
    .filter((column) => column.is_active !== false || column.leads.length > 0)
}

async function loadTasks() {
  if (!canReadTasks.value) {
    tasks.value = []
    return
  }
  tasks.value = await crmService.allTasks({ status: 'open' })
}

async function load() {
  loading.value = true
  const focusLeadId = initialised ? '' : queryParam('lead')
  try {
    const [pipelineResponse, taskResponse] = await Promise.all([
      crmService.pipelines(),
      canReadTasks.value ? crmService.allTasks({ status: 'open' }) : Promise.resolve(null),
    ])
    pipelines.value = unwrapListResponse<Pipeline>(pipelineResponse, 'pipelines')
    tasks.value = taskResponse ?? []

    if (!initialised || !pipelines.value.some((item) => item.id === selectedPipelineId.value)) {
      selectedPipelineId.value = pickBoardPipeline(initialised ? '' : queryParam('pipeline'))
    }
    initialised = true
    await loadLeads()
  } catch (error) {
    toast.error('Pipeline could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
  if (focusLeadId) void focusLead(focusLeadId)
}

async function loadLeads() {
  if (!selectedPipelineId.value) {
    columns.value = []
    return
  }
  const leads = await crmService.allLeads(
    showArchived.value
      ? { pipeline_id: selectedPipelineId.value, status: 'archived' }
      : { pipeline_id: selectedPipelineId.value, include_archived: false },
  )
  rebuildBoard(leads)
}

async function refreshLeadsQuietly() {
  try {
    await loadLeads()
  } catch (error) {
    toast.error('Pipeline could not be refreshed', getErrorMessage(error))
  }
}

async function toggleArchived() {
  showArchived.value = !showArchived.value
  loading.value = true
  try {
    await loadLeads()
  } catch (error) {
    showArchived.value = !showArchived.value
    toast.error('Pipeline could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function switchPipeline(pipelineId: string) {
  if (!pipelineId) return
  selectedPipelineId.value = pipelineId
  rememberPipelineId(organizationId(), pipelineId)
  loading.value = true
  try {
    await loadLeads()
  } catch (error) {
    toast.error('Pipeline could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

function changePipeline() {
  void switchPipeline(selectedPipelineId.value)
}

function scrollLeadIntoView(leadId: string) {
  if (typeof document === 'undefined') return
  const escaped = typeof CSS !== 'undefined' && CSS.escape ? CSS.escape(leadId) : leadId
  const card = document.querySelector<HTMLElement>(`[data-lead-id="${escaped}"]`)
  if (card && typeof card.scrollIntoView === 'function') {
    card.scrollIntoView({ behavior: 'smooth', block: 'center', inline: 'center' })
  }
}

async function focusLead(leadId: string) {
  if (!leadId) return
  if (highlightTimer) clearTimeout(highlightTimer)
  highlightedLeadId.value = leadId
  await nextTick()
  scrollLeadIntoView(leadId)
  highlightTimer = setTimeout(() => {
    if (highlightedLeadId.value === leadId) highlightedLeadId.value = ''
    highlightTimer = null
  }, 4000)
}

function canShowLead(task: FollowUpTask) {
  if (!task.lead_id) return false
  if (boardLeadById.value.has(task.lead_id)) return true
  const pipelineId = task.lead?.pipeline_id
  return Boolean(pipelineId && pipelines.value.some((pipeline) => pipeline.id === pipelineId))
}

async function showLead(task: FollowUpTask) {
  const leadId = task.lead_id
  if (!leadId) return
  followUpsOpen.value = false
  if (!boardLeadById.value.has(leadId)) {
    const pipelineId = task.lead?.pipeline_id
    if (!pipelineId) return
    showArchived.value = task.lead?.status === 'archived'
    await switchPipeline(pipelineId)
  }
  await focusLead(leadId)
}

async function handleMove(event: any, stage: BoardColumn) {
  const lead = event?.added?.element as CRMLead | undefined
  if (!lead || lead.stage_id === stage.id) return

  if (stage.kind === 'won' || stage.kind === 'lost') {
    requestOutcome(lead, stage.kind, true)
    return
  }
  await moveLeadToStage(lead, stage)
}

async function moveLeadToStage(lead: CRMLead, stage: Pick<PipelineStage, 'id' | 'name'>, refreshBoard = false) {
  if (showArchived.value || lead.status === 'archived' || lead.stage_id === stage.id || movingLeadId.value) return
  const previousStage = lead.stage_id
  lead.stage_id = stage.id
  movingLeadId.value = lead.id
  try {
    const response = await crmService.moveLead(lead.id, stage.id, lead.version)
    const updated = response.data?.data ?? response.data
    lead.version = updated?.version ?? lead.version + 1
    lead.status = updated?.status ?? lead.status
    toast.success(`Moved to ${stage.name}`)
    if (refreshBoard) await refreshLeadsQuietly()
  } catch (error) {
    lead.stage_id = previousStage
    toast.error('Lead was not moved', getErrorMessage(error))
    await refreshLeadsQuietly()
  } finally {
    movingLeadId.value = ''
  }
}

function leadColumn(lead: CRMLead) {
  return columns.value.find((column) => column.id === lead.stage_id)
}

function isOpenLead(lead: CRMLead) {
  return lead.status === 'open' && leadColumn(lead)?.kind === 'open'
}

/** Previous/Next only walk between open stages; Won and Lost have their own buttons. */
function adjacentStage(lead: CRMLead, direction: -1 | 1) {
  if (!isOpenLead(lead)) return undefined
  const stages = openColumns.value
  const index = stages.findIndex((column) => column.id === lead.stage_id)
  if (index < 0) return undefined
  return stages[index + direction]
}

function moveLeadByOffset(lead: CRMLead, direction: -1 | 1) {
  const stage = adjacentStage(lead, direction)
  if (stage) void moveLeadToStage(lead, stage, true)
}

function reopenClosedLead(lead: CRMLead) {
  const stage = firstOpenColumn.value
  if (!stage || !canWriteLeads.value) return
  void moveLeadToStage(lead, stage, true)
}

function requestOutcome(lead: CRMLead, kind: OutcomeKind, fromDrag = false) {
  const stage = kind === 'won' ? wonColumn.value : lostColumn.value
  if (!stage || !canWriteLeads.value || showArchived.value || outcomeSaving.value) {
    if (fromDrag) void refreshLeadsQuietly()
    return
  }
  outcomeKind.value = kind
  outcomeLead.value = lead
  outcomeStage.value = stage
  outcomeFromDrag = fromDrag
  outcomeOpen.value = true
}

function setOutcomeOpen(value: boolean) {
  if (outcomeSaving.value) return
  outcomeOpen.value = value
  if (!value && outcomeFromDrag) {
    // The card was dropped into Won/Lost but not confirmed: put it back.
    outcomeFromDrag = false
    void refreshLeadsQuietly()
  }
}

async function confirmOutcome(payload: { reason?: string; createInvoice?: boolean }) {
  const lead = outcomeLead.value
  const stage = outcomeStage.value
  if (!lead || !stage || outcomeSaving.value) return
  const kind = outcomeKind.value
  outcomeSaving.value = true
  movingLeadId.value = lead.id
  try {
    const response = await crmService.moveLead(
      lead.id,
      stage.id,
      lead.version,
      kind === 'lost' ? payload.reason : undefined,
    )
    const updated = (response.data?.data ?? response.data) as CRMLead | undefined
    outcomeFromDrag = false
    outcomeOpen.value = false
    toast.success(kind === 'won' ? 'Marked as won' : 'Marked as lost')
    await refreshLeadsQuietly()
    if (kind === 'won') {
      if (canInvoice.value && payload.createInvoice) {
        invoiceLead.value = boardLeadById.value.get(lead.id) ?? { ...lead, ...(updated ?? {}) }
        invoiceOpen.value = true
      } else if (!canInvoice.value) {
        toast.info('Ask a manager with billing access to create the invoice.')
      }
    }
  } catch (error) {
    toast.error('Lead was not moved', getErrorMessage(error))
    outcomeFromDrag = false
    outcomeOpen.value = false
    await refreshLeadsQuietly()
  } finally {
    outcomeSaving.value = false
    movingLeadId.value = ''
  }
}

function openInvoice(lead: CRMLead) {
  if (!canInvoice.value) return
  invoiceLead.value = lead
  invoiceOpen.value = true
}

function handleInvoiceCreated(_invoice: CommerceInvoice) {
  const leadId = invoiceLead.value?.id
  if (leadId) invoicedLeadIds.value.add(leadId)
  invoiceOpen.value = false
}

function openLeadEditor(lead: CRMLead) {
  if (!canWriteLeads.value || archiving.value) return
  editingLead.value = lead
  leadEditorOpen.value = true
}

function requestArchive(lead: CRMLead) {
  if (!canArchiveLeads.value || lead.status === 'archived' || archiving.value || movingLeadId.value) return
  archiveReturnFocus = document.querySelector<HTMLButtonElement>(`[data-lead-actions="${CSS.escape(lead.id)}"]`)
  archivingLead.value = { ...lead }
  archiveReason.value = ''
  archiveError.value = ''
  archiveIdempotencyKey.value = crypto.randomUUID()
  archiveOpen.value = true
}

function restoreArchiveFocus(event: Event) {
  event.preventDefault()
  const target = archiveReturnFocus?.isConnected
    ? archiveReturnFocus
    : document.querySelector<HTMLButtonElement>('[aria-label="Refresh pipeline"]')
  archiveReturnFocus = null
  target?.focus()
}

function setArchiveOpen(value: boolean) {
  if (!archiving.value) archiveOpen.value = value
}

async function archiveLead() {
  const lead = archivingLead.value
  if (!lead || !archiveOpen.value || archiving.value || !canArchiveLeads.value || lead.status === 'archived') return
  archiving.value = true
  archiveError.value = ''
  try {
    await crmService.archiveLead(lead.id, {
      version: lead.version,
      reason: archiveReason.value.trim() || undefined,
      idempotency_key: archiveIdempotencyKey.value,
      metadata: { source: 'crm_pipeline' },
    })
    // The mutation is acknowledged even if the subsequent board refresh fails.
    for (const column of columns.value) {
      column.leads = column.leads.filter((item) => item.id !== lead.id)
    }
    archiveOpen.value = false
    toast.success('Lead archived')
    try {
      await loadLeads()
    } catch (error) {
      toast.error('Lead archived, but the board could not be refreshed', getErrorMessage(error))
    }
  } catch (error) {
    archiveError.value = getErrorMessage(error, 'Lead could not be archived')
  } finally {
    archiving.value = false
  }
}

async function handleLeadSaved(action: 'updated' | 'archived' | 'reopened' = 'updated') {
  const messages = {
    updated: 'Lead updated',
    archived: 'Lead archived',
    reopened: 'Lead reopened',
  }
  toast.success(messages[action])
  await loadLeads()
}

async function refreshPipelineConfiguration(preferredPipelineId?: string) {
  try {
    const response = await crmService.pipelines()
    pipelines.value = unwrapListResponse<Pipeline>(response, 'pipelines')
    if (preferredPipelineId && pipelines.value.some((item) => item.id === preferredPipelineId)) {
      selectedPipelineId.value = preferredPipelineId
      rememberPipelineId(organizationId(), preferredPipelineId)
    }
    if (!pipelines.value.some((item) => item.id === selectedPipelineId.value)) {
      selectedPipelineId.value = pickBoardPipeline('')
    }
    await loadLeads()
    toast.success('Pipeline configuration saved')
  } catch (error) {
    toast.error('Pipeline could not be refreshed', getErrorMessage(error))
  }
}

function openLeadCreate() {
  if (!canCreateLeads.value || showArchived.value) return
  leadCreateKey.value = crypto.randomUUID()
  leadCreateOpen.value = true
}

function setLeadCreateOpen(value: boolean) {
  if (!value && leadCreateSaving.value) return
  leadCreateOpen.value = value
}

async function submitLeadCreate(draft: LeadDraft) {
  if (leadCreateSaving.value || !canCreateLeads.value) return
  leadCreateSaving.value = true
  try {
    const response = await crmService.createLead(
      buildCreateLeadPayload(draft, {
        source: 'other',
        idempotencyKey: leadCreateKey.value || crypto.randomUUID(),
      }) as Partial<CRMLead>,
    )
    const created = (response?.data?.data ?? response?.data) as CRMLead | undefined
    if (draft.follow_up_at && canWriteTasks.value && created?.id) {
      try {
        await crmService.createTask(
          buildFollowUpTaskPayload({
            contactId: draft.contact_id,
            leadId: created.id,
            leadTitle: created.title || draft.title,
            dueAt: draft.follow_up_at,
            source: 'crm_pipeline',
            idempotencyKey: crypto.randomUUID(),
          }) as Partial<FollowUpTask>,
        )
      } catch (error) {
        toast.error('Lead added, but the follow-up was not saved', getErrorMessage(error))
      }
    }
    leadCreateOpen.value = false
    toast.success('Lead added')
    if (draft.pipeline_id && draft.pipeline_id !== selectedPipelineId.value) {
      selectedPipelineId.value = draft.pipeline_id
    }
    rememberPipelineId(organizationId(), selectedPipelineId.value)
    try {
      await Promise.all([loadLeads(), loadTasks()])
    } catch (error) {
      toast.error('Pipeline could not be refreshed', getErrorMessage(error))
    }
    if (created?.id) void focusLead(created.id)
  } catch (error) {
    toast.error('Lead was not added', getErrorMessage(error))
  } finally {
    leadCreateSaving.value = false
  }
}

function isConflict(error: unknown) {
  return (error as { response?: { status?: number } } | null)?.response?.status === 409
}

async function completeTask(task: FollowUpTask) {
  if (completingTaskId.value) return
  completingTaskId.value = task.id
  try {
    await crmService.completeTask(task.id, task.version)
    task.status = 'completed'
    toast.success('Follow-up completed')
    // Completing a linked task bumps the lead's version on the server; reload
    // the board so the next move or edit does not hit a version conflict.
    if (task.lead_id) await refreshLeadsQuietly()
  } catch (error) {
    if (isConflict(error)) {
      // Someone else completed, moved or edited this follow-up: show the current list.
      toast.warning('This follow-up changed elsewhere. Refreshed.')
      try {
        await Promise.all([loadTasks(), task.lead_id ? loadLeads() : Promise.resolve()])
      } catch (refreshError) {
        toast.error('Pipeline could not be refreshed', getErrorMessage(refreshError))
      }
    } else {
      toast.error('Follow-up was not completed', getErrorMessage(error))
    }
  } finally {
    completingTaskId.value = ''
  }
}

function toLocalInputValue(date: Date) {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 16)
}

function openFollowUpDialog(lead: CRMLead) {
  if (!canWriteTasks.value) return
  followUpLead.value = lead
  followUpPreset.value = 'tomorrow'
  followUpCustomAt.value = ''
  followUpTaskTitle.value = `Follow up: ${lead.title}`.slice(0, 255)
  followUpError.value = ''
  followUpKey.value = crypto.randomUUID()
  followUpDialogOpen.value = true
}

function setFollowUpDialogOpen(value: boolean) {
  if (!value && followUpSaving.value) return
  followUpDialogOpen.value = value
}

function followUpDueAt() {
  if (followUpPreset.value === 'custom') return followUpCustomAt.value
  return toLocalInputValue(followUpPresetDate(followUpPreset.value))
}

const followUpMin = computed(() => toLocalInputValue(now.value))
const canSaveFollowUp = computed(
  () =>
    !followUpSaving.value &&
    Boolean(followUpTaskTitle.value.trim()) &&
    (followUpPreset.value !== 'custom' || Boolean(followUpCustomAt.value)),
)

async function saveFollowUp() {
  const lead = followUpLead.value
  if (!lead || !canSaveFollowUp.value) return
  const dueAt = followUpDueAt()
  if (!dueAt) {
    followUpError.value = 'Choose when to follow up.'
    return
  }
  followUpSaving.value = true
  followUpError.value = ''
  try {
    const payload = buildFollowUpTaskPayload({
      contactId: lead.contact_id,
      leadId: lead.id,
      leadTitle: lead.title,
      dueAt,
      source: 'crm_pipeline',
      idempotencyKey: followUpKey.value || crypto.randomUUID(),
    })
    payload.title = Array.from(followUpTaskTitle.value.trim()).slice(0, 255).join('')
    await crmService.createTask(payload as Partial<FollowUpTask>)
    followUpDialogOpen.value = false
    toast.success('Follow-up scheduled')
    try {
      await Promise.all([loadLeads(), loadTasks()])
    } catch (error) {
      toast.error('Pipeline could not be refreshed', getErrorMessage(error))
    }
  } catch (error) {
    followUpError.value = getErrorMessage(error, 'Follow-up could not be saved')
  } finally {
    followUpSaving.value = false
  }
}

function customerName(lead: Pick<CRMLead, 'contact'> | null | undefined) {
  return contactDisplayName(lead?.contact)
}

function taskCustomer(task: FollowUpTask) {
  const boardLead = task.lead_id ? boardLeadById.value.get(task.lead_id) : undefined
  if (boardLead?.contact) return customerName(boardLead)
  return task.contact ? contactDisplayName(task.contact) : ''
}

function formatMoney(amountMinor: number, currency = 'MYR') {
  try {
    return new Intl.NumberFormat('en-MY', {
      style: 'currency',
      currency,
      maximumFractionDigits: 0,
    }).format(amountMinor / 100)
  } catch {
    return `${currency} ${(amountMinor / 100).toFixed(0)}`
  }
}

function currencyTotals(leads: CRMLead[]) {
  const totals = new Map<string, number>()
  for (const lead of leads) {
    const currency = lead.currency || 'MYR'
    totals.set(currency, (totals.get(currency) ?? 0) + (lead.value_minor || 0))
  }
  return [...totals.entries()].sort(([left], [right]) => left.localeCompare(right))
}

function formatMoneyTotals(totals: Array<[string, number]>) {
  const nonZero = totals.filter(([, amount]) => amount !== 0)
  if (!nonZero.length) return formatMoney(0, totals[0]?.[0] || 'MYR')
  return nonZero.map(([currency, amount]) => formatMoney(amount, currency)).join(' · ')
}

function columnValue(column: BoardColumn) {
  const totals = currencyTotals(column.leads).filter(([, amount]) => amount > 0)
  return totals.length ? totals.map(([currency, amount]) => formatMoney(amount, currency)).join(' · ') : ''
}

function formatDue(value?: string) {
  if (!value) return 'No due date'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'No due date'
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
  }).format(date)
}

function formatTime(value: string) {
  return new Intl.DateTimeFormat('en-MY', { hour: '2-digit', minute: '2-digit' }).format(new Date(value))
}

function formatDay(value: string) {
  return new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short' }).format(new Date(value))
}

function dueLabel(dueAt: string | undefined, urgency: FollowUpUrgency) {
  if (!dueAt) return 'No due date'
  if (urgency === 'overdue') return `Overdue · ${formatDay(dueAt)}`
  if (urgency === 'today') return `Today ${formatTime(dueAt)}`
  return formatDue(dueAt)
}

const urgencyText: Record<FollowUpUrgency, string> = {
  overdue: 'text-rose-300 light:text-rose-700',
  today: 'text-amber-300 light:text-amber-700',
  upcoming: 'text-white/45 light:text-slate-600',
  none: 'text-white/45 light:text-slate-600',
}

const urgencyBadge: Record<FollowUpUrgency | 'missing', string> = {
  overdue: 'border-rose-400/30 bg-rose-400/10 text-rose-200 light:border-rose-300 light:bg-rose-50 light:text-rose-700',
  today: 'border-amber-300/30 bg-amber-300/10 text-amber-200 light:border-amber-300 light:bg-amber-50 light:text-amber-800',
  upcoming: 'border-white/10 bg-white/[0.03] text-white/50 light:border-slate-300 light:bg-slate-50 light:text-slate-600',
  none: 'border-white/10 bg-white/[0.03] text-white/50 light:border-slate-300 light:bg-slate-50 light:text-slate-600',
  missing: 'border-dashed border-amber-300/25 text-amber-200/70 light:border-amber-300 light:text-amber-700',
}

function leadFollowUpBadge(lead: CRMLead): { text: string; tone: FollowUpUrgency | 'missing' } | null {
  const next = followUpByLead.value.get(lead.id)
  if (!next) return lead.status === 'open' ? { text: 'No follow-up', tone: 'missing' } : null
  if (!next.dueAt) return { text: 'Follow-up set', tone: 'none' }
  const urgency = followUpUrgency(next.dueAt, now.value)
  return { text: dueLabel(next.dueAt, urgency), tone: urgency }
}

function taskUrgency(task: FollowUpTask) {
  return followUpUrgency(task.due_at, now.value)
}

watch(
  () => [queryParam('pipeline'), queryParam('lead')] as const,
  async ([pipelineId, leadId], [previousPipelineId, previousLeadId]) => {
    if (!initialised || loading.value) return
    if (
      pipelineId &&
      pipelineId !== previousPipelineId &&
      pipelineId !== selectedPipelineId.value &&
      activePipelineList.value.some((pipeline) => pipeline.id === pipelineId)
    ) {
      await switchPipeline(pipelineId)
    }
    if (leadId && (leadId !== previousLeadId || pipelineId !== previousPipelineId)) await focusLead(leadId)
  },
)

onMounted(() => {
  clockTimer = setInterval(() => {
    now.value = new Date()
  }, 60_000)
  void load()
})

onBeforeUnmount(() => {
  if (clockTimer) clearInterval(clockTimer)
  if (highlightTimer) clearTimeout(highlightTimer)
})

const chipBase =
  'inline-flex min-h-9 items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 disabled:cursor-not-allowed disabled:opacity-60'
const chipIdle =
  'border-white/10 bg-white/[0.03] text-white/70 hover:border-cyan-300/30 hover:text-white light:border-slate-300 light:bg-white light:text-slate-700 light:hover:text-slate-950'
const chipActive = 'border-cyan-300/60 bg-cyan-300/[0.12] text-cyan-100 light:border-cyan-600 light:bg-cyan-50 light:text-cyan-900'
</script>

<template>
  <div class="flex h-full flex-col bg-[#08090a] light:bg-slate-100">
    <PageHeader
      title="Lead pipeline"
      description="Track every lead from first reply until it is won or lost."
      :icon="Route"
      icon-gradient="bg-gradient-to-br from-cyan-400 to-blue-600 shadow-cyan-500/20"
    >
      <template #actions>
        <div class="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="icon" title="Refresh pipeline" aria-label="Refresh pipeline" @click="load">
            <RefreshCw class="h-4 w-4" />
          </Button>
          <Button
            v-if="canReadTasks"
            variant="outline"
            class="gap-2"
            data-testid="pipeline-follow-ups-button"
            :aria-label="followUpsButtonLabel"
            :title="followUpsButtonLabel"
            @click="followUpsOpen = true"
          >
            <CalendarClock class="h-4 w-4" />
            <span class="hidden sm:inline">Follow-ups</span>
            <span
              data-testid="pipeline-follow-ups-count"
              class="inline-flex h-5 min-w-5 items-center justify-center rounded-full px-1.5 text-[10px] font-semibold"
              :class="
                pipelineOverdueCount
                  ? 'bg-rose-500 text-white'
                  : 'bg-white/10 text-white/70 light:bg-slate-200 light:text-slate-700'
              "
            >
              {{ pipelineTasks.length }}
            </span>
          </Button>
          <Button
            v-if="canReadLeads"
            variant="outline"
            class="gap-2"
            :aria-pressed="showArchived"
            :aria-label="showArchived ? 'Show active leads' : 'Show archived leads'"
            :title="showArchived ? 'Show active leads' : 'Show archived leads'"
            @click="toggleArchived"
          >
            <Archive class="h-4 w-4" />
            <span class="hidden sm:inline">{{ showArchived ? 'Active leads' : 'Archived' }}</span>
          </Button>
          <Button v-if="canWritePipelines" variant="outline" class="gap-2" @click="pipelineSettingsOpen = true">
            <Settings2 class="h-4 w-4" />
            <span class="hidden sm:inline">Configure</span>
          </Button>
          <Button
            v-if="canCreateLeads && !showArchived"
            data-testid="pipeline-new-lead"
            class="bg-cyan-400 text-black hover:bg-cyan-300"
            @click="openLeadCreate"
          >
            <Plus class="mr-2 h-4 w-4" />
            New lead
          </Button>
        </div>
      </template>
    </PageHeader>

    <div
      v-if="pipelines.length"
      class="flex flex-wrap items-center gap-x-4 gap-y-1.5 border-b border-white/[0.08] bg-[#0b0c0d] px-4 py-2.5 light:border-slate-300 light:bg-white sm:px-6"
    >
      <div class="flex min-w-0 items-center gap-2">
        <label
          v-if="pipelineChoices.length > 1"
          for="pipeline-board-select"
          class="text-xs font-medium text-white/55 light:text-slate-600"
        >
          Pipeline
        </label>
        <select
          v-if="pipelineChoices.length > 1"
          id="pipeline-board-select"
          v-model="selectedPipelineId"
          data-testid="pipeline-select"
          class="h-9 min-w-0 max-w-[16rem] rounded-md border border-white/10 bg-white/[0.04] px-3 text-sm text-white outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-400 light:bg-slate-50 light:text-slate-950"
          @change="changePipeline"
        >
          <option v-for="pipeline in pipelineChoices" :key="pipeline.id" :value="pipeline.id">
            {{ pipeline.name }}
          </option>
        </select>
        <p v-else class="truncate text-sm text-white/55 light:text-slate-600" data-testid="pipeline-name">
          Pipeline:
          <span class="font-semibold text-white light:text-slate-950">{{ selectedPipeline?.name }}</span>
        </p>
      </div>
      <p
        v-if="selectedPipeline?.description"
        class="min-w-0 truncate text-xs text-white/45 light:text-slate-600"
        data-testid="pipeline-description"
      >
        {{ selectedPipeline.description }}
      </p>
    </div>

    <div v-if="loading" class="flex flex-1 items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-cyan-300" />
    </div>

    <div v-else class="flex min-h-0 flex-1 flex-col">
      <div
        class="grid grid-cols-2 gap-px border-b border-white/[0.08] bg-white/[0.08] light:border-slate-300 light:bg-slate-300 md:grid-cols-4"
        data-testid="pipeline-kpis"
      >
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-slate-50">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-slate-600">
            {{ showArchived ? 'Archived leads' : 'Open leads' }}
          </p>
          <p class="mt-1 text-xl font-semibold text-white light:text-slate-950">
            {{ showArchived ? boardLeads.length : openLeads.length }}
          </p>
        </div>
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-slate-50">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-slate-600">
            {{ showArchived ? 'Archived value' : 'Pipeline value' }}
          </p>
          <p class="mt-1 text-base font-semibold text-white light:text-slate-950">
            {{ formatMoneyTotals(showArchived ? visibleValues : openValues) }}
          </p>
        </div>
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-slate-50">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-slate-600">Won this month</p>
          <p class="mt-1 flex items-baseline gap-2">
            <span class="text-xl font-semibold text-emerald-300 light:text-emerald-700">{{ wonThisMonth.length }}</span>
            <span v-if="wonThisMonth.length" class="text-xs text-white/50 light:text-slate-600">
              {{ formatMoneyTotals(wonThisMonthValues) }}
            </span>
          </p>
        </div>
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-slate-50" data-testid="pipeline-kpi-follow-ups">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-slate-600">Open follow-ups</p>
          <p class="mt-1 flex items-baseline gap-2">
            <span class="text-xl font-semibold text-white light:text-slate-950">{{ pipelineTasks.length }}</span>
            <span
              data-testid="pipeline-kpi-overdue"
              class="text-xs"
              :class="
                pipelineOverdueCount
                  ? 'font-medium text-rose-300 light:text-rose-700'
                  : 'text-white/45 light:text-slate-600'
              "
            >
              {{ pipelineOverdueCount }} overdue
            </span>
          </p>
        </div>
      </div>

      <div v-if="!columns.length" class="flex flex-1 items-center justify-center p-6 text-center">
        <p class="max-w-sm text-sm text-white/50 light:text-slate-600">
          {{
            pipelines.length
              ? 'This pipeline has no stages yet. Ask an admin to set them up in Configure.'
              : 'There is no pipeline yet. Ask an admin to create one in Configure.'
          }}
        </p>
      </div>

      <div v-else class="min-h-0 min-w-0 flex-1 overflow-x-auto p-4 md:p-5" data-testid="pipeline-board">
        <div class="flex h-full gap-3">
          <section
            v-for="column in columns"
            :key="column.id"
            data-testid="pipeline-column"
            :aria-label="column.name"
            class="flex min-w-[220px] max-w-[340px] flex-1 basis-[240px] flex-col overflow-hidden rounded-2xl border border-white/[0.08] bg-white/[0.02] light:border-slate-300 light:bg-slate-50"
          >
            <header class="border-b border-white/[0.07] px-4 py-3 light:border-slate-300">
              <div class="flex items-center justify-between gap-2">
                <div class="flex min-w-0 items-center gap-2">
                  <span
                    class="h-2.5 w-2.5 shrink-0 rounded-full"
                    :style="{ backgroundColor: column.color || '#67e8f9' }"
                  />
                  <h3 class="truncate text-sm font-semibold text-white light:text-slate-950">
                    {{ column.name }}
                  </h3>
                </div>
                <Badge variant="secondary" class="h-5 min-w-5 justify-center px-1.5 text-[10px]">{{
                  column.leads.length
                }}</Badge>
              </div>
              <p
                v-if="columnValue(column)"
                class="mt-1.5 text-[11px] text-white/40 light:text-slate-600"
                data-testid="pipeline-column-value"
              >
                {{ columnValue(column) }}
              </p>
            </header>

            <draggable
              v-model="column.leads"
              item-key="id"
              group="pipeline-leads"
              :disabled="showArchived || !canWriteLeads || Boolean(movingLeadId) || archiving"
              filter="button, a, input, select, textarea, [role=menuitem]"
              :prevent-on-filter="false"
              class="min-h-28 flex-1 space-y-2 overflow-y-auto p-2.5"
              ghost-class="opacity-30"
              drag-class="rotate-1"
              @change="handleMove($event, column)"
            >
              <template #item="{ element: lead }">
                <article
                  :data-lead-id="lead.id"
                  data-testid="pipeline-lead-card"
                  class="rounded-xl border bg-[#121416] p-3 shadow-lg shadow-black/10 transition light:bg-white"
                  :class="[
                    canWriteLeads && !showArchived ? 'cursor-grab active:cursor-grabbing' : '',
                    highlightedLeadId === lead.id
                      ? 'border-cyan-300 ring-2 ring-cyan-300/70 light:border-cyan-600 light:ring-cyan-600/50'
                      : 'border-white/[0.07] hover:border-cyan-300/20 light:border-slate-300 light:hover:border-cyan-600/40',
                  ]"
                >
                  <div class="flex items-start gap-2">
                    <GripVertical class="mt-0.5 h-4 w-4 shrink-0 text-white/20 light:text-slate-500" />
                    <div class="min-w-0 flex-1">
                      <p class="line-clamp-2 text-sm font-medium leading-5 text-white light:text-slate-950">
                        {{ lead.title }}
                      </p>
                      <RouterLink
                        v-if="canOpenChat && lead.contact_id"
                        :to="`/chat/${lead.contact_id}`"
                        :aria-label="`Open chat with ${customerName(lead)}`"
                        :title="`Open chat with ${customerName(lead)}`"
                        data-testid="pipeline-lead-chat"
                        class="mt-2 inline-flex max-w-full items-center gap-1.5 rounded text-[11px] text-cyan-200/80 underline-offset-2 hover:text-cyan-100 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-800 light:hover:text-cyan-950"
                      >
                        <MessageCircle class="h-3 w-3 shrink-0" />
                        <span class="truncate">{{ customerName(lead) }}</span>
                      </RouterLink>
                      <div v-else class="mt-2 flex items-center gap-1.5 text-[11px] text-white/40 light:text-slate-600">
                        <UserRound class="h-3 w-3 shrink-0" />
                        <span class="truncate">{{ customerName(lead) }}</span>
                      </div>
                    </div>
                    <DropdownMenu
                      v-if="canWriteLeads || (canArchiveLeads && lead.status !== 'archived')"
                      :open="openLeadMenuId === lead.id"
                      @update:open="
                        openLeadMenuId = $event ? lead.id : openLeadMenuId === lead.id ? '' : openLeadMenuId
                      "
                    >
                      <DropdownMenuTrigger as-child>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          class="-mr-1 -mt-1 h-11 w-11 shrink-0 cursor-pointer text-white/55 focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-slate-600"
                          :aria-label="`Actions for ${lead.title}`"
                          :data-lead-actions="lead.id"
                          :disabled="archiving || Boolean(movingLeadId)"
                        >
                          <MoreHorizontal class="h-4 w-4" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem
                          v-if="canWriteLeads"
                          :aria-label="`Edit ${lead.title}`"
                          class="min-h-11 cursor-pointer"
                          @select="openLeadEditor(lead)"
                        >
                          <Pencil class="mr-2 h-4 w-4" />
                          {{ lead.status === 'archived' ? 'Review and reopen' : 'Edit lead' }}
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          v-if="
                            canWriteLeads &&
                            !showArchived &&
                            (lead.status === 'won' || lead.status === 'lost') &&
                            firstOpenColumn
                          "
                          :aria-label="`Reopen ${lead.title}`"
                          class="min-h-11 cursor-pointer"
                          @select="reopenClosedLead(lead)"
                        >
                          <RotateCcw class="mr-2 h-4 w-4" />
                          Reopen lead
                        </DropdownMenuItem>
                        <DropdownMenuItem
                          v-if="canArchiveLeads && lead.status !== 'archived'"
                          :aria-label="`Archive ${lead.title}`"
                          class="min-h-11 cursor-pointer text-destructive focus:text-destructive"
                          @select="requestArchive(lead)"
                        >
                          <Archive class="mr-2 h-4 w-4" />
                          Archive lead
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </div>

                  <div
                    v-if="lead.value_minor > 0 || leadFollowUpBadge(lead)"
                    class="mt-3 flex flex-wrap items-center justify-between gap-2 border-t border-white/[0.06] pt-2.5 light:border-slate-300"
                  >
                    <span
                      v-if="lead.value_minor > 0"
                      data-testid="pipeline-lead-value"
                      class="flex items-center gap-1 text-xs font-medium text-emerald-300 light:text-emerald-700"
                    >
                      <CircleDollarSign class="h-3.5 w-3.5" />
                      {{ formatMoney(lead.value_minor, lead.currency) }}
                    </span>
                    <span
                      v-if="leadFollowUpBadge(lead)"
                      data-testid="pipeline-lead-follow-up"
                      class="inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[10px] font-medium"
                      :class="urgencyBadge[leadFollowUpBadge(lead)!.tone]"
                    >
                      <CalendarClock class="h-3 w-3" />
                      {{ leadFollowUpBadge(lead)!.text }}
                    </span>
                  </div>

                  <p
                    v-if="lead.status === 'lost' && lead.lost_reason"
                    class="mt-2 line-clamp-2 text-[11px] leading-4 text-rose-200/80 light:text-rose-700"
                    data-testid="pipeline-lead-lost-reason"
                  >
                    Reason: {{ lead.lost_reason }}
                  </p>

                  <!--
                    Sized to stay on one line in a 240px column (card body ~192px: two 28px arrows plus
                    Won, Lost and Follow-up at ~130px). Wrapping is only a fallback for narrower columns.
                  -->
                  <div
                    v-if="!showArchived && lead.status !== 'archived' && (canWriteLeads || canWriteTasks || canInvoice)"
                    data-testid="pipeline-lead-actions"
                    class="mt-2 flex flex-wrap items-center gap-y-1 border-t border-white/[0.05] pt-2 light:border-slate-200"
                  >
                    <template v-if="canWriteLeads && isOpenLead(lead)">
                      <Button
                        v-if="adjacentStage(lead, -1)"
                        type="button"
                        variant="ghost"
                        size="icon"
                        class="h-7 w-7 shrink-0 text-white/50 light:text-slate-600"
                        :disabled="movingLeadId === lead.id"
                        :aria-label="`Move ${lead.title} to ${adjacentStage(lead, -1)?.name}`"
                        :title="`Move to ${adjacentStage(lead, -1)?.name}`"
                        data-testid="pipeline-lead-move-previous"
                        @click="moveLeadByOffset(lead, -1)"
                      >
                        <ChevronLeft class="h-4 w-4" aria-hidden="true" />
                      </Button>
                      <Button
                        v-if="adjacentStage(lead, 1)"
                        type="button"
                        variant="ghost"
                        size="icon"
                        class="h-7 w-7 shrink-0 text-white/50 light:text-slate-600"
                        :disabled="movingLeadId === lead.id"
                        :aria-label="`Move ${lead.title} to ${adjacentStage(lead, 1)?.name}`"
                        :title="`Move to ${adjacentStage(lead, 1)?.name}`"
                        data-testid="pipeline-lead-move-next"
                        @click="moveLeadByOffset(lead, 1)"
                      >
                        <ChevronRight class="h-4 w-4" aria-hidden="true" />
                      </Button>
                      <Button
                        v-if="wonColumn"
                        type="button"
                        variant="ghost"
                        size="sm"
                        class="ml-auto h-7 shrink-0 px-1.5 text-[11px] text-emerald-300 hover:bg-emerald-400/10 hover:text-emerald-200 light:text-emerald-700 light:hover:bg-emerald-50"
                        :disabled="movingLeadId === lead.id"
                        :aria-label="`Mark ${lead.title} as won`"
                        @click="requestOutcome(lead, 'won')"
                      >
                        Won
                      </Button>
                      <Button
                        v-if="lostColumn"
                        type="button"
                        variant="ghost"
                        size="sm"
                        class="h-7 shrink-0 px-1.5 text-[11px] text-rose-300 hover:bg-rose-400/10 hover:text-rose-200 light:text-rose-700 light:hover:bg-rose-50"
                        :class="wonColumn ? '' : 'ml-auto'"
                        :disabled="movingLeadId === lead.id"
                        :aria-label="`Mark ${lead.title} as lost`"
                        @click="requestOutcome(lead, 'lost')"
                      >
                        Lost
                      </Button>
                    </template>
                    <Button
                      v-if="canWriteTasks && lead.status === 'open'"
                      type="button"
                      variant="ghost"
                      size="sm"
                      class="h-7 shrink-0 px-1.5 text-[11px] text-white/50 light:text-slate-600"
                      :class="canWriteLeads && isOpenLead(lead) && (wonColumn || lostColumn) ? '' : 'ml-auto'"
                      :aria-label="`Schedule follow-up for ${lead.title}`"
                      data-testid="pipeline-lead-schedule-follow-up"
                      @click="openFollowUpDialog(lead)"
                    >
                      Follow-up
                    </Button>
                    <template v-if="canInvoice && lead.status === 'won'">
                      <span
                        v-if="invoicedLeadIds.has(lead.id)"
                        data-testid="pipeline-lead-invoiced"
                        class="inline-flex flex-wrap items-center gap-2"
                      >
                        <span
                          class="inline-flex items-center gap-1 rounded-full border border-emerald-400/30 bg-emerald-400/10 px-2 py-0.5 text-[10px] font-medium text-emerald-200 light:border-emerald-300 light:bg-emerald-50 light:text-emerald-800"
                        >
                          <Check class="h-3 w-3" aria-hidden="true" />
                          Invoice created
                        </span>
                        <RouterLink
                          to="/commerce?tab=invoices"
                          data-testid="pipeline-lead-open-invoices"
                          class="rounded text-[11px] font-medium text-cyan-200 underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-800"
                        >
                          Open invoices
                        </RouterLink>
                      </span>
                      <Button
                        v-else
                        type="button"
                        variant="ghost"
                        size="sm"
                        class="h-7 gap-1 px-1.5 text-[11px] text-cyan-200 light:text-cyan-800"
                        :aria-label="`Create invoice for ${lead.title}`"
                        data-testid="pipeline-lead-create-invoice"
                        @click="openInvoice(lead)"
                      >
                        <Receipt class="h-3 w-3" aria-hidden="true" />
                        Create invoice
                      </Button>
                    </template>
                  </div>
                </article>
              </template>
            </draggable>
          </section>
        </div>
      </div>
    </div>

    <Sheet :open="followUpsOpen" @update:open="followUpsOpen = $event">
      <SheetContent
        side="right"
        data-testid="pipeline-follow-ups-sheet"
        class="flex !w-full !max-w-[420px] flex-col gap-0 border-white/10 bg-[#0b0c0d] !p-0 text-white light:border-slate-200 light:bg-white light:text-slate-950 [&>button:last-child]:hidden"
      >
        <SheetHeader class="space-y-1 border-b border-white/[0.08] px-5 py-4 text-left light:border-slate-200">
          <div class="flex items-start justify-between gap-3">
            <div>
              <SheetTitle class="text-base text-white light:text-slate-950">Follow-ups</SheetTitle>
              <SheetDescription class="text-xs text-white/50 light:text-slate-600">
                {{
                  showAllFollowUps
                    ? 'Every open follow-up in the clinic.'
                    : `Open follow-ups for leads in ${selectedPipeline?.name || 'this pipeline'}.`
                }}
              </SheetDescription>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              class="-mr-2 -mt-1 h-9 w-9 shrink-0 text-white/60 light:text-slate-600"
              aria-label="Close follow-ups"
              @click="followUpsOpen = false"
            >
              <X class="h-4 w-4" />
            </Button>
          </div>
          <div
            role="group"
            aria-label="Which follow-ups to show"
            class="mt-2 inline-flex rounded-lg border border-white/10 p-0.5 light:border-slate-300"
          >
            <button
              type="button"
              data-testid="pipeline-follow-ups-scope-pipeline"
              class="rounded-md px-3 py-1.5 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300"
              :class="
                !showAllFollowUps
                  ? 'bg-white/10 text-white light:bg-slate-900 light:text-white'
                  : 'text-white/55 hover:text-white light:text-slate-600 light:hover:text-slate-950'
              "
              :aria-pressed="!showAllFollowUps"
              @click="showAllFollowUps = false"
            >
              This pipeline
            </button>
            <button
              type="button"
              data-testid="pipeline-follow-ups-scope-all"
              class="rounded-md px-3 py-1.5 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300"
              :class="
                showAllFollowUps
                  ? 'bg-white/10 text-white light:bg-slate-900 light:text-white'
                  : 'text-white/55 hover:text-white light:text-slate-600 light:hover:text-slate-950'
              "
              :aria-pressed="showAllFollowUps"
              @click="showAllFollowUps = true"
            >
              All follow-ups
            </button>
          </div>
        </SheetHeader>

        <div class="min-h-0 flex-1 space-y-2 overflow-y-auto p-4">
          <article
            v-for="task in sheetTasks"
            :key="task.id"
            data-testid="pipeline-follow-up-row"
            class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-slate-300 light:bg-slate-50"
          >
            <div class="flex items-start gap-3">
              <button
                v-if="canWriteTasks"
                type="button"
                class="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full border border-white/20 text-transparent transition hover:border-emerald-300 hover:text-emerald-300 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-emerald-300 disabled:opacity-50 light:border-slate-400 light:hover:border-emerald-700 light:hover:text-emerald-700"
                :aria-label="`Complete ${task.title}`"
                :title="`Complete ${task.title}`"
                :disabled="Boolean(completingTaskId)"
                data-testid="pipeline-follow-up-complete"
                @click="completeTask(task)"
              >
                <Loader2 v-if="completingTaskId === task.id" class="h-3 w-3 animate-spin text-white/60" />
                <Check v-else class="h-3.5 w-3.5" />
              </button>
              <div class="min-w-0 flex-1">
                <p class="text-sm font-medium leading-5 text-white light:text-slate-950">{{ task.title }}</p>
                <p v-if="taskCustomer(task)" class="mt-0.5 truncate text-xs text-white/55 light:text-slate-600">
                  {{ taskCustomer(task) }}
                </p>
                <div class="mt-1.5 flex flex-wrap items-center justify-between gap-2">
                  <span class="text-[11px] font-medium" :class="urgencyText[taskUrgency(task)]">
                    {{ dueLabel(task.due_at, taskUrgency(task)) }}
                  </span>
                  <button
                    v-if="canShowLead(task)"
                    type="button"
                    class="rounded text-[11px] font-medium text-cyan-200 underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-cyan-800"
                    data-testid="pipeline-follow-up-show-lead"
                    @click="showLead(task)"
                  >
                    Show lead
                  </button>
                </div>
              </div>
            </div>
          </article>
          <div v-if="!sheetTasks.length" class="rounded-xl bg-emerald-400/[0.06] p-5 text-center">
            <Check class="mx-auto h-5 w-5 text-emerald-300 light:text-emerald-700" />
            <p class="mt-2 text-xs text-emerald-300 light:text-emerald-700">
              {{ showAllFollowUps ? 'No open follow-ups.' : 'No open follow-ups for this pipeline.' }}
            </p>
          </div>
        </div>

        <div class="border-t border-white/[0.08] p-3 light:border-slate-200">
          <RouterLink
            to="/crm/tasks"
            class="flex min-h-10 items-center justify-between rounded-lg px-3 text-sm text-white/65 hover:bg-white/[0.05] hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:text-slate-700 light:hover:bg-slate-100 light:hover:text-slate-950"
          >
            Open follow-ups page
            <ArrowRight class="h-4 w-4" />
          </RouterLink>
        </div>
      </SheetContent>
    </Sheet>

    <LeadCreateDialog
      :open="leadCreateOpen"
      :saving="leadCreateSaving"
      :pipelines="pipelines"
      :default-pipeline-id="selectedPipelineId"
      :can-schedule-follow-up="canWriteTasks"
      :existing-open-leads="openLeads"
      @update:open="setLeadCreateOpen"
      @submit="submitLeadCreate"
    />
    <LeadOutcomeDialog
      :open="outcomeOpen"
      :outcome="outcomeKind"
      :lead="outcomeLead"
      :stage-name="outcomeStage?.name"
      :saving="outcomeSaving"
      :can-invoice="canInvoice"
      @update:open="setOutcomeOpen"
      @confirm="confirmOutcome"
    />
    <InvoiceQuickDialog
      v-if="canInvoice"
      :open="invoiceOpen"
      :contact="invoiceLead ? { id: invoiceLead.contact_id, name: customerName(invoiceLead) } : null"
      :lead="invoiceLead"
      :can-sell-packages="canSellPackages"
      source="crm_pipeline"
      @update:open="invoiceOpen = $event"
      @created="handleInvoiceCreated"
    />

    <Dialog :open="followUpDialogOpen" @update:open="setFollowUpDialogOpen">
      <DialogContent
        data-testid="pipeline-follow-up-dialog"
        class="w-[calc(100vw-1.5rem)] max-w-md border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950"
      >
        <DialogHeader>
          <DialogTitle>Schedule a follow-up</DialogTitle>
          <DialogDescription>
            {{ followUpLead ? `For ${followUpLead.title} (${customerName(followUpLead)}).` : '' }}
          </DialogDescription>
        </DialogHeader>
        <form id="pipeline-follow-up-form" class="space-y-4" @submit.prevent="saveFollowUp">
          <p
            v-if="followUpError"
            role="alert"
            class="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
          >
            {{ followUpError }}
          </p>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">What to do</span>
            <Input v-model="followUpTaskTitle" maxlength="255" :disabled="followUpSaving" required />
          </label>
          <div class="space-y-1.5">
            <span id="pipeline-follow-up-when" class="block text-xs font-medium text-white/60 light:text-slate-700">
              When
            </span>
            <div role="group" aria-labelledby="pipeline-follow-up-when" class="flex flex-wrap gap-2">
              <button
                v-for="preset in FOLLOW_UP_PRESETS"
                :key="preset.value"
                type="button"
                data-testid="pipeline-follow-up-preset"
                :aria-pressed="followUpPreset === preset.value"
                :disabled="followUpSaving"
                :class="[chipBase, followUpPreset === preset.value ? chipActive : chipIdle]"
                @click="followUpPreset = preset.value"
              >
                <Check v-if="followUpPreset === preset.value" class="h-3.5 w-3.5" aria-hidden="true" />
                {{ preset.label }}
              </button>
            </div>
            <input
              v-if="followUpPreset === 'custom'"
              v-model="followUpCustomAt"
              type="datetime-local"
              aria-label="Follow-up date and time"
              :min="followUpMin"
              :disabled="followUpSaving"
              class="mt-2 h-10 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
            />
          </div>
        </form>
        <DialogFooter class="gap-2">
          <Button variant="outline" :disabled="followUpSaving" @click="setFollowUpDialogOpen(false)">Cancel</Button>
          <Button
            type="submit"
            form="pipeline-follow-up-form"
            data-testid="pipeline-follow-up-save"
            class="gap-2 bg-cyan-400 text-black hover:bg-cyan-300"
            :disabled="!canSaveFollowUp"
          >
            <Loader2 v-if="followUpSaving" class="h-4 w-4 animate-spin" />
            Save follow-up
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <PipelineSettingsDialog
      v-model="pipelineSettingsOpen"
      :pipeline="selectedPipeline"
      :can-delete-stages="canDeletePipelineStages"
      @saved="refreshPipelineConfiguration"
    />
    <LeadEditDialog v-model="leadEditorOpen" :lead="editingLead" @saved="handleLeadSaved" />
    <AlertDialog :open="archiveOpen" @update:open="setArchiveOpen">
      <AlertDialogContent @close-auto-focus="restoreArchiveFocus">
        <AlertDialogHeader>
          <AlertDialogTitle>Archive this lead?</AlertDialogTitle>
          <AlertDialogDescription>
            {{ archivingLead?.title }} will leave the active board. Its customer, messages, tasks and stage history stay
            intact. A staff member with lead-write permission can reopen it later.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <p
          v-if="archiveError"
          role="alert"
          class="rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive"
        >
          {{ archiveError }}
        </p>
        <label class="block">
          <span class="mb-1.5 block text-sm font-medium">Reason (optional)</span>
          <textarea
            v-model="archiveReason"
            rows="3"
            maxlength="2000"
            :disabled="archiving"
            class="w-full resize-y rounded-md border border-input bg-background px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
        </label>
        <AlertDialogFooter>
          <AlertDialogCancel :disabled="archiving">Keep as is</AlertDialogCancel>
          <Button
            class="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            :disabled="archiving || !canArchiveLeads"
            @click.prevent="archiveLead"
          >
            <Loader2 v-if="archiving" class="mr-2 h-4 w-4 animate-spin" />
            Archive lead
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  </div>
</template>
