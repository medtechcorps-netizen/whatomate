import { computed, effectScope, getCurrentScope, markRaw, onScopeDispose, ref, watch } from 'vue'
import { messagesService } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
import { convertImageToJpeg } from '@/lib/chatImageConversion'
import { SELECTED_ORGANIZATION_STORAGE_KEY, readSelectedOrganizationId } from '@/lib/browserIdentity'
import {
  MAX_CAPTION_LENGTH,
  MAX_CHAT_ATTACHMENTS,
  kindSupportsCaption,
  validateChatAttachment,
  withInferredMimeType,
  type AttachmentValidation,
  type ChatMediaKind,
} from '@/lib/chatAttachments'

/**
 * 'unconfirmed': the request was cut off (Stop, or no answer before the
 * deadline) after the server may already have received the file. It is
 * never resent automatically; the agent checks the chat.
 */
export type StagedAttachmentStatus =
  | 'ready'
  | 'invalid'
  | 'converting'
  | 'sending'
  | 'sent'
  | 'failed'
  | 'unconfirmed'

export interface StagedAttachment {
  id: string
  file: File
  /** Object URL for image/video thumbnails; revoked when the file leaves the list. */
  previewUrl: string | null
  caption: string
  asDocument: boolean
  validation: AttachmentValidation
  status: StagedAttachmentStatus
  /** Upload progress, 0-100. */
  progress: number
  /** Reason the server gave for the last failure, if any. */
  errorMessage: string | null
  /** MIME type of the original when the file was converted to JPEG here. */
  convertedFrom: string | null
  /** The last in-browser JPEG conversion failed (e.g. HEIC outside Safari). */
  conversionFailed: boolean
}

/** The conversation a batch was staged for. */
export interface ChatAttachmentTarget {
  contactId: string
  /** The patient's display name, for reports after the chat was left. */
  label?: string
  /** False once that conversation is no longer the one on screen. */
  isActive: () => boolean
}

export interface SendChatMediaRequest {
  file: File
  contact_id: string
  type: ChatMediaKind
  caption?: string
  whatsapp_account?: string
  reply_to_message_id?: string
}

export type SendChatMedia = (
  request: SendChatMediaRequest,
  options: {
    onUploadProgress: (loaded: number, total: number | undefined) => void
    signal: AbortSignal
  },
) => Promise<unknown>

export interface StageResult {
  added: number
  /** Files over the per-batch limit that were not added. */
  overflow: number
  /** Nothing was staged: no conversation is open, or a batch is still sending. */
  refused: 'no-conversation' | 'busy' | null
}

export interface BatchSummary {
  sent: number
  failed: number
  /** Files the batch never sent (stopped before they started). */
  notSent: number
  /** Files that may or may not have reached the server. */
  unconfirmed: number
  failedNames: string[]
  notSentNames: string[]
  unconfirmedNames: string[]
  /** The patient the batch went to. */
  recipient: string
  /** The agent pressed Stop. */
  stopped: boolean
  /** The agent left the chat while the batch kept sending. */
  detached: boolean
}

/** A realtime event as wsService.onInboxActivity delivers it. */
export interface RealtimeActivity {
  type?: string
  payload?: any
}

export interface UseChatAttachmentsOptions {
  /** Resolves the open conversation when files are staged. */
  resolveTarget: () => ChatAttachmentTarget | null
  /** Sending WhatsApp account, read when a batch starts. */
  getWhatsAppAccount?: () => string | null | undefined
  /**
   * Message the batch replies to, read when a batch starts. Only the first
   * file sent in the batch carries it. Not wired yet: the media endpoint
   * does not read reply_to_message_id today.
   */
  replyTo?: () => string | null | undefined
  /** Called with each message the server created, while its conversation is still open. */
  onSent?: (message: unknown) => void
  onBatchFinished?: (summary: BatchSummary) => void
  /** Uploads one file. Defaults to messagesService.sendMedia. */
  send?: SendChatMedia
  /** Re-encodes a photo as JPEG. Defaults to convertImageToJpeg. */
  convertImage?: (file: File) => Promise<File>
  /**
   * Realtime message events (wsService.onInboxActivity). When given, each
   * file waits for the previous file's message to leave 'pending' before
   * the next upload starts.
   */
  subscribeRealtime?: (listener: (event: RealtimeActivity) => void) => () => void
  /** Longest wait for a message to leave 'pending'. */
  settleTimeoutMs?: number
  /** Once a file has fully uploaded, how long to wait for the server's answer. */
  responseDeadlineMs?: number
  /**
   * The signed-in session. A batch only ever runs for the user who confirmed
   * it: sign-out, expiry or a different user ends it at once, silently.
   */
  session?: {
    userId: () => string | number | null | undefined
    isAuthenticated: () => boolean
    /**
     * The workspace requests go to (selected organization / token
     * organization). A batch never continues into a different workspace.
     */
    workspace?: () => string | null | undefined
    /** A marker that changes on every login/logout (see LOGICAL_SESSION_STORAGE_KEY). */
    logicalSession?: () => string | null | undefined
  }
  /**
   * Holds workspace switching while a batch is sending (the coordinated
   * boundary). holdId is unique per batch, so a finishing batch only ever
   * releases its own hold. Returns the release function.
   */
  holdWorkspace?: (recipient: string, holdId: string) => () => void
}

