/** @vitest-environment happy-dom */

import { afterEach, describe, expect, it, vi } from 'vitest'
import type { CommerceInvoice, CRMLead, FollowUpTask, Pipeline, PipelineStage } from '@/services/productSuite'
import {
  LOST_REASONS,
  LOST_REASON_MAX_LENGTH,
  activePipelines,
  buildCreateLeadPayload,
  buildFollowUpTaskPayload,
  buildLostReason,
  defaultPipelineId,
  firstOpenStage,
  followUpPresetDate,
  followUpUrgency,
  invoiceForLead,
  isFollowUpDueToday,
  isFollowUpOverdue,
  leadFlowState,
  leadSourceForChannel,
  lostStage,
  nextFollowUpForLead,
  openStages,
  pickFocusLead,
  rememberPipelineId,
  rememberedPipelineId,
  stageChainLabel,
  wonStage,
  type LeadDraft,
} from './crmFlow'

function stage(overrides: Partial<PipelineStage> & { id: string; name: string }): PipelineStage {
  return {
    pipeline_id: 'pipeline-1',
    color: '#ffffff',
    display_order: 0,
    kind: 'open',
    probability: 0,
    sla_hours: 0,
    is_active: true,
    version: 1,
    ...overrides,
  }
}

function pipeline(overrides: Partial<Pipeline> & { id: string; name: string }): Pipeline {
  return {
    is_default: false,
    is_active: true,
    stages: [],
    ...overrides,
  }
}

function lead(overrides: Partial<CRMLead> & { id: string }): CRMLead {
  return {
    contact_id: 'contact-1',
    pipeline_id: 'pipeline-1',
    stage_id: 'stage-new',
    title: 'Test lead',
    status: 'open',
    value_minor: 0,
    currency: 'MYR',
    version: 1,
    ...overrides,
  }
}

function task(overrides: Partial<FollowUpTask> & { id: string }): FollowUpTask {
  return {
    title: 'Follow up',
    status: 'open',
    priority: 'normal',
    version: 1,
    ...overrides,
  }
}

function invoice(overrides: Partial<CommerceInvoice> & { id: string }): CommerceInvoice {
  return {
    contact_id: 'contact-1',
    invoice_number: 'INV-1',
    status: 'open',
    currency: 'MYR',
    total_minor: 1000,
    paid_minor: 0,
    due_minor: 1000,
    version: 1,
    ...overrides,
  }
}

const consultation = pipeline({
  id: 'pipeline-1',
  name: 'Consultation',
  stages: [
    stage({ id: 'stage-won', name: 'Won', kind: 'won', display_order: 4 }),
    stage({ id: 'stage-contacted', name: 'Contacted', display_order: 2 }),
    stage({ id: 'stage-old', name: 'Old', display_order: 1, is_active: false }),
    stage({ id: 'stage-new', name: 'New', display_order: 1 }),
    stage({ id: 'stage-qualified', name: 'Qualified', display_order: 3 }),
    stage({ id: 'stage-lost-old', name: 'Lost (old)', kind: 'lost', display_order: 5, is_active: false }),
    stage({ id: 'stage-lost', name: 'Lost', kind: 'lost', display_order: 6 }),
  ],
})

describe('LOST_REASONS', () => {
  it('lists the plain-language reasons', () => {
    expect(LOST_REASONS).toEqual([
      'Price',
      'Timing not right',
      'Not suitable',
      'No response',
      'Chose another provider',
      'Other',
    ])
  })
})

describe('activePipelines', () => {
  it('drops inactive pipelines and sorts by display order then name', () => {
    const result = activePipelines([
      pipeline({ id: 'b', name: 'Bravo', display_order: 2 }),
      pipeline({ id: 'z', name: 'Zulu', display_order: 1 }),
      pipeline({ id: 'a', name: 'Alpha', display_order: 2 }),
      pipeline({ id: 'x', name: 'Hidden', display_order: 0, is_active: false }),
    ])
    expect(result.map((item) => item.id)).toEqual(['z', 'a', 'b'])
  })

  it('filters and sorts stages without mutating the input', () => {
    const [result] = activePipelines([consultation])
    expect(result.stages.map((item) => item.id)).toEqual([
      'stage-new',
      'stage-contacted',
      'stage-qualified',
      'stage-won',
      'stage-lost',
    ])
    expect(consultation.stages[0].id).toBe('stage-won')
    expect(consultation.stages).toHaveLength(7)
  })

  it('treats a missing is_active as active', () => {
    const legacy = { id: 'legacy', name: 'Legacy', is_default: false, stages: [] } as unknown as Pipeline
    expect(activePipelines([legacy])).toHaveLength(1)
  })
})

