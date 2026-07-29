import { api } from '@/services/api'
import { unwrapListResponse } from '@/lib/api-utils'
import type { AxiosResponse } from 'axios'

const PRODUCT_PAGE_LIMIT = 100
const PRODUCT_MAX_PAGES = 200

type APIEnvelope<T> = {
  data: T
}

function listTotal(response: AxiosResponse): number | null {
  const payload = response.data?.data ?? response.data
  return typeof payload?.total === 'number' ? payload.total : null
}

async function fetchAllPages<T>(
  key: string,
  request: (page: number, limit: number) => Promise<AxiosResponse>,
): Promise<T[]> {
  const collected: T[] = []
  for (let page = 1; page <= PRODUCT_MAX_PAGES; page += 1) {
    const response = await request(page, PRODUCT_PAGE_LIMIT)
    const items = unwrapListResponse<T>(response, key)
    collected.push(...items)
    const total = listTotal(response)
    if (
      items.length < PRODUCT_PAGE_LIMIT ||
      (total !== null && collected.length >= total)
    ) {
      return collected
    }
  }
  throw new Error(`Too many ${key} records to load safely`)
}

export type Money = {
  amount_minor: number
  currency: string
}

export type BillingInterval = 'one_time' | 'month' | 'year'

export interface PlanPriceSummary {
  id: string
  code: string
  currency: string
  unit_amount_minor: number
  setup_amount_minor: number
  interval: BillingInterval
  interval_count: number
  tax_behavior: string
}

export interface PlanSummary {
  id: string
  code: string
  name: string
  description?: string
  vertical: string
  status: 'draft' | 'active' | 'archived'
  trial_days: number
  is_public: boolean
  entitlements: Record<string, unknown>
  prices: PlanPriceSummary[]
}

export interface SubscriptionSummary {
  id?: string
  plan_id?: string
  plan_price_id?: string
  plan_code: string
  plan_name?: string
  status: string
  provider: string
  trial_ends_at?: string
  current_period_start?: string
  current_period_end?: string
  grace_until?: string
  cancel_at_period_end?: boolean
  cancel_at?: string
}

export interface SetOrganizationSubscriptionRequest {
  plan_id?: string
  plan_code?: string
  plan_price_id?: string
  price_code?: string
  status: 'active' | 'trialing'
  trial_days?: number
  manual_reference: string
}

export interface OnboardingStep {
  key: string
  label: string
  description: string
  completed: boolean
  inferred: boolean
  action_path?: string
}

export interface OnboardingSummary {
  id?: string
  vertical: string
  status: string
  current_step?: string
  progress_percent: number
  steps: OnboardingStep[]
  profile: Record<string, unknown>
}

export interface WorkspaceTemplateSummary {
  key: string
  name: string
  vertical: 'clinic' | 'pharmacy' | 'wellness' | 'general'
  description: string
  version: number
  highlights: string[]
}

export interface RetentionPolicy {
  id: string
  data_category: string
  retention_days: number
  grace_period_days?: number
  action: 'delete' | 'anonymize' | 'archive' | 'review'
  legal_basis?: string
  is_enabled: boolean
}

export interface PrivacyRequest {
  id: string
  request_number?: string
  contact_id?: string
  request_type:
    | 'access' | 'portability' | 'correction' | 'restriction' | 'erasure'
  status: string
  verification_status?: string
  due_at?: string
  assigned_user_id?: string
  resolution?: string
  created_at: string
}

export interface SupportCase {
  id: string
  case_number?: string
  title: string
  description: string
  severity: 'low' | 'normal' | 'high' | 'critical'
  category?: string
  status:
    | 'open' | 'investigating' | 'waiting' | 'waiting_customer' | 'waiting_internal' | 'resolved' | 'closed'
  assigned_user_id?: string
  resolution?: string
  created_at: string
  updated_at: string
}

export interface TenantHealth {
  status: 'healthy' | 'attention' | 'degraded'
  checks: Array<{
    key: string
    label: string
    status: 'pass' | 'warn' | 'fail' | 'not_configured'
    detail: string
  }>
  checked_at: string
}

export interface PipelineStage {
  id: string
  pipeline_id: string
  name: string
  color: string
  display_order: number
  kind: 'open' | 'won' | 'lost'
  probability: number
}

export interface Pipeline {
  id: string
  name: string
  description?: string
  is_default: boolean
  is_active: boolean
  stages: PipelineStage[]
}

