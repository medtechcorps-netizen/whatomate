/** @vitest-environment happy-dom */

import { effectScope, nextTick, reactive } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  SESSION_ENDING_EVENT,
  messageStatusFromRealtimeEvent,
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
