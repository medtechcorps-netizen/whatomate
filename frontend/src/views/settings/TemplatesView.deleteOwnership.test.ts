/** @vitest-environment happy-dom */
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import TemplatesView from './TemplatesView.vue'

const mocks = vi.hoisted(() => ({ get: vi.fn(), list: vi.fn(), remove: vi.fn(), success: vi.fn(), error: vi.fn() }))
const context = reactive({ selectedOrgId: 'org-A', session: 'session-A', live: true, canDelete: true })
vi.mock('@/services/api', () => ({
  AUTH_SESSION_EVENT: 'auth-session-state',
  api: { get: mocks.get, delete: mocks.remove, post: vi.fn() },
  templatesService: { list: mocks.list },
}))
vi.mock('@/stores/organizations', () => ({ useOrganizationsStore: () => context }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ hasPermission: (_resource: string, action: string) => action === 'delete' && context.canDelete }) }))
vi.mock('@/lib/tabSession', () => ({ currentTabSession: () => context.session, isTabSessionCurrent: () => context.live }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('vue-sonner', () => ({ toast: { success: mocks.success, error: mocks.error } }))
vi.mock('@/components/shared', async () => {
  const { defineComponent: define } = await import('vue')
  const pass = define({ template: '<div><slot /></div>' })
  return {
    PageHeader: define({ template: '<div><slot name="actions" /><slot /></div>' }),
    SearchInput: pass,
    DataTable: define({ props: ['items'], template: '<div><section v-for="item in items" :key="item.id" :data-row="item.id"><span>{{ item.name }}</span><slot name="cell-actions" :item="item" /></section></div>' }),
    IconButton: define({ props: ['label'], emits: ['click'], template: '<button :aria-label="label" @click="$emit(\'click\')">{{ label }}</button>' }),
    ErrorState: pass,
    DeleteConfirmDialog: define({ name: 'DeleteDialog', props: ['open', 'itemName', 'isSubmitting'], emits: ['confirm', 'update:open'], template: '<div v-if="open" data-dialog><p>{{ itemName }}</p><button data-confirm :disabled="isSubmitting" @click="$emit(\'confirm\')">Confirm</button><button data-dismiss @click="$emit(\'update:open\', false)">Dismiss</button></div>' }),
  }
})
vi.mock('@/components/ui/select', async () => {
  const { defineComponent: define } = await import('vue')
  const pass = define({ template: '<div><slot /></div>' })
  return { Select: pass, SelectContent: pass, SelectItem: pass, SelectTrigger: pass, SelectValue: pass }
})

const Pass = defineComponent({ template: '<div><slot /></div>' })
const templates = [{ id: 'first', name: 'First template' }, { id: 'second', name: 'Second template' }]
function deferred() {
  let resolve!: (value: object) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<object>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const wrappers: VueWrapper[] = []
async function render() {
  const wrapper = mount(TemplatesView, { global: {
    mocks: { $t: (key: string) => key },
    stubs: { Card: Pass, CardContent: Pass, CardDescription: Pass, CardHeader: Pass, CardTitle: Pass, ScrollArea: Pass, Label: Pass, Badge: Pass, RouterLink: Pass },
  } })
  wrappers.push(wrapper)
  await flushPromises()
  return wrapper
}
async function open(wrapper: VueWrapper, id = 'first') {
  await wrapper.get(`[data-row="${id}"] [aria-label="common.delete"]`).trigger('click')
}
const dialog = (wrapper: VueWrapper) => wrapper.findComponent({ name: 'DeleteDialog' })
async function confirm(wrapper: VueWrapper) {
  await wrapper.get('[data-confirm]').trigger('click')
}

describe('template delete feedback ownership', () => {
  beforeEach(() => {
    Object.values(mocks).forEach(mock => mock.mockReset())
    context.selectedOrgId = 'org-A'
    context.session = 'session-A'
    context.live = true
    context.canDelete = true
    localStorage.clear()
    mocks.get.mockResolvedValue({ data: { data: { accounts: [] } } })
    mocks.list.mockResolvedValue({ data: { data: { templates, total: 2 } } })
    mocks.remove.mockResolvedValue({})
  })
  afterEach(() => wrappers.splice(0).forEach(wrapper => wrapper.unmount()))

  it('completes the current delete once and refreshes the current list', async () => {
    const wrapper = await render()
    await open(wrapper)
    await confirm(wrapper)
    await flushPromises()
    expect(mocks.remove).toHaveBeenCalledExactlyOnceWith('/templates/first')
    expect(mocks.success).toHaveBeenCalledTimes(1)
    expect(dialog(wrapper).props('open')).toBe(false)
    expect(dialog(wrapper).props('isSubmitting')).toBe(false)
    expect(mocks.list).toHaveBeenCalledTimes(2)
  })

  it('suppresses late success after a workspace A to B to A transition', async () => {
    const wrapper = await render()
    const old = deferred()
    mocks.remove.mockReturnValueOnce(old.promise)
    await open(wrapper)
    await confirm(wrapper)
    context.selectedOrgId = 'org-B'
    context.selectedOrgId = 'org-A'
    await flushPromises()
    await open(wrapper, 'second')
    const listCalls = mocks.list.mock.calls.length
    old.resolve({})
    await flushPromises()
    expect(mocks.remove).toHaveBeenCalledExactlyOnceWith('/templates/first')
    expect(mocks.success).not.toHaveBeenCalled()
    expect(mocks.list).toHaveBeenCalledTimes(listCalls)
    expect(dialog(wrapper).props('open')).toBe(true)
    expect(dialog(wrapper).props('itemName')).toBe('Second template')
  })

  it('suppresses a late error after session expiry and clears the old dialog', async () => {
    const wrapper = await render()
    const old = deferred()
    mocks.remove.mockReturnValueOnce(old.promise)
    await open(wrapper)
    await confirm(wrapper)
    context.live = false
    window.dispatchEvent(new CustomEvent('auth-session-state', { detail: { state: 'expired' } }))
    await nextTick()
    old.reject(new Error('old session failure'))
    await flushPromises()
    expect(dialog(wrapper).props('open')).toBe(false)
    expect(dialog(wrapper).props('isSubmitting')).toBe(false)
    expect(mocks.error).not.toHaveBeenCalled()
    expect(mocks.remove).toHaveBeenCalledTimes(1)
    expect(mocks.list).toHaveBeenCalledTimes(1)
  })

  it('checks the logical session on completion even without a notification', async () => {
    const wrapper = await render()
    const old = deferred()
    mocks.remove.mockReturnValueOnce(old.promise)
    await open(wrapper)
    await confirm(wrapper)
    context.session = 'session-B'
    old.resolve({})
    await flushPromises()
    expect(mocks.success).not.toHaveBeenCalled()
    expect(mocks.list).toHaveBeenCalledTimes(1)
  })

  it('does not let an old delete close a newer dialog or clear its loading state', async () => {
    const wrapper = await render()
    const old = deferred()
    const latest = deferred()
    mocks.remove.mockReturnValueOnce(old.promise).mockReturnValueOnce(latest.promise)
    await open(wrapper)
    await confirm(wrapper)
    await open(wrapper, 'second')
    await confirm(wrapper)
    expect(mocks.remove.mock.calls.map(call => call[0])).toEqual(['/templates/first', '/templates/second'])
    old.resolve({})
    await flushPromises()
    expect(dialog(wrapper).props('open')).toBe(true)
    expect(dialog(wrapper).props('itemName')).toBe('Second template')
    expect(dialog(wrapper).props('isSubmitting')).toBe(true)
    expect(mocks.success).not.toHaveBeenCalled()
    latest.resolve({})
    await flushPromises()
    expect(mocks.success).toHaveBeenCalledTimes(1)
    expect(dialog(wrapper).props('open')).toBe(false)
  })

  it('invalidates an old error when the same template dialog is dismissed and reopened', async () => {
    const wrapper = await render()
    const old = deferred()
    const latest = deferred()
    mocks.remove.mockReturnValueOnce(old.promise).mockReturnValueOnce(latest.promise)
    await open(wrapper)
    await confirm(wrapper)
    await wrapper.get('[data-dismiss]').trigger('click')
    await open(wrapper)
    await confirm(wrapper)
    old.reject(new Error('obsolete failure'))
    await flushPromises()
    expect(mocks.error).not.toHaveBeenCalled()
    expect(dialog(wrapper).props('open')).toBe(true)
    expect(dialog(wrapper).props('isSubmitting')).toBe(true)
    latest.resolve({})
    await flushPromises()
    expect(mocks.remove).toHaveBeenCalledTimes(2)
    expect(mocks.success).toHaveBeenCalledTimes(1)
  })

  it('does not issue another DELETE for repeated confirmation of the active request', async () => {
    const wrapper = await render()
    const pending = deferred()
    mocks.remove.mockReturnValueOnce(pending.promise)
    await open(wrapper)
    dialog(wrapper).vm.$emit('confirm')
    dialog(wrapper).vm.$emit('confirm')
    await nextTick()
    expect(mocks.remove).toHaveBeenCalledTimes(1)
    pending.resolve({})
    await flushPromises()
  })

  it('closes stale authority on permission loss and never sends a new delete', async () => {
    const wrapper = await render()
    await open(wrapper)
    context.canDelete = false
    await nextTick()
    dialog(wrapper).vm.$emit('confirm')
    await flushPromises()
    expect(dialog(wrapper).props('open')).toBe(false)
    expect(mocks.remove).not.toHaveBeenCalled()
  })

  it('does not publish the result of a pending delete after permission loss', async () => {
    const wrapper = await render()
    const old = deferred()
    mocks.remove.mockReturnValueOnce(old.promise)
    await open(wrapper)
    await confirm(wrapper)
    context.canDelete = false
    old.reject(new Error('no longer current'))
    await flushPromises()
    expect(mocks.error).not.toHaveBeenCalled()
    expect(dialog(wrapper).props('open')).toBe(false)
    expect(dialog(wrapper).props('isSubmitting')).toBe(false)
    expect(mocks.remove).toHaveBeenCalledTimes(1)
  })

  it('keeps a current failed delete open and retryable without automatically resending', async () => {
    const wrapper = await render()
    mocks.remove.mockRejectedValueOnce(new Error('current failure'))
    await open(wrapper)
    await confirm(wrapper)
    await flushPromises()
    expect(dialog(wrapper).props('open')).toBe(true)
    expect(dialog(wrapper).props('isSubmitting')).toBe(false)
    expect(mocks.error).toHaveBeenCalledTimes(1)
    expect(mocks.remove).toHaveBeenCalledTimes(1)
    expect(mocks.list).toHaveBeenCalledTimes(1)
  })

  it('does not toast or refresh after unmount while a delete is pending', async () => {
    const wrapper = await render()
    const old = deferred()
    mocks.remove.mockReturnValueOnce(old.promise)
    await open(wrapper)
    await confirm(wrapper)
    wrapper.unmount()
    wrappers.splice(wrappers.indexOf(wrapper), 1)
    old.resolve({})
    await flushPromises()
    expect(mocks.success).not.toHaveBeenCalled()
    expect(mocks.error).not.toHaveBeenCalled()
    expect(mocks.remove).toHaveBeenCalledTimes(1)
    expect(mocks.list).toHaveBeenCalledTimes(1)
  })
})