export interface CRMLead {
  id: string
  contact_id: string
  pipeline_id: string
  stage_id: string
  title: string
  status: 'open' | 'won' | 'lost' | 'archived'
  owner_user_id?: string
  source?: string
  source_reference?: string
  value_minor: number
  currency: string
  next_action_at?: string
  expected_close_date?: string
  last_activity_at?: string
  lost_reason?: string
  metadata?: Record<string, unknown>
  idempotency_key?: string
  version: number
  contact?: {
    id: string
    profile_name?: string
    phone_number?: string
  }
  pipeline?: {
    id: string
    name: string
  }
  stage?: {
    id: string
    name: string
    color?: string
    kind?: 'open' | 'won' | 'lost'
  }
  owner?: {
    id: string
    name?: string
    full_name?: string
  }
}

export interface FollowUpTask {
  id: string
  contact_id?: string
  lead_id?: string
  booking_id?: string
  title: string
  description?: string
  status: 'open' | 'in_progress' | 'completed' | 'cancelled'
  priority: 'low' | 'normal' | 'high' | 'urgent'
  owner_user_id?: string
  due_at?: string
  remind_at?: string
  completed_at?: string
  source?: string
  metadata?: Record<string, unknown>
  idempotency_key?: string
  version: number
}

export interface BookingService {
  id: string
  name: string
  description?: string
  kind: 'appointment' | 'class'
  duration_minutes: number
  buffer_before_minutes?: number
  buffer_after_minutes?: number
  default_capacity: number
  price_minor: number
  currency: string
  is_active: boolean
  reminder_policy?: Record<string, unknown>
  metadata?: Record<string, unknown>
  resource_ids?: string[]
  version: number
}

export interface BookingResource {
  id: string
  user_id?: string
  name: string
  kind: 'practitioner' | 'room' | 'equipment' | 'instructor'
  timezone: string
  location?: string
  is_active: boolean
  metadata?: Record<string, unknown>
  version: number
}

export interface BookingEvent {
  id: string
  service_id: string
  resource_id: string
  starts_at: string
  ends_at: string
  capacity: number
  booked_quantity?: number
  status: 'scheduled' | 'completed' | 'cancelled'
  location?: string
  service?: BookingService
  resource?: BookingResource
  version: number
}

export interface Booking {
  id: string
  event_id: string
  contact_id: string
  status:
    | 'reserved' | 'confirmed' | 'waitlisted' | 'checked_in' | 'completed' | 'no_show' | 'cancelled'
  quantity: number
  source: string
  notes?: string
  contact_package_id?: string
  idempotency_key?: string
  allow_waitlist?: boolean
  created_at: string
  version: number
  contact?: {
    id: string
    profile_name?: string
    phone_number?: string
  }
  event?: BookingEvent
}

export interface PackageDefinition {
  id: string
  name: string
  description?: string
  price_minor: number
  currency: string
  validity_days: number
  is_active: boolean
  version: number
  entitlements?: Array<{
    id?: string
    booking_service_id: string
    credits: number
    is_unlimited: boolean
  }>
}

export interface ContactPackage {
  id: string
  contact_id: string
  package_definition_id: string
  invoice_id?: string
  status:
    | 'pending' | 'active' | 'exhausted' | 'expired' | 'cancelled' | 'refunded'
  starts_at?: string
  expires_at?: string
  purchase_amount_minor: number
  currency: string
  source?: string
  version: number
  contact?: {
    id: string
    profile_name?: string
    phone_number?: string
  }
  package_definition?: PackageDefinition
  balances?: Array<{
    id: string
    granted: number
    reserved: number
    consumed: number
    available: number
    version: number
  }>
}

export interface CommerceInvoice {
  id: string
  contact_id: string
  invoice_number: string
  idempotency_key?: string
  status: string
  currency: string
  total_minor: number
  paid_minor: number
  due_minor: number
  issued_at?: string
  due_at?: string
  paid_at?: string
  version: number
}

export interface CustomerWorkspaceContact {
  id: string
  phone_number: string
  name?: string
  profile_name?: string
  avatar_url?: string
  status?: string
  tags?: string[]
  metadata?: Record<string, unknown>
  assigned_user_id?: string
  marketing_opt_out?: boolean
  created_at?: string
  updated_at?: string
}

export interface CustomerIdentity {
  id: string
  channel: ChannelType | string
  display_name?: string
  address?: string
  normalized_address?: string
  verified?: boolean
  is_verified?: boolean
  first_seen_at?: string
  last_seen_at?: string
}

export interface CustomerWorkspacePayment {
  id: string
  invoice_id?: string
  type: string
  status: string
  amount_minor: number
  currency: string
  occurred_at: string
}

