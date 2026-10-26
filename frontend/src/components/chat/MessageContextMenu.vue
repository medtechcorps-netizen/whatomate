<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { Copy, Forward, Reply } from 'lucide-vue-next'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import type { Message } from '@/stores/contacts'
import { canOfferForward } from './messageForward'

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
  /** Focused synchronously by Reply, inside the user gesture (iOS keyboard). */
  composer?: HTMLElement | null
  canForward?: boolean
  /** Changes immediately when the requested conversation or auth scope changes. */
  scopeKey?: string
}>()

const emit = defineEmits<{
  reply: [message: Message]
  react: [messageId: string, emoji: string]
  forward: [message: Message]
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
const menuOpen = ref(false)
// Reka ContextMenuRoot does not accept a controlled `open` prop. Replacing
// its root is the supported reset boundary for scroll/scope invalidation.
const menuEpoch = ref(0)
type MenuTarget = {
  id: string
  bubble: HTMLElement
  row: Element
  container: HTMLElement
  scopeKey: string | undefined
  generation: number
}
let generation = 0
let capturedTarget: MenuTarget | null = null

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
const showForward = computed(() =>
  props.canForward === true && target.value !== null && canOfferForward(target.value),
)

function messageAt(eventTarget: EventTarget | null): MenuTarget | null {
  const container = props.container
  if (!container || !(eventTarget instanceof Element)) return null
  if (eventTarget.closest(NATIVE_MENU_SELECTOR)) return null
  const bubble = eventTarget.closest<HTMLElement>('.chat-bubble')
  const row = bubble?.closest(ROW_SELECTOR)
  if (!bubble || !row || !container.contains(row)) return null
  const id = row.getAttribute('data-message-id')
  const message = props.messages.find(m => m.id === id)
  return message ? { id: message.id, bubble, row, container, scopeKey: props.scopeKey, generation } : null
}

function hasSelectionIn(element: Element): boolean {
  const selection = element.ownerDocument.getSelection?.()
  if (!selection || selection.isCollapsed) return false
  for (let index = 0; index < selection.rangeCount; index++) {
    if (selection.getRangeAt(index).intersectsNode(element)) return true
  }
  return false
}

function currentMessage(hit: MenuTarget | null): Message | undefined {
  if (!hit || hit.generation !== generation || hit.scopeKey !== props.scopeKey ||
    hit.container !== props.container || !hit.row.isConnected || !hit.bubble.isConnected ||
    !hit.container.contains(hit.row) || hit.bubble.closest(ROW_SELECTOR) !== hit.row ||
    hit.row.getAttribute('data-message-id') !== hit.id) return
  return props.messages.find(message => message.id === hit.id)
}

async function openAt(x: number, y: number, hit: MenuTarget) {
  const message = currentMessage(hit)
  if (!message) return
  capturedTarget = hit
  target.value = message
  await nextTick()
  const anchor = anchorHostRef.value?.firstElementChild
  if (!anchor || capturedTarget !== hit || !currentMessage(hit) || hasSelectionIn(hit.bubble)) return
  // reka-ui's ContextMenuTrigger opens at the event's client coordinates.
  anchor.dispatchEvent(new MouseEvent('contextmenu', {
    bubbles: false,
    cancelable: true,
    clientX: x,
    clientY: y,
  }))
}

let lastLongPress: { hit: MenuTarget; openedAt: number } | null = null

function onContextMenu(event: MouseEvent) {
  const hit = messageAt(event.target)
  if (!hit) {
    dismissMenu()
    return
  }
  if (hasSelectionIn(hit.bubble)) {
    dismissMenu()
    return
  }
  event.preventDefault()
  const touchContext = event instanceof PointerEvent ? event.pointerType === 'touch' :
    press !== null && event.button !== 2
  if (touchContext && menuOpen.value && lastLongPress?.hit.bubble === hit.bubble &&
    currentMessage(lastLongPress.hit) && Date.now() - lastLongPress.openedAt < LONG_PRESS_CONTEXTMENU_GRACE_MS) return
  dismissMenu()
  hit.generation = generation
  openAt(event.clientX, event.clientY, hit)
}

// ---- Long-press -----------------------------------------------------------

let longPressTimer = 0
let press: { x: number; y: number; pointerId: number; hit: MenuTarget } | null = null
let suppressed: { bubble: HTMLElement; callout: string; priority: string } | null = null

const TOUCH_CALLOUT_PROPERTY = '-webkit-touch-callout'

/**
 * Stop only the native callout on the pressed bubble. Text selection stays
 * available, so an agent can still select part of a message (an IC or phone
 * number); a selection that starts during the press wins over the menu.
 */
function suppressNativeGesture(bubble: HTMLElement) {
  restoreNativeGesture()
  const callout = bubble.style.getPropertyValue(TOUCH_CALLOUT_PROPERTY)
  suppressed = { bubble, callout, priority: bubble.style.getPropertyPriority(TOUCH_CALLOUT_PROPERTY) }
  bubble.style.setProperty(TOUCH_CALLOUT_PROPERTY, 'none')
}

function restoreNativeGesture() {
  if (!suppressed) return
  if (suppressed.callout) {
    suppressed.bubble.style.setProperty(TOUCH_CALLOUT_PROPERTY, suppressed.callout, suppressed.priority)
  } else {
    suppressed.bubble.style.removeProperty(TOUCH_CALLOUT_PROPERTY)
  }
  suppressed = null
}

function clearLongPress() {
  if (longPressTimer) window.clearTimeout(longPressTimer)
  longPressTimer = 0
  press = null
}

function endPress() {
  clearLongPress()
  restoreNativeGesture()
}

function onPointerDown(event: PointerEvent) {
  // Finger only: a pen press is for selecting and writing.
  if (event.pointerType !== 'touch' || !event.isPrimary) return
  const hit = messageAt(event.target)
  // An existing selection keeps the platform's own selection handles/menu.
  if (!hit || hasSelectionIn(hit.bubble)) return
  dismissMenu()
  // Dismissal invalidates the previous target; this is a new gesture.
  hit.generation = generation
  suppressNativeGesture(hit.bubble)
  const origin = { x: event.clientX, y: event.clientY, pointerId: event.pointerId, hit }
  press = origin
  longPressTimer = window.setTimeout(() => {
    longPressTimer = 0
    if (press !== origin || !currentMessage(hit) || hasSelectionIn(hit.bubble)) {
      endPress()
      return
    }
    lastLongPress = { hit, openedAt: Date.now() }
    openAt(origin.x, origin.y, hit)
  }, LONG_PRESS_MS)
}

function onPointerMove(event: PointerEvent) {
  if (!press || event.pointerId !== press.pointerId) return
  const dx = event.clientX - press.x
  const dy = event.clientY - press.y
  if (Math.hypot(dx, dy) > LONG_PRESS_SLOP_PX) dismissMenu()
}

const listeners: Array<[string, EventListener]> = [
  ['contextmenu', onContextMenu as EventListener],
  ['pointerdown', onPointerDown as EventListener],
]

function onPointerEnd(event: PointerEvent) {
  if (event.pointerId !== press?.pointerId) return
  if (event.type === 'pointercancel') dismissMenu()
  else endPress()
}

function onAdditionalPointer(event: PointerEvent) {
  if (press && event.pointerType === 'touch' && event.pointerId !== press.pointerId) dismissMenu()
}

function onSelectionChange() {
  const bubble = press?.hit.bubble ?? capturedTarget?.bubble
  if (bubble && hasSelectionIn(bubble)) dismissMenu()
}

function onScroll(event: Event) {
  const container = props.container
  // Scroll does not bubble. Capture sees the actual ScrollArea viewport,
  // which is an ancestor of the inner transcript passed as container.
  if (container && (event.target === container.ownerDocument ||
    event.target instanceof Element && event.target.contains(container))) dismissMenu()
}

const documentListeners: Array<[string, EventListener]> = [
  ['scroll', onScroll],
  ['selectionchange', onSelectionChange],
  ['pointerdown', onAdditionalPointer as EventListener],
  ['pointermove', onPointerMove as EventListener],
  ['pointerup', onPointerEnd as EventListener],
  ['pointercancel', onPointerEnd as EventListener],
]
let rowObserver: MutationObserver | null = null

function validateTargets() {
  if (capturedTarget && !currentMessage(capturedTarget) || press && !currentMessage(press.hit)) dismissMenu()
}

function dismissMenu() {
  generation++
  endPress()
  menuOpen.value = false
  menuEpoch.value++
  capturedTarget = null
  target.value = null
  lastLongPress = null
}

function onMenuOpenChange(open: boolean) {
  if (open && (!currentMessage(capturedTarget) || !capturedTarget || hasSelectionIn(capturedTarget.bubble))) {
    dismissMenu()
    return
  }
  menuOpen.value = open
  if (!open) endPress()
}

function detach(element: HTMLElement | null | undefined) {
  if (!element) return
  for (const [type, listener] of listeners) element.removeEventListener(type, listener)
  for (const [type, listener] of documentListeners) element.ownerDocument.removeEventListener(type, listener, true)
  rowObserver?.disconnect()
  rowObserver = null
}

watch(
  () => props.container,
  (element, previous) => {
    detach(previous)
    dismissMenu()
    if (!element) return
    for (const [type, listener] of listeners) element.addEventListener(type, listener)
    for (const [type, listener] of documentListeners) element.ownerDocument.addEventListener(type, listener, { capture: true, passive: true })
    rowObserver = new MutationObserver(validateTargets)
    rowObserver.observe(element, { subtree: true, childList: true, attributes: true, attributeFilter: ['data-message-id'] })
  },
  { immediate: true },
)

watch(() => props.scopeKey, dismissMenu, { flush: 'sync' })
watch(() => props.messages.map(message => message.id), validateTargets, { flush: 'sync' })

// ---- Actions ---------------------------------------------------------------
//
// Work that needs the user gesture runs synchronously in the select handler:
// clipboard writes (WebKit refuses them later) and the composer focus that
// raises the iOS keyboard. The menu is non-modal, so no focus trap pulls that
// focus back while it animates out. The remaining bookkeeping (Reply's reply
// state, React, Forward's dialog) runs once the menu's close focus event fired.

let pendingAction: (() => void) | null = null
let pendingFallback = 0

function runPendingAction() {
  if (pendingFallback) window.clearTimeout(pendingFallback)
  pendingFallback = 0
  const action = pendingAction
  pendingAction = null
  action?.()
}

function queueAction(hit: MenuTarget, action: (message: Message) => void) {
  pendingAction = () => {
    const message = currentMessage(hit)
    if (message) action(message)
  }
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
  const hit = capturedTarget
  if (menuOpen.value && hit && currentMessage(hit)) queueAction(hit, message => emit('react', message.id, emoji))
}

function replyToTarget() {
  const hit = capturedTarget
  if (!menuOpen.value || !hit || !currentMessage(hit)) return
  props.composer?.focus({ preventScroll: true })
  queueAction(hit, message => emit('reply', message))
}

function copyTarget() {
  // Inside the gesture: WebKit only allows clipboard writes there.
  const message = menuOpen.value && currentMessage(capturedTarget)
  const text = message ? messageCopyText(message) : ''
  if (text) void copyMessageText(text)
}

function forwardTarget() {
  const hit = capturedTarget
  const message = menuOpen.value && currentMessage(hit)
  if (hit && message && props.canForward === true && canOfferForward(message)) {
    queueAction(hit, current => {
      if (props.canForward === true && canOfferForward(current)) emit('forward', current)
    })
  }
}
</script>

<template>
  <ContextMenu :key="menuEpoch" :modal="false" @update:open="onMenuOpenChange">
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
      <ContextMenuItem v-if="showForward" @select="forwardTarget">
        <Forward class="mr-2 h-4 w-4" aria-hidden="true" />
        {{ t('chat.forward.action') }}
      </ContextMenuItem>
      <ContextMenuItem v-if="copyText" @select="copyTarget">
        <Copy class="mr-2 h-4 w-4" aria-hidden="true" />
        {{ t('chat.messageActions.copyText') }}
      </ContextMenuItem>
      </div>
    </ContextMenuContent>
  </ContextMenu>
</template>
