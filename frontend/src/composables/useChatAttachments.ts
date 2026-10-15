import { computed, getCurrentScope, onScopeDispose, ref } from 'vue'
import { messagesService } from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
import {
  MAX_CAPTION_LENGTH,
  MAX_CHAT_ATTACHMENTS,
  kindSupportsCaption,
  validateChatAttachment,
  withInferredMimeType,
  type AttachmentValidation,
  type ChatMediaKind,
} from '@/lib/chatAttachments'

export type StagedAttachmentStatus = 'ready' | 'invalid' | 'sending' | 'sent' | 'failed'

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
}

/** The conversation a batch was staged for. */
export interface ChatAttachmentTarget {
  contactId: string
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
  options: { onUploadProgress: (loaded: number, total: number | undefined) => void },
) => Promise<unknown>

export interface StageResult {
  added: number
  /** Files over the per-batch limit that were not added. */
  overflow: number
  /** Nothing was staged: no conversation is open, or a batch is uploading. */
  refused: 'no-conversation' | 'busy' | null
}

export interface BatchSummary {
  sent: number
  failed: number
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
}

const defaultSend: SendChatMedia = async (request, options) => {
  const response = await messagesService.sendMedia(request, options)
  const body = response.data as { data?: unknown } | undefined
  return body && typeof body === 'object' && 'data' in body ? body.data : body
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

/**
 * Staging and sequential sending of chat attachments. Each file becomes its
 * own WhatsApp message; files go one request at a time, in order, and the
 * next starts only after the previous response.
 */
export function useChatAttachments(options: UseChatAttachmentsOptions) {
  const send = options.send ?? defaultSend
  const items = ref<StagedAttachment[]>([])
  const isOpen = ref(false)
  const isSending = ref(false)
  let target: ChatAttachmentTarget | null = null
  let sequence = 0
  // Bumped by reset(); a batch that sees a different value stops touching state.
  let generation = 0

  const sendableCount = computed(() => items.value.filter(item => item.status === 'ready').length)
  const failedCount = computed(() => items.value.filter(item => item.status === 'failed').length)
  const invalidCount = computed(() => items.value.filter(item => item.status === 'invalid').length)
  const sentCount = computed(() => items.value.filter(item => item.status === 'sent').length)
  const remainingSlots = computed(() => Math.max(0, MAX_CHAT_ATTACHMENTS - items.value.length))

  function find(id: string): StagedAttachment | undefined {
    return items.value.find(item => item.id === id)
  }

  function reset() {
    generation++
    for (const item of items.value) revokePreviewUrl(item.previewUrl)
    items.value = []
    isOpen.value = false
    isSending.value = false
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
        file,
        previewUrl: createPreviewUrl(file, validation),
        caption: '',
        asDocument: false,
        validation,
        status: validation.ok ? 'ready' : 'invalid',
        progress: 0,
        errorMessage: null,
      })
    }
    if (items.value.length > 0) isOpen.value = true
    return { added: accepted.length, overflow: list.length - accepted.length, refused: null }
  }

  function remove(id: string) {
    const index = items.value.findIndex(item => item.id === id)
    if (index < 0 || items.value[index].status === 'sending') return
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

  async function runBatch(statuses: StagedAttachmentStatus[]): Promise<BatchSummary> {
    const summary: BatchSummary = { sent: 0, failed: 0 }
    if (isSending.value || !target) return summary
    if (!target.isActive()) {
      reset()
      return summary
    }
    const batchTarget = target
    const queue = items.value.filter(item => statuses.includes(item.status)).map(item => item.id)
    if (queue.length === 0) return summary

    const batchGeneration = generation
    const whatsappAccount = options.getWhatsAppAccount?.() || undefined
    let replyTo = options.replyTo?.() || undefined
    isSending.value = true

    for (const id of queue) {
      if (batchGeneration !== generation) return summary
      const item = find(id)
      if (!item || !item.validation.ok) continue
      if (!batchTarget.isActive()) {
        reset()
        return summary
      }

      const kind = item.validation.kind
      const caption = kindSupportsCaption(kind) ? item.caption.trim() : ''
      item.status = 'sending'
      item.progress = 0
      item.errorMessage = null
      try {
        const message = await send(
          {
            file: withInferredMimeType(item.file),
            contact_id: batchTarget.contactId,
            type: kind,
            ...(caption ? { caption } : {}),
            ...(whatsappAccount ? { whatsapp_account: whatsappAccount } : {}),
            ...(replyTo ? { reply_to_message_id: replyTo } : {}),
          },
          {
            onUploadProgress: (loaded, total) => {
              if (batchGeneration !== generation || !total) return
              item.progress = Math.min(100, Math.round((loaded / total) * 100))
            },
          },
        )
        if (batchGeneration !== generation) return summary
        item.status = 'sent'
        item.progress = 100
        summary.sent++
        replyTo = undefined
        if (message && batchTarget.isActive()) options.onSent?.(message)
      } catch (error) {
        if (batchGeneration !== generation) return summary
        item.status = 'failed'
        item.errorMessage = getErrorMessage(error, '') || null
        summary.failed++
      }
    }

    isSending.value = false
    options.onBatchFinished?.(summary)
    // Done when nothing is left to send or retry; invalid files were shown as skipped.
    if (!items.value.some(item => item.status === 'ready' || item.status === 'failed')) reset()
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

  /** Closes the staging dialog and drops its files; ignored while uploading. */
  function close() {
    if (isSending.value) return
    reset()
  }

  if (getCurrentScope()) onScopeDispose(reset)

  return {
    items,
    isOpen,
    isSending,
    sendableCount,
    failedCount,
    invalidCount,
    sentCount,
    remainingSlots,
    maxFiles: MAX_CHAT_ATTACHMENTS,
    stage,
    remove,
    setCaption,
    sendAsDocument,
    sendAll,
    retryFailed,
    close,
    reset,
  }
}

export type ChatAttachments = ReturnType<typeof useChatAttachments>
