<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, FileText, Loader2, RotateCw } from 'lucide-vue-next'
import { messagesService } from '@/services/api'
import { displaySafeMediaBlob, INLINE_IMAGE_TYPES, type ChatMediaKind } from '@/lib/chatMedia'
import type { Message } from '@/stores/contacts'

// Renders chat media from the authenticated API client instead of a native
// src=/api/media/{id} URL. Native element requests cannot send the
// X-Organization-ID header, so in any workspace other than the login default
// the server looked the message up in the wrong tenant and answered 404.

type MediaKind = ChatMediaKind

const props = defineProps<{
  // Workspace the transcript was loaded from; every request is pinned to it.
  organizationId: string
  message: Pick<Message, 'id' | 'message_type' | 'media_url' | 'media_mime_type' | 'media_filename' | 'content'>
}>()

const { t } = useI18n()

const status = ref<'idle' | 'loading' | 'ready' | 'error'>('idle')
const mediaURL = ref('')
const loadedType = ref('')
const documentLink = ref<HTMLAnchorElement | null>(null)
let requestController: AbortController | null = null
let generation = 0

const kind = computed<MediaKind>(() => {
  const { message_type: type, media_mime_type: mimeType } = props.message
  if (type === 'template') {
    if (mimeType?.startsWith('image/')) return 'image'
    if (mimeType?.startsWith('video/')) return 'video'
    return 'document'
  }
  if (type === 'image' || type === 'sticker' || type === 'video' || type === 'audio') return type
  return 'document'
})

const documentName = computed(() => props.message.media_filename || t('chat.document'))

const imageAlt = computed(() => {
  if (props.message.message_type === 'sticker') return t('chat.sticker')
  if (props.message.message_type === 'template') return 'Template header'
  return props.message.content?.body || t('chat.image')
})

// Only raster images open in a new tab (see displaySafeMediaBlob).
const previewable = computed(() => status.value === 'ready' && INLINE_IMAGE_TYPES.has(loadedType.value))

function releaseMedia() {
  generation++
  requestController?.abort()
  requestController = null
  if (mediaURL.value) URL.revokeObjectURL(mediaURL.value)
  mediaURL.value = ''
  loadedType.value = ''
}

async function loadMedia(): Promise<string> {
  releaseMedia()
  const requestGeneration = generation
  const organizationId = props.organizationId.trim()
  const messageId = props.message.id
  if (!organizationId || !messageId || !props.message.media_url) {
    // Fail closed: without a workspace the request would silently fall back
    // to the login workspace.
    status.value = 'error'
    return ''
  }

  status.value = 'loading'
  const controller = new AbortController()
  requestController = controller
  try {
    const response = await messagesService.getMedia(messageId, organizationId, controller.signal)
    // A newer load, a workspace change or unmount superseded this response.
    if (requestGeneration !== generation) return ''
    const blob = response.data
    if (!(blob instanceof Blob) || blob.size === 0) throw new Error('Empty media response')
    const displayBlob = displaySafeMediaBlob(blob, kind.value)
    mediaURL.value = URL.createObjectURL(displayBlob)
    loadedType.value = displayBlob.type
    status.value = 'ready'
    return mediaURL.value
  } catch {
    if (requestGeneration === generation) status.value = 'error'
    return ''
  } finally {
    if (requestController === controller) requestController = null
  }
}

// Documents can be large and are rarely opened, so they load on demand.
async function downloadDocument() {
  const url = await loadMedia()
  if (!url) return
  await nextTick()
  if (mediaURL.value === url) documentLink.value?.click()
}

function retry() {
  if (kind.value === 'document') void downloadDocument()
  else void loadMedia()
}

function openPreview() {
  if (!previewable.value || !mediaURL.value) return
  window.open(mediaURL.value, '_blank')
}

function handleImageError(event: Event) {
  // An image removed by a reload can still report an error for its old URL.
  const target = event.target as HTMLElement | null
  if (!mediaURL.value || target?.getAttribute('src') !== mediaURL.value) return
  releaseMedia()
  status.value = 'error'
}

// Separate sources are compared value by value, so a transcript refresh that
// replaces the message object with an identical one keeps the loaded media.
watch(
  [
    () => props.organizationId,
    () => props.message.id,
    () => props.message.media_url,
    () => props.message.media_mime_type,
    () => props.message.message_type,
  ],
  () => {
    if (kind.value === 'document') {
      releaseMedia()
      status.value = 'idle'
      return
    }
    void loadMedia()
  },
  // Sync flush aborts the previous workspace's request before anything else
  // can observe the new props.
  { immediate: true, flush: 'sync' },
)

onBeforeUnmount(releaseMedia)
</script>

<template>
  <div class="mb-2" data-testid="chat-message-media" :data-media-kind="kind" :data-media-status="status">
    <button
      v-if="status === 'idle'"
      type="button"
      class="flex items-center gap-2 px-3 py-2 bg-background/50 rounded-lg hover:bg-background/80 transition-colors text-left"
      @click="downloadDocument"
    >
      <FileText class="h-5 w-5 text-muted-foreground" aria-hidden="true" />
      <span class="text-sm truncate max-w-[200px]">{{ documentName }}</span>
    </button>
    <div
      v-else-if="status === 'loading'"
      role="status"
      class="flex items-center gap-2 px-3 py-3 bg-background/50 rounded-lg text-sm text-muted-foreground"
    >
      <Loader2 class="h-4 w-4 animate-spin" aria-hidden="true" />
      {{ t('chat.mediaLoading') }}
    </div>
    <div
      v-else-if="status === 'error'"
      role="status"
      class="flex max-w-[280px] items-center gap-2 px-3 py-3 bg-background/50 rounded-lg text-sm"
    >
      <AlertCircle class="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span class="flex-1">{{ t('chat.mediaLoadFailed') }}</span>
      <button
        type="button"
        class="inline-flex items-center gap-1 rounded px-2 py-1 font-medium hover:bg-background/80 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2"
        @click="retry"
      >
        <RotateCw class="h-3.5 w-3.5" aria-hidden="true" />
        {{ t('common.retry') }}
      </button>
    </div>
    <template v-else-if="kind === 'image' || kind === 'sticker'">
      <button
        v-if="previewable"
        type="button"
        class="block w-fit rounded-lg focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2"
        @click="openPreview"
      >
        <img
          :src="mediaURL"
          :alt="imageAlt"
          :class="kind === 'sticker' ? 'max-w-[128px] max-h-[128px] cursor-pointer' : 'max-w-[280px] max-h-[300px] rounded-lg cursor-pointer object-cover'"
          @error="handleImageError"
        />
      </button>
      <img
        v-else
        :src="mediaURL"
        :alt="imageAlt"
        :class="kind === 'sticker' ? 'max-w-[128px] max-h-[128px]' : 'max-w-[280px] max-h-[300px] rounded-lg object-cover'"
        @error="handleImageError"
      />
    </template>
    <video
      v-else-if="kind === 'video'"
      :src="mediaURL"
      controls
      class="max-w-[280px] max-h-[300px] rounded-lg"
    />
    <audio
      v-else-if="kind === 'audio'"
      :src="mediaURL"
      controls
      class="max-w-[280px]"
    />
    <a
      v-else
      ref="documentLink"
      :href="mediaURL"
      :download="message.media_filename || 'document'"
      class="flex items-center gap-2 px-3 py-2 bg-background/50 rounded-lg hover:bg-background/80 transition-colors"
    >
      <FileText class="h-5 w-5 text-muted-foreground" aria-hidden="true" />
      <span class="text-sm truncate max-w-[200px]">{{ documentName }}</span>
    </a>
  </div>
</template>
