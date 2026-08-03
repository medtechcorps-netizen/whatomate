export type ThreadsEngagementType = 'reply' | 'mention'

export interface ThreadsConversationLike {
  channel?: string
  external_conversation_id?: unknown
  metadata?: Record<string, unknown>
}

export interface ThreadsAccountLike {
  channel?: string
  provider?: string
  status?: string
  capabilities?: Record<string, unknown>
  config?: Record<string, unknown>
}

export interface ThreadsPublicEngagementTarget {
  externalId: string
  engagementType: ThreadsEngagementType
}

/**
 * Returns the provider-owned public target for a Threads reply or mention.
 *
 * The identifier is deliberately not trimmed before returning it. A padded or
 * otherwise non-canonical value fails closed so the UI can never manufacture a
 * target that differs from the selected provider conversation.
 */
export function threadsPublicEngagementTarget(
  conversation?: ThreadsConversationLike | null,
): ThreadsPublicEngagementTarget | null {
  if (conversation?.channel !== 'threads') return null

  const externalId =
    typeof conversation.external_conversation_id === 'string'
      ? conversation.external_conversation_id
      : ''
  if (!externalId || externalId !== externalId.trim()) return null

  const rawEngagementType = conversation.metadata?.engagement_type
  const engagementType =
    typeof rawEngagementType === 'string' ? rawEngagementType.trim().toLowerCase() : ''
  if (engagementType !== 'reply' && engagementType !== 'mention') return null

  return { externalId, engagementType }
}

/**
 * Checks the complete account policy needed to expose the public-reply
 * composer. Every safety-sensitive capability is explicit so missing or stale
 * account metadata keeps the composer disabled.
 */
export function threadsPublicEngagementAccountReady(
  account: ThreadsAccountLike | null | undefined,
  target: ThreadsPublicEngagementTarget | null,
): boolean {
  if (!account || !target) return false

  const capabilities = account.capabilities ?? {}
  const config = account.config ?? {}

  return (
    account.channel === 'threads' &&
    account.provider === 'threads' &&
    account.status === 'active' &&
    config.outbound_enabled === true &&
    config.engagement_mode === 'public_replies_mentions' &&
    config.direct_messages_supported === false &&
    capabilities.text === true &&
    capabilities.replies === true &&
    capabilities.public_replies === true &&
    capabilities.reply_target_required === true &&
    capabilities.business_initiation === false &&
    capabilities.direct_messages === false &&
    (target.engagementType !== 'mention' || capabilities.mentions === true)
  )
}
