/** @vitest-environment happy-dom */
import type { InternalAxiosRequestConfig } from 'axios'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, contactsService, type ContactIdentityReviewDecisionRequest } from './api'

describe('identity review request ownership', () => {
  const previousAdapter = api.defaults.adapter
  afterEach(() => { api.defaults.adapter = previousAdapter; localStorage.clear(); vi.restoreAllMocks() })

  it('pins all seven calls through the real interceptor and opts detail into current-v1', async () => {
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ({ data: {}, status: 200, statusText: 'OK', headers: {}, config }))
    api.defaults.adapter = adapter
    localStorage.setItem('selected_organization_id', 'different-current-workspace')
    const controller = new AbortController()
    const scope = { organizationId: 'captured-workspace', signal: controller.signal }
    const decision: ContactIdentityReviewDecisionRequest = {
      request_id: 'request', hold_id: 'hold', target_contact_id: 'contact', expected_version: 1,
      expected_member_digest: 'a'.repeat(64), expected_chain_digest: 'b'.repeat(64), request_digest: 'c'.repeat(64),
    }
    await contactsService.getIdentityReviewState('contact', scope)
    await contactsService.previewIdentityReview('contact', scope)
    await contactsService.decideIdentityReview('contact', decision, scope)
    await contactsService.listStagedIdentityReviews({ page: 2, limit: 100 }, scope)
    await contactsService.listStagedIdentityReviewsForHolds(['hold-one', 'hold-two'], 25, scope, 3)
    await contactsService.getStagedIdentityReview('event', scope)
    await contactsService.getStagedIdentityReviewMedia('event', 'a'.repeat(64), scope)
    expect(adapter).toHaveBeenCalledTimes(7)
    for (const [config] of adapter.mock.calls) {
      expect(config.headers.get('X-Organization-ID')).toBe('captured-workspace')
      expect(config.signal).toBe(controller.signal)
    }
    expect(adapter.mock.calls[4]![0].params.toString()).toBe('hold_id=hold-one&hold_id=hold-two&order=oldest&page=3&limit=25')
    expect(adapter.mock.calls[5]![0].params).toEqual({ eligibility: 'current-v1' })
    expect(adapter.mock.calls[6]![0].responseType).toBe('blob')
    expect(JSON.parse(adapter.mock.calls[2]![0].data)).toEqual(decision)
  })

  it('never lets an empty captured scope fall back to selected storage', () => {
    const get = vi.spyOn(api, 'get')
    localStorage.setItem('selected_organization_id', 'fallback-must-not-be-used')
    expect(() => contactsService.getStagedIdentityReview('event', { organizationId: ' ' })).toThrow('current workspace')
    expect(get).not.toHaveBeenCalled()
  })
})