describe('stage helpers', () => {
  it('returns active open stages in order', () => {
    expect(openStages(consultation).map((item) => item.name)).toEqual(['New', 'Contacted', 'Qualified'])
    expect(firstOpenStage(consultation)?.id).toBe('stage-new')
  })

  it('finds the first active won and lost stages', () => {
    expect(wonStage(consultation)?.id).toBe('stage-won')
    expect(lostStage(consultation)?.id).toBe('stage-lost')
  })

  it('handles a missing pipeline', () => {
    expect(openStages(null)).toEqual([])
    expect(firstOpenStage(undefined)).toBeNull()
    expect(wonStage(null)).toBeNull()
    expect(lostStage(null)).toBeNull()
    expect(stageChainLabel(null)).toBe('')
  })

  it('builds the stage chain label with arrows', () => {
    expect(stageChainLabel(consultation)).toBe('New → Contacted → Qualified')
  })
})

describe('pickFocusLead', () => {
  it('returns null for no leads', () => {
    expect(pickFocusLead(undefined)).toBeNull()
    expect(pickFocusLead([])).toBeNull()
  })

  it('ignores archived leads', () => {
    expect(pickFocusLead([lead({ id: 'archived', status: 'archived' })])).toBeNull()
  })

  it('prefers the most recently active open lead', () => {
    const result = pickFocusLead([
      lead({ id: 'won', status: 'won', won_at: '2026-09-30T00:00:00Z' }),
      lead({ id: 'older', last_activity_at: '2026-09-01T00:00:00Z' }),
      lead({ id: 'newer', last_activity_at: '2026-09-10T00:00:00Z' }),
    ])
    expect(result?.id).toBe('newer')
  })

  it('falls back to updated_at and created_at for open leads', () => {
    expect(
      pickFocusLead([
        lead({ id: 'a', updated_at: '2026-09-01T00:00:00Z' }),
        lead({ id: 'b', updated_at: '2026-09-05T00:00:00Z' }),
      ])?.id,
    ).toBe('b')
    expect(
      pickFocusLead([
        lead({ id: 'a', updated_at: '2026-09-05T00:00:00Z', created_at: '2026-09-01T00:00:00Z' }),
        lead({ id: 'b', updated_at: '2026-09-05T00:00:00Z', created_at: '2026-09-02T00:00:00Z' }),
      ])?.id,
    ).toBe('b')
  })

  it('falls back to the newest won lead, then the newest lost lead', () => {
    expect(
      pickFocusLead([
        lead({ id: 'lost', status: 'lost', lost_at: '2026-09-30T00:00:00Z' }),
        lead({ id: 'won-old', status: 'won', won_at: '2026-09-01T00:00:00Z' }),
        lead({ id: 'won-new', status: 'won', won_at: '2026-09-02T00:00:00Z' }),
      ])?.id,
    ).toBe('won-new')
    expect(
      pickFocusLead([
        lead({ id: 'lost-old', status: 'lost', lost_at: '2026-09-01T00:00:00Z' }),
        lead({ id: 'lost-new', status: 'lost', updated_at: '2026-09-03T00:00:00Z', lost_at: '2026-09-03T00:00:00Z' }),
      ])?.id,
    ).toBe('lost-new')
  })
})

describe('leadFlowState', () => {
  it('maps lead status to a flow state', () => {
    expect(leadFlowState(null)).toBe('none')
    expect(leadFlowState(lead({ id: '1', status: 'open' }))).toBe('open')
    expect(leadFlowState(lead({ id: '1', status: 'won' }))).toBe('won')
    expect(leadFlowState(lead({ id: '1', status: 'lost' }))).toBe('lost')
    expect(leadFlowState(lead({ id: '1', status: 'archived' }))).toBe('none')
  })
})

