import { api } from '@/services/api'

export interface CRMInsightMoney {
  currency: string
  amount_minor: number
}

export interface CRMInsights {
  range: {
    from: string
    to: string
  }
  pipeline: {
    open_count: number
    won_count: number
    lost_count: number
    conversion_rate: number
    open_value: CRMInsightMoney[]
  }
  bookings: {
    total: number
    completed: number
    no_show: number
    cancelled: number
    attendance_rate: number
    no_show_rate: number
  }
  revenue: {
    collected: CRMInsightMoney[]
    outstanding: CRMInsightMoney[]
    overdue_invoices: number
  }
  packages: {
    active: number
    low_balance: number
    expiring_soon: number
  }
  tasks: {
    open: number
    overdue: number
  }
  generated_at: string
}

export interface CRMSystemSegment {
  key: string
  label: string
  description: string
  count: number
}

export interface CRMSystemSegmentContact {
  id: string
  profile_name?: string
  phone_number?: string
  last_message_at?: string
  assigned_user_id?: string
}

export interface CRMSystemSegmentContacts {
  segment: CRMSystemSegment
  contacts: CRMSystemSegmentContact[]
  total: number
  page: number
  limit: number
}

export const crmInsightsService = {
  get: (params: { from: string; to: string }) =>
    api.get<{ data: CRMInsights }>('/crm/insights', { params }),
  segments: () =>
    api.get<{ data: { segments: CRMSystemSegment[] } }>('/crm/segments'),
  segmentContacts: (
    key: string,
    params: { page?: number; limit?: number } = {},
  ) =>
    api.get<{ data: CRMSystemSegmentContacts }>(
      `/crm/segments/${encodeURIComponent(key)}/contacts`,
      { params },
    ),
}
