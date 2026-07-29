import { api } from '@/services/api'

export type AutomationPolicyStatus = 'draft' | 'active' | 'paused' | 'archived'
export type AutomationNodeType = 'trigger' | 'condition' | 'delay' | 'create_task'
export type AutomationEdgeBranch = 'true' | 'false' | 'always'

export interface AutomationNodePosition {
  x: number
  y: number
}

export interface AutomationPolicyNode {
  id: string
  type: AutomationNodeType
  position: AutomationNodePosition
  config: Record<string, unknown>
}

export interface AutomationPolicyEdge {
  source: string
  target: string
  branch?: AutomationEdgeBranch
}

export interface AutomationPolicyGraph {
  nodes: AutomationPolicyNode[]
  edges: AutomationPolicyEdge[]
}

export interface AutomationPolicyCatalog {
  event_types: string[]
  node_types: AutomationNodeType[]
  condition_fields: string[]
  condition_operators: string[]
  branches: Array<Exclude<AutomationEdgeBranch, 'always'>>
  task_priorities: string[]
  task_owner_modes: string[]
  limits: Record<string, number>
}

export interface AutomationPolicy {
  id: string
  name: string
  description?: string
  status: AutomationPolicyStatus
  version: number
  graph: AutomationPolicyGraph
  active_version_id?: string
  active_version_number?: number
  trigger_event_types?: string[]
  activated_at?: string
  paused_at?: string
  created_at: string
  updated_at: string
  validation_errors?: AutomationPreviewIssue[]
  validation_warnings?: AutomationPreviewIssue[]
}

export interface AutomationPreviewStep {
  node_id: string
  node_type: AutomationNodeType
  status: string
  branch?: AutomationEdgeBranch
  scheduled_at?: string
  detail?: string
  reason_code?: string
}

export interface AutomationPreviewAction {
  node_id: string
  type: 'create_task'
  title: string
  priority: string
  owner: string
  scheduled_at?: string
  due_at?: string
  remind_at?: string
}

export interface AutomationPreviewResult {
  valid: boolean
  errors: Array<string | AutomationPreviewIssue>
  warnings: Array<string | AutomationPreviewIssue>
  trigger_event_type?: string
  trigger_event_types?: string[]
  version?: number
  checksum?: string
  steps: AutomationPreviewStep[]
  actions: AutomationPreviewAction[]
}

export interface AutomationPreviewIssue {
  code?: string
  message: string
  node_id?: string
  edge?: number
}

export interface AutomationPolicyVersion {
  id: string
  policy_id: string
  number: number
  trigger_event_types: string[]
  graph: AutomationPolicyGraph
  checksum: string
  created_by_id?: string
  published_at?: string
  created_at: string
}

export interface AutomationExecutionStep {
  id: string
  node_id: string
  node_type: AutomationNodeType
  status: string
  scheduled_at?: string
  started_at?: string
  completed_at?: string
  task_id?: string
  reason_code?: string
  branch?: AutomationEdgeBranch
  has_error?: boolean
}

export interface AutomationExecution {
  id: string
  policy_id: string
  policy_version_id: string
  policy_version_number: number
  activity_event_id: string
  contact_id?: string
  status: string
  triggered_at: string
  started_at?: string
  completed_at?: string
  steps?: AutomationExecutionStep[]
}

export interface AutomationPolicyCreateInput {
  name: string
  description?: string
  graph: AutomationPolicyGraph
}

export interface AutomationPolicyUpdateInput extends AutomationPolicyCreateInput {
  version: number
}

export interface AutomationPreviewEventInput {
  event_type: string
  category?: string
  source_type?: string
  actor_type?: string
  title?: string
  summary?: string
  metadata?: Record<string, unknown>
  contact?: {
    marketing_opt_out: boolean
  }
}

export interface AutomationPreviewInput {
  version: number
  graph?: AutomationPolicyGraph
  event?: AutomationPreviewEventInput
}

export type AutomationPreviewSimulationInput = Pick<AutomationPreviewInput, 'event'>

export const automationPolicyService = {
  catalog: () => api.get('/automation-policies/catalog'),
  list: (params?: { status?: AutomationPolicyStatus; page?: number; limit?: number }) =>
    api.get('/automation-policies', { params }),
  get: (id: string) => api.get(`/automation-policies/${encodeURIComponent(id)}`),
  create: (data: AutomationPolicyCreateInput) => api.post('/automation-policies', data),
  update: (id: string, data: AutomationPolicyUpdateInput) =>
    api.put(`/automation-policies/${encodeURIComponent(id)}`, data),
  remove: (id: string, version: number) =>
    api.delete(`/automation-policies/${encodeURIComponent(id)}`, {
      data: { version },
    }),
  activate: (id: string, version: number) =>
    api.post(`/automation-policies/${encodeURIComponent(id)}/activate`, { version }),
  pause: (id: string, version: number) =>
    api.post(`/automation-policies/${encodeURIComponent(id)}/pause`, { version }),
  preview: (id: string, data: AutomationPreviewInput) =>
    api.post(`/automation-policies/${encodeURIComponent(id)}/preview`, data),
  versions: (id: string) =>
    api.get(`/automation-policies/${encodeURIComponent(id)}/versions`),
  executions: (id: string, params?: { cursor?: string; limit?: number }) =>
    api.get(`/automation-policies/${encodeURIComponent(id)}/executions`, { params }),
}
