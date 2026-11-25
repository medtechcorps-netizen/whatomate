/** @vitest-environment happy-dom */
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import axios, { AxiosHeaders } from 'axios'
import { defineComponent, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import TemplatesView from './TemplatesView.vue'

const mocks = vi.hoisted(() => ({
  get: vi.fn(), post: vi.fn(), list: vi.fn(), remove: vi.fn(), adapter: vi.fn(),
  success: vi.fn(), error: vi.fn(),
}))
const context = reactive({ selectedOrgId: 'org-A' as string | null, session: 'session-A', live: true, canSync: true })
vi.mock('@/services/api', () => ({
  AUTH_SESSION_EVENT: 'auth-session-state',
  api: { get: mocks.get, post: mocks.post, delete: mocks.remove, defaults: { adapter: mocks.adapter } },
  templatesService: { list: mocks.list },
}))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => context }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ organizationId: 'org-A', hasPermission: (_resource: string, action: string) => action === 'sync' && context.canSync }) }))
vi.mock('@/lib/tabSession', () => ({ currentTabSession: () => context.session, isTabSessionCurrent: () => context.live }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, params?: Record<string, string | number>) => params ? `${key} ${Object.values(params).join(' ')}` : key }) }))
vi.mock('vue-sonner', () => ({ toast: { success: mocks.success, error: mocks.error } }))
vi.mock('@/components/shared', async () => {
  const { defineComponent: define } = await import('vue')
  return {
    PageHeader: define({ template: '<div><slot name="actions" /><slot /></div>' }),
    SearchInput: define({ props: ['modelValue'], emits: ['update:modelValue'], template: '<input data-search :value="modelValue" @input="$emit(\'update:modelValue\', $event.target.value)" />' }),
    DataTable: define({ name: 'TemplateTable', props: ['items', 'isLoading', 'totalItems', 'currentPage'], emits: ['page-change'], template: '<div data-table :data-loading="isLoading" :data-total="totalItems"><span v-for="item in items" :key="item.id">{{ item.name }}</span></div>' }),
    ErrorState: define({ props: ['description'], emits: ['retry'], template: '<div role="alert">{{ description }}<button data-retry @click="$emit(\'retry\')">Retry</button></div>' }),
    IconButton: define({ template: '<button />' }),
    DeleteConfirmDialog: define({ template: '<div />' }),
  }
})
vi.mock('@/components/ui/select', async () => {
  const { defineComponent: define } = await import('vue')
  const pass = define({ template: '<div><slot /></div>' })
  return {
    Select: define({ name: 'AccountSelect', props: ['modelValue'], emits: ['update:modelValue'], template: '<div><slot /></div>' }),
    SelectContent: pass, SelectItem: pass, SelectTrigger: pass, SelectValue: pass,
  }
})

const Pass = defineComponent({ template: '<div><slot /></div>' })
const Button = defineComponent({ props: ['disabled'], template: '<button :disabled="disabled"><slot /></button>' })
const accounts = (names = ['A', 'B']) => ({ data: { data: { accounts: names.map(name => ({ id: name, name, phone_id: name })) } } })
const rows = (name: string, total = 1) => ({ data: { data: { templates: [{ id: name, name }], total } } })
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const wrappers: VueWrapper[] = []
async function render() {
  const wrapper = mount(TemplatesView, { global: {
    mocks: { $t: (key: string, params?: Record<string, string | number>) => params ? `${key} ${Object.values(params).join(' ')}` : key },
    stubs: { Card: Pass, CardContent: Pass, CardDescription: Pass, CardHeader: Pass, CardTitle: Pass, ScrollArea: Pass, Label: Pass, Badge: Pass, Button, RouterLink: Pass },
  } })
  wrappers.push(wrapper)
  await flushPromises()
  return wrapper
}
const table = (wrapper: VueWrapper) => wrapper.findComponent({ name: 'TemplateTable' })
const syncButton = (wrapper: VueWrapper) => wrapper.findAll('button').find(button => button.text() === 'templates.syncFromMeta')!
async function account(wrapper: VueWrapper, name: string) {
  wrapper.findComponent({ name: 'AccountSelect' }).vm.$emit('update:modelValue', `account:${name}`)
  await nextTick()
}
async function page(wrapper: VueWrapper, value: number) {
  table(wrapper).vm.$emit('page-change', value)
  await nextTick()
}
function selectWorkspace(organizationId: string) {
  // Match the Pinia selection update order.
  context.selectedOrgId = organizationId
  localStorage.setItem('selected_organization_id', organizationId)
}
function sessionEvent(state: string) {
  window.dispatchEvent(new CustomEvent('auth-session-state', { detail: { state } }))
}

