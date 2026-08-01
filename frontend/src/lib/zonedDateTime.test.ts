import { describe, expect, it } from 'vitest'
import {
  formatDateInTimeZone,
  formatDateTimeInputInTimeZone,
  zonedLocalDateTimeToUTC,
} from './zonedDateTime'

describe('zonedLocalDateTimeToUTC', () => {
  it('converts fixed and fractional-offset IANA timezones', () => {
    expect(zonedLocalDateTimeToUTC('2026-08-04T09:00', 'Asia/Kuala_Lumpur')).toBe(
      '2026-08-04T01:00:00.000Z',
    )
    expect(zonedLocalDateTimeToUTC('2026-08-04T09:00', 'Asia/Kathmandu')).toBe(
      '2026-08-04T03:15:00.000Z',
    )
  })

  it('uses the seasonal offset for valid local times', () => {
    expect(zonedLocalDateTimeToUTC('2026-01-15T09:00', 'America/New_York')).toBe(
      '2026-01-15T14:00:00.000Z',
    )
    expect(zonedLocalDateTimeToUTC('2026-07-15T09:00', 'America/New_York')).toBe(
      '2026-07-15T13:00:00.000Z',
    )
  })

  it('rejects local times skipped by a daylight-saving transition', () => {
    expect(() => zonedLocalDateTimeToUTC('2026-03-08T02:30', 'America/New_York')).toThrow(
      'does not exist',
    )
  })

  it('rejects local times repeated by daylight-saving transitions', () => {
    expect(() => zonedLocalDateTimeToUTC('2026-11-01T01:30', 'America/New_York')).toThrow(
      'occurs twice',
    )
    expect(() => zonedLocalDateTimeToUTC('2026-04-05T01:45', 'Australia/Lord_Howe')).toThrow(
      'occurs twice',
    )
  })
})

describe('timezone-safe input formatting', () => {
  it('formats an absolute instant as the resource wall clock and calendar date', () => {
    const instant = '2026-08-04T01:30:00.000Z'
    expect(formatDateTimeInputInTimeZone(instant, 'Asia/Kuala_Lumpur')).toBe('2026-08-04T09:30')
    expect(formatDateInTimeZone(instant, 'Asia/Kuala_Lumpur')).toBe('2026-08-04')
  })
})
