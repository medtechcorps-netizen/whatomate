<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Copy, Reply } from 'lucide-vue-next'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import type { Message } from '@/stores/contacts'

/**
 * Right-click / long-press menu for chat bubbles.
 *
 * It listens on the message list instead of wrapping each bubble, so the
 * transcript DOM (ids, test ids, the hover action strip) is unchanged. A
 * pointer gesture on a bubble is forwarded to a hidden reka-ui trigger, which
 * opens the menu at the gesture's coordinates. Links, images, media controls,
 * and an active text selection keep the browser's own menu.
 */
const props = defineProps<{
  container: HTMLElement | null | undefined
  messages: readonly Message[]
  quickReactions: readonly string[]
}>()

const emit = defineEmits<{
  reply: [message: Message]
  react: [messageId: string, emoji: string]
}>()

const { t } = useI18n()

const LONG_PRESS_MS = 500
const LONG_PRESS_SLOP_PX = 10
// Android also fires a native contextmenu for the same long-press.
const LONG_PRESS_CONTEXTMENU_GRACE_MS = 1000
// Runs a chosen action even if the close focus event never arrives.
const ACTION_FALLBACK_MS = 1000
const ROW_SELECTOR = '[data-testid="chat-message"][data-message-id]'
const NATIVE_MENU_SELECTOR = 'a[href], img, picture, canvas, video, audio, input, textarea, select'

const anchorHostRef = ref<HTMLElement | null>(null)
const target = ref<Message | null>(null)

const TEXT_TYPES = new Set(['text', 'button_reply', 'nfm_reply', 'interactive', 'flow', 'template'])
const CAPTION_TYPES = new Set(['image', 'video', 'document'])

/**
 * The message's own text, never a UI or server placeholder such as
 * "[Template: …]" or "[Message deleted …]". Empty means nothing to copy.
 */
function messageCopyText(message: Message): string {
  if (!TEXT_TYPES.has(message.message_type) && !CAPTION_TYPES.has(message.message_type)) return ''
  let text = ''
  if (typeof message.content === 'string') text = message.content
  else if (typeof message.content?.body === 'string') text = message.content.body
  if (!text && (message.message_type === 'interactive' || message.message_type === 'flow')) {
    text = message.interactive_data?.body ?? ''
  }
  text = text.trim()
  return /^\[[^\]]*\]$/.test(text) ? '' : text
}

const copyText = computed(() => (target.value ? messageCopyText(target.value) : ''))

function messageAt(eventTarget: EventTarget | null): { message: Message; bubble: HTMLElement } | null {
  const container = props.container
  if (!container || !(eventTarget instanceof Element)) return null
  if (eventTarget.closest(NATIVE_MENU_SELECTOR)) return null
  const bubble = eventTarget.closest<HTMLElement>('.chat-bubble')
  const row = bubble?.closest(ROW_SELECTOR)
  if (!bubble || !row || !container.contains(row)) return null
  const id = row.getAttribute('data-message-id')
  const message = props.messages.find(m => m.id === id)
  return message ? { message, bubble } : null
}

function hasSelectionIn(element: Element): boolean {
  const selection = window.getSelection?.()
  if (!selection || selection.isCollapsed || !selection.anchorNode) return false
  return element.contains(selection.anchorNode)
}

function openAt(x: number, y: number, message: Message) {
  const anchor = anchorHostRef.value?.firstElementChild
  if (!anchor) return
  target.value = message
  // reka-ui's ContextMenuTrigger opens at the event's client coordinates.
  anchor.dispatchEvent(new MouseEvent('contextmenu', {
    bubbles: false,
    cancelable: true,
    clientX: x,
    clientY: y,
  }))
}

let lastLongPressOpen = 0

function onContextMenu(event: MouseEvent) {
  const hit = messageAt(event.target)
  if (!hit || hasSelectionIn(hit.bubble)) return
  event.preventDefault()
  if (Date.now() - lastLongPressOpen < LONG_PRESS_CONTEXTMENU_GRACE_MS) return
  clearLongPress()
  openAt(event.clientX, event.clientY, hit.message)
}

// ---- Long-press -----------------------------------------------------------

let longPressTimer = 0
let pressOrigin: { x: number; y: number } | null = null
let suppressed: { bubble: HTMLElement; callout: string; select: string; webkitSelect: string } | null = null

/** Stop the native callout and text selection on the pressed bubble only. */
function suppressNativeGesture(bubble: HTMLElement) {
  restoreNativeGesture()
  const style = bubble.style as CSSStyleDeclaration & { webkitTouchCallout?: string; webkitUserSelect?: string }
  suppressed = {
    bubble,
    callout: style.webkitTouchCallout ?? '',
    select: style.userSelect,
    webkitSelect: style.webkitUserSelect ?? '',
  }
  style.webkitTouchCallout = 'none'
  style.userSelect = 'none'
  style.webkitUserSelect = 'none'
}

function restoreNativeGesture() {
  if (!suppressed) return
  const style = suppressed.bubble.style as CSSStyleDeclaration & { webkitTouchCallout?: string; webkitUserSelect?: string }
  style.webkitTouchCallout = suppressed.callout
  style.userSelect = suppressed.select
  style.webkitUserSelect = suppressed.webkitSelect
  suppressed = null
}

