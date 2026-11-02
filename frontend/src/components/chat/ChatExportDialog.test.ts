/** @vitest-environment happy-dom */
import { reactive } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ChatExportDialog from './ChatExportDialog.vue'
import type { ChatExportConversation, ChatExportRequest } from '@/services/chatExport'

const mocks = vi.hoisted(() => ({ capabilities: vi.fn(), preview: vi.fn(), download: vi.fn(), cancel: vi.fn(), marker: vi.fn(), auth: {} as any, organizations: {} as any }))
vi.mock('@/services/chatExport', async importOriginal => ({ ...(await importOriginal<typeof import('@/services/chatExport')>()), chatExportsService: mocks }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => mocks.auth }))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => mocks.organizations }))
vi.mock('@/composables/useChatAttachments', () => ({ readLogicalSessionMarker: mocks.marker }))
vi.mock('@/components/ui/dialog', () => ({
  Dialog: { props: ['open'], template: '<div v-if="open"><slot /></div>' },
  DialogContent: { template: '<section><slot /></section>' }, DialogHeader: { template: '<header><slot /></header>' },
  DialogFooter: { template: '<footer><slot /></footer>' }, DialogTitle: { template: '<h2><slot /></h2>' }, DialogDescription: { template: '<p><slot /></p>' },
}))
vi.mock('@/components/ui/button', () => ({ Button: { template: '<button><slot /></button>' } }))
vi.mock('@/components/ui/input', () => ({ Input: { props: ['modelValue'], emits: ['update:modelValue'], template: '<input :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />' } }))

let wrapper: VueWrapper | undefined
let objectURL: ReturnType<typeof vi.spyOn>
const row = (number: number): ChatExportConversation => ({ key: `inbox:${number}`, contact_id: `contact-${number}`, name: `Person ${number}`, account: 'Clinic', channel: 'whatsapp', messages: 5 })
const allRows = Array.from({ length: 85 }, (_, index) => row(index + 1))
const button = (label: string) => wrapper!.findAll('button').find(item => item.text() === label)!
async function settle() { await new Promise(resolve => setTimeout(resolve, 225)); await flushPromises() }
async function open() {
  wrapper = mount(ChatExportDialog, { props: { filters: { search: 'Clinic', unread: true }, initialAccount: null } })
  await flushPromises()
  await wrapper.get('[aria-label="Export chats"]').trigger('click')
  await settle()
}
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(done => { resolve = done }); return { promise, resolve } }

beforeEach(() => {
  vi.clearAllMocks()
  mocks.auth = reactive({ user: { id: 'user-1' }, organizationId: 'org-1' })
  mocks.organizations = reactive({ selectedOrgId: 'org-1' })
  mocks.marker.mockReturnValue('session-1')
  mocks.capabilities.mockResolvedValue({ available: true, max_selected: 2000 })
  mocks.cancel.mockResolvedValue({ cancelled: true, preparation_finished: false })
  mocks.download.mockResolvedValue(new Blob(['synthetic zip'], { type: 'application/zip' }))
  mocks.preview.mockImplementation(async (request: ChatExportRequest) => {
    const matches = request.scope === 'selected' ? allRows.filter(item => request.selected?.includes(item.key)) : allRows
    const offset = request.after ? matches.findIndex(item => item.key === request.after) + 1 : 0
    const rows = matches.slice(offset, offset + 40)
    return { as_of: '2026-10-26T00:00:00Z', conversations: matches.length, messages: matches.length * 5, rows, accounts: [], next: offset + 40 < matches.length ? rows.at(-1)!.key : undefined, can_download: matches.length > 0, max_messages: 100000, max_bytes: 67108864 }
  })
  objectURL = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:synthetic')
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
})
afterEach(async () => { wrapper?.unmount(); wrapper = undefined; await flushPromises(); vi.restoreAllMocks() })

