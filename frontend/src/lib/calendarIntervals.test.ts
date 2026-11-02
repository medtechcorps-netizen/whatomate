import { describe, expect, it } from 'vitest'
import { calendarInterval, occupiesCalendarDay } from './calendarIntervals'

describe('resource-local half-open calendar intervals', () => {
  it('includes an overnight continuation but not the day at an exact midnight end', () => {
    const overnight = calendarInterval('2026-11-01T23:30:00Z', '2026-11-02T00:30:00Z', 'UTC')
    expect(occupiesCalendarDay(overnight, '2026-11-01')).toBe(true)
    expect(occupiesCalendarDay(overnight, '2026-11-02')).toBe(true)
    const midnight = calendarInterval('2026-11-01T23:30:00Z', '2026-11-02T00:00:00Z', 'UTC')
    expect(occupiesCalendarDay(midnight, '2026-11-02')).toBe(false)
  })

  it.each([
    ['2026-11-01T04:30:00Z', false],
    ['2026-11-01T03:30:00Z', true],
  ])('includes the occupied date at Havana repeated midnight from %s', (start, includesPreviousDay) => {
    const interval = calendarInterval(start, '2026-11-01T05:00:00Z', 'America/Havana')
    expect(interval.lastOccupiedDate).toBe('2026-11-01')
    expect(interval.endAtMidnight).toBe(false)
    expect(occupiesCalendarDay(interval, '2026-11-01')).toBe(true)
    expect(occupiesCalendarDay(interval, '2026-10-31')).toBe(includesPreviousDay)
    expect(occupiesCalendarDay(interval, '2026-11-02')).toBe(false)
  })

  it('excludes Havana November1 at its first exact midnight', () => {
    const interval = calendarInterval('2026-11-01T03:30:00Z', '2026-11-01T04:00:00Z', 'America/Havana')
    expect(interval.lastOccupiedDate).toBe('2026-10-31')
    expect(interval.endAtMidnight).toBe(true)
    expect(occupiesCalendarDay(interval, '2026-10-31')).toBe(true)
    expect(occupiesCalendarDay(interval, '2026-11-01')).toBe(false)
  })

  it('includes a positive nanosecond tail after Havana first midnight', () => {
    const interval = calendarInterval('2026-11-01T03:30:00Z', '2026-11-01T04:00:00.000000001Z', 'America/Havana')
    expect(interval.lastOccupiedDate).toBe('2026-11-01')
    expect(interval.endAtMidnight).toBe(false)
    expect(occupiesCalendarDay(interval, '2026-11-01')).toBe(true)
  })

  it('excludes the new date when Santiago skips midnight at the exclusive end', () => {
    const interval = calendarInterval('2026-09-06T03:30:00Z', '2026-09-06T04:00:00Z', 'America/Santiago')
    expect(interval.lastOccupiedDate).toBe('2026-09-05')
    expect(interval.endDate).toBe('2026-09-06')
    expect(interval.endAtMidnight).toBe(false)
    expect(occupiesCalendarDay(interval, '2026-09-05')).toBe(true)
    expect(occupiesCalendarDay(interval, '2026-09-06')).toBe(false)
  })

  it('clips a whole-week interval to every intersected date without a start-date margin', () => {
    const interval = calendarInterval('2026-10-01T00:00:00Z', '2026-12-01T00:00:00Z', 'UTC')
    for (let day = 2; day <= 8; day++) expect(occupiesCalendarDay(interval, `2026-11-0${day}`)).toBe(true)
    expect(occupiesCalendarDay(interval, '2026-12-01')).toBe(false)
  })

  it.each([
    ['Pacific/Kiritimati', '2026-11-03', '2026-11-02'],
    ['Etc/GMT+12', '2026-11-01', '2026-11-03'],
  ])('uses the resource date in %s', (timezone, included, excluded) => {
    const interval = calendarInterval('2026-11-02T10:30:00Z', '2026-11-02T12:30:00Z', timezone)
    expect(occupiesCalendarDay(interval, included)).toBe(true)
    expect(occupiesCalendarDay(interval, excluded)).toBe(false)
  })

  it.each([
    ['2026-11-01T05:50:00Z', '2026-11-01T06:10:00Z'],
    ['2026-03-08T06:50:00Z', '2026-03-08T07:10:00Z'],
  ])('retains real elapsed intervals across DST: %s', (start, end) => {
    const interval = calendarInterval(start, end, 'America/New_York')
    expect(occupiesCalendarDay(interval, start.slice(0, 10))).toBe(true)
  })

  it('does not round a positive submillisecond midnight tail into an exclusive midnight end', () => {
    const interval = calendarInterval('2026-11-01T23:59:59.999999999Z', '2026-11-02T00:00:00.000000001Z', 'UTC')
    expect(occupiesCalendarDay(interval, '2026-11-02')).toBe(true)
  })

  it.each([
    ['', '2026-11-02T01:00:00Z'],
    ['2026-11-02T00:00:00Z', ''],
    ['2026-02-30T00:00:00Z', '2026-03-03T00:00:00Z'],
    ['2026-11-02T00:00:00Z', 'invalid'],
    ['2026-11-02T00:00:00Z', '2026-11-02T00:00:00Z'],
    ['2026-11-02T01:00:00Z', '2026-11-02T00:00:00Z'],
    ['2026-11-02T00:00:00.000000002Z', '2026-11-02T00:00:00.000000001Z'],
  ])('refuses missing, invalid or non-increasing boundaries: %s / %s', (start, end) => {
    expect(() => calendarInterval(start, end, 'UTC')).toThrow()
  })

  it('refuses an invalid resource timezone', () => {
    expect(() => calendarInterval('2026-11-02T00:00:00Z', '2026-11-02T01:00:00Z', 'unknown/zone')).toThrow()
  })
})
