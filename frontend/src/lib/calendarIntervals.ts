// Calendar presentation only. Booking admission and capacity remain server-owned.
export interface CalendarInterval {
  startDate: string
  endDate: string
  lastOccupiedDate: string
  endAtMidnight: boolean
}

function instant(value: string) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(?:Z|[+-]\d{2}:\d{2})$/.exec(value)
  if (!match) throw new Error('A schedule has an invalid time interval. Refresh the calendar.')
  const [year, month, day, hour, minute, second] = match.slice(1, 7).map(Number)
  const check = new Date(0)
  check.setUTCFullYear(year!, month! - 1, day!)
  const millis = Date.parse(value)
  if (!Number.isFinite(millis) || year! < 1 || check.getUTCFullYear() !== year ||
      check.getUTCMonth() !== month! - 1 || check.getUTCDate() !== day ||
      hour! > 23 || minute! > 59 || second! > 59) {
    throw new Error('A schedule has an invalid time interval. Refresh the calendar.')
  }
  const fraction = (match[7] ?? '').padEnd(9, '0')
  return { millis, fraction, subMillis: Number(fraction.slice(3)) }
}

export function calendarInterval(start: string, end: string, timezone: string): CalendarInterval {
  const first = instant(start)
  const last = instant(end)
  if (last.millis < first.millis || (last.millis === first.millis && last.subMillis <= first.subMillis)) {
    throw new Error('A schedule has an invalid time interval. Refresh the calendar.')
  }
  const formatter = new Intl.DateTimeFormat('en-CA', {
    timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
  })
  const parts = (millis: number) => Object.fromEntries(formatter.formatToParts(new Date(millis))
    .filter(part => part.type !== 'literal').map(part => [part.type, part.value]))
  const a = parts(first.millis)
  const b = parts(last.millis)
  // Intl resolves milliseconds. A positive submillisecond tail still occupies
  // its millisecond; an exact millisecond end excludes that instant.
  const occupied = parts(last.millis - (last.subMillis === 0 ? 1 : 0))
  const startDate = `${a.year}-${a.month}-${a.day}`
  const endDate = `${b.year}-${b.month}-${b.day}`
  const lastOccupiedDate = `${occupied.year}-${occupied.month}-${occupied.day}`
  if (endDate < startDate || lastOccupiedDate < startDate) {
    throw new Error('This schedule crosses an unsupported calendar-date transition.')
  }
  // Midnight may repeat during an offset rollback. Only the first instant of
  // a new local date is an exclusive midnight boundary for the prior date.
  const endAtMidnight = lastOccupiedDate < endDate && b.hour === '00' &&
    b.minute === '00' && b.second === '00' && Number(last.fraction) === 0
  return { startDate, endDate, lastOccupiedDate, endAtMidnight }
}

export function occupiesCalendarDay(interval: CalendarInterval, day: string) {
  return interval.startDate <= day && day <= interval.lastOccupiedDate
}
