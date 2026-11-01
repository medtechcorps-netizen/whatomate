import { describe, expect, it } from 'vitest'
import { parseStagedReviewDetail, parseStagedReviewList, stagedReviewExplanation } from './stagedIdentityReview'

const row = {
  id: '11111111-1111-4111-8111-111111111111', hold_id: '22222222-2222-4222-8222-222222222222',
  protocol_version: 1, status: 'pending', message_type: 'text', received_at: '2026-10-28T08:00:00Z',
  eligibility_known: true, decision_available: true, eligibility_reason: 'actionable', read_only: false,
}
const page = { reviews: [row], page: 1, limit: 1, total: 2, eligibility_known: true, actionable_total: 2, actionable_chain_total: 1, read_only_total: 0 }

describe('current staged review representations', () => {
  it('keeps receipt totals separate from complete decidable chains and accepts known zero', () => {
    expect(parseStagedReviewList(page, 1, 1).actionable_chain_total).toBe(1)
    expect(parseStagedReviewList({ ...page, reviews: [], total: 0, actionable_total: 0, actionable_chain_total: 0 }, 1, 1).total).toBe(0)
  })

  it.each([
    { eligibility_known: false }, { actionable_total: -1 }, { read_only_total: 1 },
    { actionable_chain_total: 3 }, { actionable_chain_total: 0 }, { limit: 100 },
    { actionable_total: 0, actionable_chain_total: 0, read_only_total: 2 },
    { reviews: [] }, { reviews: [{ ...row, read_only: true }] },
    { reviews: [{ ...row, eligibility_reason: 'unavailable' }] },
    { reviews: [{ ...row, hold_id: 'wrong' }] }, { reviews: [{ ...row, received_at: 'yesterday' }] },
  ])('refuses malformed/partial current counts %#', change => {
    expect(() => parseStagedReviewList({ ...page, ...change }, 1, 1)).toThrow()
  })

  it('does not convert the old supported/read-only response into current counts', () => {
    expect(() => parseStagedReviewList({ reviews: [], total: 0, read_only_total: 0 }, 1, 1)).toThrow()
    expect(() => parseStagedReviewList({ ...page, reviews: [row, row], limit: 2 }, 1, 2)).toThrow()
    expect(() => parseStagedReviewList(page, 1, 1, ['33333333-3333-4333-8333-333333333333'])).toThrow()
    expect(() => parseStagedReviewList({ ...page, reviews: [{ ...row, read_only: true, decision_available: false, eligibility_reason: 'no_eligible_member' }] }, 1, 1)).toThrow()
  })

  it('accepts unknown only as an explicit fresh detail representation, never actionable guidance', () => {
    const unknown = { ...row, eligibility_known: false, decision_available: false, read_only: null, eligibility_reason: 'unavailable', content: 'authorized copy', media_available: false }
    const detail = parseStagedReviewDetail(unknown, row.id, row.hold_id)
    expect(detail.content).toBe('authorized copy')
    expect(stagedReviewExplanation(detail)).toContain('could not be checked')
    expect(stagedReviewExplanation(detail)).not.toContain('choose')
    expect(() => parseStagedReviewDetail({ ...unknown, read_only: false }, row.id)).toThrow()
    expect(() => parseStagedReviewDetail({ ...unknown, decision_available: true }, row.id)).toThrow()
    expect(() => parseStagedReviewDetail(unknown, row.hold_id)).toThrow()
  })

  it('does not grant media availability without its saved ready revision', () => {
    const detail = { ...row, content: '', media_available: true }
    expect(() => parseStagedReviewDetail(detail, row.id)).toThrow()
    expect(() => parseStagedReviewDetail({ ...detail, revision: 'a'.repeat(64), media_status: 'pending' }, row.id)).toThrow()
    expect(parseStagedReviewDetail({ ...detail, revision: 'a'.repeat(64), media_status: 'ready' }, row.id).media_available).toBe(true)
  })
})
