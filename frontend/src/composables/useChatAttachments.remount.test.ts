/** @vitest-environment happy-dom */
import { mount, type VueWrapper } from '@vue/test-utils'
import { defineComponent, h, nextTick, reactive, ref, watch } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { SESSION_ENDING_EVENT, useChatAttachments, type ChatAttachments, type SendChatMedia } from './useChatAttachments'

vi.mock('@/services/api', () => ({ messagesService: { sendMedia: vi.fn() } }))

function deferred() {
  let resolve!: (value: unknown) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<unknown>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
const file = (name: string) => new File(['image'], name, { type: 'image/png' })
const mounted: VueWrapper[] = []

function fixture({ ignoreAbort = false, plainIdentity = false } = {}) {
  const authState = { user: 'agent-a', workspace: 'org-a', session: 'login-a', authenticated: true }
  const auth = plainIdentity ? authState : reactive(authState)
  const contact = ref<string | null>('contact-a')
  let selectionGeneration = 0
  const stopSelectionWatch = watch(contact, () => { selectionGeneration++ }, { flush: 'sync' })
  const visible = ref(true)
  const calls: Array<{ request: Parameters<SendChatMedia>[0]; signal: AbortSignal;
    progress: (loaded: number, total: number | undefined) => void; result: ReturnType<typeof deferred> }> = []
  const send: SendChatMedia = (request, options) => {
    const result = deferred()
    calls.push({ request, signal: options.signal, progress: options.onUploadProgress, result })
    if (!ignoreAbort) options.signal.addEventListener('abort', () => result.reject(new DOMException('Aborted', 'AbortError')))
    return result.promise
  }
  const holds = new Set<string>()
  const views: Array<{ api: ChatAttachments; sent: ReturnType<typeof vi.fn>;
    finished: ReturnType<typeof vi.fn>; replySent: ReturnType<typeof vi.fn> }> = []
  const Child = defineComponent({
    setup() {
      const sent = vi.fn(), finished = vi.fn(), replySent = vi.fn()
      const api = useChatAttachments({
        resolveTarget: () => {
          const id = contact.value
          if (!id) return null
          const generation = selectionGeneration
          return { contactId: id, label: `Patient ${id}`, isActive: () => contact.value === id && generation === selectionGeneration }
        },
        session: { userId: () => auth.user, workspace: () => auth.workspace,
          logicalSession: () => auth.session, isAuthenticated: () => auth.authenticated },
        send, onSent: sent, onBatchFinished: finished, onReplySent: replySent,
        replyTo: () => 'reply-a',
        holdWorkspace: (_recipient, id) => { holds.add(id); return () => { holds.delete(id) } },
      })
      views.push({ api, sent, finished, replySent })
      return () => h('section', [
        h('span', { 'data-testid': 'busy' }, String(api.isSending.value)),
        h('span', { 'data-testid': 'recipient' }, api.recipientName.value),
        h('span', { 'data-testid': 'progress' }, api.items.value.map(item => `${item.file.name}:${item.status}:${item.progress}`).join(',')),
        h('button', { onClick: api.stop, 'data-testid': 'stop' }, 'Stop'),
      ])
    },
  })
  const wrapper = mount(defineComponent({ setup: () => () => visible.value ? h(Child) : h('aside', 'Other route'), unmounted: stopSelectionWatch }))
  mounted.push(wrapper)
  const current = () => views[views.length - 1]!
  const leave = async () => { visible.value = false; await nextTick() }
  const returnToChat = async () => { visible.value = true; await nextTick(); await nextTick() }
  return { wrapper, auth, contact, calls, holds, views, current, leave, returnToChat }
}

beforeEach(() => {
  vi.stubGlobal('URL', Object.assign(URL, {
    createObjectURL: vi.fn(() => 'blob:batch'),
    revokeObjectURL: vi.fn(),
  }))
})
afterEach(async () => {
  window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
  for (const wrapper of mounted.splice(0)) wrapper.unmount()
  await Promise.resolve()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('application-owned attachment batches across real component remounts', () => {
  it('remounts the original queue/progress and Stop; does not create a second request owner', async () => {
    const f = fixture()
    const original = f.current()
    original.api.stage([file('first.png'), file('second.png')])
    const request = original.api.sendAll()
    f.calls[0]!.progress(2, 5)
    await f.leave()
    expect(f.holds.size).toBe(1)
    await f.returnToChat()
    const remounted = f.current()
    expect(remounted).not.toBe(original)
    expect(f.wrapper.get('[data-testid="busy"]').text()).toBe('true')
    expect(f.wrapper.get('[data-testid="progress"]').text()).toContain('first.png:sending:40')
    expect(remounted.api.isOpen.value).toBe(true)
    expect(remounted.api.stage([file('duplicate.png')]).refused).toBe('busy')
    expect(await remounted.api.sendAll()).toBeNull()
    await f.wrapper.get('[data-testid="stop"]').trigger('click')
    expect(f.calls[0]!.signal.aborted).toBe(true)
    const summary = await request
    expect(summary).toMatchObject({ unconfirmed: 1, notSent: 1, stopped: true, detached: true })
    expect(f.calls).toHaveLength(1)
    expect(f.holds.size).toBe(0)
    expect(original.sent).not.toHaveBeenCalled()
    expect(original.finished).not.toHaveBeenCalled()
    expect(remounted.finished).toHaveBeenCalledTimes(1)
    expect(await remounted.api.retryFailed()).toBeNull()
  })

  it('keeps an accepted upload on its original promise and suppresses disposed view callbacks', async () => {
    const f = fixture()
    const original = f.current()
    original.api.stage([file('accepted.png'), file('queued.png')])
    const request = original.api.sendAll()
    f.calls[0]!.progress(5, 5)
    await f.leave()
    await f.returnToChat()
    const remounted = f.current()
    remounted.api.stop()
    expect(f.calls[0]!.signal.aborted).toBe(false)
    expect(remounted.api.isStopping.value).toBe(true)
    f.calls[0]!.result.resolve({ id: 'message-a', status: 'sent' })
    await request
    expect(f.calls).toHaveLength(1)
    expect(original.sent).not.toHaveBeenCalled()
    expect(original.replySent).not.toHaveBeenCalled()
    expect(remounted.sent).toHaveBeenCalledTimes(1)
    expect(remounted.sent).toHaveBeenCalledWith({ id: 'message-a', status: 'sent' })
    expect(remounted.replySent).toHaveBeenCalledTimes(1)
    expect(remounted.api.items.value.map(item => item.status)).toEqual(['sent', 'ready'])
  })

  it('binds recipient and callbacks to the original contact across a different-chat remount', async () => {
    const f = fixture()
    f.current().api.stage([file('one.png'), file('two.png')])
    const request = f.current().api.sendAll()
    await f.leave()
    f.contact.value = 'contact-b'
    await f.returnToChat()
    expect(f.wrapper.get('[data-testid="recipient"]').text()).toBe('Patient contact-a')
    f.calls[0]!.result.resolve({ id: 'a-one' })
    await Promise.resolve()
    expect(f.calls).toHaveLength(2)
    expect(f.calls.every(call => call.request.contact_id === 'contact-a')).toBe(true)
    f.calls[1]!.result.resolve({ id: 'a-two' })
    await request
    expect(f.current().sent).not.toHaveBeenCalled()
    expect(f.current().replySent).not.toHaveBeenCalled()
    expect(f.current().finished).toHaveBeenCalledWith(expect.objectContaining({ recipient: 'Patient contact-a', sent: 2 }))
  })

  it.each(['user', 'workspace', 'session'] as const)('hides old files on %s change and waits for the original aborted request to settle', async field => {
    const f = fixture({ ignoreAbort: true })
    const original = f.current()
    original.api.stage([file('private.png'), file('never-started.png')])
    const oldRequest = original.api.sendAll()
    f.auth[field] += '-new'
    expect(f.calls[0]!.signal.aborted).toBe(true)
    expect(original.api.items.value).toHaveLength(0)
    expect(original.api.recipientName.value).toBe('')
    await f.leave()
    await f.returnToChat()
    expect(f.current().api.stage([file('new.png')]).refused).toBe('busy')
    f.calls[0]!.result.resolve({ id: 'accepted-in-old-scope' })
    expect(await oldRequest).toBeNull()
    expect(f.calls).toHaveLength(1)
    expect(f.views.every(view => view.sent.mock.calls.length === 0 && view.finished.mock.calls.length === 0)).toBe(true)
    f.current().api.stage([file('new.png')])
    const next = f.current().api.sendAll()
    expect(f.calls).toHaveLength(2)
    expect(f.current().api.isSending.value).toBe(true)
    f.calls[1]!.result.resolve({ id: 'new-scope-message' })
    await next
    expect(f.current().api.isSending.value).toBe(false)
  })

  it('does not resurrect an invalidated batch after an A-to-B-to-A identity change', async () => {
    const f = fixture({ ignoreAbort: true })
    f.current().api.stage([file('old.png'), file('never.png')])
    const request = f.current().api.sendAll()
    f.auth.workspace = 'org-b'
    f.auth.workspace = 'org-a'
    await f.leave()
    await f.returnToChat()
    expect(f.current().api.items.value).toHaveLength(0)
    f.calls[0]!.result.resolve({ id: 'late' })
    expect(await request).toBeNull()
    expect(f.calls).toHaveLength(1)
    expect(f.current().sent).not.toHaveBeenCalled()
    expect(f.current().finished).not.toHaveBeenCalled()
  })

  it('observes a transient identity change even while no ChatView is mounted', async () => {
    const f = fixture({ ignoreAbort: true })
    f.current().api.stage([file('old.png'), file('never.png')])
    const request = f.current().api.sendAll()
    await f.leave()
    f.auth.session = 'replacement-login'
    f.auth.session = 'login-a'
    expect(f.calls[0]!.signal.aborted).toBe(true)
    await f.returnToChat()
    f.calls[0]!.result.resolve({ id: 'old-accepted' })
    expect(await request).toBeNull()
    expect(f.calls).toHaveLength(1)
    expect(f.current().sent).not.toHaveBeenCalled()
    expect(f.current().finished).not.toHaveBeenCalled()
  })

  it('does not apply a late completion to a newer selection generation in the same view', async () => {
    const f = fixture()
    f.current().api.stage([file('old.png')])
    const request = f.current().api.sendAll()
    f.contact.value = 'contact-b'
    f.contact.value = 'contact-a'
    f.calls[0]!.result.resolve({ id: 'old-selection-message' })
    await request
    expect(f.current().sent).not.toHaveBeenCalled()
    expect(f.current().replySent).not.toHaveBeenCalled()
    expect(f.current().finished).toHaveBeenCalledTimes(1)
  })

  it.each(['ready', 'failed'] as const)('retains %s files until the remounted original contact finishes loading', async state => {
    const f = fixture()
    f.current().api.stage(state === 'ready' ? [file('accepted.png'), file('remaining.png')] : [file('retry.png')])
    const original = f.current().api.sendAll()
    if (state === 'ready') f.calls[0]!.progress(5, 5)
    await f.leave()
    f.contact.value = null
    await f.returnToChat()
    if (state === 'ready') {
      f.current().api.stop()
      f.calls[0]!.result.resolve({ id: 'accepted' })
    } else f.calls[0]!.result.reject(Object.assign(new Error('refused'), { response: { status: 400 } }))
    await original
    const retained = f.current().api.items.value
    expect(retained.some(item => item.status === state)).toBe(true)
    const send = () => state === 'ready' ? f.current().api.sendAll() : f.current().api.retryFailed()
    expect(await send()).toBeNull()
    expect(f.current().api.items.value).toBe(retained)
    expect(f.calls).toHaveLength(1)
    f.contact.value = 'contact-a'
    await nextTick()
    const retry = send()
    expect(f.calls).toHaveLength(2)
    expect(f.calls[1]!.request.file.name).toBe(state === 'ready' ? 'remaining.png' : 'retry.png')
    expect(f.calls[1]!.request.contact_id).toBe('contact-a')
    f.calls[1]!.result.resolve({ id: 'new-accepted' })
    await retry
  })

  it('invalidates staged files on a storage-only scope change before any request', async () => {
    const f = fixture({ plainIdentity: true })
    f.current().api.stage([file('private.png')])
    f.auth.workspace = 'org-b'
    window.dispatchEvent(new Event('storage'))
    expect(f.current().api.items.value).toHaveLength(0)
    expect(f.current().api.isOpen.value).toBe(false)
    expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1)
    expect(f.calls).toHaveLength(0)
  })

  it('releases previews and workspace hold after a batch settles with no mounted consumer', async () => {
    const f = fixture()
    const original = f.current()
    original.api.stage([file('one.png')])
    const request = original.api.sendAll()
    await f.leave()
    expect(URL.revokeObjectURL).not.toHaveBeenCalled()
    f.calls[0]!.result.resolve({ id: 'accepted' })
    await request
    await Promise.resolve()
    expect(f.holds.size).toBe(0)
    expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1)
    expect(original.sent).not.toHaveBeenCalled()
    expect(original.finished).not.toHaveBeenCalled()
    await f.returnToChat()
    expect(f.current().api.items.value).toHaveLength(0)
    expect(f.current().api.isSending.value).toBe(false)
    expect(f.current().finished).toHaveBeenCalledTimes(1)
    expect(f.current().finished).toHaveBeenCalledWith(expect.objectContaining({ sent: 1, recipient: 'Patient contact-a' }))
    await f.leave()
    await f.returnToChat()
    expect(f.current().finished).not.toHaveBeenCalled()
  })

  it('does not deliver a completed off-screen report to another login', async () => {
    const f = fixture()
    f.current().api.stage([file('private.png')])
    const request = f.current().api.sendAll()
    await f.leave()
    f.calls[0]!.result.resolve({ id: 'accepted' })
    await request
    f.auth.session = 'new-login'
    await f.returnToChat()
    expect(f.current().finished).not.toHaveBeenCalled()
    expect(f.current().api.items.value).toHaveLength(0)
    expect(f.current().api.recipientName.value).toBe('')
  })

  it('retains an unconfirmed response across remount without offering it as a failed retry', async () => {
    const f = fixture()
    f.current().api.stage([file('unknown.png')])
    const request = f.current().api.sendAll()
    f.calls[0]!.progress(5, 5)
    await f.leave()
    await f.returnToChat()
    f.calls[0]!.result.reject(Object.assign(new Error('unknown'), { response: { status: 503 } }))
    await request
    expect(f.current().api.items.value[0]!.status).toBe('unconfirmed')
    expect(await f.current().api.retryFailed()).toBeNull()
    expect(f.calls).toHaveLength(1)
  })

  it.each(['', 'ended:login-a'])('refuses app-owned staging with a missing or ended session (%s)', marker => {
    const f = fixture()
    f.auth.session = marker
    expect(f.current().api.stage([file('no-session.png')]).refused).toBe('no-conversation')
    expect(f.current().api.items.value).toHaveLength(0)
    expect(f.calls).toHaveLength(0)
  })
})
