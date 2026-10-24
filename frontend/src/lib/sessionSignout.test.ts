/** @vitest-environment happy-dom */

import { beforeEach, describe, expect, it } from 'vitest'
import {
  SIGN_OUT_REASON_KEY,
  clearSignOutReason,
  readSignOutReason,
  formatIdleDuration,
  recordSignOutReason,
} from './sessionSignout'

const t = (key: string, params?: Record<string, unknown>) =>
  params ? `${key}:${JSON.stringify(params)}` : key

describe('sessionSignout', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('keeps a reason for every tab until it is cleared', () => {
    recordSignOutReason({ reason: 'idle', minutes: 60 })
    expect(readSignOutReason()).toMatchObject({ reason: 'idle', minutes: 60 })
    expect(readSignOutReason()).toMatchObject({ reason: 'idle', minutes: 60 })
    clearSignOutReason()
    expect(readSignOutReason()).toBeNull()
    expect(localStorage.getItem(SIGN_OUT_REASON_KEY)).toBeNull()
  })

  it('does not relabel a fresh inactivity sign-out as expired', () => {
    recordSignOutReason({ reason: 'idle', minutes: 30 })
    recordSignOutReason({ reason: 'expired' })
    expect(readSignOutReason()).toMatchObject({ reason: 'idle', minutes: 30 })
  })

  it('lets a later expiry replace an old inactivity reason', () => {
    localStorage.setItem(
      SIGN_OUT_REASON_KEY,
      JSON.stringify({ reason: 'idle', minutes: 30, at: Date.now() - 5 * 60_000 }),
    )
    recordSignOutReason({ reason: 'expired' })
    expect(readSignOutReason()).toMatchObject({ reason: 'expired' })
  })

  it('drops stale or malformed reasons', () => {
    localStorage.setItem(
      SIGN_OUT_REASON_KEY,
      JSON.stringify({ reason: 'expired', at: Date.now() - 11 * 60_000 }),
    )
    expect(readSignOutReason()).toBeNull()
    expect(localStorage.getItem(SIGN_OUT_REASON_KEY)).toBeNull()

    localStorage.setItem(SIGN_OUT_REASON_KEY, '{not json')
    expect(readSignOutReason()).toBeNull()

    localStorage.setItem(SIGN_OUT_REASON_KEY, JSON.stringify({ reason: 'idle', at: Date.now() }))
    expect(readSignOutReason()).toBeNull()
  })

  it('formats the idle duration', () => {
    expect(formatIdleDuration(30, t)).toBe('sessionSecurity.durationMinutes:{"count":30}')
    expect(formatIdleDuration(60, t)).toBe('sessionSecurity.durationHour')
    expect(formatIdleDuration(240, t)).toBe('sessionSecurity.durationHours:{"count":4}')
  })
})
