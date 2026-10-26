import { api, getRequestHeaders } from '@/services/api'

export interface ChatExportFilters {
  search?: string
  tags?: string[]
  unread?: boolean
  assigned?: 'me' | 'unassigned' | ''
}

export interface ChatExportRequest extends ChatExportFilters {
  scope: 'selected' | 'filtered' | 'all'
  selected?: string[]
  account?: string
  from?: string
  until?: string
  after?: string
  request_id?: string
}

export interface ChatExportConversation {
  key: string
  contact_id: string
  name: string
  account: string
  channel: string
  messages: number
}

export interface ChatExportPreview {
  as_of: string
  conversations: number
  messages: number
  rows: ChatExportConversation[]
  accounts: { key: string; name: string; channel: string }[]
  next?: string
  can_download: boolean
  limit_reason?: string
  max_messages: number
  max_bytes: number
}

export interface ChatExportCapabilities {
  available: boolean
  native_inbox: boolean
  max_selected: number
}

function unwrap<T>(data: { data?: T } | T): T {
  return ((data as { data?: T }).data ?? data) as T
}

export function legacyExportAccount(name: string | null | undefined): string {
  return name ? `whatsapp:${Array.from(new TextEncoder().encode(name), byte => byte.toString(16).padStart(2, '0')).join('')}` : ''
}

export function exportDateBoundary(value: string, exclusiveEnd = false): string | undefined {
  if (!value) return undefined
  // Date controls are local calendar dates; the manifest records their exact
  // UTC boundaries so a selected local day never becomes a truncated UTC day.
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value)
  if (!match) throw new Error('Choose a valid date')
  const date = new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3]))
  if (date.getFullYear() !== Number(match[1]) || date.getMonth() !== Number(match[2]) - 1 || date.getDate() !== Number(match[3])) throw new Error('Choose a valid date')
  if (exclusiveEnd) date.setDate(date.getDate() + 1)
  return date.toISOString()
}

export async function chatExportError(error: unknown): Promise<string> {
  const response = (error as { response?: { data?: unknown } })?.response?.data
  let value = response
  if (response instanceof Blob && response.size < 65536) {
    try { value = JSON.parse(await response.text()) } catch { value = undefined }
  }
  const body = value as { message?: string; error?: { message?: string } } | undefined
  return body?.message || body?.error?.message || (error instanceof Error ? error.message : 'The export could not be completed. Please try again.')
}

export const chatExportsService = {
  async capabilities(organizationID: string, signal?: AbortSignal): Promise<ChatExportCapabilities> {
    return unwrap((await api.get('/chat-exports/capabilities', { signal, headers: { 'X-Organization-ID': organizationID } })).data)
  },
  async preview(request: ChatExportRequest, organizationID: string, signal?: AbortSignal): Promise<ChatExportPreview> {
    return unwrap((await api.post('/chat-exports/preview', request, { signal, timeout: 35000, headers: { 'X-Organization-ID': organizationID } })).data)
  },
  async download(request: ChatExportRequest, organizationID: string, signal: AbortSignal, onProgress: (loaded: number) => void): Promise<Blob> {
    const response = await api.post('/chat-exports/download', request, {
      signal, timeout: 330000, responseType: 'blob', headers: { 'X-Organization-ID': organizationID },
      onDownloadProgress: event => onProgress(event.loaded),
    })
    const blob = response.data as Blob
    if (!(blob instanceof Blob) || !blob.type.includes('application/zip')) throw new Error('The server did not return a complete chat archive.')
    return blob
  },
  async cancel(requestID: string, organizationID: string): Promise<{ cancelled: boolean; preparation_finished: boolean }> {
    // A workspace switch must cancel the captured owner's request, not a key
    // in the newly selected workspace. No refresh/replay is needed for cancel.
    const response = await fetch(`${api.defaults.baseURL}/chat-exports/${encodeURIComponent(requestID)}/cancel`, {
      method: 'POST', credentials: 'include', keepalive: true,
      headers: { ...getRequestHeaders({ csrf: true }), 'X-Organization-ID': organizationID, 'Content-Type': 'application/json' },
      body: '{}',
    })
    if (!response.ok) throw new Error('Download stopped, but cancellation of preparation could not be confirmed.')
    return unwrap(await response.json())
  },
}