describe('invoiceForLead', () => {
  it('returns the newest invoice linked to the lead', () => {
    const invoices = [
      invoice({ id: 'other', metadata: { lead_id: 'lead-2' }, issued_at: '2026-09-30T00:00:00Z' }),
      invoice({ id: 'old', metadata: { lead_id: 'lead-1' }, issued_at: '2026-09-01T00:00:00Z' }),
      invoice({ id: 'new', metadata: { lead_id: 'lead-1' }, issued_at: '2026-09-15T00:00:00Z' }),
      invoice({ id: 'unlinked', issued_at: '2026-09-20T00:00:00Z' }),
    ]
    expect(invoiceForLead(invoices, 'lead-1')?.id).toBe('new')
  })

  it('returns null when nothing matches', () => {
    expect(invoiceForLead(undefined, 'lead-1')).toBeNull()
    expect(invoiceForLead([invoice({ id: 'x' })], 'lead-1')).toBeNull()
    expect(invoiceForLead([invoice({ id: 'x', metadata: { lead_id: '' } })], '')).toBeNull()
  })
})

describe('nextFollowUpForLead', () => {
  const focus = lead({ id: 'lead-1', next_action_at: '2026-10-09T02:00:00Z' })

  it('returns null without a lead', () => {
    expect(nextFollowUpForLead([task({ id: 't', lead_id: 'lead-1' })], null)).toBeNull()
  })

  it('picks the earliest due open or in-progress task for the lead', () => {
    const result = nextFollowUpForLead(
      [
        task({ id: 'later', lead_id: 'lead-1', due_at: '2026-10-05T02:00:00Z' }),
        task({ id: 'done', lead_id: 'lead-1', status: 'completed', due_at: '2026-10-01T02:00:00Z' }),
        task({ id: 'other-lead', lead_id: 'lead-2', due_at: '2026-10-01T02:00:00Z' }),
        task({ id: 'earliest', lead_id: 'lead-1', status: 'in_progress', due_at: '2026-10-03T02:00:00Z' }),
      ],
      focus,
    )
    expect(result?.task?.id).toBe('earliest')
    expect(result?.dueAt).toBe('2026-10-03T02:00:00Z')
  })

  it('falls back to the lead next action date', () => {
    expect(nextFollowUpForLead([], focus)).toEqual({ dueAt: '2026-10-09T02:00:00Z' })
  })

  it('returns an undated task when nothing is dated', () => {
    const undated = task({ id: 'undated', lead_id: 'lead-1' })
    expect(nextFollowUpForLead([undated], lead({ id: 'lead-1' }))).toEqual({ task: undated })
  })

  it('returns null when there is nothing scheduled', () => {
    expect(nextFollowUpForLead(undefined, lead({ id: 'lead-1' }))).toBeNull()
  })
})

describe('followUpUrgency', () => {
  const now = new Date(2026, 9, 1, 12, 0, 0)

  it('classifies due dates', () => {
    expect(followUpUrgency(undefined, now)).toBe('none')
    expect(followUpUrgency(null, now)).toBe('none')
    expect(followUpUrgency('not a date', now)).toBe('none')
    expect(followUpUrgency(new Date(2026, 8, 30, 18, 0).toISOString(), now)).toBe('overdue')
    expect(followUpUrgency(new Date(2026, 8, 30, 23, 59, 59).toISOString(), now)).toBe('overdue')
    expect(followUpUrgency(new Date(2026, 9, 1, 0, 0).toISOString(), now)).toBe('today')
    expect(followUpUrgency(new Date(2026, 9, 1, 17, 0).toISOString(), now)).toBe('today')
    expect(followUpUrgency(new Date(2026, 9, 1, 23, 59, 59).toISOString(), now)).toBe('today')
    expect(followUpUrgency(new Date(2026, 9, 2, 0, 0).toISOString(), now)).toBe('upcoming')
    expect(followUpUrgency(new Date(2026, 9, 2, 9, 0).toISOString(), now)).toBe('upcoming')
  })

  it('keeps a follow-up due earlier today as today, not overdue', () => {
    const afternoon = new Date(2026, 9, 1, 15, 0, 0)
    const dueThisMorning = new Date(2026, 9, 1, 9, 0).toISOString()
    expect(followUpUrgency(dueThisMorning, afternoon)).toBe('today')
    expect(isFollowUpOverdue(dueThisMorning, afternoon)).toBe(false)
    expect(isFollowUpDueToday(dueThisMorning, afternoon)).toBe(true)
  })

  it('becomes overdue from the next day', () => {
    const nextMorning = new Date(2026, 9, 2, 8, 0, 0)
    const dueYesterday = new Date(2026, 9, 1, 9, 0).toISOString()
    expect(followUpUrgency(dueYesterday, nextMorning)).toBe('overdue')
    expect(isFollowUpOverdue(dueYesterday, nextMorning)).toBe(true)
    expect(isFollowUpDueToday(dueYesterday, nextMorning)).toBe(false)
  })

  it('wrappers return false without a valid date', () => {
    expect(isFollowUpOverdue(undefined, now)).toBe(false)
    expect(isFollowUpOverdue('nope', now)).toBe(false)
    expect(isFollowUpDueToday(null, now)).toBe(false)
    expect(isFollowUpDueToday(new Date(2026, 9, 3, 9, 0).toISOString(), now)).toBe(false)
  })
})

