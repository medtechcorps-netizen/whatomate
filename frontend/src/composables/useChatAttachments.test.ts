/** @vitest-environment happy-dom */

import { effectScope, nextTick, reactive } from 'vue'
import { clearBrowserOrganizationIdentity, setSelectedOrganizationId } from '@/lib/browserIdentity'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  SESSION_ENDING_EVENT,
  isAmbiguousAfterUpload,
  isReplyRejection,
  messageStatusFromRealtimeEvent,
  readWorkspaceIdentity,
  useChatAttachments,
  type RealtimeActivity,
  type SendChatMedia,
  type SendChatMediaRequest,
} from './useChatAttachments'

const mocks = vi.hoisted(() => ({ sendMedia: vi.fn() }))
vi.mock('@/services/api', () => ({ messagesService: { sendMedia: mocks.sendMedia } }))

const MiB = 1024 * 1024

function file(name: string, type: string, size = 100) {
  const f = new File(['x'], name, { type })
  Object.defineProperty(f, 'size', { value: size })
  return f
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

type Call = {
  request: SendChatMediaRequest
  progress: (loaded: number, total: number | undefined) => void
  result: ReturnType<typeof deferred<unknown>>
  signal: AbortSignal
}

function setup(overrides: Partial<Parameters<typeof useChatAttachments>[0]> = {}) {
  const calls: Call[] = []
  const send: SendChatMedia = (request, options) => {
    const result = deferred<unknown>()
    // Like axios: an aborted request rejects.
    options.signal.addEventListener('abort', () => result.reject(Object.assign(new Error('canceled'), { name: 'CanceledError' })))
    calls.push({ request, progress: options.onUploadProgress, result, signal: options.signal })
    return result.promise
  }
  const state = { contactId: 'contact-a', active: true }
  const onSent = vi.fn()
  const onBatchFinished = vi.fn()
  const scope = effectScope()
  const attachments = scope.run(() =>
    useChatAttachments({
      resolveTarget: () => {
        const id = state.contactId
        return { contactId: id, label: `Patient ${id}`, isActive: () => state.active && state.contactId === id }
      },
      getWhatsAppAccount: () => 'clinic-main',
      onSent,
      onBatchFinished,
      send,
      ...overrides,
    }),
  )!
  return { attachments, calls, state, onSent, onBatchFinished, scope }
}

async function settle() {
  for (let i = 0; i < 5; i++) await Promise.resolve()
}

beforeEach(() => {
  vi.stubGlobal('URL', Object.assign(URL, {
    createObjectURL: vi.fn(() => 'blob:preview'),
    revokeObjectURL: vi.fn(),
  }))
})

afterEach(() => {
  // End batches a test left in flight, so their window listeners go away.
  window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('useChatAttachments staging', () => {
  it('stages files with validation and opens the dialog', () => {
    const { attachments } = setup()
    const result = attachments.stage([
      file('photo.jpg', 'image/jpeg'),
      file('IMG_1.heic', 'image/heic'),
      file('lab.pdf', 'application/pdf'),
    ])
    expect(result).toEqual({ added: 3, overflow: 0, refused: null })
    expect(attachments.isOpen.value).toBe(true)
    expect(attachments.items.value.map(item => item.status)).toEqual(['ready', 'invalid', 'ready'])
    expect(attachments.items.value[0].previewUrl).toBe('blob:preview')
    expect(attachments.items.value[2].previewUrl).toBeNull()
    expect(attachments.sendableCount.value).toBe(2)
    expect(attachments.invalidCount.value).toBe(1)
  })

  it('keeps at most 10 files per batch and reports the overflow', () => {
    const { attachments } = setup()
    const twelve = Array.from({ length: 12 }, (_, i) => file(`p${i}.png`, 'image/png'))
    expect(attachments.stage(twelve)).toEqual({ added: 10, overflow: 2, refused: null })
    expect(attachments.stage([file('more.png', 'image/png')])).toEqual({ added: 0, overflow: 1, refused: null })
    expect(attachments.items.value).toHaveLength(10)
  })

  it('refuses to stage without an open conversation', () => {
    const { attachments } = setup({ resolveTarget: () => null })
    expect(attachments.stage([file('a.png', 'image/png')]).refused).toBe('no-conversation')
    expect(attachments.isOpen.value).toBe(false)
  })

  it('drops files staged for another conversation before staging new ones', () => {
    const { attachments, state } = setup()
    attachments.stage([file('for-a.png', 'image/png')])
    state.contactId = 'contact-b'
    attachments.stage([file('for-b.png', 'image/png')])
    expect(attachments.items.value.map(item => item.file.name)).toEqual(['for-b.png'])
  })

  it('removes files, revokes previews and closes when the list empties', () => {
    const { attachments } = setup()
    attachments.stage([file('a.png', 'image/png')])
    attachments.remove(attachments.items.value[0].id)
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
    expect(attachments.items.value).toHaveLength(0)
    expect(attachments.isOpen.value).toBe(false)
  })

  it('switches an oversized photo to a document on request', () => {
    const { attachments } = setup()
    attachments.stage([file('scan.jpg', 'image/jpeg', 8 * MiB)])
    const item = attachments.items.value[0]
    expect(item.status).toBe('invalid')
    attachments.sendAsDocument(item.id)
    expect(attachments.items.value[0]).toMatchObject({ status: 'ready', asDocument: true })
    expect(attachments.items.value[0].validation).toMatchObject({ ok: true, kind: 'document' })
  })

  it('caps captions at 1024 characters', () => {
    const { attachments } = setup()
    attachments.stage([file('a.png', 'image/png')])
    attachments.setCaption(attachments.items.value[0].id, 'x'.repeat(2000))
    expect(attachments.items.value[0].caption).toHaveLength(1024)
  })
})

describe('useChatAttachments sending', () => {
  it('sends one request per file, strictly one after another, in order', async () => {
    const { attachments, calls, onSent, onBatchFinished } = setup()
    attachments.stage([
      file('one.jpg', 'image/jpeg'),
      file('two.pdf', 'application/pdf'),
      file('three.ogg', 'audio/ogg'),
    ])
    attachments.setCaption(attachments.items.value[0].id, '  Your X-ray  ')
    attachments.setCaption(attachments.items.value[2].id, 'audio has no caption')

    const done = attachments.sendAll()
    await settle()
    expect(calls).toHaveLength(1)
    expect(calls[0].request).toMatchObject({
      contact_id: 'contact-a',
      type: 'image',
      caption: 'Your X-ray',
      whatsapp_account: 'clinic-main',
    })
    expect(attachments.isSending.value).toBe(true)

    calls[0].progress(50, 100)
    expect(attachments.items.value[0]).toMatchObject({ status: 'sending', progress: 50 })

    calls[0].result.resolve({ id: 'm1' })
    await settle()
    expect(attachments.items.value[0].status).toBe('sent')
    expect(onSent).toHaveBeenCalledWith({ id: 'm1' })
    expect(calls).toHaveLength(2)
    expect(calls[1].request).toMatchObject({ type: 'document' })
    expect(calls[1].request.caption).toBeUndefined()

    calls[1].result.resolve({ id: 'm2' })
    await settle()
    expect(calls).toHaveLength(3)
    expect(calls[2].request).toMatchObject({ type: 'audio' })
    expect(calls[2].request.caption).toBeUndefined()

    calls[2].result.resolve({ id: 'm3' })
    await done
    expect(onBatchFinished).toHaveBeenCalledWith(
      expect.objectContaining({ sent: 3, failed: 0, notSent: 0, recipient: 'Patient contact-a', detached: false }),
    )
    // Everything went out: the dialog closes and the list clears.
    expect(attachments.isOpen.value).toBe(false)
    expect(attachments.items.value).toHaveLength(0)
  })

  it('skips invalid files and sends oversized photos as documents', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('bad.heic', 'image/heic'), file('big.png', 'image/png', 6 * MiB)])
    attachments.sendAsDocument(attachments.items.value[1].id)
    const done = attachments.sendAll()
    await settle()
    expect(calls).toHaveLength(1)
    expect(calls[0].request).toMatchObject({ type: 'document' })
    calls[0].result.resolve({ id: 'm1' })
    await done
    // Only the skipped invalid file is left, so the batch is finished.
    expect(attachments.isOpen.value).toBe(false)
  })

  it('records per-file failures and retries only the failed files', async () => {
    const { attachments, calls, onBatchFinished } = setup()
    attachments.stage([
      file('a.png', 'image/png'),
      file('b.png', 'image/png'),
      file('c.png', 'image/png'),
    ])
    const first = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    calls[1].result.reject(Object.assign(new Error('Failed to send message'), {}))
    await settle()
    calls[2].result.resolve({ id: 'm3' })
    await first

    expect(attachments.items.value.map(item => item.status)).toEqual(['sent', 'failed', 'sent'])
    expect(attachments.items.value[1].errorMessage).toBe('Failed to send message')
    expect(attachments.failedCount.value).toBe(1)
    expect(attachments.isOpen.value).toBe(true)
    expect(onBatchFinished).toHaveBeenLastCalledWith(
      expect.objectContaining({ sent: 2, failed: 1, notSent: 0, failedNames: ['b.png'] }),
    )

    // Send does nothing now: nothing is ready.
    await attachments.sendAll()
    expect(calls).toHaveLength(3)

    const retry = attachments.retryFailed()
    await settle()
    expect(calls).toHaveLength(4)
    expect(calls[3].request.file.name).toBe('b.png')
    calls[3].result.resolve({ id: 'm2' })
    await retry
    expect(onBatchFinished).toHaveBeenLastCalledWith(expect.objectContaining({ sent: 1, failed: 0 }))
    expect(attachments.isOpen.value).toBe(false)
  })

  it('ignores a second send while a batch is uploading', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('a.png', 'image/png')])
    void attachments.sendAll()
    await settle()
    await attachments.sendAll()
    expect(calls).toHaveLength(1)
    expect(attachments.stage([file('b.png', 'image/png')]).refused).toBe('busy')
    attachments.close()
    expect(attachments.isOpen.value).toBe(true)
  })

  it('finishes a confirmed batch for its patient after the agent leaves the chat, then reports it', async () => {
    const { attachments, calls, state, onSent, onBatchFinished } = setup()
    attachments.stage([file('lab-1.pdf', 'application/pdf'), file('lab-2.pdf', 'application/pdf'), file('lab-3.pdf', 'application/pdf')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    expect(onSent).toHaveBeenCalledTimes(1)

    // The agent switches to another patient: ChatView resets the dialog.
    state.contactId = 'contact-b'
    attachments.reset()
    expect(attachments.isOpen.value).toBe(false)
    expect(attachments.items.value).toHaveLength(0)
    // Still sending, so nothing new can start until the last request settles.
    expect(attachments.isSending.value).toBe(true)
    expect(attachments.sendingTo.value).toBe('Patient contact-a')
    expect(attachments.stage([file('for-b.png', 'image/png')]).refused).toBe('busy')

    calls[1].result.resolve({ id: 'm2' })
    await settle()
    expect(calls[2].request.contact_id).toBe('contact-a')
    calls[2].result.reject(new Error('Failed to send message'))
    await done

    // Messages for the patient no longer on screen are not added to the open chat.
    expect(onSent).toHaveBeenCalledTimes(1)
    expect(onBatchFinished).toHaveBeenCalledWith({
      sent: 2,
      failed: 1,
      notSent: 0,
      unconfirmed: 0,
      failedNames: ['lab-3.pdf'],
      notSentNames: [],
      unconfirmedNames: [],
      recipient: 'Patient contact-a',
      stopped: false,
      detached: true,
    })
    expect(attachments.isSending.value).toBe(false)
    expect(attachments.stage([file('for-b.png', 'image/png')]).refused).toBeNull()
  })

  it('keeps isSending true after reset() until the in-flight upload settles', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    attachments.reset()
    expect(attachments.isSending.value).toBe(true)
    expect(await attachments.sendAll()).toBeNull()
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(attachments.isSending.value).toBe(false)
  })

  it('asks before the tab is closed while a batch is sending', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    const during = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(during)
    expect(during.defaultPrevented).toBe(true)

    calls[0].result.resolve({ id: 'm1' })
    await done
    const after = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(after)
    expect(after.defaultPrevented).toBe(false)
  })

  it('attaches replyTo to the first file of the batch only', async () => {
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1' })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request.reply_to_message_id).toBe('incoming-1')
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    expect(calls[1].request.reply_to_message_id).toBeUndefined()
    calls[1].result.resolve({ id: 'm2' })
    await done
  })

  it('types untyped files from their extension before upload', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('Results.docx', '')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request.file.type).toBe(
      'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
    )
    calls[0].result.resolve({ id: 'm1' })
    await done
  })

  it('uses messagesService.sendMedia by default and unwraps the envelope', async () => {
    mocks.sendMedia.mockResolvedValue({ data: { status: 'success', data: { id: 'server-message' } } })
    const onSent = vi.fn()
    const scope = effectScope()
    const attachments = scope.run(() =>
      useChatAttachments({
        resolveTarget: () => ({ contactId: 'c1', isActive: () => true }),
        onSent,
      }),
    )!
    attachments.stage([file('a.png', 'image/png')])
    await attachments.sendAll()
    expect(mocks.sendMedia).toHaveBeenCalledTimes(1)
    expect(mocks.sendMedia.mock.calls[0][0]).toMatchObject({ contact_id: 'c1', type: 'image' })
    expect(mocks.sendMedia.mock.calls[0][0]).not.toHaveProperty('whatsapp_account')
    expect(onSent).toHaveBeenCalledWith({ id: 'server-message' })
    scope.stop()
  })

  it('cleans up previews when its scope is disposed', () => {
    const { attachments, scope } = setup()
    attachments.stage([file('a.png', 'image/png')])
    scope.stop()
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
    expect(attachments.items.value).toHaveLength(0)
  })

  it('converts a rejected photo to a JPEG and makes it sendable', async () => {
    const jpeg = file('IMG_1.jpg', 'image/jpeg', 2 * MiB)
    const convertImage = vi.fn(async () => jpeg)
    const { attachments, calls } = setup({ convertImage })
    attachments.stage([file('IMG_1.HEIC', 'image/heic', 3 * MiB)])
    const id = attachments.items.value[0].id
    expect(attachments.items.value[0].validation).toMatchObject({ ok: false, canConvertToJpeg: true })

    const converting = attachments.convertToJpeg(id)
    expect(attachments.items.value[0].status).toBe('converting')
    expect(attachments.convertingCount.value).toBe(1)
    // Sending waits for conversions to finish.
    await attachments.sendAll()
    expect(calls).toHaveLength(0)

    await expect(converting).resolves.toBe(true)
    expect(attachments.items.value[0]).toMatchObject({
      status: 'ready',
      convertedFrom: 'image/heic',
      conversionFailed: false,
      previewUrl: 'blob:preview',
    })
    expect(attachments.items.value[0].file).toBe(jpeg)
    expect(attachments.items.value[0].validation).toMatchObject({ ok: true, kind: 'image' })

    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request).toMatchObject({ type: 'image' })
    expect(calls[0].request.file.name).toBe('IMG_1.jpg')
    calls[0].result.resolve({ id: 'm1' })
    await done
  })

  it('marks a failed conversion and keeps the file rejected', async () => {
    const { attachments } = setup({ convertImage: vi.fn(async () => Promise.reject(new Error('decode'))) })
    attachments.stage([file('IMG_2.HEIC', 'image/heic', MiB)])
    await expect(attachments.convertToJpeg(attachments.items.value[0].id)).resolves.toBe(false)
    expect(attachments.items.value[0]).toMatchObject({ status: 'invalid', conversionFailed: true })
  })

  it('converts photos one at a time and skips ones that already failed', async () => {
    const pending: Array<ReturnType<typeof deferred<File>>> = []
    const convertImage = vi.fn(() => {
      const next = deferred<File>()
      pending.push(next)
      return next.promise
    })
    const { attachments } = setup({ convertImage })
    attachments.stage([
      file('a.webp', 'image/webp', MiB),
      file('ok.png', 'image/png', MiB),
      file('b.gif', 'image/gif', MiB),
    ])
    const all = attachments.convertAllToJpeg()
    await settle()
    expect(convertImage).toHaveBeenCalledTimes(1)
    pending[0].resolve(file('a.jpg', 'image/jpeg', MiB))
    await settle()
    expect(convertImage).toHaveBeenCalledTimes(2)
    pending[1].resolve(file('b.jpg', 'image/jpeg', MiB))
    await all
    expect(attachments.items.value.map(item => item.status)).toEqual(['ready', 'ready', 'ready'])
    expect(attachments.items.value[2].convertedFrom).toBe('image/gif')
  })

  it('drops a conversion that finishes after the conversation changed', async () => {
    const pending = deferred<File>()
    const { attachments } = setup({ convertImage: () => pending.promise })
    attachments.stage([file('a.webp', 'image/webp', MiB)])
    const converting = attachments.convertToJpeg(attachments.items.value[0].id)
    attachments.reset()
    pending.resolve(file('a.jpg', 'image/jpeg', MiB))
    await expect(converting).resolves.toBe(false)
    expect(attachments.items.value).toHaveLength(0)
  })

  it('does not start a conversion while a batch is uploading', async () => {
    const convertImage = vi.fn(async () => file('a.jpg', 'image/jpeg', MiB))
    const { attachments, calls } = setup({ convertImage })
    attachments.stage([file('ready.png', 'image/png'), file('a.webp', 'image/webp', MiB)])
    const done = attachments.sendAll()
    await settle()
    await expect(attachments.convertToJpeg(attachments.items.value[1].id)).resolves.toBe(false)
    expect(convertImage).not.toHaveBeenCalled()
    calls[0].result.resolve({ id: 'm1' })
    await done
  })

  it('does not convert files that are not convertible', async () => {
    const convertImage = vi.fn()
    const { attachments } = setup({ convertImage })
    attachments.stage([file('ok.png', 'image/png'), file('a.mov', 'video/quicktime', MiB)])
    await attachments.convertToJpeg(attachments.items.value[0].id)
    await attachments.convertToJpeg(attachments.items.value[1].id)
    expect(convertImage).not.toHaveBeenCalled()
  })
})