export interface CustomerTimelineEvent {
  id: string
  type?: string
  event_type?: string
  category: string
  title: string
  summary?: string
  occurred_at: string
  source_type?: string
  source_id?: string
  source_object_type?: string
  source_object_id?: string
  actor?: {
    id: string
    name: string
  }
  actor_type?: string
  actor_user_id?: string
  metadata?: Record<string, unknown>
}

export interface CustomerWorkspaceCapabilities {
  crm: boolean
  tasks: boolean
  bookings: boolean
  packages: boolean
  payments: boolean
  copilot: boolean
  merge: boolean
}

export interface CustomerWorkspace {
  contact: CustomerWorkspaceContact
  contact_ids?: string[]
  capabilities?: Partial<CustomerWorkspaceCapabilities>
  permissions?: {
    crm?: boolean
    tasks?: boolean
    bookings?: boolean
    commerce?: boolean
    packages?: boolean
    payments?: boolean
    copilot?: boolean
  }
  identities?: CustomerIdentity[]
  journeys?: CRMLead[]
  tasks?: FollowUpTask[]
  bookings?: Booking[]
  packages?: ContactPackage[]
  invoices?: CommerceInvoice[]
  payments?: CustomerWorkspacePayment[]
  summary?: {
    open_journeys?: number
    journey_count?: number
    pipeline_value?: Money[]
    overdue_tasks?: number
    open_task_count?: number
    next_booking?: Booking
    booking_count?: number
    active_packages?: number
    active_package_count?: number
    available_credits?: number
    package_expiring_soon?: number
    outstanding?: Money[]
    collected?: Money[]
    outstanding_minor?: number
    lifetime_value_minor?: number
    currency?: string
    last_activity_at?: string
  }
  timeline?: CustomerTimelineEvent[]
}

export interface CommerceSummary {
  active_packages: number
  packages_visible: boolean
  outstanding: Array<{ currency: string; amount_minor: number }>
  collected_charges: Array<{ currency: string; amount_minor: number }>
}

export interface CopilotRun {
  id: string
  contact_id?: string
  task_type: 'reply' | 'summary' | 'qualify' | 'extract_actions'
  status: string
  model: string
  result_text?: string
  structured_result?: Record<string, unknown>
  source_names?: string[]
  safety_warnings?: string[]
  created_at: string
}

export type ChannelType =
  | 'whatsapp' | 'instagram' | 'messenger' | 'threads'
  | 'email' | 'webchat' | 'tiktok'

export interface ChannelAccount {
  id: string
  channel: ChannelType
  provider: string
  name: string
  external_account_id?: string
  status: 'pending' | 'active' | 'degraded' | 'suspended' | 'disconnected'
  capabilities: Record<string, boolean>
  config: Record<string, unknown>
  has_credentials: boolean
  token_expires_at?: string
  last_health_check_at?: string
  last_inbound_at?: string
  last_outbound_at?: string
  last_error_at?: string
  last_error?: string
  outbox_pending: number
  outbox_failed: number
}

export interface InboxConversation {
  id: string
  channel_account_id: string
  contact_id: string
  channel: ChannelType
  external_conversation_id: string
  subject?: string
  status: string
  last_message_preview?: string
  last_message_at?: string
  unread_count: number
  ai_paused: boolean
  ai_pause_reason?: string
  assigned_user_id?: string
  metadata?: Record<string, unknown>
  contact?: {
    id: string
    profile_name?: string
    phone_number?: string
  }
  contact_identity?: {
    display_name?: string
    external_id?: string
    address?: string
    normalized_address?: string
  }
}

export const productService = {
  plans: () => api.get<APIEnvelope<{ plans: PlanSummary[] }>>('/product/plans'),
  subscription: () => api.get<APIEnvelope<SubscriptionSummary>>('/product/subscription'),
  entitlements: () => api.get('/product/entitlements'),
}

export const organizationSubscriptionService = {
  plans: (organizationId: string) =>
    api.get<APIEnvelope<{ plans: PlanSummary[] }>>(
      `/admin/organizations/${encodeURIComponent(organizationId)}/product/plans`,
    ),
  get: (organizationId: string) =>
    api.get<APIEnvelope<SubscriptionSummary>>(
      `/admin/organizations/${encodeURIComponent(organizationId)}/subscription`,
    ),
  set: (organizationId: string, data: SetOrganizationSubscriptionRequest) =>
    api.put<APIEnvelope<SubscriptionSummary>>(
      `/admin/organizations/${encodeURIComponent(organizationId)}/subscription`,
      data,),
}

