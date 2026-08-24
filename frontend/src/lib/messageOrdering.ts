export interface IngestionOrderedMessage {
  id: string
  ingested_at?: string
  created_at: string
}

const rfc3339Nanoseconds = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/

function exactRFC3339Nanoseconds(value: string): bigint | null {
  const match = rfc3339Nanoseconds.exec(value)
  if (!match) return null
  const epochMilliseconds = Date.parse(`${match[1]}${match[3]}`)
  if (!Number.isFinite(epochMilliseconds)) return null
  const fraction = (match[2] ?? '').padEnd(9, '0')
  return BigInt(Math.trunc(epochMilliseconds / 1_000)) * 1_000_000_000n
    + BigInt(fraction || '0')
}

/**
 * Matches the backend's `(COALESCE(ingested_at, created_at), id)` cursor tuple
 * without collapsing PostgreSQL microseconds through JavaScript Date.
 */
export function compareMessageIngestionOrder(
  left: IngestionOrderedMessage,
  right: IngestionOrderedMessage,
) {
  const leftValue = left.ingested_at || left.created_at
  const rightValue = right.ingested_at || right.created_at
  const leftExact = exactRFC3339Nanoseconds(leftValue)
  const rightExact = exactRFC3339Nanoseconds(rightValue)

  if (leftExact !== null && rightExact !== null && leftExact !== rightExact) {
    return leftExact < rightExact ? -1 : 1
  }
  if (leftExact === null || rightExact === null) {
    const leftMilliseconds = Date.parse(leftValue)
    const rightMilliseconds = Date.parse(rightValue)
    const timeDifference = (Number.isFinite(leftMilliseconds) ? leftMilliseconds : 0)
      - (Number.isFinite(rightMilliseconds) ? rightMilliseconds : 0)
    if (timeDifference !== 0) return timeDifference
  }
  if (left.id === right.id) return 0
  return left.id < right.id ? -1 : 1
}
