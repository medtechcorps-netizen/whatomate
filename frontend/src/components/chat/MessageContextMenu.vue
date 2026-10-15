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
 * opens the menu at the gesture's coordinates. Links, media controls, and an
 * active text selection keep the browser's own menu.
 */
const props = defineProps<{
  container: HTMLElement | null | undefined
  messages: readonly Message[]
  quickReactions: readonly string[]
  getText: (message: Message) => string
}>()

const emit = defineEmits<{
  reply: [message: Message]
  react: [messageId: string, emoji: string]
}>()

const { t } = useI18n()

const LONG_PRESS_MS = 500
const LONG_PRESS_SLOP_PX = 10
const ROW_SELECTOR = '[data-testid="chat-message"][data-message-id]'
const NATIVE_MENU_SELECTOR = 'a[href], video, audio, input, textarea, select'

const anchorHostRef = ref<HTMLElement | null>(null)
const target = ref<Message | null>(null)

const copyText = computed(() => (target.value ? props.getText(target.value).trim() : ''))

function messageAt(eventTarget: EventTarget | null): { message: Message; bubble: Element } | null {
  const container = props.container
  if (!container || !(eventTarget instanceof Element)) return null
  if (eventTarget.closest(NATIVE_MENU_SELECTOR)) return null
  const bubble = eventTarget.closest('.chat-bubble')
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

function onContextMenu(event: MouseEvent) {
  const hit = messageAt(event.target)
  if (!hit || hasSelectionIn(hit.bubble)) return
  event.preventDefault()
  clearLongPress()
  openAt(event.clientX, event.clientY, hit.message)
}

let longPressTimer = 0
let pressOrigin: { x: number; y: number } | null = null

function clearLongPress() {
  if (longPressTimer) window.clearTimeout(longPressTimer)
  longPressTimer = 0
  pressOrigin = null
}

function onPointerDown(event: PointerEvent) {
  if (event.pointerType !== 'touch' && event.pointerType !== 'pen') return
  const hit = messageAt(event.target)
  if (!hit) return
  clearLongPress()
  const origin = { x: event.clientX, y: event.clientY }
  pressOrigin = origin
  longPressTimer = window.setTimeout(() => {
    longPressTimer = 0
    pressOrigin = null
    openAt(origin.x, origin.y, hit.message)
  }, LONG_PRESS_MS)
}

function onPointerMove(event: PointerEvent) {
  if (!pressOrigin) return
  const dx = event.clientX - pressOrigin.x
  const dy = event.clientY - pressOrigin.y
  if (Math.hypot(dx, dy) > LONG_PRESS_SLOP_PX) clearLongPress()
}

const listeners: Array<[string, EventListener]> = [
  ['contextmenu', onContextMenu as EventListener],
  ['pointerdown', onPointerDown as EventListener],
  ['pointermove', onPointerMove as EventListener],
  ['pointerup', clearLongPress],
  ['pointercancel', clearLongPress],
]

function detach(element: HTMLElement | null | undefined) {
  if (!element) return
  for (const [type, listener] of listeners) element.removeEventListener(type, listener)
}

watch(
  () => props.container,
  (element, previous) => {
    detach(previous)
    clearLongPress()
    if (!element) return
    for (const [type, listener] of listeners) element.addEventListener(type, listener)
  },
  { immediate: true },
)

onBeforeUnmount(() => {
  detach(props.container)
  clearLongPress()
})

async function copyMessageText() {
  const text = copyText.value
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    toast.success(t('chat.messageActions.copied'))
  } catch {
    toast.error(t('chat.messageActions.copyFailed'))
  }
}

function reactWith(emoji: string) {
  if (target.value) emit('react', target.value.id, emoji)
}

function replyToTarget() {
  if (target.value) emit('reply', target.value)
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
    <ContextMenuContent v-if="target" class="w-56" @close-auto-focus.prevent>
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
      <ContextMenuItem :disabled="!copyText" @select="copyMessageText">
        <Copy class="mr-2 h-4 w-4" aria-hidden="true" />
        {{ t('chat.messageActions.copyText') }}
      </ContextMenuItem>
      </div>
    </ContextMenuContent>
  </ContextMenu>
</template>
