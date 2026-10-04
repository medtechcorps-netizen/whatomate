// Lets at most `limit` loads run at once; later callers wait for a free slot.
// A caller that aborts while waiting leaves the queue without taking a slot.
export interface LoadLimiter {
  /** Resolves with a release function once a slot is free. */
  acquire(signal: AbortSignal, options?: { priority?: boolean }): Promise<() => void>
  readonly active: number
  readonly waiting: number
}

export interface LoadLimiterOptions {
  // Serve waiting callers newest first instead of in arrival order. Slots are
  // handed out once the current task ends, so callers that queue together
  // (say, every item of a list rendered at once) are ordered before any of
  // them starts. Priority callers still go first, in arrival order.
  newestFirst?: boolean
}

function abortReason(signal: AbortSignal) {
  return signal.reason ?? new DOMException('The load was aborted.', 'AbortError')
}

export function createLoadLimiter(limit: number, options: LoadLimiterOptions = {}): LoadLimiter {
  const newestFirst = options.newestFirst ?? false
  let active = 0
  let drainScheduled = false
  const priorityQueue: Array<() => void> = []
  const queue: Array<() => void> = []

  function next() {
    if (priorityQueue.length > 0) return priorityQueue.shift()
    return newestFirst ? queue.pop() : queue.shift()
  }

  function drain() {
    while (active < limit) {
      const grant = next()
      if (!grant) return
      active++
      grant()
    }
  }

  function scheduleDrain() {
    if (!newestFirst) {
      drain()
      return
    }
    if (drainScheduled) return
    drainScheduled = true
    queueMicrotask(() => {
      drainScheduled = false
      drain()
    })
  }

  function acquire(signal: AbortSignal, acquireOptions: { priority?: boolean } = {}) {
    return new Promise<() => void>((resolve, reject) => {
      if (signal.aborted) {
        reject(abortReason(signal))
        return
      }
      const waitingIn = acquireOptions.priority ? priorityQueue : queue
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
        const index = waitingIn.indexOf(grant)
        if (index !== -1) waitingIn.splice(index, 1)
        reject(abortReason(signal))
      }
      signal.addEventListener('abort', cancel, { once: true })
      waitingIn.push(grant)
      scheduleDrain()
    })
  }

  return {
    acquire,
    get active() {
      return active
    },
    get waiting() {
      return priorityQueue.length + queue.length
    },
  }
}