export const onboardingService = {
  get: () => api.get('/onboarding'),
  updateProfile: (profile: Record<string, unknown>) => api.put('/onboarding/profile', { profile }),
  completeStep: (key: string) => api.post(`/onboarding/steps/${encodeURIComponent(key)}/complete`),
  templates: () => api.get('/workspace-templates'),
  applyTemplate: (key: string) => api.post(`/workspace-templates/${encodeURIComponent(key)}/apply`),
}

export const privacyService = {
  settings: () => api.get('/privacy/settings'),
  updateSettings: (data: Record<string, unknown>) => api.put('/privacy/settings', data),
  consents: (params?: Record<string, string | number | boolean>) => api.get('/privacy/consents', { params }),
  recordConsent: (data: Record<string, unknown>) => api.post('/privacy/consents', data),
  requests: (params?: Record<string, string | number>) => api.get('/privacy/requests', { params }),
  allRequests: (params?: Record<string, string | number>) =>
    fetchAllPages<PrivacyRequest>('requests', (page, limit) =>
      api.get('/privacy/requests', { params: { ...params, page, limit } }),
    ),
  createRequest: (data: Record<string, unknown>) => api.post('/privacy/requests', data),
  updateRequest: (id: string, data: Record<string, unknown>) => api.put(`/privacy/requests/${id}`, data),
}

export const supportService = {
  summary: () => api.get('/trust/summary'),
  health: () => api.get('/support/health'),
  cases: (params?: Record<string, string | number>) => api.get('/support/cases', { params }),
  allCases: (params?: Record<string, string | number>) =>
    fetchAllPages<SupportCase>('cases', (page, limit) =>
      api.get('/support/cases', { params: { ...params, page, limit } }),
    ),
  createCase: (data: Pick<SupportCase, 'title' | 'description' | 'severity'>) => api.post('/support/cases', data),
  updateCase: (id: string, data: Partial<SupportCase>) => api.put(`/support/cases/${id}`, data),
  recovery: () => api.get('/support/recovery'),
}

export const crmService = {
  pipelines: () => api.get('/crm/pipelines'),
  createPipeline: (data: Partial<Pipeline>) => api.post('/crm/pipelines', data),
  leads: (params?: Record<string, string | number | boolean>) => api.get('/crm/leads', { params }),
  allLeads: (params?: Record<string, string | number | boolean>) =>
    fetchAllPages<CRMLead>('leads', (page, limit) =>
      api.get('/crm/leads', { params: { ...params, page, limit } }),
    ),
  createLead: (data: Partial<CRMLead>) => api.post('/crm/leads', data),
  updateLead: (id: string, data: Partial<CRMLead>) => api.put(`/crm/leads/${id}`, data),
  moveLead: (id: string, stageId: string, version: number) =>
    api.put(`/crm/leads/${id}/move`, { stage_id: stageId, version }),
  tasks: (params?: Record<string, string | number | boolean>) => api.get('/tasks', { params }),
  allTasks: (params?: Record<string, string | number | boolean>) =>
    fetchAllPages<FollowUpTask>('tasks', (page, limit) =>
      api.get('/tasks', { params: { ...params, page, limit } }),
    ),
  createTask: (data: Partial<FollowUpTask>) => api.post('/tasks', data),
  updateTask: (id: string, data: Partial<FollowUpTask>) => api.put(`/tasks/${id}`, data),
  completeTask: (id: string, version: number) => api.put(`/tasks/${id}/complete`, { version }),
}

export const customerWorkspaceService = {
  get: (contactId: string) =>
    api.get(`/contacts/${encodeURIComponent(contactId)}/workspace`),
}

