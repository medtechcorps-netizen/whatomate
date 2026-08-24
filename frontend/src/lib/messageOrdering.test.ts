import { describe, expect, it } from 'vitest'
import { compareMessageIngestionOrder } from './messageOrdering'

describe('message ingestion ordering', () => {
  it('preserves RFC3339 microseconds before applying the id tie-break', () => {
    const messages = [
      {
        id: 'message-a',
        ingested_at: '2026-08-24T12:00:00.000900Z',
        created_at: '2026-08-20T12:00:00Z',
      },
      {
        id: 'message-z',
        ingested_at: '2026-08-24T12:00:00.000100Z',
        created_at: '2026-08-23T12:00:00Z',
      },
    ]

    expect(messages.sort(compareMessageIngestionOrder).map(message => message.id)).toEqual([
      'message-z',
      'message-a',
    ])
  })

  it('normalizes equivalent fractional precision and timezone offsets', () => {
    const messages = [
      {
        id: 'message-z',
        ingested_at: '2026-08-24T20:00:00.1+08:00',
        created_at: '2026-08-24T12:00:00Z',
      },
      {
        id: 'message-a',
        ingested_at: '2026-08-24T12:00:00.100000Z',
        created_at: '2026-08-24T12:00:00Z',
      },
    ]

    expect(messages.sort(compareMessageIngestionOrder).map(message => message.id)).toEqual([
      'message-a',
      'message-z',
    ])
  })
})