/**
 * Logical session marker kept by the auth layer (lane A); it changes only on
 * login, registration, logout or expiry. Absent on builds without it: the
 * batch then relies on the user/workspace checks and fails closed on change.
 */
export const LOGICAL_SESSION_STORAGE_KEY = 'whm_session_id'

export function readLogicalSessionMarker(): string | null {
  return readStorage(LOGICAL_SESSION_STORAGE_KEY)
}

/** Where the auth store persists the signed-in user (on every login and switch-org). */
const PERSISTED_USER_STORAGE_KEY = 'user'

function readStorage(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

function persistedUserOrganizationId(): string | null {
  const raw = readStorage(PERSISTED_USER_STORAGE_KEY)
  if (!raw) return null
  try {
    const user = JSON.parse(raw) as { organization_id?: unknown } | null
    return user && typeof user.organization_id === 'string' ? user.organization_id : null
  } catch {
    return null
  }
}

/**
 * The workspace this tab's requests go to, plus the shared storage that can
 * change it from another tab. A tab that signed in without reloading has its
 * selected-organization override suppressed for its whole life
 * (clearBrowserOrganizationIdentity), so the override alone would miss a
 * switch made in another tab. Parts:
 * - the effective organization of this tab's requests: the override api.ts
 *   sends (readSelectedOrganizationId), else the token organization;
 * - the same, read from the raw shared selected organization whatever the
 *   suppression says (a switch written by another tab);
 * - the token organization: the persisted user's (the cookie/JWT org,
 *   rewritten by switch-org in any tab), else the in-memory user's;
 * - the in-memory user's organization (reactive, same tab).
 * Effective values, not raw store fields: a super-admin's switcher selecting
 * its own organization after page load is not a switch. Store fields that
 * initialise late are deliberately not part of it.
 */
export function readWorkspaceIdentity(inMemoryOrganizationId?: string | null): string {
  const tokenOrganization = persistedUserOrganizationId() || inMemoryOrganizationId || ''
  return JSON.stringify([
    readSelectedOrganizationId() || tokenOrganization,
    readStorage(SELECTED_ORGANIZATION_STORAGE_KEY) || tokenOrganization,
    tokenOrganization,
    inMemoryOrganizationId ?? '',
  ])
}

let batchSequence = 0

/**
 * Dispatched on window by the auth layer, synchronously, before it clears
 * the session and navigates (sign-out, signed out elsewhere, expiry).
 */
export const SESSION_ENDING_EVENT = 'rereply:session-ending'

/**
 * Pacing bound. The server holds the contact row lock across Meta's media
 * upload in the previous file's provider transaction; starting the next
 * upload before that finishes pins a second pool connection behind it.
 */
export const MEDIA_SETTLE_TIMEOUT_MS = 10_000
/** No answer this long after the body uploaded: give up and mark the file unconfirmed. */
export const MEDIA_RESPONSE_DEADLINE_MS = 120_000

/**
 * After the whole body reached the server, a missing answer (network error)
 * or any 5xx without the app's own error envelope {status:'error', message}
 * does not tell whether the file was sent: the media route never emits a
 * bare 5xx itself, so those come from gateways (502/503/504, Cloudflare
 * 520-524, an empty 500). Retrying could send it to the patient twice.
 * A 4xx or an enveloped 5xx is the app's definite answer.
 */
export function isAmbiguousAfterUpload(error: unknown): boolean {
  const response = (error as { response?: { status?: number; data?: unknown } } | null)?.response
  if (!response) return true
  if ((response.status ?? 0) < 500) return false
  const data = response.data as { status?: unknown; message?: unknown } | null | undefined
  const appRefusal = !!data && typeof data === 'object' && data.status === 'error' && typeof data.message === 'string'
  return !appRefusal
}
const STATUS_LOG_LIMIT = 500

const defaultSend: SendChatMedia = async (request, options) => {
  const response = await messagesService.sendMedia(request, options)
  const body = response.data as { data?: unknown } | undefined
  return body && typeof body === 'object' && 'data' in body ? body.data : body
}

export interface MessageStatusUpdate {
  messageId: string
  status: string
  errorMessage?: string
}

/** Reads a message status change from a status_update or realtime_sync event. */
export function messageStatusFromRealtimeEvent(event: RealtimeActivity | null | undefined): MessageStatusUpdate | null {
  const payload = event?.payload
  if (!payload || typeof payload !== 'object') return null
  const isStatusEvent =
    event?.type === 'status_update' ||
    (event?.type === 'realtime_sync' && payload.kind === 'message_status_changed')
  if (!isStatusEvent) return null
  const messageId = payload.message_id
  const status = payload.status
  if (typeof messageId !== 'string' || !messageId || typeof status !== 'string' || !status) return null
  return {
    messageId,
    status,
    ...(typeof payload.error_message === 'string' && payload.error_message ? { errorMessage: payload.error_message } : {}),
  }
}

function createPreviewUrl(file: File, validation: AttachmentValidation): string | null {
  const kind = validation.kind
  if (kind !== 'image' && kind !== 'video') return null
  if (typeof URL === 'undefined' || typeof URL.createObjectURL !== 'function') return null
  try {
    return URL.createObjectURL(file)
  } catch {
    return null
  }
}

function revokePreviewUrl(url: string | null) {
  if (!url || typeof URL === 'undefined' || typeof URL.revokeObjectURL !== 'function') return
  try {
    URL.revokeObjectURL(url)
  } catch {
    // Already revoked.
  }
}

interface ActiveBatch {
  target: ChatAttachmentTarget
  /** The user who confirmed the batch. */
  userId: string | null
  /** The workspace and logical session the batch was confirmed in. */
  workspace: string | null
  logicalSession: string | null
  /** The session ended or changed: abort everything and report nothing. */
  terminated: boolean
  controller: AbortController | null
  /** The in-flight file's body has been fully uploaded. */
  uploadComplete: boolean
  stopRequested: boolean
  /** The staging dialog was reset (the agent left the chat); keep sending anyway. */
  detached: boolean
  /** Ends a pacing wait early (Stop). */
  wake: (() => void) | null
}

/**
 * Staging and sequential sending of chat attachments. Each file becomes its
 * own WhatsApp message; files go one request at a time, in order.
 *
 * A batch the agent confirmed always runs to the end for the patient it was
 * confirmed for, even if the agent leaves the chat, and finishes with a
 * report (onBatchFinished). Only Stop ends it early.
 */
export function useChatAttachments(options: UseChatAttachmentsOptions) {
  const send = options.send ?? defaultSend
  const convertImage = options.convertImage ?? ((file: File) => convertImageToJpeg(file))
  const settleTimeoutMs = options.settleTimeoutMs ?? MEDIA_SETTLE_TIMEOUT_MS
  const responseDeadlineMs = options.responseDeadlineMs ?? MEDIA_RESPONSE_DEADLINE_MS
  const session = options.session

  function normalizeIdentity(value: string | number | null | undefined): string | null {
    return value === null || value === undefined || value === '' ? null : String(value)
  }

  function currentUserId(): string | null {
    return normalizeIdentity(session?.userId())
  }

  function currentWorkspace(): string | null {
    return normalizeIdentity(session?.workspace?.())
  }

  function currentLogicalSession(): string | null {
    return normalizeIdentity(session?.logicalSession?.())
  }

  /**
   * True while the batch's user, workspace and logical session are all still
   * the current ones. Any change (sign-out, another user, a workspace switch
   * in this or another tab, a new login) ends the batch: fail closed.
   */
  function sessionIsCurrent(batch: ActiveBatch): boolean {
    if (!session) return true
    return (
      session.isAuthenticated() &&
      currentUserId() === batch.userId &&
      currentWorkspace() === batch.workspace &&
      currentLogicalSession() === batch.logicalSession
    )
  }
  const items = ref<StagedAttachment[]>([])
  const isOpen = ref(false)
  /** A batch is in flight; stays true until its last request settles. */
  const isSending = ref(false)
  const isStopping = ref(false)
  /** The patient the in-flight batch goes to. */
  const sendingTo = ref('')
  let target: ChatAttachmentTarget | null = null
  let activeBatch: ActiveBatch | null = null
  let sequence = 0
  // Bumped by reset(); a conversion that sees a different value is dropped.
  let generation = 0

  const sendableCount = computed(() => items.value.filter(item => item.status === 'ready').length)
  const failedCount = computed(() => items.value.filter(item => item.status === 'failed').length)
  const invalidCount = computed(() => items.value.filter(item => item.status === 'invalid').length)
  const sentCount = computed(() => items.value.filter(item => item.status === 'sent').length)
  const convertingCount = computed(() => items.value.filter(item => item.status === 'converting').length)
  const remainingSlots = computed(() => Math.max(0, MAX_CHAT_ATTACHMENTS - items.value.length))

  function find(id: string): StagedAttachment | undefined {
    return items.value.find(item => item.id === id)
  }

  /**
   * Clears the staging dialog. A batch that is already sending is detached,
   * not cancelled: it keeps sending to its patient and reports at the end.
   */
  function reset() {
    generation++
    if (activeBatch) activeBatch.detached = true
    for (const item of items.value) revokePreviewUrl(item.previewUrl)
    items.value = []
    isOpen.value = false
    target = null
  }

  function stage(files: Iterable<File> | ArrayLike<File>): StageResult {
    const list = Array.from(files as ArrayLike<File>).filter(Boolean)
    if (list.length === 0) return { added: 0, overflow: 0, refused: null }
    if (isSending.value) return { added: 0, overflow: list.length, refused: 'busy' }

    const current = options.resolveTarget()
    if (!current) return { added: 0, overflow: list.length, refused: 'no-conversation' }
    // Never mix files staged for one patient into another patient's batch.
    if (target && (target.contactId !== current.contactId || !target.isActive())) reset()
    target = current

    const accepted = list.slice(0, remainingSlots.value)
    for (const file of accepted) {
      const validation = validateChatAttachment(file)
      items.value.push({
        id: `attachment-${++sequence}`,
        // Files are opaque payloads; never wrap them in reactive proxies.
        file: markRaw(file),
        previewUrl: createPreviewUrl(file, validation),
        caption: '',
        asDocument: false,
        validation,
        status: validation.ok ? 'ready' : 'invalid',
        progress: 0,
        errorMessage: null,
        convertedFrom: null,
        conversionFailed: false,
      })
    }
    if (items.value.length > 0) isOpen.value = true
    return { added: accepted.length, overflow: list.length - accepted.length, refused: null }
  }

  function remove(id: string) {
    // Files queued in a running batch stay until it ends.
    if (isSending.value) return
    const index = items.value.findIndex(item => item.id === id)
    if (index < 0) return
    revokePreviewUrl(items.value[index].previewUrl)
    items.value.splice(index, 1)
    if (items.value.length === 0) reset()
  }

  function setCaption(id: string, caption: string) {
    const item = find(id)
    if (!item || item.status === 'sending' || item.status === 'sent') return
    item.caption = caption.slice(0, MAX_CAPTION_LENGTH)
  }

  /** Re-validates a too-large JPEG/PNG as a document (no inline preview on WhatsApp, 15 MB cap). */
  function sendAsDocument(id: string) {
    const item = find(id)
    if (!item || item.status !== 'invalid' || item.validation.ok || !item.validation.canSendAsDocument) return
    const validation = validateChatAttachment(item.file, { asDocument: true })
    item.asDocument = true
    item.validation = validation
    item.status = validation.ok ? 'ready' : 'invalid'
  }

  function canConvert(item: StagedAttachment | undefined): item is StagedAttachment {
    return !!item && item.status === 'invalid' && !item.validation.ok && item.validation.canConvertToJpeg
  }

  /**
   * Re-encodes a rejected photo (HEIC/WebP/GIF…, or JPEG/PNG over 5 MB) as a
   * JPEG under 5 MB so it can go as an inline photo.
   */
  async function convertToJpeg(id: string): Promise<boolean> {
    const item = find(id)
    if (isSending.value || !canConvert(item)) return false
    const conversionGeneration = generation
    const original = item.file
    item.status = 'converting'
    item.conversionFailed = false
    try {
      const converted = await convertImage(original)
      const current = find(id)
      if (conversionGeneration !== generation || !current || current.status !== 'converting') return false
      const validation = validateChatAttachment(converted)
      revokePreviewUrl(current.previewUrl)
      current.file = markRaw(converted)
      current.validation = validation
      current.previewUrl = createPreviewUrl(converted, validation)
      current.asDocument = false
      current.convertedFrom = original.type || validation.mimeType
      current.status = validation.ok ? 'ready' : 'invalid'
      current.conversionFailed = !validation.ok
      return validation.ok
    } catch {
      const current = find(id)
      if (conversionGeneration !== generation || !current || current.status !== 'converting') return false
      current.status = 'invalid'
      current.conversionFailed = true
      return false
    }
  }

  /** Converts every convertible photo, one at a time to keep memory use low. */
  async function convertAllToJpeg() {
    const ids = items.value.filter(item => canConvert(item) && !item.conversionFailed).map(item => item.id)
    for (const id of ids) await convertToJpeg(id)
  }

  async function runBatch(statuses: StagedAttachmentStatus[]): Promise<BatchSummary | null> {
    if (activeBatch || !target || convertingCount.value > 0) return null
    if (!target.isActive()) {
      reset()
      return null
    }
    // The batch holds its own references, so it can finish after the
    // staging dialog is reset.
    const queue = items.value.filter(item => statuses.includes(item.status))
    if (queue.length === 0) return null

    if (session && !session.isAuthenticated()) return null
    const batch: ActiveBatch = {
      target,
      userId: currentUserId(),
      workspace: currentWorkspace(),
      logicalSession: currentLogicalSession(),
      terminated: false,
      controller: null,
      uploadComplete: false,
      stopRequested: false,
      detached: false,
      wake: null,
    }
    activeBatch = batch
    isSending.value = true
    isStopping.value = false
    sendingTo.value = batch.target.label ?? ''
    const whatsappAccount = options.getWhatsAppAccount?.() || undefined
    let replyTo = options.replyTo?.() || undefined

    // Status changes are logged from the start, so an update that arrives
    // before its HTTP response still counts.
    const statusLog = new Map<string, MessageStatusUpdate>()
    let waiter: { messageId: string; finish: (update: MessageStatusUpdate | null) => void } | null = null
    let unsubscribe: (() => void) | null = null
    try {
      unsubscribe =
        options.subscribeRealtime?.(event => {
          const update = messageStatusFromRealtimeEvent(event)
          if (!update) return
          statusLog.set(update.messageId, update)
          if (statusLog.size > STATUS_LOG_LIMIT) statusLog.delete(statusLog.keys().next().value as string)
          if (waiter && waiter.messageId === update.messageId && update.status !== 'pending') waiter.finish(update)
        }) ?? null
    } catch {
      unsubscribe = null
    }
    const canPace = unsubscribe !== null

    function waitForSettle(messageId: string): Promise<MessageStatusUpdate | null> {
      const known = statusLog.get(messageId)
      if (known && known.status !== 'pending') return Promise.resolve(known)
      return new Promise(resolve => {
        const timer = setTimeout(() => finish(null), settleTimeoutMs)
        const finish = (update: MessageStatusUpdate | null) => {
          clearTimeout(timer)
          waiter = null
          batch.wake = null
          resolve(update)
        }
        waiter = { messageId, finish }
        batch.wake = () => finish(null)
      })
    }

    const hasWindow = typeof window !== 'undefined'
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      // Closing or reloading the tab would cut off the files still queued.
      // Never prompt once the session or workspace changed: an idle/remote
      // sign-out or a workspace switch must reload without a dialog, and a
      // cancelled reload must not leave the batch running in the new identity.
      if (batch.terminated) return
      if (!sessionIsCurrent(batch)) {
        onSessionEnding()
        return
      }
      event.preventDefault()
      event.returnValue = ''
    }
    const removeUnloadPrompt = () => {
      if (hasWindow) window.removeEventListener('beforeunload', onBeforeUnload)
    }
    const terminate = () => {
      if (batch.terminated) return
      batch.terminated = true
      batch.stopRequested = true
      removeUnloadPrompt()
      batch.controller?.abort()
      batch.wake?.()
    }
    const onSessionEnding = () => {
      terminate()
      reset()
    }
    // Another tab switched workspace or signed in/out: localStorage changed.
    const onStorage = () => {
      if (!batch.terminated && !sessionIsCurrent(batch)) onSessionEnding()
    }
    if (hasWindow) {
      window.addEventListener('beforeunload', onBeforeUnload)
      window.addEventListener(SESSION_ENDING_EVENT, onSessionEnding)
      window.addEventListener('storage', onStorage)
    }
    let releaseWorkspace: (() => void) | null = null
    try {
      releaseWorkspace = options.holdWorkspace?.(batch.target.label ?? '', `chat-media-batch:${++batchSequence}`) ?? null
    } catch {
      // The fail-closed identity checks still guard the batch.
      releaseWorkspace = null
    }
    // Also follow the auth state directly, independent of the event's timing.
    // A detached scope: the batch may outlive the component that started it.
    const sessionWatch = session ? effectScope(true) : null
    sessionWatch?.run(() => {
      watch(
        () => sessionIsCurrent(batch),
        current => {
          if (!current) onSessionEnding()
        },
      )
    })

    try {
      for (const [index, item] of queue.entries()) {
        if (!batch.terminated && !sessionIsCurrent(batch)) onSessionEnding()
        if (batch.stopRequested) break
        if (!item.validation.ok) continue
        const isLast = index === queue.length - 1

        const kind = item.validation.kind
        const caption = kindSupportsCaption(kind) ? item.caption.trim() : ''
        const controller = new AbortController()
        let responseDeadline: ReturnType<typeof setTimeout> | null = null
        batch.controller = controller
        batch.uploadComplete = false
        item.status = 'sending'
        item.progress = 0
        item.errorMessage = null
        try {
          const message = await send(
            {
              file: withInferredMimeType(item.file),
              contact_id: batch.target.contactId,
              type: kind,
              ...(caption ? { caption } : {}),
              ...(whatsappAccount ? { whatsapp_account: whatsappAccount } : {}),
              ...(replyTo ? { reply_to_message_id: replyTo } : {}),
            },
            {
              signal: controller.signal,
              onUploadProgress: (loaded, total) => {
                if (!total) return
                item.progress = Math.min(100, Math.round((loaded / total) * 100))
                if (loaded >= total && !batch.uploadComplete) {
                  batch.uploadComplete = true
                  // The body is on the server; never wait forever for its answer.
                  responseDeadline = setTimeout(() => controller.abort(), responseDeadlineMs)
                }
              },
            },
          )
          item.progress = 100
          replyTo = undefined
          if (message && !batch.terminated && sessionIsCurrent(batch) && batch.target.isActive()) {
            options.onSent?.(message)
          }

          const created = (message && typeof message === 'object' ? message : {}) as {
            id?: unknown
            status?: unknown
            error_message?: unknown
          }
          let outcome: MessageStatusUpdate | null = null
          if (created.status === 'failed') {
            outcome = {
              messageId: String(created.id ?? ''),
              status: 'failed',
              ...(typeof created.error_message === 'string' ? { errorMessage: created.error_message } : {}),
            }
          } else if (created.status === 'pending' && typeof created.id === 'string' && created.id) {
            if (canPace && !isLast && !batch.stopRequested) {
              // Pace the batch: the next upload starts once this message
              // leaves 'pending' at the provider, or after the bounded wait.
              outcome = await waitForSettle(created.id)
            } else {
              // Last file: nothing to pace, use a status that already arrived.
              outcome = statusLog.get(created.id) ?? null
            }
          }
          if (outcome?.status === 'failed') {
            item.status = 'failed'
            item.errorMessage = outcome.errorMessage ?? null
          } else {
            item.status = 'sent'
          }
        } catch (error) {
          if (controller.signal.aborted) {
            // Cut off by Stop, the response deadline or the end of the
            // session. Upload progress lags the network, so the server may
            // already have the file: never claim it was not sent.
            item.status = 'unconfirmed'
          } else if (batch.uploadComplete && isAmbiguousAfterUpload(error)) {
            // The whole file reached the server and the answer was lost
            // (network error, gateway 502/503/504/524): it may have been
            // sent, so it is not offered for an automatic retry.
            item.status = 'unconfirmed'
          } else {
            item.status = 'failed'
            item.errorMessage = getErrorMessage(error, '') || null
          }
        } finally {
          if (responseDeadline) clearTimeout(responseDeadline)
          batch.controller = null
        }
      }
    } finally {
      unsubscribe?.()
      sessionWatch?.stop()
      removeUnloadPrompt()
      if (hasWindow) {
        window.removeEventListener(SESSION_ENDING_EVENT, onSessionEnding)
        window.removeEventListener('storage', onStorage)
      }
      try {
        releaseWorkspace?.()
      } catch {
        // Nothing to release.
      }
      activeBatch = null
      isSending.value = false
      isStopping.value = false
      sendingTo.value = ''
    }

    const named = (status: StagedAttachmentStatus) =>
      queue.filter(item => item.status === status).map(item => item.file.name)
    const summary: BatchSummary = {
      sent: named('sent').length,
      failed: named('failed').length,
      notSent: named('ready').length,
      unconfirmed: named('unconfirmed').length,
      failedNames: named('failed'),
      notSentNames: named('ready'),
      unconfirmedNames: named('unconfirmed'),
      recipient: batch.target.label ?? '',
      stopped: batch.stopRequested,
      detached: batch.detached,
    }
    // After the session or workspace changed, report nothing: the patient's
    // name must not appear on the login page or in another workspace. A
    // silent change (no event) during the last file still tears down the
    // staging here, so nothing stays live in the new identity.
    if (!batch.terminated && !sessionIsCurrent(batch)) onSessionEnding()
    if (batch.terminated) return null
    options.onBatchFinished?.(summary)
    // Done when nothing is left to send, retry, convert or check; invalid
    // files were shown as skipped. A detached batch leaves the (new) staging alone.
    const open: StagedAttachmentStatus[] = ['ready', 'failed', 'converting', 'unconfirmed']
    if (!batch.detached && !items.value.some(item => open.includes(item.status))) {
      reset()
    }
    return summary
  }

  /** Sends every ready file, in order. */
  function sendAll() {
    return runBatch(['ready'])
  }

  /** Sends only the files whose last attempt failed. */
  function retryFailed() {
    return runBatch(['failed'])
  }

  /**
   * Stops the running batch: files not started stay 'ready'. A file still
   * uploading is aborted and marked 'unconfirmed' (progress events lag, so
   * the server may have it); one already uploaded is left to finish, bounded
   * by the response deadline.
   */
  function stop() {
    const batch = activeBatch
    if (!batch || batch.detached || batch.stopRequested) return
    batch.stopRequested = true
    isStopping.value = true
    if (!batch.uploadComplete) batch.controller?.abort()
    batch.wake?.()
  }

  /** Closes the staging dialog and drops its files; ignored while uploading. */
  function close() {
    if (isSending.value) return
    reset()
  }

  if (getCurrentScope()) {
    onScopeDispose(reset)
    // Clear staged patient files when the session ends, even with no batch running.
    if (typeof window !== 'undefined') {
      const onSessionEnding = () => reset()
      window.addEventListener(SESSION_ENDING_EVENT, onSessionEnding)
      onScopeDispose(() => window.removeEventListener(SESSION_ENDING_EVENT, onSessionEnding))
    }
  }

  return {
    items,
    isOpen,
    isSending,
    isStopping,
    sendingTo,
    sendableCount,
    failedCount,
    invalidCount,
    sentCount,
    convertingCount,
    remainingSlots,
    maxFiles: MAX_CHAT_ATTACHMENTS,
    stage,
    remove,
    setCaption,
    sendAsDocument,
    convertToJpeg,
    convertAllToJpeg,
    sendAll,
    retryFailed,
    stop,
    close,
    reset,
  }
}

export type ChatAttachments = ReturnType<typeof useChatAttachments>
