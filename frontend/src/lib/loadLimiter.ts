// Lets at most `limit` loads run at once; later callers wait for a free slot.
// A caller that aborts while waiting leaves the queue without taking a slot.
export interface LoadLimiter {
  /** Resolves with a release function once a slot is free. */
  acquire(signal: AbortSignal, options?: { priority?: boolean }): Promise<() => void>
  readonly active: number
  readonly waiting: number
}

function abortReason(signal: AbortSignal) {
  return signal.reason ?? new DOMException('The load was aborted.', 'AbortError')
}

export function createLoadLimiter(limit: number): LoadLimiter {
  let active = 0
  const queue: Array<() => void> = []

  function drain() {
    while (active < limit && queue.length > 0) {
      active++
      queue.shift()!()
    }
  }

  function acquire(signal: AbortSignal, options: { priority?: boolean } = {}) {
    return new Promise<() => void>((resolve, reject) => {
      if (signal.aborted) {
        reject(abortReason(signal))
        return
      }
      let released = false
      const release = () => {
        if (released) return
        released = true
        active--
        drain()
      }
      const grant = () => {
        signal.removeEventListener('abort', cancel)
        resolve(release)
      }
      const cancel = () => {
        const index = queue.indexOf(grant)
        if (index !== -1) queue.splice(index, 1)
        reject(abortReason(signal))
      }
      signal.addEventListener('abort', cancel, { once: true })
      if (options.priority) queue.unshift(grant)
      else queue.push(grant)
      drain()
    })
  }

  return {
    acquire,
    get active() {
      return active
    },
    get waiting() {
      return queue.length
    },
  }
}
