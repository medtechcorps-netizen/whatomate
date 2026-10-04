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

  describe('newest first', () => {
    it('starts the newest of the callers that queued together, then the next newest', async () => {
      const limiter = createLoadLimiter(2, { newestFirst: true })
      const granted: string[] = []
      const releases: Record<string, () => void> = {}
      // Items of a list rendered at once queue oldest first.
      for (const name of ['oldest', 'older', 'newer', 'newest']) {
        void limiter.acquire(new AbortController().signal).then(release => {
          granted.push(name)
          releases[name] = release
        })
      }
      expect(limiter.active).toBe(0)
      await flush()
      expect(granted).toEqual(['newest', 'newer'])
      expect(limiter.waiting).toBe(2)

      releases.newest()
      await flush()
      expect(granted).toEqual(['newest', 'newer', 'older'])
      releases.newer()
      releases.older()
      await flush()
      expect(granted).toEqual(['newest', 'newer', 'older', 'oldest'])
      releases.oldest()
      expect(limiter.active).toBe(0)
    })

    it('still serves priority callers first, in the order they asked', async () => {
      const limiter = createLoadLimiter(1, { newestFirst: true })
      const granted: string[] = []
      const releaseFirst = await limiter.acquire(new AbortController().signal)
      for (const [name, priority] of [['background', false], ['first request', true], ['second request', true]] as const) {
        void limiter.acquire(new AbortController().signal, { priority }).then(release => {
          granted.push(name)
          release()
        })
      }

      releaseFirst()
      await flush()
      expect(granted).toEqual(['first request', 'second request', 'background'])
      expect(limiter.active).toBe(0)
    })

    it('drops a caller that aborts before slots are handed out', async () => {
      const limiter = createLoadLimiter(1, { newestFirst: true })
      const controller = new AbortController()
      const granted: string[] = []
      void limiter.acquire(new AbortController().signal).then(release => {
        granted.push('older')
        release()
      })
      const aborted = limiter.acquire(controller.signal)
      controller.abort()

      await expect(aborted).rejects.toMatchObject({ name: 'AbortError' })
      await flush()
      expect(granted).toEqual(['older'])
      expect(limiter.active).toBe(0)
      expect(limiter.waiting).toBe(0)
    })
  })
})
