/** @vitest-environment happy-dom */

import { effectScope, nextTick, ref } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { installWindowFileDropGuard, useChatDropZone, useWindowFileDropGuard } from './useChatDropZone'
import ChatDropZoneOverlay from '@/components/chat/ChatDropZoneOverlay.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, named?: Record<string, unknown>) => (named ? `${key}:${JSON.stringify(named)}` : key) }),
}))

type FakeTransfer = {
  types: string[]
  items: Array<{ kind: string; type: string }>
  files: File[]
  dropEffect: string
}

function fileTransfer(files: File[]): FakeTransfer {
  return {
    types: ['Files'],
    items: files.map(f => ({ kind: 'file', type: f.type })),
    files,
    dropEffect: 'copy',
  }
}

function textTransfer(): FakeTransfer {
  return { types: ['text/plain'], items: [{ kind: 'string', type: 'text/plain' }], files: [], dropEffect: 'copy' }
}

function dragEvent(type: string, dataTransfer: FakeTransfer) {
  const event = new Event(type, { bubbles: true, cancelable: true }) as DragEvent
  Object.defineProperty(event, 'dataTransfer', { value: dataTransfer })
  Object.defineProperty(event, 'relatedTarget', { value: null })
  return event
}

const cleanups: Array<() => void> = []

afterEach(() => {
  while (cleanups.length) cleanups.pop()!()
  document.body.innerHTML = ''
})

function guarded() {
  const scope = effectScope()
  const result = scope.run(() => useWindowFileDropGuard())!
  cleanups.push(() => scope.stop())
  return result
}

describe('window file-drop guard', () => {
  it('stops a file dropped outside any drop zone from opening in the browser', () => {
    guarded()
    const element = document.createElement('div')
    document.body.appendChild(element)
    const transfer = fileTransfer([new File(['%PDF'], 'lab.pdf', { type: 'application/pdf' })])

    const over = dragEvent('dragover', transfer)
    element.dispatchEvent(over)
    expect(over.defaultPrevented).toBe(true)
    expect(transfer.dropEffect).toBe('none')

    const drop = dragEvent('drop', transfer)
    element.dispatchEvent(drop)
    expect(drop.defaultPrevented).toBe(true)
  })

  it('leaves non-file drags (text, sortable lists) alone', () => {
    guarded()
    const element = document.createElement('div')
    document.body.appendChild(element)
    const over = dragEvent('dragover', textTransfer())
    element.dispatchEvent(over)
    expect(over.defaultPrevented).toBe(false)
    const drop = dragEvent('drop', textTransfer())
    element.dispatchEvent(drop)
    expect(drop.defaultPrevented).toBe(false)
  })

  it('does not override a drop zone that accepted the drag, or a native file input', () => {
    guarded()
    const zone = document.createElement('div')
    zone.addEventListener('dragover', event => event.preventDefault())
    document.body.appendChild(zone)
    const transfer = fileTransfer([new File(['x'], 'a.png', { type: 'image/png' })])
    zone.dispatchEvent(dragEvent('dragover', transfer))
    expect(transfer.dropEffect).toBe('copy')

    const input = document.createElement('input')
    input.type = 'file'
    document.body.appendChild(input)
    const over = dragEvent('dragover', fileTransfer([]))
    input.dispatchEvent(over)
    expect(over.defaultPrevented).toBe(false)
  })

  it('is reference counted and fully removed by the last uninstall', () => {
    const element = document.createElement('div')
    document.body.appendChild(element)
    const first = installWindowFileDropGuard(window)
    const second = installWindowFileDropGuard(window)
    first()
    first() // idempotent
    const stillGuarded = dragEvent('drop', fileTransfer([]))
    element.dispatchEvent(stillGuarded)
    expect(stillGuarded.defaultPrevented).toBe(true)

    second()
    const unguarded = dragEvent('drop', fileTransfer([]))
    element.dispatchEvent(unguarded)
    expect(unguarded.defaultPrevented).toBe(false)
  })

  it('reports an active file drag until the drop', () => {
    const { isFileDragActive } = guarded()
    const element = document.createElement('div')
    document.body.appendChild(element)
    element.dispatchEvent(dragEvent('dragenter', fileTransfer([])))
    expect(isFileDragActive.value).toBe(true)
    element.dispatchEvent(dragEvent('drop', fileTransfer([])))
    expect(isFileDragActive.value).toBe(false)
  })
})

describe('useChatDropZone', () => {
  function setupZone() {
    const panel = document.createElement('div')
    const textarea = document.createElement('textarea')
    panel.appendChild(textarea)
    document.body.appendChild(panel)
    const target = ref<HTMLElement | null>(panel)
    const onFiles = vi.fn()
    const scope = effectScope()
    const zone = scope.run(() => useChatDropZone(target, onFiles))!
    cleanups.push(() => scope.stop())
    return { panel, textarea, onFiles, zone }
  }

  it('hands dropped files to the caller and lights up while a file drag is over the panel', async () => {
    const { panel, textarea, onFiles, zone } = setupZone()
    await nextTick()
    const pdf = new File(['%PDF'], 'lab.pdf', { type: 'application/pdf' })
    const transfer = fileTransfer([pdf])

    textarea.dispatchEvent(dragEvent('dragenter', transfer))
    textarea.dispatchEvent(dragEvent('dragover', transfer))
    expect(zone.isActive.value).toBe(true)

    const drop = dragEvent('drop', transfer)
    textarea.dispatchEvent(drop)
    expect(drop.defaultPrevented).toBe(true)
    expect(onFiles).toHaveBeenCalledWith([pdf])
    expect(zone.isActive.value).toBe(false)
    expect(panel.contains(textarea)).toBe(true)
  })

  it('ignores text drags so dragging text into the composer keeps working', async () => {
    const { textarea, onFiles, zone } = setupZone()
    await nextTick()
    const transfer = textTransfer()
    textarea.dispatchEvent(dragEvent('dragenter', transfer))
    expect(zone.isActive.value).toBe(false)
    const drop = dragEvent('drop', transfer)
    textarea.dispatchEvent(drop)
    expect(drop.defaultPrevented).toBe(false)
    expect(onFiles).not.toHaveBeenCalled()
  })
})

describe('ChatDropZoneOverlay', () => {
  it('shows the overlay with the recipient during a file drag and emits dropped files', async () => {
    const panel = document.createElement('div')
    document.body.appendChild(panel)
    const wrapper = mount(ChatDropZoneOverlay, {
      props: { target: panel, recipientName: 'Aisyah' },
      attachTo: panel,
    })
    cleanups.push(() => wrapper.unmount())
    await nextTick()
    expect(wrapper.find('[data-testid="composer-drop-overlay"]').exists()).toBe(false)

    const png = new File(['png'], 'rash.png', { type: 'image/png' })
    panel.dispatchEvent(dragEvent('dragenter', fileTransfer([png])))
    panel.dispatchEvent(dragEvent('dragover', fileTransfer([png])))
    await nextTick()
    const overlay = wrapper.find('[data-testid="composer-drop-overlay"]')
    expect(overlay.exists()).toBe(true)
    expect(overlay.text()).toContain('Aisyah')
    // The overlay must not intercept the drop or introduce buttons.
    expect(overlay.classes()).toContain('pointer-events-none')
    expect(overlay.findAll('button')).toHaveLength(0)

    panel.dispatchEvent(dragEvent('drop', fileTransfer([png])))
    await nextTick()
    expect(wrapper.emitted('files')).toEqual([[[png]]])
    expect(wrapper.find('[data-testid="composer-drop-overlay"]').exists()).toBe(false)
  })
})
