import type { Contact, Message } from '@/stores/contacts'

/** One forward request carries one target; the dialog sends at most this many. */
export const MAX_FORWARD_TARGETS = 5

/**
 * Types the server can rebuild from stored content. Stickers, locations,
 * contacts, interactive and template messages need provider objects or
 * approvals that cannot be replayed (mirrors forwardableMessageType in Go).
 */
const FORWARDABLE_TYPES = new Set(['text', 'image', 'video', 'audio', 'document'])

export type ForwardSourceBlock = 'unsupported' | 'media_pending' | null

/** Body the server stores for a message deleted in the WhatsApp Business App. */
const DELETED_MESSAGE_BODY = '[Message deleted from WhatsApp Business App]'

export function forwardSourceBlock(message: Pick<Message, 'message_type' | 'media_url' | 'content'>): ForwardSourceBlock {
  if (!FORWARDABLE_TYPES.has(message.message_type)) return 'unsupported'
  if (message.message_type === 'text') {
    const body = typeof message.content === 'string' ? message.content : message.content?.body
    // A deleted message's tombstone is refused by the server as well.
    if (typeof body !== 'string' || !body.trim() || body.trim() === DELETED_MESSAGE_BODY) return 'unsupported'
    return null
  }
  return message.media_url ? null : 'media_pending'
}

export function canOfferForward(message: Pick<Message, 'message_type' | 'media_url' | 'content'>): boolean {
  // Pending media is still offered so the dialog can explain the wait.
  return forwardSourceBlock(message) !== 'unsupported'
}

/**
 * CRM-only marker set by the server (`is_forwarded`, or the stored
 * `metadata.forwarded_from` when a payload carries metadata); customers never
 * see it.
 */
export function isForwardedMessage(message: Message): boolean {
  const extended = message as Message & { is_forwarded?: boolean; metadata?: { forwarded_from?: unknown } }
  return extended.is_forwarded === true || Boolean(extended.metadata?.forwarded_from)
}

export type ForwardTargetBlock = 'identity_review' | 'window_closed' | null

/** Mirrors the server's pre-send refusals so closed targets cannot be picked. */
export function forwardTargetBlock(contact: Pick<Contact, 'service_window_open' | 'identity_review_ai_state'>): ForwardTargetBlock {
  // Fail closed like the server: a missing or unknown review state blocks.
  const review = contact.identity_review_ai_state
  if (!review || review.blocked !== false) return 'identity_review'
  if (contact.service_window_open !== true) return 'window_closed'
  return null
}

export type ForwardFailure = 'retryable' | 'uncertain' | 'failed'

/**
 * The server's forward contract for a request that did not return 2xx:
 * - 503 forward_temporarily_unavailable: definitely not accepted (nothing was
 *   saved or sent), so a manual retry is safe;
 * - forward_blocked: definitely not sent, and retrying will not help;
 * - any other 5xx, or no response at all: it may have been accepted, so the
 *   agent must check the chat; never retried automatically;
 * - 4xx: a refusal (window closed, identity review, ...), not sent.
 */
export function classifyForwardError(error: unknown): ForwardFailure {
  const response = (error as { response?: { status?: number; data?: { error_type?: string } } } | null)?.response
  const status = response?.status
  const errorType = response?.data?.error_type
  if (!status) return 'uncertain'
  if (status === 503 && errorType === 'forward_temporarily_unavailable') return 'retryable'
  if (errorType === 'forward_blocked') return 'failed'
  if (status >= 500) return 'uncertain'
  return 'failed'
}

export type ForwardDeliveryState = 'delayed' | 'uncertain' | null

/**
 * Server-derived state of a stalled forward. Neither means "not sent", and
 * ReReply never resends either one automatically:
 * - "delayed": no send attempt recorded yet; it may still be sent;
 * - "uncertain": a send attempt began but its result was not recorded; it may
 *   have been sent.
 */
export function forwardDeliveryState(message: Message): ForwardDeliveryState {
  const state = (message as Message & { forward_delivery_state?: string }).forward_delivery_state
  return state === 'delayed' || state === 'uncertain' ? state : null
}
