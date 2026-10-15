<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  AlertTriangle,
  CheckCircle2,
  FileText,
  Loader2,
  Music,
  Plus,
  RotateCw,
  Send,
  ShieldCheck,
  Trash2,
} from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Progress } from '@/components/ui/progress'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  MAX_CAPTION_LENGTH,
  formatFileSize,
  kindSupportsCaption,
  type ChatMediaKind,
} from '@/lib/chatAttachments'
import type { StagedAttachment } from '@/composables/useChatAttachments'

const props = withDefaults(
  defineProps<{
    open: boolean
    items: StagedAttachment[]
    /** Shown so the agent confirms who receives the files (PDPA). */
    recipientName: string
    recipientDetail?: string
    sending?: boolean
    maxFiles?: number
  }>(),
  { recipientDetail: '', sending: false, maxFiles: 10 },
)

const emit = defineEmits<{
  'update:open': [open: boolean]
  remove: [id: string]
  'update:caption': [id: string, caption: string]
  'send-as-document': [id: string]
  send: []
  'retry-failed': []
  'add-more': []
}>()

const { t } = useI18n()

const readyCount = computed(() => props.items.filter(item => item.status === 'ready').length)
const failedCount = computed(() => props.items.filter(item => item.status === 'failed').length)
const invalidCount = computed(() => props.items.filter(item => item.status === 'invalid').length)
const sentCount = computed(() => props.items.filter(item => item.status === 'sent').length)
const canAddMore = computed(() => !props.sending && props.items.length < props.maxFiles)

const brokenPreviews = ref<Record<string, boolean>>({})

function plural(count: number, one: string, many: string) {
  return t(count === 1 ? one : many, { count })
}

const description = computed(() =>
  plural(props.items.length, 'chat.composer.descriptionOne', 'chat.composer.descriptionMany'),
)

const sendLabel = computed(() =>
  plural(readyCount.value, 'chat.composer.sendOne', 'chat.composer.sendMany'),
)

function mimeOf(item: StagedAttachment) {
  return item.validation.mimeType || item.file.type || ''
}

function showImagePreview(item: StagedAttachment) {
  return !!item.previewUrl && mimeOf(item).startsWith('image/') && !brokenPreviews.value[item.id]
}

function showVideoPreview(item: StagedAttachment) {
  return !!item.previewUrl && mimeOf(item).startsWith('video/')
}

function kindLabel(kind: ChatMediaKind | null) {
  return t(`chat.composer.kind.${kind ?? 'document'}`)
}

function canCaption(item: StagedAttachment) {
  return item.validation.ok && kindSupportsCaption(item.validation.kind)
}

function problemText(item: StagedAttachment) {
  const validation = item.validation
  if (validation.ok) return ''
  switch (validation.problem) {
    case 'empty':
      return t('chat.composer.problem.empty')
    case 'too-large':
      return t('chat.composer.problem.tooLarge', {
        limit: formatFileSize(validation.limitBytes ?? 0),
      })
    case 'unsupported-image':
      return t('chat.composer.problem.unsupportedImage')
    case 'unsupported-video':
      return t('chat.composer.problem.unsupportedVideo')
    default:
      return t('chat.composer.problem.unsupportedType')
  }
}

function canSendAsDocument(item: StagedAttachment) {
  return item.status === 'invalid' && !item.validation.ok && item.validation.canSendAsDocument
}

function onOpenChange(open: boolean) {
  // Uploads are already on their way to WhatsApp; keep the progress visible.
  if (!open && props.sending) return
  emit('update:open', open)
}
</script>