describe('followUpPresetDate', () => {
  const now = new Date(2026, 9, 1, 15, 45, 30)

  it('returns 10:00 local time on the preset day', () => {
    expect(followUpPresetDate('tomorrow', now)).toEqual(new Date(2026, 9, 2, 10, 0, 0, 0))
    expect(followUpPresetDate('3days', now)).toEqual(new Date(2026, 9, 4, 10, 0, 0, 0))
    expect(followUpPresetDate('week', now)).toEqual(new Date(2026, 9, 8, 10, 0, 0, 0))
  })

  it('rolls over month ends without mutating now', () => {
    const endOfMonth = new Date(2026, 9, 31, 8, 0)
    expect(followUpPresetDate('tomorrow', endOfMonth)).toEqual(new Date(2026, 10, 1, 10, 0, 0, 0))
    expect(endOfMonth.getDate()).toBe(31)
  })
})

describe('remembered pipeline', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    localStorage.clear()
  })

  it('stores the pipeline per organisation', () => {
    rememberPipelineId('org-1', 'pipeline-2')
    expect(localStorage.getItem('rereply.crm.lastPipeline.org-1')).toBe('pipeline-2')
    expect(rememberedPipelineId('org-1')).toBe('pipeline-2')
    expect(rememberedPipelineId('org-2')).toBeNull()
  })

  it('uses a default key without an organisation', () => {
    rememberPipelineId(undefined, 'pipeline-3')
    expect(localStorage.getItem('rereply.crm.lastPipeline.default')).toBe('pipeline-3')
    expect(rememberedPipelineId(undefined)).toBe('pipeline-3')
  })

  it('survives storage that throws', () => {
    const blocked = () => {
      throw new Error('blocked')
    }
    vi.stubGlobal('localStorage', { getItem: blocked, setItem: blocked, removeItem: blocked })
    expect(() => rememberPipelineId('org-1', 'pipeline-1')).not.toThrow()
    expect(rememberedPipelineId('org-1')).toBeNull()
    expect(() => rememberPipelineId('org-1', '')).not.toThrow()
  })

  it('survives a storage accessor that throws', () => {
    const original = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
    Object.defineProperty(globalThis, 'localStorage', {
      configurable: true,
      get() {
        throw new Error('denied')
      },
    })
    try {
      expect(() => rememberPipelineId('org-1', 'pipeline-1')).not.toThrow()
      expect(rememberedPipelineId('org-1')).toBeNull()
    } finally {
      if (original) Object.defineProperty(globalThis, 'localStorage', original)
    }
  })
})

describe('defaultPipelineId', () => {
  const pipelines = [
    pipeline({ id: 'first', name: 'First', display_order: 1 }),
    pipeline({ id: 'default', name: 'Default', display_order: 2, is_default: true }),
    pipeline({ id: 'inactive', name: 'Inactive', display_order: 0, is_active: false }),
  ]

  it('prefers the requested pipeline, then the remembered one', () => {
    expect(defaultPipelineId(pipelines, { preferredId: 'first', rememberedId: 'default' })).toBe('first')
    expect(defaultPipelineId(pipelines, { rememberedId: 'first' })).toBe('first')
  })

  it('ignores ids that are not active pipelines', () => {
    expect(defaultPipelineId(pipelines, { preferredId: 'inactive', rememberedId: 'missing' })).toBe('default')
  })

  it('falls back to the default, then the first active pipeline', () => {
    expect(defaultPipelineId(pipelines, {})).toBe('default')
    expect(defaultPipelineId([pipelines[0]], { rememberedId: null })).toBe('first')
    expect(defaultPipelineId([], {})).toBe('')
  })
})

