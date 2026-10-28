import type { StagedIdentityReviewDetail, StagedIdentityReviewItem, StagedIdentityReviewList } from '@/services/api'

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const digest = /^[a-f0-9]{64}$/
const reasons = new Set(['actionable', 'unsupported', 'no_eligible_member', 'membership_changed', 'reviewer_scope', 'account_unavailable'])

function object(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Review availability could not be verified.')
  return value as Record<string, unknown>
}

function id(value: unknown): boolean {
  return typeof value === 'string' && uuid.test(value) && value !== '00000000-0000-0000-0000-000000000000'
}

function count(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

function item(value: unknown, unknownDetail = false): Record<string, unknown> {
  const row = object(value)
  if (!id(row.id) || !id(row.hold_id) || row.protocol_version !== 1 ||
    typeof row.status !== 'string' || !row.status || typeof row.message_type !== 'string' || !row.message_type ||
    typeof row.received_at !== 'string' || !Number.isFinite(Date.parse(row.received_at)) ||
    (row.revision !== undefined && row.revision !== '' && (typeof row.revision !== 'string' || !digest.test(row.revision))) ||
    (row.media_status !== undefined && typeof row.media_status !== 'string')) {
    throw new Error('The protected review returned invalid metadata.')
  }
  if (unknownDetail && row.eligibility_known === false && row.decision_available === false &&
    row.eligibility_reason === 'unavailable' && row.read_only === null) return row
  if (row.eligibility_known !== true || typeof row.eligibility_reason !== 'string' || !reasons.has(row.eligibility_reason) ||
    row.decision_available !== (row.eligibility_reason === 'actionable') || row.read_only !== !row.decision_available) {
    throw new Error('Current review availability could not be verified.')
  }
  return row
}

export function parseStagedReviewList(value: unknown, page: number, limit: number, holdIds: readonly string[] = []): StagedIdentityReviewList {
  const data = object(value)
  if (data.eligibility_known !== true || !count(data.total) || !count(data.actionable_total) ||
    !count(data.read_only_total) || !count(data.actionable_chain_total) ||
    data.total !== data.actionable_total + data.read_only_total || data.actionable_chain_total > data.actionable_total ||
    (data.actionable_total > 0 && data.actionable_chain_total === 0) ||
    data.page !== page || data.limit !== limit || !Array.isArray(data.reviews) ||
    data.reviews.length !== Math.min(limit, Math.max(0, data.total - (page - 1) * limit))) {
    throw new Error('Current review availability could not be verified.')
  }
  const seen = new Set<string>()
  let actionableOnPage = 0
  for (const value of data.reviews) {
    const row = item(value)
    const rowID = row.id as string
    if (seen.has(rowID) || (holdIds.length > 0 && !holdIds.includes(row.hold_id as string))) {
      throw new Error('The protected review page changed. Retry the current page.')
    }
    seen.add(rowID)
    if (row.decision_available) actionableOnPage++
  }
  if (actionableOnPage > data.actionable_total || data.reviews.length - actionableOnPage > data.read_only_total) {
    throw new Error('Current review availability could not be verified.')
  }
  return data as unknown as StagedIdentityReviewList
}

// Only callers which requested current-v1 may accept the unknown representation.
export function parseStagedReviewDetail(value: unknown, eventID: string, holdID?: string): StagedIdentityReviewDetail {
  const data = item(value, true)
  if (data.id !== eventID || (holdID !== undefined && data.hold_id !== holdID) ||
    typeof data.content !== 'string' || typeof data.media_available !== 'boolean' ||
    (data.media_mime_type !== undefined && typeof data.media_mime_type !== 'string') ||
    (data.media_filename !== undefined && typeof data.media_filename !== 'string') ||
    (data.media_available && (typeof data.revision !== 'string' || !digest.test(data.revision) || data.media_status !== 'ready'))) {
    throw new Error('The protected held copy could not be verified.')
  }
  return data as unknown as StagedIdentityReviewDetail
}

export function stagedReviewExplanation(value: StagedIdentityReviewItem): string {
  if (!value.eligibility_known) return 'Current decision availability could not be checked. This is a protected held copy, not a decision preview.'
  switch (value.eligibility_reason) {
    case 'actionable': return 'Open the matching contact’s Identity review to inspect the complete current candidate set before choosing future routing. This held copy is never moved into a conversation.'
    case 'unsupported': return 'The saved review does not contain the verified sender evidence needed for a decision. The held copy remains protected.'
    case 'no_eligible_member': return 'No candidate can currently receive this sender’s future messages. The held copy remains protected.'
    case 'membership_changed': return 'The candidate set has changed. Current decision availability must be checked again.'
    case 'reviewer_scope': return 'The complete candidate set is outside your current review access. The held copy remains protected.'
    case 'account_unavailable': return 'The account is not currently available for a review decision. The held copy remains protected.'
    default: return 'Current decision availability could not be checked.'
  }
}