function clearLongPress() {
  if (longPressTimer) window.clearTimeout(longPressTimer)
  longPressTimer = 0
  pressOrigin = null
}

function endPress() {
  clearLongPress()
  restoreNativeGesture()
}

function onPointerDown(event: PointerEvent) {
  if (event.pointerType !== 'touch' && event.pointerType !== 'pen') return
  const hit = messageAt(event.target)
  // An existing selection keeps the platform's own selection handles/menu.
  if (!hit || hasSelectionIn(hit.bubble)) return
  endPress()
  suppressNativeGesture(hit.bubble)
  const origin = { x: event.clientX, y: event.clientY }
  pressOrigin = origin
  longPressTimer = window.setTimeout(() => {
    longPressTimer = 0
    pressOrigin = null
    if (hasSelectionIn(hit.bubble)) return
    window.getSelection?.()?.removeAllRanges()
    lastLongPressOpen = Date.now()
    openAt(origin.x, origin.y, hit.message)
  }, LONG_PRESS_MS)
}

function onPointerMove(event: PointerEvent) {
  if (!pressOrigin) return
  const dx = event.clientX - pressOrigin.x
  const dy = event.clientY - pressOrigin.y
  if (Math.hypot(dx, dy) > LONG_PRESS_SLOP_PX) endPress()
}

const listeners: Array<[string, EventListener]> = [
  ['contextmenu', onContextMenu as EventListener],
  ['pointerdown', onPointerDown as EventListener],
  ['pointermove', onPointerMove as EventListener],
  ['pointerup', endPress],
  ['pointercancel', endPress],
]

function detach(element: HTMLElement | null | undefined) {
  if (!element) return
  for (const [type, listener] of listeners) element.removeEventListener(type, listener)
}

watch(
  () => props.container,
  (element, previous) => {
    detach(previous)
    endPress()
    if (!element) return
    for (const [type, listener] of listeners) element.addEventListener(type, listener)
  },
  { immediate: true },
)

// ---- Actions run after the menu has closed --------------------------------
//
// reka-ui's trapped FocusScope pulls focus back into the closing menu and only
// releases it after unmount, so an action that moves focus (Reply focuses the
// composer, Forward opens a dialog) runs once the close focus event has fired.

let pendingAction: (() => void) | null = null
let pendingFallback = 0

function runPendingAction() {
  if (pendingFallback) window.clearTimeout(pendingFallback)
  pendingFallback = 0
  const action = pendingAction
  pendingAction = null
  action?.()
}

function queueAction(action: () => void) {
  pendingAction = action
  if (pendingFallback) window.clearTimeout(pendingFallback)
  pendingFallback = window.setTimeout(runPendingAction, ACTION_FALLBACK_MS)
}

function onCloseAutoFocus(event: Event) {
  event.preventDefault()
  // After reka's own post-unmount focus step (also a 0 ms timer).
  window.setTimeout(runPendingAction, 0)
}

onBeforeUnmount(() => {
  detach(props.container)
  endPress()
  if (pendingFallback) window.clearTimeout(pendingFallback)
  pendingAction = null
})

async function copyMessageText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(t('chat.messageActions.copied'))
  } catch {
    toast.error(t('chat.messageActions.copyFailed'))
  }
}

function reactWith(emoji: string) {
  const message = target.value
  if (message) queueAction(() => emit('react', message.id, emoji))
}

function replyToTarget() {
  const message = target.value
  if (message) queueAction(() => emit('reply', message))
}

function copyTarget() {
  const text = copyText.value
  if (text) queueAction(() => void copyMessageText(text))
}
</script>

<template>
  <ContextMenu>
    <!-- A template ref through the trigger's as-child slot is not reliable
         across remounts, so the host span locates the trigger element. -->
    <span
      ref="anchorHostRef"
      aria-hidden="true"
      class="pointer-events-none fixed left-0 top-0 h-0 w-0 overflow-hidden"
    >
      <ContextMenuTrigger />
    </span>
    <ContextMenuContent v-if="target" class="w-56" @close-auto-focus="onCloseAutoFocus">
      <!-- The shadcn content wrapper renders a portal, so attributes go here. -->
      <div data-testid="chat-message-context-menu" role="none">
      <div
        role="group"
        :aria-label="t('chat.messageActions.react')"
        class="flex items-center justify-between gap-0.5 px-1 pb-1"
      >
        <ContextMenuItem
          v-for="emoji in quickReactions"
          :key="emoji"
          class="h-8 w-8 justify-center p-0 text-lg"
          :aria-label="t('chat.messageActions.reactWith', { emoji })"
          @select="reactWith(emoji)"
        >
          {{ emoji }}
        </ContextMenuItem>
      </div>
      <ContextMenuSeparator />
      <ContextMenuItem @select="replyToTarget">
        <Reply class="mr-2 h-4 w-4" aria-hidden="true" />
        {{ t('chat.reply') }}
      </ContextMenuItem>
      <ContextMenuItem v-if="copyText" @select="copyTarget">
        <Copy class="mr-2 h-4 w-4" aria-hidden="true" />
        {{ t('chat.messageActions.copyText') }}
      </ContextMenuItem>
      </div>
    </ContextMenuContent>
  </ContextMenu>
</template>
