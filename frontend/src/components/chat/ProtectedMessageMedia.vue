<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, FileText, Loader2, Play, RotateCw } from 'lucide-vue-next'
import { messagesService } from '@/services/api'
import { chatMediaLoadLimiter, displaySafeMediaBlob, INLINE_IMAGE_TYPES, type ChatMediaKind } from '@/lib/chatMedia'
import type { Message } from '@/stores/contacts'

// Renders chat media from the authenticated API client instead of a native
// src=/api/media/{id} URL. Native element requests cannot send the
// X-Organization-ID header, so in any workspace other than the login default
// the server looked the message up in the wrong tenant and answered 404.

const props = defineProps<{
  // Workspace the transcript was requested from; every request is pinned to it.
  organizationId: string
  message: Pick<Message, 'id' | 'message_type' | 'media_url' | 'media_mime_type' | 'media_filename' | 'content'>
}>()

const { t } = useI18n()

const documentLink = ref<HTMLAnchorElement | null>(null)
const mediaElement = ref<HTMLMediaElement | null>(null)
const status = ref<'idle' | 'loading' | 'ready' | 'error'>('idle')
const mediaURL = ref('')
const loadedType = ref('')
const progress = ref<number | null>(null)
let requestController: AbortController | null = null
let releaseSlot: (() => void) | null = null
let generation = 0

const kind = computed<ChatMediaKind>(() => {
  const { message_type: type, media_mime_type: mimeType } = props.message
  if (type === 'template') {
    if (mimeType?.startsWith('image/')) return 'image'
    if (mimeType?.startsWith('video/')) return 'video'
    return 'document'
  }
  if (type === 'image' || type === 'sticker' || type === 'video' || type === 'audio') return type
  return 'document'
})

// Images and stickers load as soon as they render, as native images did, so
// the transcript has its full height before the agent scrolls through it or
// jumps to a quoted message. Images that load later, while the agent reads,
// would push the text being read out of place: the chat viewport has scroll
// anchoring turned off so it can follow the newest message. Video and audio
// can be up to 16 MB and documents are rarely opened, so those load only when
// the agent asks for them.
const loadsAutomatically = computed(() => kind.value === 'image' || kind.value === 'sticker')
const busy = computed(() => status.value === 'loading' || (status.value === 'idle' && loadsAutomatically.value))
const progressPercent = computed(() => (progress.value === null ? null : Math.round(progress.value * 100)))

const documentName = computed(() => props.message.media_filename || t('chat.document'))

const imageAlt = computed(() => {
  if (props.message.message_type === 'sticker') return t('chat.sticker')
  if (props.message.message_type === 'template') return t('chat.headerImage')
  return props.message.content?.body || t('chat.image')
})

// Only raster images open in a new tab (see displaySafeMediaBlob).
const previewable = computed(() => status.value === 'ready' && INLINE_IMAGE_TYPES.has(loadedType.value))

function releaseMedia() {
  generation++
  requestController?.abort()
  requestController = null
  releaseSlot?.()
  releaseSlot = null
  progress.value = null
  if (mediaURL.value) URL.revokeObjectURL(mediaURL.value)
  mediaURL.value = ''
  loadedType.value = ''
}

async function loadMedia(options: { userInitiated?: boolean } = {}): Promise<string> {
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
  let slot: (() => void) | null = null
  try {
    // Loads the agent asked for go ahead of images that load by themselves.
    slot = await chatMediaLoadLimiter.acquire(controller.signal, { priority: options.userInitiated })
    if (requestGeneration !== generation) return ''
    releaseSlot = slot
    const response = await messagesService.getMedia(messageId, organizationId, {
      signal: controller.signal,
      onProgress: (loaded, total) => {
        if (requestGeneration === generation && total) progress.value = Math.min(loaded / total, 1)
      },
    })
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
    slot?.()
    if (releaseSlot === slot) releaseSlot = null
    if (requestController === controller) requestController = null
    if (requestGeneration === generation) progress.value = null
  }
}

// Documents download from the protected bytes once they are loaded.
async function downloadDocument() {
  const url = await loadMedia({ userInitiated: true })
  if (!url) return
  await nextTick()
  if (mediaURL.value === url) documentLink.value?.click()
}