<template>
  <Dialog :open="open" @update:open="onOpenChange">
    <DialogContent class="max-w-lg" data-testid="composer-media-dialog">
      <DialogHeader>
        <DialogTitle>{{ t('chat.sendMedia') }}</DialogTitle>
        <DialogDescription>{{ description }}</DialogDescription>
      </DialogHeader>

      <div
        class="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm"
        data-testid="composer-media-recipient"
      >
        <ShieldCheck class="mt-0.5 h-4 w-4 shrink-0 text-amber-500" aria-hidden="true" />
        <div class="min-w-0">
          <p class="truncate">
            {{ t('chat.composer.recipientLabel') }}
            <strong class="font-semibold">{{ recipientName }}</strong>
            <span v-if="recipientDetail" class="text-muted-foreground"> · {{ recipientDetail }}</span>
          </p>
          <p class="text-xs text-muted-foreground">{{ t('chat.composer.recipientCheck') }}</p>
        </div>
      </div>

      <ul class="-mx-1 max-h-[min(50vh,420px)] space-y-2 overflow-y-auto px-1" data-testid="composer-media-list">
        <li
          v-for="item in items"
          :key="item.id"
          class="flex gap-3 rounded-lg border border-white/[0.08] bg-white/[0.02] p-2 light:border-gray-200 light:bg-gray-50"
          data-testid="composer-media-item"
          :data-status="item.status"
        >
          <div class="flex h-12 w-12 shrink-0 items-center justify-center overflow-hidden rounded-md bg-muted">
            <img
              v-if="showImagePreview(item)"
              :src="item.previewUrl!"
              alt=""
              class="h-full w-full object-cover"
              @error="brokenPreviews[item.id] = true"
            />
            <video
              v-else-if="showVideoPreview(item)"
              :src="item.previewUrl!"
              muted
              preload="metadata"
              class="h-full w-full object-cover"
            />
            <Music v-else-if="item.validation.kind === 'audio'" class="h-5 w-5 text-muted-foreground" aria-hidden="true" />
            <FileText v-else class="h-5 w-5 text-muted-foreground" aria-hidden="true" />
          </div>

          <div class="min-w-0 flex-1 space-y-1.5">
            <div class="flex items-start gap-2">
              <div class="min-w-0 flex-1">
                <p class="truncate text-sm font-medium" :title="item.file.name">{{ item.file.name }}</p>
                <p class="text-xs text-muted-foreground">
                  {{ kindLabel(item.validation.kind) }} · {{ formatFileSize(item.file.size) }}
                </p>
              </div>
              <Button
                variant="ghost"
                size="icon-sm"
                class="h-7 w-7 shrink-0"
                :aria-label="t('chat.composer.removeFile', { name: item.file.name })"
                :title="t('chat.composer.removeFile', { name: item.file.name })"
                :disabled="sending"
                data-testid="composer-media-remove"
                @click="emit('remove', item.id)"
              >
                <Trash2 class="h-3.5 w-3.5" aria-hidden="true" />
              </Button>
            </div>

            <Input
              v-if="canCaption(item) && item.status !== 'sent'"
              :model-value="item.caption"
              :placeholder="t('chat.mediaCaption') + '...'"
              :disabled="sending"
              :maxlength="MAX_CAPTION_LENGTH"
              :aria-label="t('chat.composer.captionFor', { name: item.file.name })"
              class="h-8 text-xs"
              data-testid="composer-media-caption"
              @update:model-value="(value: string) => emit('update:caption', item.id, value)"
            />

            <div v-if="item.status === 'invalid'" class="space-y-1">
              <p class="flex items-start gap-1 text-xs text-destructive">
                <AlertTriangle class="mt-0.5 h-3 w-3 shrink-0" aria-hidden="true" />
                <span>{{ problemText(item) }}</span>
              </p>
              <Button
                v-if="canSendAsDocument(item)"
                variant="outline"
                size="xs"
                class="text-xs"
                :disabled="sending"
                data-testid="composer-media-as-document"
                @click="emit('send-as-document', item.id)"
              >
                {{ t('chat.composer.sendAsDocument') }}
              </Button>
            </div>
            <p v-else-if="item.asDocument && item.status === 'ready'" class="text-xs text-muted-foreground">
              {{ t('chat.composer.willSendAsDocument') }}
            </p>

            <div v-if="item.status === 'sending'" class="space-y-1" role="status">
              <Progress :model-value="item.progress" class="h-1.5" />
              <p class="text-xs text-muted-foreground">
                {{
                  item.progress < 100
                    ? t('chat.composer.uploading', { progress: item.progress })
                    : t('chat.composer.deliveringToWhatsApp')
                }}
              </p>
            </div>
            <p v-else-if="item.status === 'sent'" class="flex items-center gap-1 text-xs text-emerald-500">
              <CheckCircle2 class="h-3 w-3" aria-hidden="true" />
              {{ t('chat.composer.sent') }}
            </p>
            <p v-else-if="item.status === 'failed'" class="flex items-start gap-1 text-xs text-destructive" role="alert">
              <AlertTriangle class="mt-0.5 h-3 w-3 shrink-0" aria-hidden="true" />
              <span>{{ item.errorMessage || t('chat.composer.failedDefault') }}</span>
            </p>
          </div>
        </li>
      </ul>

      <p v-if="invalidCount > 0 && readyCount > 0" class="text-xs text-muted-foreground">
        {{ plural(invalidCount, 'chat.composer.skippedOne', 'chat.composer.skippedMany') }}
      </p>

      <div class="flex flex-wrap items-center justify-between gap-2">
        <Button
          v-if="canAddMore"
          variant="ghost"
          size="sm"
          data-testid="composer-media-add"
          @click="emit('add-more')"
        >
          <Plus class="h-4 w-4" aria-hidden="true" />
          {{ t('chat.composer.addFiles') }}
        </Button>
        <span v-else />
        <div class="flex flex-wrap justify-end gap-2">
          <Button variant="outline" :disabled="sending" @click="onOpenChange(false)">
            {{ sentCount > 0 ? t('common.close') : t('common.cancel') }}
          </Button>
          <Button
            v-if="failedCount > 0 && !sending"
            variant="outline"
            data-testid="composer-media-retry"
            @click="emit('retry-failed')"
          >
            <RotateCw class="h-4 w-4" aria-hidden="true" />
            {{ t('chat.composer.retryFailed', { count: failedCount }) }}
          </Button>
          <Button
            v-if="readyCount > 0 || failedCount === 0 || sending"
            :disabled="sending || readyCount === 0"
            data-testid="composer-media-send"
            @click="emit('send')"
          >
            <Loader2 v-if="sending" class="h-4 w-4 animate-spin" aria-hidden="true" />
            <Send v-else class="h-4 w-4" aria-hidden="true" />
            {{ sending ? t('chat.sending') + '...' : sendLabel }}
          </Button>
        </div>
      </div>
    </DialogContent>
  </Dialog>
</template>
