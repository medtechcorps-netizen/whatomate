/**
 * Pure helpers shared by the pipeline page and the customer workspace rail.
 * No Vue imports: everything here is plain data in, plain data out.
 */
import type {
  CommerceInvoice,
  CRMLead,
  FollowUpTask,
  Pipeline,
  PipelineStage,
} from '@/services/productSuite'

export type LeadFlowState = 'none' | 'open' | 'won' | 'lost'

export interface WorkspaceRequestedAction {
  kind: 'lead' | 'follow-up' | 'booking'
  nonce: number
}

export const LOST_REASONS: readonly string[] = [
  'Price',
  'Timing not right',
  'Not suitable',
  'No response',
  'Chose another provider',
  'Other',
]

/** Mirrors productCRMOptionalString("reason", ..., 2000) in internal/handlers/product_crm.go. */
export const LOST_REASON_MAX_LENGTH = 2000

const PIPELINE_STORAGE_PREFIX = 'rereply.crm.lastPipeline.'

function isActive(item: { is_active?: boolean }) {
  return item.is_active !== false
}

function byDisplayOrder(a: { display_order?: number }, b: { display_order?: number }) {
  return (a.display_order ?? 0) - (b.display_order ?? 0)
}

function activeStages(pipeline?: Pipeline | null): PipelineStage[] {
  return (pipeline?.stages ?? []).filter(isActive).sort(byDisplayOrder)
}

export function activePipelines(pipelines: Pipeline[]): Pipeline[] {
  return (pipelines ?? [])
    .filter(isActive)
    .map((pipeline) => ({ ...pipeline, stages: activeStages(pipeline) }))
    .sort((a, b) => byDisplayOrder(a, b) || a.name.localeCompare(b.name))
}

export function openStages(pipeline?: Pipeline | null): PipelineStage[] {
  return activeStages(pipeline).filter((stage) => stage.kind === 'open')
}

export function firstOpenStage(pipeline?: Pipeline | null): PipelineStage | null {
  return openStages(pipeline)[0] ?? null
}

export function wonStage(pipeline?: Pipeline | null): PipelineStage | null {
  return activeStages(pipeline).find((stage) => stage.kind === 'won') ?? null
}

export function lostStage(pipeline?: Pipeline | null): PipelineStage | null {
  return activeStages(pipeline).find((stage) => stage.kind === 'lost') ?? null
}

export function stageChainLabel(pipeline?: Pipeline | null): string {
  return openStages(pipeline)
    .map((stage) => stage.name)
    .join(' → ')
}

function timeOf(value?: string | null): number {
  if (!value) return 0
  const time = new Date(value).getTime()
  return Number.isNaN(time) ? 0 : time
}

function newestBy(leads: CRMLead[], keys: Array<(lead: CRMLead) => string | undefined>): CRMLead | null {
  let best: CRMLead | null = null
  for (const lead of leads) {
    if (!best) {
      best = lead
      continue
    }
    for (const key of keys) {
      const diff = timeOf(key(lead)) - timeOf(key(best))
      if (diff > 0) {
        best = lead
        break
      }
      if (diff < 0) break
    }
  }
  return best
}

export function pickFocusLead(leads: CRMLead[] | undefined): CRMLead | null {
  const visible = (leads ?? []).filter((lead) => lead && lead.status !== 'archived')
  const open = visible.filter((lead) => lead.status === 'open')
  if (open.length) {
    return newestBy(open, [
      (lead) => lead.last_activity_at,
      (lead) => lead.updated_at,
      (lead) => lead.created_at,
    ])
  }
  const won = visible.filter((lead) => lead.status === 'won')
  if (won.length) return newestBy(won, [(lead) => lead.won_at, (lead) => lead.updated_at])
  const lost = visible.filter((lead) => lead.status === 'lost')
  if (lost.length) return newestBy(lost, [(lead) => lead.lost_at, (lead) => lead.updated_at])
  return null
}

export function leadFlowState(lead: CRMLead | null): LeadFlowState {
  if (!lead) return 'none'
  if (lead.status === 'open' || lead.status === 'won' || lead.status === 'lost') return lead.status
  return 'none'
}

export function invoiceForLead(
  invoices: CommerceInvoice[] | undefined,
  leadId: string,
): CommerceInvoice | null {
  if (!leadId) return null
  const matches = (invoices ?? []).filter((invoice) => invoice?.metadata?.lead_id === leadId)
  if (!matches.length) return null
  return [...matches].sort((a, b) => timeOf(b.issued_at) - timeOf(a.issued_at))[0] ?? null
}

/**
 * The next follow-up for a lead: the earliest dated open/in-progress task linked
 * to it, else the lead's own next_action_at, else an undated linked task.
 */
export function nextFollowUpForLead(
  tasks: FollowUpTask[] | undefined,
  lead: CRMLead | null,
): { dueAt?: string; task?: FollowUpTask } | null {
  if (!lead) return null
  const linked = (tasks ?? []).filter(
    (task) => task?.lead_id === lead.id && (task.status === 'open' || task.status === 'in_progress'),
  )
  const dated = linked
    .filter((task) => timeOf(task.due_at) > 0)
    .sort((a, b) => timeOf(a.due_at) - timeOf(b.due_at))
  if (dated[0]) return { dueAt: dated[0].due_at, task: dated[0] }
  if (lead.next_action_at && timeOf(lead.next_action_at) > 0) return { dueAt: lead.next_action_at }
  if (linked[0]) return { task: linked[0] }
  return null
}

