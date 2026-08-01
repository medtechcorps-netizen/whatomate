export type ZonedDateTimeParts = Record<
  'year' | 'month' | 'day' | 'hour' | 'minute' | 'second',
  number
>

export function zonedDateTimeParts(value: string | Date, timezone: string): ZonedDateTimeParts {
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: timezone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hourCycle: 'h23',
  }).formatToParts(new Date(value))
  return Object.fromEntries(
    parts
      .filter((part) => part.type !== 'literal')
      .map((part) => [part.type, Number(part.value)]),
  ) as ZonedDateTimeParts
}

export function formatDateTimeInputInTimeZone(value: string | Date, timezone: string) {
  const parts = zonedDateTimeParts(value, timezone)
  const pad = (part: number) => String(part).padStart(2, '0')
  return `${parts.year}-${pad(parts.month)}-${pad(parts.day)}T${pad(parts.hour)}:${pad(parts.minute)}`
}

export function formatDateInTimeZone(value: string | Date, timezone: string) {
  return formatDateTimeInputInTimeZone(value, timezone).slice(0, 10)
}

export function zonedLocalDateTimeToUTC(value: string, timezone: string) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(value)
  if (!match) throw new Error('Choose a valid local date and time')
  const target = match.slice(1).map(Number)
  const targetAsUTC = Date.UTC(target[0], target[1] - 1, target[2], target[3], target[4], 0)
  let candidate = targetAsUTC

  for (let attempt = 0; attempt < 4; attempt += 1) {
    const observed = zonedDateTimeParts(new Date(candidate), timezone)
    const observedAsUTC = Date.UTC(
      observed.year,
      observed.month - 1,
      observed.day,
      observed.hour,
      observed.minute,
      0,
    )
    const correction = targetAsUTC - observedAsUTC
    candidate += correction
    if (correction === 0) break
  }

  const verified = zonedDateTimeParts(new Date(candidate), timezone)
  if (
    verified.year !== target[0] ||
    verified.month !== target[1] ||
    verified.day !== target[2] ||
    verified.hour !== target[3] ||
    verified.minute !== target[4]
  ) {
    throw new Error(`That local time does not exist in ${timezone} because of a clock change`)
  }

  for (const deltaMinutes of [-120, -90, -60, -30, 30, 60, 90, 120]) {
    const alternate = candidate + deltaMinutes * 60 * 1000
    const alternateParts = zonedDateTimeParts(new Date(alternate), timezone)
    if (
      alternateParts.year === target[0] &&
      alternateParts.month === target[1] &&
      alternateParts.day === target[2] &&
      alternateParts.hour === target[3] &&
      alternateParts.minute === target[4]
    ) {
      throw new Error(`That local time occurs twice in ${timezone}; choose a time outside the clock change`)
    }
  }
  return new Date(candidate).toISOString()
}