function realtime() {
  const listeners = new Set<(event: RealtimeActivity) => void>()
  return {
    subscribeRealtime: (listener: (event: RealtimeActivity) => void) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    emit: (event: RealtimeActivity) => listeners.forEach(listener => listener(event)),
    get listenerCount() {
      return listeners.size
    },
  }
}

describe('useChatAttachments pacing', () => {
  it('starts the next upload only after the previous message leaves pending', async () => {
    const live = realtime()
    const { attachments, calls } = setup({ subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    // Accepted by the server but still pending at the provider: hold file 2.
    expect(calls).toHaveLength(1)
    expect(attachments.items.value[0]).toMatchObject({ status: 'sending', progress: 100 })

    live.emit({ type: 'status_update', payload: { message_id: 'other', status: 'sent' } })
    await settle()
    expect(calls).toHaveLength(1)

    live.emit({ type: 'status_update', payload: { message_id: 'm1', status: 'sent' } })
    await settle()
    expect(calls).toHaveLength(2)
    expect(attachments.items.value[0].status).toBe('sent')

    calls[1].result.resolve({ id: 'm2', status: 'pending' })
    await settle()
    live.emit({ type: 'realtime_sync', payload: { kind: 'message_status_changed', message_id: 'm2', status: 'delivered' } })
    await done
    expect(live.listenerCount).toBe(0)
  })

  it('counts a status update that arrives before the HTTP response', async () => {
    const live = realtime()
    const { attachments, calls } = setup({ subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    live.emit({ type: 'status_update', payload: { message_id: 'm1', status: 'sent' } })
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    expect(calls).toHaveLength(2)
    calls[1].result.resolve({ id: 'm2', status: 'sent' })
    await done
  })

  it('reports a message the provider rejected as failed', async () => {
    const live = realtime()
    const { attachments, calls, onBatchFinished } = setup({ subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    live.emit({ type: 'status_update', payload: { message_id: 'm1', status: 'failed', error_message: 'Media upload error' } })
    await settle()
    calls[1].result.resolve({ id: 'm2', status: 'sent' })
    await done
    expect(attachments.items.value[0]).toMatchObject({ status: 'failed', errorMessage: 'Media upload error' })
    expect(onBatchFinished).toHaveBeenCalledWith(expect.objectContaining({ sent: 1, failed: 1, failedNames: ['a.png'] }))
  })

  it('moves on after the bounded wait when no status arrives', async () => {
    const live = realtime()
    const { attachments, calls } = setup({ subscribeRealtime: live.subscribeRealtime, settleTimeoutMs: 20 })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    expect(calls).toHaveLength(1)
    await new Promise(resolve => setTimeout(resolve, 40))
    expect(calls).toHaveLength(2)
    expect(attachments.items.value[0].status).toBe('sent')
    calls[1].result.resolve({ id: 'm2', status: 'sent' })
    await done
  })

  it('sends without pacing when realtime events are unavailable', async () => {
    const { attachments, calls } = setup({
      subscribeRealtime: () => {
        throw new Error('websocket not ready')
      },
    })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    expect(calls).toHaveLength(2)
    calls[1].result.resolve({ id: 'm2', status: 'pending' })
    await done
  })
})

describe('useChatAttachments stop', () => {
  it('aborts a file still uploading, marks it unconfirmed and leaves unstarted files ready', async () => {
    const { attachments, calls, onBatchFinished } = setup()
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png'), file('c.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    calls[1].progress(30, 100)
    attachments.stop()
    expect(attachments.isStopping.value).toBe(true)
    expect(calls[1].signal.aborted).toBe(true)
    await done

    expect(calls).toHaveLength(2)
    // Progress events lag the network: the server may already have b.png.
    expect(attachments.items.value.map(item => item.status)).toEqual(['sent', 'unconfirmed', 'ready'])
    expect(onBatchFinished).toHaveBeenCalledWith(
      expect.objectContaining({
        sent: 1,
        failed: 0,
        notSent: 1,
        unconfirmed: 1,
        notSentNames: ['c.png'],
        unconfirmedNames: ['b.png'],
        stopped: true,
      }),
    )
    // The dialog stays open with the unsent files ready to send again.
    expect(attachments.isOpen.value).toBe(true)
    expect(attachments.isSending.value).toBe(false)
    expect(attachments.isStopping.value).toBe(false)
  })

  it('lets a fully uploaded file finish instead of aborting it', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(100, 100)
    attachments.stop()
    expect(calls[0].signal.aborted).toBe(false)
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(calls).toHaveLength(1)
    expect(attachments.items.value.map(item => item.status)).toEqual(['sent', 'ready'])
  })

  it('ends a pacing wait at once', async () => {
    const live = realtime()
    const { attachments, calls } = setup({ subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(100, 100)
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    attachments.stop()
    await done
    expect(calls).toHaveLength(1)
    expect(attachments.items.value.map(item => item.status)).toEqual(['sent', 'ready'])
  })

  it('does nothing without a running batch or once the batch is detached', async () => {
    const { attachments, calls } = setup()
    attachments.stop()
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    attachments.reset()
    attachments.stop()
    expect(calls[0].signal.aborted).toBe(false)
    calls[0].result.resolve({ id: 'm1' })
    await done
  })
})

describe('messageStatusFromRealtimeEvent', () => {
  it('reads status_update and realtime_sync message status events', () => {
    expect(messageStatusFromRealtimeEvent({ type: 'status_update', payload: { message_id: 'm1', status: 'sent' } })).toEqual({
      messageId: 'm1',
      status: 'sent',
    })
    expect(
      messageStatusFromRealtimeEvent({
        type: 'realtime_sync',
        payload: { kind: 'message_status_changed', message_id: 'm2', status: 'failed', error_message: 'x' },
      }),
    ).toEqual({ messageId: 'm2', status: 'failed', errorMessage: 'x' })
  })

  it('ignores other events and malformed payloads', () => {
    expect(messageStatusFromRealtimeEvent({ type: 'new_message', payload: { message_id: 'm1', status: 'sent' } })).toBeNull()
    expect(messageStatusFromRealtimeEvent({ type: 'realtime_sync', payload: { kind: 'conversation_changed', message_id: 'm1', status: 'sent' } })).toBeNull()
    expect(messageStatusFromRealtimeEvent({ type: 'status_update', payload: { status: 'sent' } })).toBeNull()
    expect(messageStatusFromRealtimeEvent({ type: 'status_update' })).toBeNull()
    expect(messageStatusFromRealtimeEvent(null)).toBeNull()
  })
})

describe('useChatAttachments session binding', () => {
  function signedIn(userId: string | null = 'agent-1') {
    const auth = reactive({ userId: userId as string | null })
    return {
      auth,
      session: { userId: () => auth.userId, isAuthenticated: () => auth.userId !== null },
    }
  }

  it('aborts everything, drops the unload prompt and reports nothing when the session is ending', async () => {
    const { session } = signedIn()
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('lab-1.pdf', 'application/pdf'), file('lab-2.pdf', 'application/pdf')])
    const done = attachments.sendAll()
    await settle()

    window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
    expect(calls[0].signal.aborted).toBe(true)
    // Right away, before the request settles: no "Leave site?" over patient data.
    const unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(false)
    // The staging dialog is cleared too.
    expect(attachments.isOpen.value).toBe(false)
    expect(attachments.items.value).toHaveLength(0)

    await done
    expect(calls).toHaveLength(1)
    expect(onSent).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
    expect(attachments.isSending.value).toBe(false)
  })

  it('ends the batch when the auth store signs out, without waiting for the event', async () => {
    const { auth, session } = signedIn()
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    auth.userId = null
    await nextTick()
    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(calls).toHaveLength(1)
    expect(onBatchFinished).not.toHaveBeenCalled()
  })

  it('never continues a batch under a different user', async () => {
    const { auth, session } = signedIn('agent-1')
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    // Another agent signs in in the same tab while file 1 is in flight.
    auth.userId = 'agent-2'
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(calls).toHaveLength(1)
    expect(onSent).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
  })

  it('does not start a batch when nobody is signed in', async () => {
    const { session } = signedIn(null)
    const { attachments, calls } = setup({ session })
    attachments.stage([file('a.png', 'image/png')])
    expect(await attachments.sendAll()).toBeNull()
    expect(calls).toHaveLength(0)
  })

  it('clears staged files on session end even with no batch running', () => {
    const { attachments } = setup()
    attachments.stage([file('a.png', 'image/png')])
    window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
    expect(attachments.isOpen.value).toBe(false)
    expect(attachments.items.value).toHaveLength(0)
  })

  it('removes its session listeners when its scope is disposed', () => {
    const { attachments, scope } = setup()
    scope.stop()
    attachments.stage([file('a.png', 'image/png')])
    window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
    expect(attachments.items.value).toHaveLength(1)
  })
})

describe('useChatAttachments deadlines and the last file', () => {
  it('marks a file unconfirmed when the server does not answer in time after the upload', async () => {
    const { attachments, calls, onBatchFinished } = setup({ responseDeadlineMs: 20 })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(100, 100)
    await new Promise(resolve => setTimeout(resolve, 40))
    expect(calls[0].signal.aborted).toBe(true)
    await settle()
    // The batch moves on to the next file.
    expect(calls).toHaveLength(2)
    calls[1].result.resolve({ id: 'm2' })
    await done
    expect(attachments.items.value.map(item => item.status)).toEqual(['unconfirmed', 'sent'])
    expect(onBatchFinished).toHaveBeenCalledWith(
      expect.objectContaining({ sent: 1, unconfirmed: 1, unconfirmedNames: ['a.png'] }),
    )
    // Kept on screen so the agent sees which file to check.
    expect(attachments.isOpen.value).toBe(true)
  })

  it('does not hold a one-file batch on Sending for the pacing wait', async () => {
    const live = realtime()
    const { attachments, calls } = setup({ subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await done
    expect(attachments.isSending.value).toBe(false)
    expect(live.listenerCount).toBe(0)
  })

  it('still uses a status that arrived for the last file before its response', async () => {
    const live = realtime()
    const { attachments, calls } = setup({ subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    live.emit({ type: 'status_update', payload: { message_id: 'm1', status: 'failed', error_message: 'Rejected' } })
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await done
    expect(attachments.items.value[0]).toMatchObject({ status: 'failed', errorMessage: 'Rejected' })
  })
})

describe('useChatAttachments reply with attachment', () => {
  it('asks for the reply with the batch patient, and only the first file quotes it', async () => {
    const replyTo = vi.fn(() => 'incoming-1')
    const onReplySent = vi.fn()
    const { attachments, calls } = setup({ replyTo, onReplySent })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png'), file('c.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(replyTo).toHaveBeenCalledTimes(1)
    expect(replyTo).toHaveBeenCalledWith(expect.objectContaining({ contactId: 'contact-a' }))
    expect(calls[0].request.reply_to_message_id).toBe('incoming-1')
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    expect(onReplySent).toHaveBeenCalledTimes(1)
    expect(onReplySent).toHaveBeenCalledWith('incoming-1', expect.objectContaining({ contactId: 'contact-a' }))
    expect(calls[1].request).not.toHaveProperty('reply_to_message_id')
    calls[1].result.resolve({ id: 'm2' })
    await settle()
    expect(calls[2].request).not.toHaveProperty('reply_to_message_id')
    calls[2].result.resolve({ id: 'm3' })
    await done
    expect(onReplySent).toHaveBeenCalledTimes(1)
  })

  it('does not move the quote to the next file when the first one fails', async () => {
    const onReplySent = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1', onReplySent })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(new Error('Failed to send message'))
    await settle()
    expect(calls[1].request).not.toHaveProperty('reply_to_message_id')
    calls[1].result.resolve({ id: 'm2' })
    await done
    expect(onReplySent).not.toHaveBeenCalled()

    // Retrying the failed first file quotes again while the reply is still set.
    const retry = attachments.retryFailed()
    await settle()
    expect(calls[2].request.file.name).toBe('a.png')
    expect(calls[2].request.reply_to_message_id).toBe('incoming-1')
    calls[2].result.resolve({ id: 'm1' })
    await retry
    expect(onReplySent).toHaveBeenCalledTimes(1)
  })

  it('keeps the reply when the provider rejects the quoting file', async () => {
    const live = realtime()
    const onReplySent = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1', onReplySent, subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    live.emit({ type: 'status_update', payload: { message_id: 'm1', status: 'failed', error_message: 'Reply context invalid' } })
    await settle()
    calls[1].result.resolve({ id: 'm2', status: 'sent' })
    await done
    expect(onReplySent).not.toHaveBeenCalled()
  })

  it('sends without a quote when there is no reply', async () => {
    const { attachments, calls } = setup({ replyTo: () => undefined })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request).not.toHaveProperty('reply_to_message_id')
    calls[0].result.resolve({ id: 'm1' })
    await done
  })
})

describe('useChatAttachments reply quote ownership', () => {
  function replyRefused() {
    return Object.assign(new Error('Request failed with status code 400'), {
      response: { status: 400, data: { message: 'Reply message not found for this contact' } },
    })
  }

  it('binds the quote to the first file when the batch starts', async () => {
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1' })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(attachments.items.value.map(item => item.quotesReply)).toEqual(['incoming-1', null])
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    calls[1].result.resolve({ id: 'm2' })
    await done
  })

  it('(a) unconfirmed A + failed B: retrying B does not inherit A\'s quote', async () => {
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1', responseDeadlineMs: 20 })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request.reply_to_message_id).toBe('incoming-1')
    calls[0].progress(100, 100)
    await new Promise(resolve => setTimeout(resolve, 40))
    await settle()
    calls[1].result.reject(new Error('Failed to send message'))
    await done
    expect(attachments.items.value.map(item => item.status)).toEqual(['unconfirmed', 'failed'])

    const retry = attachments.retryFailed()
    await settle()
    expect(calls[2].request.file.name).toBe('b.png')
    expect(calls[2].request).not.toHaveProperty('reply_to_message_id')
    calls[2].result.resolve({ id: 'm2' })
    await retry
  })

  it('(b) A fails and is removed: retrying B does not inherit the quote', async () => {
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1' })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(new Error('Failed to send message'))
    await settle()
    calls[1].result.reject(new Error('Failed to send message'))
    await done

    attachments.remove(attachments.items.value[0].id)
    expect(attachments.items.value.map(item => item.file.name)).toEqual(['b.png'])
    const retry = attachments.retryFailed()
    await settle()
    expect(calls[2].request.file.name).toBe('b.png')
    expect(calls[2].request).not.toHaveProperty('reply_to_message_id')
    calls[2].result.resolve({ id: 'm2' })
    await retry
  })

  it('a resumed run after Stop does not move the quote to the next file', async () => {
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1' })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(30, 100)
    attachments.stop()
    await done
    expect(attachments.items.value.map(item => item.status)).toEqual(['unconfirmed', 'ready'])

    const resumed = attachments.sendAll()
    await settle()
    expect(calls[1].request.file.name).toBe('b.png')
    expect(calls[1].request).not.toHaveProperty('reply_to_message_id')
    calls[1].result.resolve({ id: 'm2' })
    await resumed
  })

  it('the owner sends without the quote once the agent cancels the reply', async () => {
    let reply: string | undefined = 'incoming-1'
    const { attachments, calls } = setup({ replyTo: () => reply })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(new Error('Failed to send message'))
    await done
    reply = undefined
    const retry = attachments.retryFailed()
    await settle()
    expect(calls[1].request).not.toHaveProperty('reply_to_message_id')
    calls[1].result.resolve({ id: 'm1' })
    await retry
  })

  it('offers send-without-quote when the server refuses the reply, and the quote never moves', async () => {
    const onReplyRejected = vi.fn()
    const onReplySent = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1', onReplyRejected, onReplySent })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(replyRefused())
    await settle()
    expect(attachments.items.value[0]).toMatchObject({ status: 'failed', replyRejected: true })
    expect(onReplyRejected).toHaveBeenCalledWith('incoming-1', expect.objectContaining({ contactId: 'contact-a' }))
    expect(calls[1].request).not.toHaveProperty('reply_to_message_id')
    calls[1].result.reject(new Error('Failed to send message'))
    await done

    // Only A is resent, as a plain attachment; B's failure is left for Retry.
    const plain = attachments.sendWithoutQuote(attachments.items.value[0].id)
    await settle()
    expect(calls).toHaveLength(3)
    expect(calls[2].request.file.name).toBe('a.png')
    expect(calls[2].request).not.toHaveProperty('reply_to_message_id')
    calls[2].result.resolve({ id: 'm1' })
    await plain
    expect(attachments.items.value.map(item => item.status)).toEqual(['sent', 'failed'])
    expect(onReplySent).not.toHaveBeenCalled()

    // Even with the reply still selected, no other file picks the quote up.
    const retry = attachments.retryFailed()
    await settle()
    expect(calls[3].request.file.name).toBe('b.png')
    expect(calls[3].request).not.toHaveProperty('reply_to_message_id')
    calls[3].result.resolve({ id: 'm2' })
    await retry
  })

  it('ignores send-without-quote for a file the server did not refuse', async () => {
    const { attachments } = setup({ replyTo: () => 'incoming-1' })
    attachments.stage([file('a.png', 'image/png')])
    expect(await attachments.sendWithoutQuote(attachments.items.value[0].id)).toBeNull()
  })

  it('a new staging session can quote again', async () => {
    const { attachments, calls } = setup({ replyTo: () => 'incoming-1' })
    attachments.stage([file('a.png', 'image/png')])
    const first = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1' })
    await first
    // All sent: the dialog closed and its claims went with it.
    attachments.stage([file('b.png', 'image/png')])
    const second = attachments.sendAll()
    await settle()
    expect(calls[1].request.reply_to_message_id).toBe('incoming-1')
    calls[1].result.resolve({ id: 'm2' })
    await second
  })
})

describe('useChatAttachments workspace and session binding', () => {
  // Reactive identity: a same-tab change through the stores.
  function reactiveIdentity(marker: string | null = 'session-1') {
    const ws = reactive({ workspace: 'org-1', marker })
    return {
      ws,
      session: {
        userId: () => 'agent-1',
        isAuthenticated: () => true,
        workspace: () => ws.workspace,
        logicalSession: () => ws.marker,
      },
    }
  }

  // Plain identity: localStorage written without any reactive store.
  function plainIdentity() {
    const ws = { workspace: 'org-1', marker: 'session-1' as string | null }
    return {
      ws,
      session: {
        userId: () => 'agent-1',
        isAuthenticated: () => true,
        workspace: () => ws.workspace,
        logicalSession: () => ws.marker,
      },
    }
  }

  function expectStopped(
    attachments: ReturnType<typeof setup>['attachments'],
    onBatchFinished: ReturnType<typeof vi.fn>,
  ) {
    expect(onBatchFinished).not.toHaveBeenCalled()
    expect(attachments.isSending.value).toBe(false)
    expect(attachments.items.value).toHaveLength(0)
    expect(attachments.isOpen.value).toBe(false)
  }

  it('stops queued work when the workspace changes during pacing', async () => {
    const live = realtime()
    const { ws, session } = reactiveIdentity()
    const { attachments, calls, onBatchFinished, onSent } = setup({ session, subscribeRealtime: live.subscribeRealtime })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.resolve({ id: 'm1', status: 'pending' })
    await settle()
    expect(onSent).toHaveBeenCalledTimes(1)
    // Same user switches workspace while file 1 is still pending at the provider.
    ws.workspace = 'org-2'
    await nextTick()
    await done
    expect(calls).toHaveLength(1)
    expectStopped(attachments, onBatchFinished)
  })

  it('aborts an upload in flight when the workspace changes, and never resends it', async () => {
    const { ws, session } = reactiveIdentity()
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(40, 100)
    ws.workspace = 'org-2'
    await nextTick()
    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(calls).toHaveLength(1)
    expect(onSent).not.toHaveBeenCalled()
    expectStopped(attachments, onBatchFinished)
  })

  it('lets a workspace-switch reload go through: no prompt, and nothing continues if it is cancelled', async () => {
    const { ws, session } = plainIdentity()
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    // The switcher writes the new organization, then reloads.
    ws.workspace = 'org-2'
    const unload = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(unload)
    expect(unload.defaultPrevented).toBe(false)
    expect(calls[0].signal.aborted).toBe(true)
    // Had the reload been cancelled, the batch is already over.
    await done
    expect(calls).toHaveLength(1)
    expectStopped(attachments, onBatchFinished)
    const again = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(again)
    expect(again.defaultPrevented).toBe(false)
  })

  it('stops when another tab selects a different organization', async () => {
    const { ws, session } = plainIdentity()
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    ws.workspace = 'org-2'
    window.dispatchEvent(new Event('storage'))
    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(calls).toHaveLength(1)
    expectStopped(attachments, onBatchFinished)
  })

  it('stops when the same user signs in again (new logical session)', async () => {
    const { ws, session } = plainIdentity()
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    ws.marker = 'session-2'
    window.dispatchEvent(new Event('storage'))
    await done
    expect(calls).toHaveLength(1)
    expectStopped(attachments, onBatchFinished)
  })

  it('checks identity before each dispatch even without any event', async () => {
    const { ws, session } = plainIdentity()
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    ws.workspace = 'org-2'
    calls[0].result.resolve({ id: 'm1' })
    await done
    // File 1 was accepted (in the old workspace) but nothing is added to the
    // new one, file 2 is never dispatched and no summary appears.
    expect(onSent).not.toHaveBeenCalled()
    expect(calls).toHaveLength(1)
    expectStopped(attachments, onBatchFinished)
  })

  it('keeps sending across ordinary chat navigation in the same workspace', async () => {
    const { session } = reactiveIdentity()
    const { attachments, calls, state, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    state.contactId = 'contact-b'
    attachments.reset()
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    expect(calls).toHaveLength(2)
    expect(calls[1].request.contact_id).toBe('contact-a')
    calls[1].result.resolve({ id: 'm2' })
    await done
    expect(onBatchFinished).toHaveBeenCalledWith(expect.objectContaining({ sent: 2, detached: true }))
  })

  it('works without the logical-session marker and fails closed when it appears', async () => {
    const { ws, session } = reactiveIdentity(null)
    const first = setup({ session })
    first.attachments.stage([file('a.png', 'image/png')])
    const done = first.attachments.sendAll()
    await settle()
    first.calls[0].result.resolve({ id: 'm1' })
    await done
    expect(first.onBatchFinished).toHaveBeenCalledWith(expect.objectContaining({ sent: 1 }))

    first.attachments.stage([file('b.png', 'image/png'), file('c.png', 'image/png')])
    const second = first.attachments.sendAll()
    await settle()
    ws.marker = 'session-9'
    await nextTick()
    await second
    expect(first.calls).toHaveLength(2)
  })

  it('holds workspace switching for the whole batch and releases it at the end', async () => {
    const release = vi.fn()
    const holdWorkspace = vi.fn(() => release)
    const { attachments, calls } = setup({ holdWorkspace })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(holdWorkspace).toHaveBeenCalledWith('Patient contact-a', expect.stringMatching(/^chat-media-batch:\d+$/))
    expect(release).not.toHaveBeenCalled()
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(release).toHaveBeenCalledTimes(1)
  })

  it('releases the workspace hold when the session ends', async () => {
    const release = vi.fn()
    const { attachments } = setup({ holdWorkspace: () => release })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    window.dispatchEvent(new CustomEvent(SESSION_ENDING_EVENT))
    await done
    expect(release).toHaveBeenCalledTimes(1)
  })
})

describe('useChatAttachments ambiguous failure after upload (FU-12)', () => {
  function httpError(status: number, data?: unknown) {
    return Object.assign(new Error(`Request failed with status code ${status}`), { response: { status, data } })
  }

  it('marks a 504 after the full upload unconfirmed and does not offer a retry', async () => {
    const { attachments, calls, onBatchFinished } = setup()
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(100, 100)
    calls[0].result.reject(httpError(504, '<html>Gateway Timeout</html>'))
    await done
    expect(attachments.items.value[0].status).toBe('unconfirmed')
    expect(onBatchFinished).toHaveBeenCalledWith(expect.objectContaining({ failed: 0, unconfirmed: 1 }))
    expect(await attachments.retryFailed()).toBeNull()
    expect(calls).toHaveLength(1)
  })

  it('treats a lost connection after the full upload as unconfirmed', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(100, 100)
    calls[0].result.reject(Object.assign(new Error('Network Error'), { code: 'ERR_NETWORK' }))
    await done
    expect(attachments.items.value[0].status).toBe('unconfirmed')
  })

  it('keeps a 4xx after the full upload as failed: a definite refusal', async () => {
    for (const error of [httpError(400, { status: 'error', message: 'Invalid multipart form' }), httpError(404, '<html>Not Found</html>')]) {
      const { attachments, calls } = setup()
      attachments.stage([file('a.png', 'image/png')])
      const done = attachments.sendAll()
      await settle()
      calls[0].progress(100, 100)
      calls[0].result.reject(error)
      await done
      expect(attachments.items.value[0].status).toBe('failed')
    }
  })

  it('keeps a 5xx before the upload completed as failed', async () => {
    const { attachments, calls } = setup()
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(50, 100)
    calls[0].result.reject(httpError(503))
    await done
    expect(attachments.items.value[0].status).toBe('failed')
  })

  it('classifies errors with isAmbiguousAfterUpload', () => {
    expect(isAmbiguousAfterUpload(httpError(502))).toBe(true)
    expect(isAmbiguousAfterUpload(httpError(524, 'origin timeout'))).toBe(true)
    expect(isAmbiguousAfterUpload(new Error('Network Error'))).toBe(true)
    // The media route never emits a bare 5xx: those are gateway artifacts.
    expect(isAmbiguousAfterUpload(httpError(500))).toBe(true)
    expect(isAmbiguousAfterUpload(httpError(520, '<html>unknown error</html>'))).toBe(true)
    // An enveloped 5xx proves nothing about dispatch either.
    expect(isAmbiguousAfterUpload(httpError(500, { status: 'error', message: 'Failed to send message' }))).toBe(true)
    expect(isAmbiguousAfterUpload(httpError(404))).toBe(false)
    expect(isAmbiguousAfterUpload(httpError(503, { status: 'error', message: 'Failed' }))).toBe(true)
    expect(isAmbiguousAfterUpload(httpError(400, { status: 'error', message: 'Invalid multipart form' }))).toBe(false)
  })
})

describe('useChatAttachments INT-2 follow-ups', () => {
  function refused(message = 'Reply message not found for this contact') {
    return Object.assign(new Error('Request failed with status code 400'), {
      response: { status: 400, data: { status: 'error', message } },
    })
  }

  it('INT2-PLAIN-ACTION-REBIND: a plain resend never binds a newly selected reply', async () => {
    let reply: string | undefined = 'Q1'
    const onReplyRejected = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => reply, onReplyRejected })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request.reply_to_message_id).toBe('Q1')
    calls[0].result.reject(refused())
    await settle()
    calls[1].result.reject(new Error('Failed to send message'))
    await done
    expect(onReplyRejected).toHaveBeenCalledWith('Q1', expect.anything())

    // The agent picks another message to reply to, then sends A without quote.
    reply = 'Q2'
    const plain = attachments.sendWithoutQuote(attachments.items.value[0].id)
    await settle()
    expect(calls).toHaveLength(3)
    expect(calls[2].request.file.name).toBe('a.png')
    expect(calls[2].request).not.toHaveProperty('reply_to_message_id')
    expect(attachments.items.value[0].quotesReply).toBeNull()
    calls[2].result.resolve({ id: 'm-a' })
    await plain
    expect(attachments.items.value[0].quotesReply).toBeNull()
    // B is not resent by the plain action.
    expect(calls).toHaveLength(3)
    expect(attachments.items.value.map(item => item.status)).toEqual(['sent', 'failed'])

    // Q2 was left unclaimed: an intentional send now quotes it (Q1 stays claimed).
    const retry = attachments.retryFailed()
    await settle()
    expect(calls[3].request.file.name).toBe('b.png')
    expect(calls[3].request.reply_to_message_id).toBe('Q2')
    calls[3].result.resolve({ id: 'm-b' })
    await retry
  })

  it('send-without-quote while another file is converting changes nothing', async () => {
    const pending = deferred<File>()
    const { attachments, calls } = setup({ replyTo: () => 'Q1', convertImage: () => pending.promise })
    attachments.stage([file('a.png', 'image/png'), file('c.webp', 'image/webp')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(refused())
    await done
    const a = attachments.items.value[0]
    expect(a).toMatchObject({ status: 'failed', replyRejected: true, quotesReply: 'Q1' })

    void attachments.convertToJpeg(attachments.items.value[1].id)
    expect(await attachments.sendWithoutQuote(a.id)).toBeNull()
    expect(calls).toHaveLength(1)
    expect(attachments.items.value[0]).toMatchObject({ status: 'failed', replyRejected: true, quotesReply: 'Q1' })
    pending.resolve(file('c.jpg', 'image/jpeg'))
    await settle()
  })

  it('resets the refusal on the next attempt (thrown error)', async () => {
    let reply: string | undefined = 'Q1'
    const { attachments, calls } = setup({ replyTo: () => reply })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(refused())
    await done
    expect(attachments.items.value[0].replyRejected).toBe(true)

    reply = undefined
    const retry = attachments.retryFailed()
    await settle()
    expect(attachments.items.value[0].replyRejected).toBe(false)
    calls[1].result.reject(new Error('Failed to send message'))
    await retry
    expect(attachments.items.value[0]).toMatchObject({ status: 'failed', replyRejected: false, errorMessage: 'Failed to send message' })
  })

  it('resets the refusal on the next attempt (2xx with status failed)', async () => {
    let reply: string | undefined = 'Q1'
    const { attachments, calls } = setup({ replyTo: () => reply })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].result.reject(refused('Invalid reply message ID'))
    await done
    reply = undefined
    const retry = attachments.retryFailed()
    await settle()
    calls[1].result.resolve({ id: 'm1', status: 'failed', error_message: 'Media upload error' })
    await retry
    expect(attachments.items.value[0]).toMatchObject({ status: 'failed', replyRejected: false, errorMessage: 'Media upload error' })
  })

  it('stops the quote with the batch when the workspace changes', async () => {
    const ws = reactive({ workspace: 'org-1' })
    const onReplySent = vi.fn()
    const onReplyRejected = vi.fn()
    const session = { userId: () => 'agent-1', isAuthenticated: () => true, workspace: () => ws.workspace }
    const { attachments, calls } = setup({ replyTo: () => 'Q1', onReplySent, onReplyRejected, session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request.reply_to_message_id).toBe('Q1')
    ws.workspace = 'org-2'
    await nextTick()
    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(calls).toHaveLength(1)
    expect(onReplySent).not.toHaveBeenCalled()
    expect(onReplyRejected).not.toHaveBeenCalled()
    expect(attachments.items.value).toHaveLength(0)
  })
})

describe('isReplyRejection', () => {
  const http = (status: number, data?: unknown) =>
    Object.assign(new Error(`Request failed with status code ${status}`), { response: { status, data } })

  it.each([
    ['400 Invalid reply message ID', http(400, { status: 'error', message: 'Invalid reply message ID' })],
    ['400 Reply message not found for this contact', http(400, { status: 'error', message: 'Reply message not found for this contact' })],
  ])('is true for %s', (_label, error) => {
    expect(isReplyRejection(error)).toBe(true)
  })

  const abort = Object.assign(new Error('canceled'), { name: 'CanceledError', code: 'ERR_CANCELED' })
  const negatives: Array<[string, unknown]> = [
    ['400 WhatsApp account not found', http(400, { status: 'error', message: 'WhatsApp account not found' })],
    ['500 with a reply message text', http(500, { status: 'error', message: 'Reply message not found for this contact' })],
    ['400 without data.message', http(400, {})],
    ['an abort', abort],
  ]

  it.each(negatives)('is false for %s', (_label, error) => {
    expect(isReplyRejection(error)).toBe(false)
  })

  it.each(negatives.filter(([label]) => label !== 'an abort'))(
    'does not report a refusal for %s',
    async (_label, error) => {
      const onReplyRejected = vi.fn()
      const { attachments, calls } = setup({ replyTo: () => 'Q1', onReplyRejected })
      attachments.stage([file('a.png', 'image/png')])
      const done = attachments.sendAll()
      await settle()
      calls[0].result.reject(error)
      await done
      expect(onReplyRejected).not.toHaveBeenCalled()
      expect(attachments.items.value[0].replyRejected).toBe(false)
    },
  )

  it('does not report a refusal for an aborted quoting upload', async () => {
    const onReplyRejected = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => 'Q1', onReplyRejected })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    calls[0].progress(30, 100)
    attachments.stop()
    await done
    expect(onReplyRejected).not.toHaveBeenCalled()
    expect(attachments.items.value[0]).toMatchObject({ status: 'unconfirmed', replyRejected: false })
  })
})

describe('useChatAttachments round 4: workspace identity, holds and teardown', () => {
  afterEach(() => {
    // Lift the module-level override suppression set by an SPA login.
    setSelectedOrganizationId('cleanup')
    localStorage.clear()
  })

  it('B-WS-1: sees another tab switching organization after an SPA login (override suppressed)', async () => {
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-A' }))
    clearBrowserOrganizationIdentity()
    const session = {
      userId: () => 'agent-1',
      isAuthenticated: () => true,
      workspace: () => readWorkspaceIdentity('org-A'),
    }
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()

    // The same agent switches to org B in another tab.
    localStorage.setItem('selected_organization_id', 'org-B')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-B' }))
    window.dispatchEvent(new StorageEvent('storage', { key: 'user' }))

    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(calls).toHaveLength(1)
    expect(onSent).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
    expect(attachments.items.value).toHaveLength(0)
  })

  it('readWorkspaceIdentity follows the effective request organization', () => {
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-H' }))
    const atLoad = readWorkspaceIdentity('org-H')
    // A super-admin switcher selecting its own organization after load is not a switch.
    setSelectedOrganizationId('org-H')
    expect(readWorkspaceIdentity('org-H')).toBe(atLoad)
    // Selecting another organization is.
    setSelectedOrganizationId('org-X')
    expect(readWorkspaceIdentity('org-H')).not.toBe(atLoad)
    // With a selected organization stored, a persisted-user rewrite alone
    // (a stale tab) is ignored: switch-org always writes the key as well.
    setSelectedOrganizationId('org-H')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-B' }))
    expect(readWorkspaceIdentity('org-H')).toBe(atLoad)
    // Without one (e.g. a tab that signed in without reloading), the persisted
    // user's organization is the cross-tab signal.
    localStorage.removeItem('selected_organization_id')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-H' }))
    const withoutSelection = readWorkspaceIdentity('org-H')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-B' }))
    expect(readWorkspaceIdentity('org-H')).not.toBe(withoutSelection)
  })

  it('readWorkspaceIdentity sees raw shared writes while this tab\'s override is suppressed', () => {
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-A' }))
    clearBrowserOrganizationIdentity()
    const before = readWorkspaceIdentity('org-A')
    localStorage.setItem('selected_organization_id', 'org-B')
    expect(readWorkspaceIdentity('org-A')).not.toBe(before)
  })

  it('B-T1: a silent change during the only file tears down without reporting', async () => {
    const ws = { workspace: 'org-1' }
    const session = { userId: () => 'agent-1', isAuthenticated: () => true, workspace: () => ws.workspace }
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    ws.workspace = 'org-2'
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(onSent).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
    expect(attachments.items.value).toHaveLength(0)
    expect(attachments.isOpen.value).toBe(false)
  })

  it('B-WS-2: overlapping batches from two instances each release only their own hold', async () => {
    const holds = new Map<string, string>()
    const holdWorkspace = (recipient: string, holdId: string) => {
      holds.set(holdId, recipient)
      return () => holds.delete(holdId)
    }
    const first = setup({ holdWorkspace })
    const second = setup({ holdWorkspace })
    first.attachments.stage([file('a.png', 'image/png')])
    second.attachments.stage([file('b.png', 'image/png')])
    const firstDone = first.attachments.sendAll()
    const secondDone = second.attachments.sendAll()
    await settle()
    expect(holds.size).toBe(2)
    first.calls[0].result.resolve({ id: 'm1' })
    await firstDone
    // The finished batch released its own hold; the other one still blocks switching.
    expect(holds.size).toBe(1)
    second.calls[0].result.resolve({ id: 'm2' })
    await secondDone
    expect(holds.size).toBe(0)
  })

  it('B-FU12-1: any 5xx after the full upload is unconfirmed, bare or enveloped; a 4xx stays failed', async () => {
    const cases: Array<[unknown, string]> = [
      [Object.assign(new Error('520'), { response: { status: 520, data: '<html>Web server is returning an unknown error</html>' } }), 'unconfirmed'],
      [Object.assign(new Error('500'), { response: { status: 500, data: '' } }), 'unconfirmed'],
      [Object.assign(new Error('500'), { response: { status: 500, data: { status: 'error', message: 'Failed to send message' } } }), 'unconfirmed'],
      [Object.assign(new Error('400'), { response: { status: 400, data: { status: 'error', message: 'Invalid multipart form' } } }), 'failed'],
    ]
    for (const [error, expected] of cases) {
      const { attachments, calls } = setup()
      attachments.stage([file('a.png', 'image/png')])
      const done = attachments.sendAll()
      await settle()
      calls[0].progress(100, 100)
      calls[0].result.reject(error)
      await done
      expect(attachments.items.value[0].status).toBe(expected)
    }
  })
})

describe('useChatAttachments INT2-R4: quote callbacks after a silent identity change', () => {
  function refused() {
    return Object.assign(new Error('Request failed with status code 400'), {
      response: { status: 400, data: { status: 'error', message: 'Reply message not found for this contact' } },
    })
  }

  // No event, no reactivity: the change is only seen by the checks themselves.
  function plainIdentity() {
    const ws = { workspace: 'org-1', marker: 'session-1' }
    return {
      ws,
      session: {
        userId: () => 'agent-1',
        isAuthenticated: () => true,
        workspace: () => ws.workspace,
        logicalSession: () => ws.marker,
      },
    }
  }

  it('(a) workspace changed while the quoting upload is pending, then 200: no onReplySent', async () => {
    const { ws, session } = plainIdentity()
    const onReplySent = vi.fn()
    const { attachments, calls, onBatchFinished } = setup({ replyTo: () => 'Q1', onReplySent, session })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    expect(calls[0].request.reply_to_message_id).toBe('Q1')
    ws.workspace = 'org-2'
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(onReplySent).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
    expect(attachments.items.value).toHaveLength(0)
  })

  it('(b) workspace changed, then the 400 refusal: no onReplyRejected, and send-without-quote sends nothing', async () => {
    const { ws, session } = plainIdentity()
    const onReplyRejected = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => 'Q1', onReplyRejected, session })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    const id = attachments.items.value[0].id
    ws.workspace = 'org-2'
    calls[0].result.reject(refused())
    await done
    expect(onReplyRejected).not.toHaveBeenCalled()
    expect(attachments.items.value).toHaveLength(0)
    expect(await attachments.sendWithoutQuote(id)).toBeNull()
    expect(calls).toHaveLength(1)
  })

  it('(c) the same user signed in again while the quoting upload is pending, then 200: no onReplySent', async () => {
    const { ws, session } = plainIdentity()
    const onReplySent = vi.fn()
    const { attachments, calls } = setup({ replyTo: () => 'Q1', onReplySent, session })
    attachments.stage([file('a.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    ws.marker = 'session-2'
    calls[0].result.resolve({ id: 'm1' })
    await done
    expect(onReplySent).not.toHaveBeenCalled()
    expect(attachments.items.value).toHaveLength(0)
  })
})

describe('useChatAttachments round 5: enveloped 5xx and stale persisted user', () => {
  afterEach(() => {
    setSelectedOrganizationId('cleanup')
    localStorage.clear()
  })

  it('keeps an enveloped 5xx after the full upload unconfirmed: the envelope proves nothing about dispatch', async () => {
    for (const status of [500, 503]) {
      const { attachments, calls, onBatchFinished } = setup()
      attachments.stage([file('a.png', 'image/png')])
      const done = attachments.sendAll()
      await settle()
      calls[0].progress(100, 100)
      calls[0].result.reject(
        Object.assign(new Error(`Request failed with status code ${status}`), {
          response: { status, data: { status: 'error', message: 'Failed to send message' } },
        }),
      )
      await done
      expect(attachments.items.value[0].status).toBe('unconfirmed')
      expect(onBatchFinished).toHaveBeenCalledWith(expect.objectContaining({ failed: 0, unconfirmed: 1 }))
      expect(await attachments.retryFailed()).toBeNull()
    }
  })

  it('B4-WS-1: a stale tab rewriting the persisted user with its old organization does not cancel the batch', async () => {
    // This tab switched to org B: the selected organization is stored.
    setSelectedOrganizationId('org-B')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-B' }))
    const session = {
      userId: () => 'agent-1',
      isAuthenticated: () => true,
      workspace: () => readWorkspaceIdentity('org-B'),
    }
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()

    // A stale tab still on org A toggles availability and rewrites 'user'.
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-A', is_available: false }))
    window.dispatchEvent(new StorageEvent('storage', { key: 'user' }))

    expect(calls[0].signal.aborted).toBe(false)
    calls[0].result.resolve({ id: 'm1' })
    await settle()
    expect(calls).toHaveLength(2)
    calls[1].result.resolve({ id: 'm2' })
    await done
    expect(onBatchFinished).toHaveBeenCalledWith(expect.objectContaining({ sent: 2 }))
  })

  it('still stops when the stored selected organization itself changes', async () => {
    setSelectedOrganizationId('org-B')
    const session = { userId: () => 'agent-1', isAuthenticated: () => true, workspace: () => readWorkspaceIdentity('org-B') }
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()
    localStorage.setItem('selected_organization_id', 'org-C')
    window.dispatchEvent(new StorageEvent('storage', { key: 'selected_organization_id' }))
    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(onBatchFinished).not.toHaveBeenCalled()
  })
})

describe('useChatAttachments round 6: account in the identity', () => {
  afterEach(() => {
    setSelectedOrganizationId('cleanup')
    localStorage.clear()
  })

  it('R5-B-1: another account signing in through SSO in another tab ends the batch, even with a stored key', async () => {
    setSelectedOrganizationId('org-A')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-A' }))
    const session = {
      userId: () => 'agent-1',
      isAuthenticated: () => true,
      workspace: () => readWorkspaceIdentity('org-A', 'agent-1'),
    }
    const { attachments, calls, onBatchFinished, onSent } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()

    // SSO in another tab writes 'user' for a different account; the key is untouched.
    localStorage.setItem('user', JSON.stringify({ id: 'agent-2', organization_id: 'org-B' }))
    window.dispatchEvent(new StorageEvent('storage', { key: 'user' }))

    expect(calls[0].signal.aborted).toBe(true)
    await done
    expect(calls).toHaveLength(1)
    expect(onSent).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
  })

  it('R5-B-I3: in a suppressed tab, the other tab\'s user write alone (before its key write) already stops the batch', async () => {
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-A' }))
    clearBrowserOrganizationIdentity()
    const session = {
      userId: () => 'agent-1',
      isAuthenticated: () => true,
      workspace: () => readWorkspaceIdentity('org-A', 'agent-1'),
    }
    const { attachments, calls, onBatchFinished } = setup({ session })
    attachments.stage([file('a.png', 'image/png'), file('b.png', 'image/png')])
    const done = attachments.sendAll()
    await settle()

    // switch-org in the other tab: applyAuth writes 'user' first...
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-B' }))
    window.dispatchEvent(new StorageEvent('storage', { key: 'user' }))
    expect(calls[0].signal.aborted).toBe(true)
    // ...and the selection key afterwards: nothing more happens.
    localStorage.setItem('selected_organization_id', 'org-B')
    window.dispatchEvent(new StorageEvent('storage', { key: 'selected_organization_id' }))
    await done
    expect(calls).toHaveLength(1)
    expect(onBatchFinished).not.toHaveBeenCalled()
  })

  it('a same-agent stale rewrite still does not cancel (no false cancel from the account part)', () => {
    setSelectedOrganizationId('org-B')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-B' }))
    const before = readWorkspaceIdentity('org-B', 'agent-1')
    localStorage.setItem('user', JSON.stringify({ id: 'agent-1', organization_id: 'org-A', is_available: false }))
    expect(readWorkspaceIdentity('org-B', 'agent-1')).toBe(before)
  })
})
