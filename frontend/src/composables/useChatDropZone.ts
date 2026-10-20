import { computed, getCurrentScope, onScopeDispose, readonly, ref, toValue, watch, type MaybeRefOrGetter } from 'vue'
import { useDropZone } from '@vueuse/core'
import { dataTransferHasFiles } from '@/lib/chatAttachments'

// ---------------------------------------------------------------------------
// Window-level guard: a file dropped anywhere outside a drop zone must never
// make the browser open it (which navigates away and loses the draft).
// ---------------------------------------------------------------------------

/** dragover repeats every 350 ms ± 200 ms while a drag is over the page. */
const FILE_DRAG_IDLE_MS = 1200

const fileDragActive = ref(false)
let guardInstalls = 0
let guardedWindow: Window | null = null
let idleTimer: ReturnType<typeof setTimeout> | null = null

function markFileDrag() {
  fileDragActive.value = true
  if (idleTimer) clearTimeout(idleTimer)
  idleTimer = setTimeout(clearFileDrag, FILE_DRAG_IDLE_MS)
}

function clearFileDrag() {
  if (idleTimer) clearTimeout(idleTimer)
  idleTimer = null
  fileDragActive.value = false
}

function isNativeFileInput(target: EventTarget | null): boolean {
  return typeof HTMLInputElement !== 'undefined' && target instanceof HTMLInputElement && target.type === 'file'
}

function onWindowDragEnter(event: DragEvent) {
  if (dataTransferHasFiles(event.dataTransfer)) markFileDrag()
}

function onWindowDragOver(event: DragEvent) {
  if (!dataTransferHasFiles(event.dataTransfer)) return
  markFileDrag()
  // A drop zone (or a native file input) already accepted this drag.
  if (event.defaultPrevented || isNativeFileInput(event.target)) return
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'none'
}

function onWindowDrop(event: DragEvent) {
  clearFileDrag()
  if (!dataTransferHasFiles(event.dataTransfer)) return
  if (event.defaultPrevented || isNativeFileInput(event.target)) return
  event.preventDefault()
}

function onWindowDragLeave(event: DragEvent) {
  // Leaving the browser window: there is no element to enter next.
  if (event.relatedTarget === null && dataTransferHasFiles(event.dataTransfer)) {
    const { clientX, clientY } = event
    const outside =
      clientX <= 0 || clientY <= 0 || clientX >= window.innerWidth || clientY >= window.innerHeight
    if (outside) clearFileDrag()
  }
}

/**
 * Installs the window-level file-drop guard; reference counted, so it can be
 * called from App.vue and from every drop zone. Returns an uninstaller.
 */
export function installWindowFileDropGuard(win: Window = window): () => void {
  if (guardInstalls === 0) {
    guardedWindow = win
    win.addEventListener('dragenter', onWindowDragEnter)
    win.addEventListener('dragover', onWindowDragOver)
    win.addEventListener('dragleave', onWindowDragLeave)
    win.addEventListener('drop', onWindowDrop)
  }
  guardInstalls++
  let installed = true
  return () => {
    if (!installed) return
    installed = false
    guardInstalls--
    if (guardInstalls === 0 && guardedWindow) {
      guardedWindow.removeEventListener('dragenter', onWindowDragEnter)
      guardedWindow.removeEventListener('dragover', onWindowDragOver)
      guardedWindow.removeEventListener('dragleave', onWindowDragLeave)
      guardedWindow.removeEventListener('drop', onWindowDrop)
      guardedWindow = null
      clearFileDrag()
    }
  }
}

/** Mount once near the app root (App.vue); safe to call more than once. */
export function useWindowFileDropGuard() {
  if (typeof window === 'undefined') return { isFileDragActive: readonly(fileDragActive) }
  const uninstall = installWindowFileDropGuard(window)
  if (getCurrentScope()) onScopeDispose(uninstall)
  return { isFileDragActive: readonly(fileDragActive) }
}

// ---------------------------------------------------------------------------
// Chat panel drop zone
// ---------------------------------------------------------------------------

/**
 * Drop zone for the chat panel. Only file drags light it up and only file
 * drops are taken: dragging selected text into the composer keeps working.
 */
export function useChatDropZone(
  target: MaybeRefOrGetter<HTMLElement | null | undefined>,
  onFiles: (files: File[]) => void,
) {
  const { isFileDragActive } = useWindowFileDropGuard()

  // useDropZone (vueuse 10) cancels every drop on its target, which would
  // also cancel a text drop into the composer textarea. Stop non-file drops
  // before they reach its listener; their native default still runs.
  watch(
    () => toValue(target),
    (element, _previous, onCleanup) => {
      if (!element) return
      const letTextDropThrough = (event: DragEvent) => {
        if (!dataTransferHasFiles(event.dataTransfer)) event.stopPropagation()
      }
      element.addEventListener('drop', letTextDropThrough, true)
      onCleanup(() => element.removeEventListener('drop', letTextDropThrough, true))
    },
    { immediate: true, flush: 'post' },
  )

  const { isOverDropZone } = useDropZone(target, {
    // vueuse passes the types of `kind === 'file'` items only.
    dataTypes: (types: readonly string[]) => types.length > 0,
    onDrop: (files: File[] | null) => {
      if (files && files.length > 0) onFiles(files)
    },
  })

  // A drag cancelled outside the window can leave useDropZone's enter/leave
  // counter unbalanced; the window-level idle timer clears that.
  const isActive = computed(() => isOverDropZone.value && isFileDragActive.value)

  return { isOverDropZone, isActive }
}