export const bookingService = {
  services: () => api.get('/booking/services'),
  allServices: () =>
    fetchAllPages<BookingService>('services', (page, limit) =>
      api.get('/booking/services', { params: { page, limit } }),
    ),
  createService: (data: Partial<BookingService>) => api.post('/booking/services', data),
  updateService: (id: string, data: Partial<BookingService>) => api.put(`/booking/services/${id}`, data),
  resources: () => api.get('/booking/resources'),
  allResources: () =>
    fetchAllPages<BookingResource>('resources', (page, limit) =>
      api.get('/booking/resources', { params: { page, limit } }),
    ),
  createResource: (data: Partial<BookingResource>) => api.post('/booking/resources', data),
  updateResource: (id: string, data: Partial<BookingResource>) => api.put(`/booking/resources/${id}`, data),
  events: (params?: Record<string, string | number>) => api.get('/booking/events', { params }),
  allEvents: (params?: Record<string, string | number>) =>
    fetchAllPages<BookingEvent>('events', (page, limit) =>
      api.get('/booking/events', { params: { ...params, page, limit } }),
    ),
  createEvent: (
    data: Partial<BookingEvent> & {
      local_starts_at?: string
      local_ends_at?: string
      timezone?: string
    },
  ) => api.post('/booking/events', data),
  bookings: (params?: Record<string, string | number>) => api.get('/bookings', { params }),
  allBookings: (params?: Record<string, string | number>) =>
    fetchAllPages<Booking>('bookings', (page, limit) =>
      api.get('/bookings', { params: { ...params, page, limit } }),
    ),
  createBooking: (eventId: string, data: Record<string, unknown>) =>
    api.post(`/booking/events/${eventId}/bookings`, data),
  transitionBooking: (id: string, transition: string, data?: Record<string, unknown>,) =>
    api.post(`/bookings/${id}/${encodeURIComponent(transition)}`, data ?? {}),
}

export const commerceService = {
  summary: () => api.get('/commerce/summary'),
  packages: () => api.get('/packages'),
  allPackages: () =>
    fetchAllPages<PackageDefinition>('packages', (page, limit) =>
      api.get('/packages', { params: { page, limit } }),
    ),
  createPackage: (data: Partial<PackageDefinition>) => api.post('/packages', data),
  contactPackages: (params?: Record<string, string | number>) =>
    api.get('/contact-packages', { params }),
  allContactPackages: (params?: Record<string, string | number>) =>
    fetchAllPages<ContactPackage>('contact_packages', (page, limit) =>
      api.get('/contact-packages', { params: { ...params, page, limit } }),
    ),
  createContactPackage: (data: Record<string, unknown>) =>
    api.post('/contact-packages', data),
  sellPackage: (data: Record<string, unknown>) => api.post('/package-sales', data),
  invoices: (params?: Record<string, string | number>) => api.get('/invoices', { params }),
  allInvoices: (params?: Record<string, string | number>) =>
    fetchAllPages<CommerceInvoice>('invoices', (page, limit) =>
      api.get('/invoices', { params: { ...params, page, limit } }),
    ),
  createInvoice: (data: Record<string, unknown>) => api.post('/invoices', data),
  createPaymentIntent: (invoiceId: string, data?: Record<string, unknown>) =>
    api.post(`/invoices/${invoiceId}/payment-intents`, data ?? {}),
  recordManualPayment: (invoiceId: string, data: Record<string, unknown>) =>
    api.post(`/invoices/${invoiceId}/manual-payments`, data),
  payments: (params?: Record<string, string | number>) => api.get('/payments', { params }),
  allPayments: <T = Record<string, unknown>>(params?: Record<string, string | number>,) =>
    fetchAllPages<T>('payments', (page, limit) =>
      api.get('/payments', { params: { ...params, page, limit } }),
    ),
}

export const copilotService = {
  run: (contactId: string, taskType: CopilotRun['task_type'], data?: Record<string, unknown>,) =>
    api.post(`/contacts/${contactId}/copilot/${taskType}`, data ?? {}),
  runs: (params?: { contact_id?: string; page?: number; limit?: number }) => api.get('/copilot/runs', { params }),
  feedback: (runId: string, data: Record<string, unknown>) => api.post(`/copilot/runs/${runId}/feedback`, data),
}

export const channelsService = {
  accounts: () => api.get('/channel-accounts'),
  createAccount: (data: Record<string, unknown>) => api.post('/channel-accounts', data),
  updateAccount: (id: string, data: Record<string, unknown>) => api.put(`/channel-accounts/${id}`, data),
  testAccount: (id: string) => api.post(`/channel-accounts/${id}/test`),
  disconnectAccount: (id: string) => api.delete(`/channel-accounts/${id}`),
  conversations: (params?: Record<string, string | number | boolean>) => api.get('/conversations', { params }),
  allConversations: (params?: Record<string, string | number | boolean>) =>
    fetchAllPages<InboxConversation>('conversations', (page, limit) =>
      api.get('/conversations', { params: { ...params, page, limit } }),
    ),
  messages: (id: string, params?: { before?: string; limit?: number }) =>
    api.get(`/conversations/${id}/messages`, { params }),
  send: (id: string, data: Record<string, unknown>) => api.post(`/conversations/${id}/messages`, data),
  markRead: (id: string) => api.post(`/conversations/${id}/read`),
  setAIState: (id: string, paused: boolean) =>
    api.put(`/conversations/${id}/ai`, { paused }),
}
