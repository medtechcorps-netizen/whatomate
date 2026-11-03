import { api } from '@/services/api'
import { unwrapItemResponse } from '@/lib/api-utils'

export interface ReaformPatient {
  id: string
  display_name: string
  masked_identifier: string | null
}

export interface ReaformSlot {
  doctor_id: string
  doctor_name: string
  date: string
  time: string
  duration_minutes: number
}

export interface ReaformAppointment {
  id: string
  status: string
  date: string
  time: string
  doctor_id: string | null
  doctor_name: string | null
  version: string
  scheduled_at?: string | null
  scheduled_end_at?: string | null
  checked_in_at?: string | null
  consultation_started_at?: string | null
  completed_at?: string | null
  cancelled_at?: string | null
}

export interface ReaformLink {
  command_id: string
  patient_id: string
  state: 'pending' | 'unknown' | 'linked' | 'rejected'
  can_retry?: boolean
  appointment: ReaformAppointment | null
  last_attempt_at?: string | null
  last_synced_at?: string | null
  error_code?: string | null
}

export interface ReaformLeadState {
  enabled: boolean
  clinic_name?: string
  can_write: boolean
  sync_available?: boolean
  link: ReaformLink | null
}

export interface ReaformBookingRequest {
  command_id: string
  patient_id: string
  doctor_id: string
  date: string
  time: string
  duration_minutes: 30
  identity_confirmed: true
}

export const reaformService = {
  async get(leadId: string): Promise<ReaformLeadState> {
    return unwrapItemResponse(await api.get(`/crm/leads/${encodeURIComponent(leadId)}/reaform`))
  },
  async patients(query: string): Promise<ReaformPatient[]> {
    const response = await api.get('/integrations/reaform/patients', { params: { query } })
    return unwrapItemResponse<{ patients: ReaformPatient[] | null }>(response).patients ?? []
  },
  async availability(date: string): Promise<ReaformSlot[]> {
    const response = await api.get('/integrations/reaform/availability', { params: { date } })
    return unwrapItemResponse<{ slots: ReaformSlot[] | null }>(response).slots ?? []
  },
  async book(leadId: string, payload: ReaformBookingRequest): Promise<ReaformLeadState> {
    return unwrapItemResponse(await api.post(`/crm/leads/${encodeURIComponent(leadId)}/reaform`, payload))
  },
  async refresh(leadId: string): Promise<ReaformLeadState> {
    return unwrapItemResponse(await api.post(`/crm/leads/${encodeURIComponent(leadId)}/reaform/refresh`, {}))
  },
}
