/** @vitest-environment happy-dom */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
const session = vi.hoisted(() => ({ current: 'session-1' as unknown }))
vi.mock('./tabSession', () => ({ currentTabSession: () => session.current }))
import {
  LAST_ACTIVITY_KEY, clearLastActivity, currentPolicyIdentity, currentSessionActivityScope, isIdleSessionStale,
  readLastActivity, recordActivity, rememberSessionActivity,
} from './sessionActivity'
const IDENTITY = 'user-1|org-1'
const MINUTE = 60_000

describe('scoped activity when browser storage is unavailable', () => {
  beforeEach(() => {
    vi.useFakeTimers({ now: new Date('2026-10-27T09:00:00Z') })
    clearLastActivity()
    localStorage.clear()
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-1' }))
    session.current = 'session-1'
  })
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); clearLastActivity(); vi.useRealTimers() })

  it('keeps the API idle predicate on the same clock after a rejected write', () => {
    const old = Date.now() - 40 * MINUTE
    localStorage.setItem(LAST_ACTIVITY_KEY, String(old))
    const storage = localStorage
    const deniedWrite = vi.fn(() => { throw new Error('storage denied') })
    vi.stubGlobal('localStorage', {
      getItem: storage.getItem.bind(storage), setItem: deniedWrite,
      removeItem: storage.removeItem.bind(storage), clear: storage.clear.bind(storage),
    })
    expect(() => localStorage.setItem('denial-probe', 'value')).toThrow('storage denied')
    deniedWrite.mockClear()
    recordActivity()
    expect(localStorage.getItem(LAST_ACTIVITY_KEY)).toBe(String(old))
    expect(readLastActivity()).toBe(Date.now())
    expect(isIdleSessionStale(Date.now(), 30)).toBe(false)
  })

  it('accepts current activity in memory without writing storage', () => {
    const storage = localStorage
    const write = vi.fn()
    vi.stubGlobal('localStorage', { getItem: storage.getItem.bind(storage), setItem: write, removeItem: storage.removeItem.bind(storage) })
    expect(rememberSessionActivity(Date.now(), 'session-1', IDENTITY)).toBe(true)
    expect(readLastActivity()).toBe(Date.now())
    expect(write).not.toHaveBeenCalled()
  })

  for (const field of ['id', 'organization_id'] as const) {
    it.each([undefined, null, '', ' ', ' leading', 'trailing ', 'ambiguous|part', 42, {}])(`rejects incomplete or ambiguous ${field} %j without retaining memory`, value => {
      rememberSessionActivity(Date.now(), 'session-1', IDENTITY)
      const user = { id: 'user-1', organization_id: 'org-1', [field]: value }
      localStorage.setItem('user', JSON.stringify(user))
      expect(currentSessionActivityScope()).toBeNull()
      expect(rememberSessionActivity(Date.now(), 'session-1', currentPolicyIdentity())).toBe(false)
      expect(readLastActivity()).toBe(0)
      localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-1' }))
      expect(readLastActivity()).toBe(0)
    })
  }

  it.each([undefined, null, '', ' ', 42])('rejects an unavailable or malformed live session %j', current => {
    session.current = current
    expect(currentSessionActivityScope()).toBeNull()
    expect(rememberSessionActivity(Date.now(), current, IDENTITY)).toBe(false)
    expect(readLastActivity()).toBe(0)
  })

  it('does not change legacy policy identity parsing when the organization is missing', () => {
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', email: 'u1@example.com' }))
    expect(currentPolicyIdentity()).toBe('user-1|')
    expect(currentSessionActivityScope()).toBeNull()
    expect(localStorage.getItem('user')).not.toBeNull()
  })

  it.each([NaN, Infinity, -1, 0, '1000', null])('rejects invalid timestamp %s', at => {
    expect(rememberSessionActivity(at, 'session-1', IDENTITY)).toBe(false)
    expect(readLastActivity()).toBe(0)
  })

  it('rejects future and foreign scope notices and ignores older activity', () => {
    expect(rememberSessionActivity(Date.now() + 1, 'session-1', IDENTITY)).toBe(false)
    expect(rememberSessionActivity(Date.now(), 'session-old', IDENTITY)).toBe(false)
    expect(rememberSessionActivity(Date.now(), 'session-1', 'user-2|org-1')).toBe(false)
    expect(rememberSessionActivity(Date.now(), 'session-1', 'user-1|org-2')).toBe(false)
    expect(readLastActivity()).toBe(0)
    rememberSessionActivity(Date.now(), 'session-1', IDENTITY)
    rememberSessionActivity(Date.now() - MINUTE, 'session-1', IDENTITY)
    expect(readLastActivity()).toBe(Date.now())
  })

  it.each(['session-2', null, ''])('forgets memory for a replaced, ended or unknown session %s', current => {
    rememberSessionActivity(Date.now(), 'session-1', IDENTITY)
    session.current = current
    expect(readLastActivity()).toBe(0)
    expect(rememberSessionActivity(Date.now(), 'session-1', IDENTITY)).toBe(false)
    session.current = 'session-1'
    expect(readLastActivity()).toBe(0)
  })

  it.each([{ id: 'user-2', organization_id: 'org-1' }, null])('does not carry memory into another or missing user', user => {
    rememberSessionActivity(Date.now(), 'session-1', IDENTITY)
    localStorage.setItem('user', JSON.stringify(user))
    expect(readLastActivity()).toBe(0)
  })

  it('keeps accepted activity across the same session and user organization switch without advancing it', () => {
    const accepted = Date.now() - 5 * MINUTE
    rememberSessionActivity(accepted, 'session-1', IDENTITY)
    localStorage.setItem('user', JSON.stringify({ id: 'user-1', organization_id: 'org-2' }))
    expect(readLastActivity()).toBe(accepted)
    expect(isIdleSessionStale(Date.now(), 30)).toBe(false)
    expect(rememberSessionActivity(Date.now(), 'session-1', IDENTITY)).toBe(false)
    expect(readLastActivity()).toBe(accepted)
    expect(rememberSessionActivity(Date.now(), 'session-1', 'user-1|org-2')).toBe(true)
    expect(readLastActivity()).toBe(Date.now())
  })

  it('clears memory even when removal is denied', () => {
    rememberSessionActivity(Date.now(), 'session-1', IDENTITY)
    const storage = localStorage
    const remove = vi.fn(() => { throw new Error('denied') })
    vi.stubGlobal('localStorage', { getItem: storage.getItem.bind(storage), removeItem: remove })
    clearLastActivity()
    expect(remove).toHaveBeenCalled()
    expect(readLastActivity()).toBe(0)
  })
})