// Video and audio load when the agent presses play, then start playing.
async function playMedia() {
  const url = await loadMedia({ userInitiated: true })
  if (!url) return
  await nextTick()
  if (mediaURL.value !== url) return
  try {
    await mediaElement.value?.play()
  } catch {
    // Autoplay can be refused once the click is no longer recent; the
    // player's own controls still work.
  }
}

function retry() {
  if (kind.value === 'document') void downloadDocument()
  else if (kind.value === 'video' || kind.value === 'audio') void playMedia()
  else void loadMedia({ userInitiated: true })
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
    releaseMedia()
    status.value = 'idle'
    if (loadsAutomatically.value) void loadMedia()
  },
  // Sync flush aborts the previous workspace's request before anything else
  // can observe the new props.
  { immediate: true, flush: 'sync' },
)

onBeforeUnmount(releaseMedia)
</script>

<template>
  <div
    class="mb-2"
    data-testid="chat-message-media"
    :data-media-kind="kind"
    :data-media-status="status"
    :aria-busy="busy ? 'true' : undefined"
  >
    <div
      v-if="busy"
      class="flex min-w-[180px] flex-col gap-2 px-3 py-3 bg-background/50 rounded-lg text-sm text-muted-foreground"
    >
      <span class="flex items-center gap-2">
        <Loader2 class="h-4 w-4 shrink-0 animate-spin" aria-hidden="true" />
        {{ t('chat.mediaLoading') }}
      </span>
      <div
        v-if="progressPercent !== null"
        role="progressbar"
        :aria-label="t('chat.mediaLoading')"
        aria-valuemin="0"
        aria-valuemax="100"
        :aria-valuenow="progressPercent"
        class="h-1 overflow-hidden rounded-full bg-muted"
      >
        <div class="h-full bg-primary transition-[width]" :style="{ width: `${progressPercent}%` }" />
      </div>
    </div>
    <button
      v-else-if="status === 'idle' && kind === 'document'"
      type="button"
      class="flex items-center gap-2 px-3 py-2 bg-background/50 rounded-lg hover:bg-background/80 transition-colors text-start focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2"
      @click="downloadDocument"
    >
      <FileText class="h-5 w-5 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span class="text-sm truncate max-w-[200px]">{{ documentName }}</span>
    </button>
    <button
      v-else-if="status === 'idle'"
      type="button"
      class="flex items-center gap-2 px-3 py-2 bg-background/50 rounded-lg hover:bg-background/80 transition-colors text-start focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2"
      @click="playMedia"
    >
      <Play class="h-5 w-5 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span class="text-sm">{{ kind === 'video' ? t('chat.playVideo') : t('chat.playAudio') }}</span>
    </button>
    <div
      v-else-if="status === 'error'"
      role="alert"
      class="flex max-w-[280px] items-center gap-2 px-3 py-3 bg-background/50 rounded-lg text-sm"
    >
      <AlertCircle class="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span class="flex min-w-0 flex-1 flex-col">
        <!-- Say which attachment failed: the error replaces its filename button. -->
        <span v-if="kind === 'document'" class="truncate font-medium" :title="documentName">{{ documentName }}</span>
        <span>{{ t('chat.mediaLoadFailed') }}</span>
      </span>
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
      ref="mediaElement"
      :src="mediaURL"
      controls
      class="max-w-[280px] max-h-[300px] rounded-lg"
    />
    <audio
      v-else-if="kind === 'audio'"
      ref="mediaElement"
      :src="mediaURL"
      controls
      class="max-w-[280px]"
    />
    <a
      v-else
      ref="documentLink"
      :href="mediaURL"
      :download="documentName"
      class="flex items-center gap-2 px-3 py-2 bg-background/50 rounded-lg hover:bg-background/80 transition-colors"
    >
      <FileText class="h-5 w-5 shrink-0 text-muted-foreground" aria-hidden="true" />
      <span class="text-sm truncate max-w-[200px]">{{ documentName }}</span>
    </a>
  </div>
</template>
