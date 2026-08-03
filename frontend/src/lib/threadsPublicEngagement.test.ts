import { describe, expect, it } from 'vitest'

import {
  threadsPublicEngagementAccountReady,
  threadsPublicEngagementTarget,
  type ThreadsAccountLike,
} from './threadsPublicEngagement'

const account: ThreadsAccountLike = {
  channel: 'threads',
  provider: 'threads',
  status: 'active',
  capabilities: {
    text: true,
    replies: true,
    public_replies: true,
    reply_target_required: true,
    mentions: true,
    business_initiation: false,
    direct_messages: false,
  },
  config: {
    outbound_enabled: true,
    engagement_mode: 'public_replies_mentions',
    direct_messages_supported: false,
  },
}

describe('threadsPublicEngagementTarget', () => {
  it.each(['reply', 'mention'])('accepts an existing public %s target', (engagementType) => {
    expect(
      threadsPublicEngagementTarget({
        channel: 'threads',
        external_conversation_id: 'threads-public-target-123',
        metadata: { engagement_type: engagementType },
      }),
    ).toEqual({
      externalId: 'threads-public-target-123',
      engagementType,
    })
  })

  it('normalizes provider engagement labels without changing the target', () => {
    expect(
      threadsPublicEngagementTarget({
        channel: 'threads',
        external_conversation_id: 'threads-public-target-123',
        metadata: { engagement_type: ' Mention ' },
      }),
    ).toEqual({
      externalId: 'threads-public-target-123',
      engagementType: 'mention',
    })
  })

  it.each([
    ['a direct message', 'threads-public-target-123', 'direct_message'],
    ['a standalone post', 'threads-public-target-123', 'post'],
    ['missing engagement metadata', 'threads-public-target-123', undefined],
    ['a missing target', '', 'reply'],
    ['a padded target', ' threads-public-target-123 ', 'reply'],
  ])('rejects %s', (_label, externalId, engagementType) => {
    expect(
      threadsPublicEngagementTarget({
        channel: 'threads',
        external_conversation_id: externalId,
        metadata: { engagement_type: engagementType },
      }),
    ).toBeNull()
  })
})

describe('threadsPublicEngagementAccountReady', () => {
  const replyTarget = {
    externalId: 'threads-public-target-123',
    engagementType: 'reply' as const,
  }

  it('requires the complete public-engagement account policy', () => {
    expect(threadsPublicEngagementAccountReady(account, replyTarget)).toBe(true)
  })

  it.each([
    ['inactive account', { status: 'pending' }],
    ['relay provider', { provider: 'relay' }],
    ['outbound disabled', { config: { ...account.config, outbound_enabled: false } }],
    [
      'wrong engagement mode',
      { config: { ...account.config, engagement_mode: 'direct_messages' } },
    ],
    ['unknown DM policy', { config: { ...account.config, direct_messages_supported: undefined } }],
    ['missing reply capability', { capabilities: { ...account.capabilities, replies: undefined } }],
    [
      'missing exact-target policy',
      { capabilities: { ...account.capabilities, reply_target_required: undefined } },
    ],
    [
      'business initiation enabled',
      { capabilities: { ...account.capabilities, business_initiation: true } },
    ],
  ])('fails closed for %s', (_label, override) => {
    expect(threadsPublicEngagementAccountReady({ ...account, ...override }, replyTarget)).toBe(
      false,
    )
  })

  it('requires mention capability when responding to a mention', () => {
    expect(
      threadsPublicEngagementAccountReady(
        {
          ...account,
          capabilities: { ...account.capabilities, mentions: false },
        },
        { ...replyTarget, engagementType: 'mention' },
      ),
    ).toBe(false)
  })
})