describe('template view request ownership', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    Object.values(mocks).forEach(mock => mock.mockReset())
    context.selectedOrgId = 'org-A'
    context.session = 'session-A'
    context.live = true
    context.canSync = true
    localStorage.clear()
    localStorage.setItem('selected_organization_id', 'org-A')
    mocks.get.mockResolvedValue(accounts())
    mocks.adapter.mockResolvedValue({})
    mocks.list.mockResolvedValue(rows('current'))
    mocks.post.mockResolvedValue({})
    vi.spyOn(console, 'error').mockImplementation(() => undefined)
  })
  afterEach(() => {
    wrappers.splice(0).forEach(wrapper => wrapper.unmount())
    vi.clearAllTimers()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('recovers a failed account load without syncing until an account is selected', async () => {
    localStorage.setItem('templates_selected_account_option', 'account:stale')
    mocks.get.mockRejectedValueOnce(new Error('accounts unavailable')).mockResolvedValueOnce(accounts(['all']))
    const wrapper = await render()
    expect(wrapper.get('[role="alert"]').text()).toBe('common.failedLoad resources.accounts')
    expect(table(wrapper).props('items')).toEqual([{ id: 'current', name: 'current' }])
    expect(syncButton(wrapper).attributes('disabled')).toBeDefined()

    const retry = wrapper.findAll('button').find(button => button.text() === 'common.retry')!
    await retry.trigger('click')
    await flushPromises()
    expect(mocks.get).toHaveBeenCalledTimes(2)
    expect(mocks.get).toHaveBeenLastCalledWith('/accounts', expect.objectContaining({ headers: { 'X-Organization-ID': 'org-A' } }))
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(mocks.list).toHaveBeenLastCalledWith(expect.objectContaining({ account: undefined }), expect.any(Object))
    expect(syncButton(wrapper).attributes('disabled')).toBeDefined()
    expect(mocks.post).not.toHaveBeenCalled()

    await account(wrapper, 'all')
    await syncButton(wrapper).trigger('click')
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledExactlyOnceWith('/templates/sync', { whatsapp_account: 'all' },
      expect.objectContaining({ headers: { 'X-Organization-ID': 'org-A' } }))
  })

  it('keeps newer workspace rows/count when the older response finishes last', async () => {
    const old = deferred<ReturnType<typeof rows>>()
    mocks.list.mockReturnValueOnce(old.promise)
    const wrapper = await render()
    selectWorkspace('org-B')
    await flushPromises()
    expect(table(wrapper).props('items')).toEqual([{ id: 'current', name: 'current' }])
    old.resolve(rows('old workspace', 99))
    await flushPromises()
    expect(table(wrapper).props('totalItems')).toBe(1)
    expect(wrapper.text()).not.toContain('old workspace')
  })

  it('shows loading while the new workspace accounts request is pending', async () => {
    const wrapper = await render()
    const pending = deferred<ReturnType<typeof accounts>>()
    mocks.get.mockReturnValueOnce(pending.promise)
    selectWorkspace('org-B')
    await flushPromises()
    expect(table(wrapper).props('items')).toEqual([])
    expect(table(wrapper).props('isLoading')).toBe(true)
    pending.resolve(accounts())
    await flushPromises()
    expect(table(wrapper).props('isLoading')).toBe(false)
  })

  it('does not revive accounts or mounted continuations across A to B to A', async () => {
    const old = deferred<ReturnType<typeof accounts>>()
    mocks.get.mockReturnValueOnce(old.promise)
    const wrapper = await render()
    selectWorkspace('org-B')
    selectWorkspace('org-A')
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(1)
    old.resolve(accounts(['obsolete']))
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).not.toContain('obsolete')
  })

  it('ignores an older org-change continuation still waiting for its accounts', async () => {
    const wrapper = await render()
    const old = deferred<ReturnType<typeof accounts>>()
    mocks.get.mockReturnValueOnce(old.promise)
    selectWorkspace('org-B')
    await flushPromises()
    selectWorkspace('org-C')
    await flushPromises()
    const calls = mocks.list.mock.calls.length
    old.reject(new Error('old account request'))
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(calls)
    expect(console.error).not.toHaveBeenCalled()
    expect(table(wrapper).props('isLoading')).toBe(false)
  })

  it('invalidates search before debounce and ignores an old error/finally', async () => {
    const old = deferred<ReturnType<typeof rows>>()
    mocks.list.mockReturnValueOnce(old.promise)
    const wrapper = await render()
    await wrapper.get('[data-search]').setValue('new search')
    old.reject(new Error('obsolete list'))
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(1)
    expect(table(wrapper).props('isLoading')).toBe(true)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(mocks.error).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()
    expect(mocks.list).toHaveBeenLastCalledWith(expect.objectContaining({ search: 'new search' }), expect.any(Object))
    expect(table(wrapper).props('isLoading')).toBe(false)
  })

  it('does not revive the first search request when its text returns before debounce', async () => {
    const old = deferred<ReturnType<typeof rows>>()
    mocks.list.mockReturnValueOnce(old.promise)
    const wrapper = await render()
    await wrapper.get('[data-search]').setValue('temporary')
    await wrapper.get('[data-search]').setValue('')
    old.resolve(rows('obsolete empty search', 27))
    await flushPromises()
    expect(table(wrapper).props('items')).toEqual([])
    expect(table(wrapper).props('totalItems')).toBe(0)
    expect(table(wrapper).props('isLoading')).toBe(true)
  })

  it('keeps the latest account result when account requests finish out of order', async () => {
    const wrapper = await render()
    const old = deferred<ReturnType<typeof rows>>()
    mocks.list.mockReturnValueOnce(old.promise).mockResolvedValueOnce(rows('B result', 7))
    await account(wrapper, 'A')
    await account(wrapper, 'B')
    await flushPromises()
    old.resolve(rows('A result', 22))
    await flushPromises()
    expect(table(wrapper).props('totalItems')).toBe(7)
    expect(wrapper.text()).toContain('B result')
    expect(wrapper.text()).not.toContain('A result')
  })

  it('does not let an older page finally clear the latest page loading state', async () => {
    const wrapper = await render()
    const old = deferred<ReturnType<typeof rows>>()
    const latest = deferred<ReturnType<typeof rows>>()
    mocks.list.mockReturnValueOnce(old.promise).mockReturnValueOnce(latest.promise)
    await page(wrapper, 2)
    await page(wrapper, 3)
    old.resolve(rows('old page'))
    await flushPromises()
    expect(table(wrapper).props('isLoading')).toBe(true)
    latest.resolve(rows('latest page', 61))
    await flushPromises()
    expect(table(wrapper).props('isLoading')).toBe(false)
    expect(table(wrapper).props('totalItems')).toBe(61)
  })

  it('shows a current error and clears its stale count, then permits retry', async () => {
    const wrapper = await render()
    mocks.list.mockRejectedValueOnce(new Error('current failure'))
    await page(wrapper, 2)
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('templates.errorLoadingTemplates')
    expect(mocks.error).toHaveBeenCalledTimes(1)
    await wrapper.get('[data-retry]').trigger('click')
    await flushPromises()
    expect(table(wrapper).props('totalItems')).toBe(1)
  })

  it('ignores a replaced logical session even without a session notification', async () => {
    const old = deferred<ReturnType<typeof rows>>()
    mocks.list.mockReturnValueOnce(old.promise)
    const wrapper = await render()
    context.session = 'session-B'
    old.resolve(rows('private old response'))
    await flushPromises()
    expect(wrapper.text()).not.toContain('private old response')
    expect(mocks.error).not.toHaveBeenCalled()
  })

  it('clears session-visible data and ignores an in-flight sync on expiry', async () => {
    const wrapper = await render()
    const pending = deferred<object>()
    mocks.post.mockReturnValueOnce(pending.promise)
    await account(wrapper, 'A')
    await syncButton(wrapper).trigger('click')
    context.live = false
    sessionEvent('expired')
    await nextTick()
    expect(table(wrapper).props('items')).toEqual([])
    expect(table(wrapper).props('totalItems')).toBe(0)
    pending.resolve({})
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledTimes(1)
    expect(mocks.success).not.toHaveBeenCalled()
  })

  it('does not revive an old sync or clear a new sync after A to B to A', async () => {
    const wrapper = await render()
    const old = deferred<object>()
    const latest = deferred<object>()
    mocks.post.mockReturnValueOnce(old.promise).mockReturnValueOnce(latest.promise)
    await account(wrapper, 'A')
    await syncButton(wrapper).trigger('click')
    selectWorkspace('org-B')
    selectWorkspace('org-A')
    await flushPromises()
    await account(wrapper, 'A')
    await syncButton(wrapper).trigger('click')
    old.reject(new Error('obsolete sync'))
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledTimes(2)
    expect(syncButton(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.find('[role="status"]').text()).toContain('A')
    expect(mocks.error).not.toHaveBeenCalled()
    latest.resolve({})
    await flushPromises()
    expect(mocks.post.mock.calls.map(call => call[1].whatsapp_account)).toEqual(['A', 'A'])
    expect(mocks.success).toHaveBeenCalledTimes(1)
    expect(syncButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it('requires a current selected account and preserves its failure through a same-session token refresh', async () => {
    mocks.get.mockResolvedValueOnce(accounts(['A', 'B', 'C']))
    const wrapper = await render()
    expect(syncButton(wrapper).attributes('disabled')).toBeDefined()
    await syncButton(wrapper).trigger('click')
    await account(wrapper, 'missing')
    expect(syncButton(wrapper).attributes('disabled')).toBeDefined()
    await syncButton(wrapper).trigger('click')
    wrapper.findComponent({ name: 'AccountSelect' }).vm.$emit('update:modelValue', null)
    await nextTick()
    expect(syncButton(wrapper).attributes('disabled')).toBeDefined()
    await syncButton(wrapper).trigger('click')
    expect(mocks.post).not.toHaveBeenCalled()

    await account(wrapper, 'B')
    const pending = deferred<object>()
    mocks.post.mockReturnValueOnce(pending.promise)
    await syncButton(wrapper).trigger('click')
    sessionEvent('refreshed')
    pending.reject(new Error('B unavailable'))
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledExactlyOnceWith('/templates/sync', { whatsapp_account: 'B' },
      expect.objectContaining({ headers: { 'X-Organization-ID': 'org-A' } }))
    const results = wrapper.get('[aria-label="templates.syncResults"]').text()
    expect(results).toContain('B: B unavailable')
    expect(mocks.error).toHaveBeenCalledTimes(1)
    expect(mocks.error).toHaveBeenCalledWith('templates.syncPartialFailure 1')
    expect(mocks.success).not.toHaveBeenCalled()
  })

  it('restores a legacy configured account name that contains the picker prefix', async () => {
    localStorage.setItem('templates_selected_account', 'account:B')
    mocks.get.mockResolvedValueOnce(accounts(['B', 'account:B']))
    const wrapper = await render()
    expect(mocks.list).toHaveBeenLastCalledWith(expect.objectContaining({ account: 'account:B' }), expect.any(Object))
    expect(syncButton(wrapper).attributes('disabled')).toBeUndefined()
    await syncButton(wrapper).trigger('click')
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledExactlyOnceWith('/templates/sync', { whatsapp_account: 'account:B' },
      expect.objectContaining({ headers: { 'X-Organization-ID': 'org-A' } }))
  })

  it('pins a literal all account and rejects a late adapter dispatch after the stored workspace moves', async () => {
    mocks.get.mockResolvedValueOnce(accounts(['all']))
    const pending = deferred<object>()
    mocks.post.mockReturnValueOnce(pending.promise)
    const wrapper = await render()
    const workspaceHeaders = { 'X-Organization-ID': 'org-A' }
    expect(mocks.get).toHaveBeenCalledExactlyOnceWith('/accounts', expect.objectContaining({ headers: workspaceHeaders }))

    await account(wrapper, 'all')
    expect(mocks.list).toHaveBeenLastCalledWith(expect.objectContaining({ account: 'all' }),
      expect.objectContaining({ headers: workspaceHeaders }))
    await syncButton(wrapper).trigger('click')
    expect(mocks.post).toHaveBeenCalledExactlyOnceWith('/templates/sync', { whatsapp_account: 'all' },
      expect.objectContaining({ headers: workspaceHeaders }))

    const adapter = axios.getAdapter(mocks.post.mock.calls[0]![2].adapter)
    const config = { headers: new AxiosHeaders(workspaceHeaders) }
    await adapter(config)
    localStorage.setItem('selected_organization_id', 'org-B')
    expect(context.selectedOrgId).toBe('org-A')
    await expect(adapter(config)).rejects.toMatchObject({ code: 'ERR_CANCELED' })
    expect(mocks.adapter).toHaveBeenCalledTimes(1)

    const lists = mocks.list.mock.calls.length
    pending.resolve({})
    await flushPromises()
    expect(mocks.list).toHaveBeenCalledTimes(lists)
    expect(mocks.success).not.toHaveBeenCalled()
    await syncButton(wrapper).trigger('click')
    expect(mocks.post).toHaveBeenCalledTimes(1)
  })

  it('settles loading and waits for the reactive and stored workspace to agree before listing accounts', async () => {
    localStorage.setItem('selected_organization_id', 'org-B')
    const wrapper = await render()
    expect(table(wrapper).props('isLoading')).toBe(false)
    await page(wrapper, 2)
    await flushPromises()
    expect(table(wrapper).props('isLoading')).toBe(false)
    expect(mocks.get).not.toHaveBeenCalled()
    expect(mocks.list).not.toHaveBeenCalled()

    selectWorkspace('org-B')
    await flushPromises()
    expect(mocks.get).toHaveBeenCalledExactlyOnceWith('/accounts',
      expect.objectContaining({ headers: { 'X-Organization-ID': 'org-B' } }))
    expect(mocks.list).toHaveBeenCalledTimes(1)
  })

  it('pins the authenticated workspace when there is no selected override', async () => {
    context.selectedOrgId = null
    localStorage.removeItem('selected_organization_id')
    await render()
    expect(mocks.get).toHaveBeenCalledExactlyOnceWith('/accounts',
      expect.objectContaining({ headers: { 'X-Organization-ID': 'org-A' } }))
  })

  it('keeps the started sync target snapshot when only the list filter changes', async () => {
    const wrapper = await render()
    const first = deferred<object>()
    mocks.post.mockReturnValueOnce(first.promise)
    await account(wrapper, 'A')
    await syncButton(wrapper).trigger('click')
    await account(wrapper, 'B')
    first.resolve({})
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledExactlyOnceWith('/templates/sync', { whatsapp_account: 'A' },
      expect.objectContaining({ headers: { 'X-Organization-ID': 'org-A' } }))
    expect(mocks.success).toHaveBeenCalledTimes(1)
    expect(mocks.success).toHaveBeenCalledWith('templates.syncAccountsSuccess 1')
    expect(mocks.list).toHaveBeenLastCalledWith(expect.objectContaining({ account: 'B' }), expect.any(Object))
  })

  it('starts no list continuation when unmounted during the accounts request', async () => {
    const pending = deferred<ReturnType<typeof accounts>>()
    mocks.get.mockReturnValueOnce(pending.promise)
    const wrapper = await render()
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    pending.resolve(accounts())
    await flushPromises()
    expect(mocks.list).not.toHaveBeenCalled()
    sessionEvent('expired')
    expect(mocks.get).toHaveBeenCalledTimes(1)
  })

  it('ignores sync completion after unmount without a toast or list refresh', async () => {
    const wrapper = await render()
    const pending = deferred<object>()
    mocks.post.mockReturnValueOnce(pending.promise)
    await account(wrapper, 'A')
    await syncButton(wrapper).trigger('click')
    const listCallsBeforeUnmount = mocks.list.mock.calls.length
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    pending.resolve({})
    await flushPromises()
    expect(mocks.post).toHaveBeenCalledTimes(1)
    expect(mocks.list).toHaveBeenCalledTimes(listCallsBeforeUnmount)
    expect(mocks.success).not.toHaveBeenCalled()
    expect(mocks.error).not.toHaveBeenCalled()
  })
})
