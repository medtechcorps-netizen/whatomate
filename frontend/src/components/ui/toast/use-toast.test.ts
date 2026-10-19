import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, nextTick, watchEffect } from 'vue'

const removalDelay = 1_000_000

describe('toast state notifications', () => {
  beforeEach(() => {
    vi.resetModules()
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.clearAllTimers()
    vi.useRealTimers()
  })

  it('notifies consumers on add, update, close and delayed removal', async () => {
    const { useToast } = await import('./use-toast')
    const store = useToast()
    const seen: string[][] = []
    const stop = watchEffect(() => {
      seen.push(store.toasts.value.map(toast => `${toast.title}:${toast.open}`))
    })
    try {
      const notification = store.toast({ title: 'Saved' })
      await nextTick()
      notification.update({ id: notification.id, title: 'Updated' })
      await nextTick()
      store.toasts.value[0].onOpenChange?.(false)
      await nextTick()
      await vi.advanceTimersByTimeAsync(removalDelay - 1)
      expect(store.toasts.value).toHaveLength(1)
      await vi.advanceTimersByTimeAsync(1)
      await nextTick()
      expect(seen).toEqual([[], ['Saved:true'], ['Updated:true'], ['Updated:false'], []])
    } finally {
      stop()
    }
  })

  it('keeps the newest toast and does not remove it when an older dismissal expires', async () => {
    const { useToast } = await import('./use-toast')
    const store = useToast()
    const older = store.toast({ title: 'Older' })
    older.dismiss()
    const newest = store.toast({ title: 'Newest' })
    expect(store.toasts.value.map(toast => toast.id)).toEqual([newest.id])
    await vi.advanceTimersByTimeAsync(removalDelay)
    expect(store.toasts.value.map(toast => toast.id)).toEqual([newest.id])
    store.dismiss()
    expect(store.toasts.value[0].open).toBe(false)
    expect(vi.getTimerCount()).toBe(1)
    newest.dismiss()
    expect(vi.getTimerCount()).toBe(1)
    await vi.advanceTimersByTimeAsync(removalDelay)
    expect(store.toasts.value).toEqual([])
  })

  it('preserves renderable VNodes and components as payloads through an update', async () => {
    const { useToast } = await import('./use-toast')
    const store = useToast()
    const description = h('span', 'Saved description')
    const action = defineComponent({ render: () => h('button', 'Undo') })
    const notification = store.toast({ title: 'Saved', description, action })
    expect(store.toasts.value[0].description).toBe(description)
    expect(store.toasts.value[0].action).toBe(action)
    notification.update({ id: notification.id, title: 'Changed' })
    expect(store.toasts.value[0].description).toBe(description)
    expect(store.toasts.value[0].action).toBe(action)
    expect(store.toasts.value[0].title).toBe('Changed')
  })
})