describe('leadSourceForChannel', () => {
  it('maps WhatsApp case-insensitively', () => {
    expect(leadSourceForChannel('whatsapp')).toBe('whatsapp')
    expect(leadSourceForChannel('WhatsApp')).toBe('whatsapp')
    expect(leadSourceForChannel('instagram')).toBe('other')
    expect(leadSourceForChannel(undefined)).toBe('other')
    expect(leadSourceForChannel(null)).toBe('other')
  })
})

describe('buildCreateLeadPayload', () => {
  const draft: LeadDraft = {
    contact_id: 'contact-1',
    contact_name: 'Test Customer',
    pipeline_id: 'pipeline-1',
    stage_id: 'stage-new',
    title: '  Test Customer – Consultation  ',
    value: '1250.50',
    currency: 'MYR',
    follow_up_at: '',
  }

  it('builds the create payload without optional fields', () => {
    expect(buildCreateLeadPayload(draft, { source: 'whatsapp', idempotencyKey: 'key-1' })).toEqual({
      contact_id: 'contact-1',
      pipeline_id: 'pipeline-1',
      stage_id: 'stage-new',
      title: 'Test Customer – Consultation',
      status: 'open',
      source: 'whatsapp',
      value_minor: 125050,
      currency: 'MYR',
      idempotency_key: 'key-1',
    })
  })

  it('includes the source reference and follow-up when given', () => {
    const followUp = '2026-10-02T10:00'
    const payload = buildCreateLeadPayload(
      { ...draft, value: '', follow_up_at: followUp },
      { source: 'other', sourceReference: 'conversation-1', idempotencyKey: 'key-2' },
    )
    expect(payload.value_minor).toBe(0)
    expect(payload.source_reference).toBe('conversation-1')
    expect(payload.next_action_at).toBe(new Date(followUp).toISOString())
  })

  it('omits an empty source reference', () => {
    const payload = buildCreateLeadPayload(draft, { source: 'other', sourceReference: '', idempotencyKey: 'k' })
    expect('source_reference' in payload).toBe(false)
    expect('next_action_at' in payload).toBe(false)
  })
})

describe('buildFollowUpTaskPayload', () => {
  it('builds the follow-up task payload', () => {
    const dueAt = '2026-10-02T10:00'
    expect(
      buildFollowUpTaskPayload({
        contactId: 'contact-1',
        leadId: 'lead-1',
        leadTitle: 'Consultation',
        dueAt,
        source: 'customer_workspace',
        idempotencyKey: 'key-1',
      }),
    ).toEqual({
      contact_id: 'contact-1',
      lead_id: 'lead-1',
      title: 'Follow up: Consultation',
      description: '',
      priority: 'normal',
      due_at: new Date(dueAt).toISOString(),
      source: 'customer_workspace',
      idempotency_key: 'key-1',
    })
  })

  it('keeps the title within the API limit', () => {
    const payload = buildFollowUpTaskPayload({
      contactId: 'c',
      leadId: 'l',
      leadTitle: 'x'.repeat(255),
      dueAt: '2026-10-02T10:00',
      source: 's',
      idempotencyKey: 'k',
    })
    expect((payload.title as string).length).toBe(255)
  })
})

describe('buildLostReason', () => {
  it('returns the reason alone without a note', () => {
    expect(buildLostReason('Price', '')).toBe('Price')
    expect(buildLostReason('Price', '   ')).toBe('Price')
  })

  it('appends a trimmed note', () => {
    expect(buildLostReason('Other', '  Moved abroad  ')).toBe('Other: Moved abroad')
  })

  it('truncates to the backend limit', () => {
    const result = buildLostReason('Other', 'n'.repeat(5000))
    expect(result).toHaveLength(LOST_REASON_MAX_LENGTH)
    expect(LOST_REASON_MAX_LENGTH).toBe(2000)
  })
})