describe('chat export dialog', () => {
  it('hides export when the dedicated capability is unavailable', async () => {
    mocks.capabilities.mockResolvedValue({ available: false, max_selected: 2000 })
    wrapper = mount(ChatExportDialog, { props: { filters: {} } }); await flushPromises()
    expect(wrapper.find('[aria-label="Export chats"]').exists()).toBe(false)
    expect(mocks.preview).not.toHaveBeenCalled()
  })

  it('exports all filtered results rather than the visible page', async () => {
    await open()
    expect(wrapper!.text()).toContain('425')
    expect(wrapper!.text()).toContain('85 conversations')
    expect(wrapper!.text()).not.toContain('Person 85')
    await button('Download ZIP').trigger('click'); await flushPromises()
    expect(mocks.download.mock.calls[0][0]).toEqual(expect.objectContaining({ scope: 'filtered', search: 'Clinic', unread: true }))
    expect(mocks.download.mock.calls[0][0].selected).toBeUndefined()
    expect(objectURL).toHaveBeenCalledTimes(1)
  })

  it('keeps selection across pages and exports the complete selection', async () => {
    await open(); await wrapper!.get('input[value="selected"]').setValue(); await settle()
    await wrapper!.get('input[aria-label="Export Person 1, Clinic"]').setValue(true); await settle()
    await button('Load more conversations').trigger('click'); await flushPromises()
    await wrapper!.get('input[aria-label="Export Person 41, Clinic"]').setValue(true); await settle()
    expect(wrapper!.find('[aria-label="Remove Person 1 from export"]').exists()).toBe(true)
    expect(wrapper!.find('[aria-label="Remove Person 41 from export"]').exists()).toBe(true)
    await button('Download ZIP').trigger('click'); await flushPromises()
    expect(mocks.download.mock.calls[0][0]).toEqual(expect.objectContaining({ scope: 'selected', selected: ['inbox:1', 'inbox:41'] }))
  })

  it('clears chat filters for all accessible scope without creating a selected-page substitute', async () => {
    await open(); await wrapper!.get('input[value="all"]').setValue(); await settle()
    await button('Download ZIP').trigger('click'); await flushPromises()
    expect(mocks.download.mock.calls[0][0]).toEqual(expect.objectContaining({ scope: 'all', selected: undefined, search: undefined }))
    expect(mocks.download.mock.calls[0][0].unread).toBeUndefined()
  })

  it('cancels the old organization and suppresses late bytes after a workspace change', async () => {
    const pending = deferred<Blob>(); mocks.download.mockReturnValue(pending.promise)
    await open(); await button('Download ZIP').trigger('click')
    const signal = mocks.download.mock.calls[0][2] as AbortSignal
    mocks.organizations.selectedOrgId = 'org-2'; await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(mocks.cancel).toHaveBeenCalledWith(expect.any(String), 'org-1')
    pending.resolve(new Blob(['late'], { type: 'application/zip' })); await flushPromises()
    expect(objectURL).not.toHaveBeenCalled()
  })

  it('rejects late bytes after the same user starts a different logical session', async () => {
    const pending = deferred<Blob>(); mocks.download.mockReturnValue(pending.promise)
    await open(); await button('Download ZIP').trigger('click')
    mocks.marker.mockReturnValue('session-2')
    pending.resolve(new Blob(['late'], { type: 'application/zip' })); await flushPromises()
    expect(objectURL).not.toHaveBeenCalled()
  })

  it('reports a server failure and never downloads a partial archive', async () => {
    mocks.download.mockRejectedValue(new Error('Permission changed; preview again'))
    await open(); await button('Download ZIP').trigger('click'); await flushPromises()
    expect(wrapper!.get('[role="alert"]').text()).toContain('Permission changed')
    expect(objectURL).not.toHaveBeenCalled()
    expect(wrapper!.text()).not.toContain('Your complete archive is ready')
  })

  it('does not claim preparation was cancelled after delivery has already started', async () => {
    const pending = deferred<Blob>(); mocks.download.mockReturnValue(pending.promise)
    mocks.cancel.mockResolvedValue({ cancelled: false, preparation_finished: true })
    await open(); await button('Download ZIP').trigger('click')
    await button('Cancel export').trigger('click'); await flushPromises()
    expect(wrapper!.text()).toContain('Archive preparation had already finished')
    pending.resolve(new Blob(['late'], { type: 'application/zip' })); await flushPromises()
    expect(objectURL).not.toHaveBeenCalled()
  })
})
