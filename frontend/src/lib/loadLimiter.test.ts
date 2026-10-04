import { describe, expect, it } from 'vitest'
import { createLoadLimiter } from './loadLimiter'

const flush = () => new Promise(resolve => setTimeout(resolve, 0))

describe('createLoadLimiter', () => {
  it('runs at most the limit at once and hands a released slot to the next caller', async () => {
    const limiter = createLoadLimiter(2)
    const granted: string[] = []
    const releases: Record<string, () => void> = {}
    for (const name of ['first', 'second', 'third']) {
      void limiter.acquire(new AbortController().signal).then(release => {
        granted.push(name)
        releases[name] = release
      })
    }
    await flush()
    expect(granted).toEqual(['first', 'second'])
    expect(limiter.active).toBe(2)
    expect(limiter.waiting).toBe(1)

    releases.first()
    // Releasing twice must not free a second slot.
    releases.first()
    await flush()
    expect(granted).toEqual(['first', 'second', 'third'])
    expect(limiter.active).toBe(2)

    releases.second()
    releases.third()
    expect(limiter.active).toBe(0)
  })

  it('serves priority callers before callers that were already waiting', async () => {
    const limiter = createLoadLimiter(1)
    const granted: string[] = []
    const releaseFirst = await limiter.acquire(new AbortController().signal)
    void limiter.acquire(new AbortController().signal).then(release => {
      granted.push('background')
      release()
    })
    void limiter.acquire(new AbortController().signal, { priority: true }).then(release => {
      granted.push('requested')
      release()
    })

    releaseFirst()
    await flush()
    expect(granted).toEqual(['requested', 'background'])
    expect(limiter.active).toBe(0)
  })

  it('drops an aborted caller from the queue without taking a slot', async () => {
    const limiter = createLoadLimiter(1)
    const releaseFirst = await limiter.acquire(new AbortController().signal)
    const controller = new AbortController()
    const waiting = limiter.acquire(controller.signal)
    expect(limiter.waiting).toBe(1)

    controller.abort()
    await expect(waiting).rejects.toMatchObject({ name: 'AbortError' })
    expect(limiter.waiting).toBe(0)

    releaseFirst()
    expect(limiter.active).toBe(0)
  })

  it('rejects a caller that is already aborted', async () => {
    const limiter = createLoadLimiter(1)
    const controller = new AbortController()
    controller.abort()
    await expect(limiter.acquire(controller.signal)).rejects.toMatchObject({ name: 'AbortError' })
    expect(limiter.active).toBe(0)
    expect(limiter.waiting).toBe(0)
  })
})
