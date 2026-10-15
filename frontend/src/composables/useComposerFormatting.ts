import { nextTick, type Ref } from 'vue'
import {
  formatKindForShortcut,
  insertTextAtSelection,
  toggleWhatsAppFormat,
  type TextEdit,
  type WhatsAppFormatKind,
} from '@/lib/whatsappFormat'

/**
 * Selection-aware editing for the chat composer textarea: WhatsApp format
 * toggles (toolbar + Ctrl/⌘+B, Ctrl/⌘+I, Ctrl/⌘+Shift+X) and inserting text
 * such as emoji at the caret instead of appending it.
 */
export function useComposerFormatting(
  value: Ref<string>,
  textarea: Ref<HTMLTextAreaElement | null>,
  afterEdit?: () => void,
) {
  function currentSelection(): [number, number] {
    const element = textarea.value
    const length = value.value.length
    if (!element) return [length, length]
    return [element.selectionStart ?? length, element.selectionEnd ?? length]
  }

  function apply(edit: TextEdit) {
    value.value = edit.value
    void nextTick(() => {
      const element = textarea.value
      if (!element) return
      element.focus()
      element.setSelectionRange(edit.selectionStart, edit.selectionEnd)
      afterEdit?.()
    })
  }

  function applyFormat(kind: WhatsAppFormatKind) {
    const [start, end] = currentSelection()
    apply(toggleWhatsAppFormat(value.value, start, end, kind))
  }

  function insertText(text: string) {
    const [start, end] = currentSelection()
    apply(insertTextAtSelection(value.value, start, end, text))
  }

  /** Keydown handler for the textarea; ignores everything but the format shortcuts. */
  function onKeydown(event: KeyboardEvent) {
    if (event.isComposing) return
    const kind = formatKindForShortcut(event)
    if (!kind) return
    event.preventDefault()
    applyFormat(kind)
  }

  return { applyFormat, insertText, onKeydown }
}