export type FollowUpUrgency = 'overdue' | 'today' | 'upcoming' | 'none'

/**
 * How urgent a follow-up is, judged by local calendar day:
 * 'overdue' = due before the start of today, 'today' = due any time today
 * (even if that time has already passed), 'upcoming' = due after today,
 * 'none' = no date or an invalid date.
 */
export function followUpUrgency(dueAt: string | undefined | null, now = new Date()): FollowUpUrgency {
  if (!dueAt) return 'none'
  const due = new Date(dueAt)
  if (Number.isNaN(due.getTime())) return 'none'
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  const startOfTomorrow = new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1)
  if (due.getTime() < startOfToday.getTime()) return 'overdue'
  if (due.getTime() < startOfTomorrow.getTime()) return 'today'
  return 'upcoming'
}

export function isFollowUpOverdue(dueAt: string | undefined | null, now = new Date()): boolean {
  return followUpUrgency(dueAt, now) === 'overdue'
}

export function isFollowUpDueToday(dueAt: string | undefined | null, now = new Date()): boolean {
  return followUpUrgency(dueAt, now) === 'today'
}

export function followUpPresetDate(preset: 'tomorrow' | '3days' | 'week', now = new Date()): Date {
  const days = preset === 'tomorrow' ? 1 : preset === '3days' ? 3 : 7
  const date = new Date(now.getTime())
  date.setDate(date.getDate() + days)
  date.setHours(10, 0, 0, 0)
  return date
}

function pipelineStorageKey(orgId: string | undefined) {
  return PIPELINE_STORAGE_PREFIX + (orgId || 'default')
}

export function rememberedPipelineId(orgId: string | undefined): string | null {
  try {
    const value = globalThis.localStorage?.getItem(pipelineStorageKey(orgId))
    return value || null
  } catch {
    return null
  }
}

export function rememberPipelineId(orgId: string | undefined, pipelineId: string): void {
  try {
    if (pipelineId) globalThis.localStorage?.setItem(pipelineStorageKey(orgId), pipelineId)
    else globalThis.localStorage?.removeItem(pipelineStorageKey(orgId))
  } catch {
    // Storage can be unavailable (private mode, blocked site data); the
    // default pipeline is used instead.
  }
}

export function defaultPipelineId(
  pipelines: Pipeline[],
  opts: { preferredId?: string | null; rememberedId?: string | null },
): string {
  const active = activePipelines(pipelines)
  const has = (id?: string | null) => Boolean(id && active.some((pipeline) => pipeline.id === id))
  if (has(opts.preferredId)) return opts.preferredId as string
  if (has(opts.rememberedId)) return opts.rememberedId as string
  return active.find((pipeline) => pipeline.is_default)?.id ?? active[0]?.id ?? ''
}

export function leadSourceForChannel(channel?: string | null): 'whatsapp' | 'other' {
  return (channel ?? '').trim().toLowerCase() === 'whatsapp' ? 'whatsapp' : 'other'
}

export interface LeadDraft {
  contact_id: string
  contact_name: string
  pipeline_id: string
  stage_id: string
  title: string
  value: string
  currency: string
  follow_up_at: string /* datetime-local value or '' */
}

function toIso(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

export function buildCreateLeadPayload(
  draft: LeadDraft,
  opts: { source: string; sourceReference?: string; idempotencyKey: string },
): Record<string, unknown> {
  const valueMinor = Math.round(Number(draft.value || 0) * 100)
  const payload: Record<string, unknown> = {
    contact_id: draft.contact_id,
    pipeline_id: draft.pipeline_id,
    stage_id: draft.stage_id,
    title: draft.title.trim(),
    status: 'open',
    source: opts.source,
    value_minor: Number.isFinite(valueMinor) ? valueMinor : 0,
    currency: draft.currency,
    idempotency_key: opts.idempotencyKey,
  }
  const sourceReference = opts.sourceReference?.trim()
  if (sourceReference) payload.source_reference = sourceReference
  const nextActionAt = toIso(draft.follow_up_at)
  if (nextActionAt) payload.next_action_at = nextActionAt
  return payload
}

export function buildFollowUpTaskPayload(opts: {
  contactId: string
  leadId: string
  leadTitle: string
  dueAt: string
  source: string
  idempotencyKey: string
}): Record<string, unknown> {
  return {
    contact_id: opts.contactId,
    lead_id: opts.leadId,
    // Task titles are capped at 255 characters by the API.
    title: Array.from(`Follow up: ${opts.leadTitle.trim()}`).slice(0, 255).join(''),
    description: '',
    priority: 'normal',
    due_at: toIso(opts.dueAt),
    source: opts.source,
    idempotency_key: opts.idempotencyKey,
  }
}

export function buildLostReason(reason: string, note: string): string {
  const base = reason.trim()
  const trimmedNote = note.trim()
  const combined = trimmedNote ? `${base}: ${trimmedNote}` : base
  // Count code points like the backend's utf8.RuneCountInString.
  const chars = Array.from(combined)
  return chars.length > LOST_REASON_MAX_LENGTH ? chars.slice(0, LOST_REASON_MAX_LENGTH).join('') : combined
}
