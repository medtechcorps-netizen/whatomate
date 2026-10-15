<script lang="ts">
export type MediaPreviewSource =
  // A raster image object URL (see INLINE_IMAGE_TYPES).
  | { type: 'image'; url: string; alt: string }
  // Bytes already checked to start with %PDF-.
  | { type: 'pdf'; blob: Blob }
</script>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch, type ComponentPublicInstance } from 'vue'
import { useI18n } from 'vue-i18n'
import { AlertCircle, Download, Loader2, ZoomIn, ZoomOut } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog'
import {
  isPdfPreviewAbort,
  openPdfPreview,
  PdfPreviewError,
  type PdfPageRender,
  type PdfPreviewDocument,
  type PdfPreviewFailure,
} from '@/lib/pdfPreview'

// Previews a chat attachment the bubble has already loaded, without leaving
// the CRM: images in a lightbox, PDFs drawn page by page onto canvases.
// Downloads use the same object URL and name as the bubble's own download.

const props = defineProps<{
  open: boolean
  source: MediaPreviewSource
  // The attachment's filename, or a description of an image without one.
  title: string
  downloadUrl: string
  downloadName: string
}>()

const emit = defineEmits<{ 'update:open': [open: boolean] }>()

const { t } = useI18n()

const viewport = ref<HTMLElement | null>(null)
const image = ref<HTMLImageElement | null>(null)
const zoomedWidth = ref<number | null>(null)
const imageFailed = ref(false)

const zoomed = computed(() => zoomedWidth.value !== null)

// Zooming doubles the size the image has while it fits the screen, so small
// images grow too, and the view starts at the image's centre.
async function toggleZoom() {
  if (zoomed.value) {
    zoomedWidth.value = null
    return
  }
  const fitted = image.value?.getBoundingClientRect().width || image.value?.naturalWidth || 0
  if (!fitted) return
  zoomedWidth.value = fitted * 2
  await nextTick()
  const element = viewport.value
  if (!element) return
  element.scrollLeft = (element.scrollWidth - element.clientWidth) / 2
  element.scrollTop = (element.scrollHeight - element.clientHeight) / 2
}

type PageState = 'idle' | 'queued' | 'rendering' | 'rendered' | 'failed'
interface PreviewPage {
  number: number
  width: number
  height: number
  state: PageState
}

// At most this many pages keep a drawn canvas; pages scrolled far away give
// theirs back and are drawn again when they return.
const MAX_DRAWN_PAGES = 6
// Width to draw at when the page has no layout yet.
const DEFAULT_PAGE_WIDTH = 816

const pdfStatus = ref<'idle' | 'loading' | 'ready' | 'error'>('idle')
const pdfFailure = ref<PdfPreviewFailure>('invalid')
const pages = ref<PreviewPage[]>([])
const pageCount = ref(0)
let pdf: PdfPreviewDocument | null = null
let loadController: AbortController | null = null
let observer: IntersectionObserver | null = null
let activeRender: { page: PreviewPage; render: PdfPageRender } | null = null
const pageElements = new Map<number, HTMLElement>()
const nearbyPages = new Set<number>()
const renderQueue: PreviewPage[] = []
const drawnPages: PreviewPage[] = []

const status = computed(() => {
  if (props.source.type === 'pdf') return pdfStatus.value
  return imageFailed.value ? 'error' : 'ready'
})

const failureMessage = computed(() =>
  props.source.type === 'pdf' && pdfFailure.value === 'password'
    ? t('chat.mediaPreview.passwordProtected')
    : t('chat.mediaPreview.previewFailed'),
)

function setPageElement(page: PreviewPage, element: Element | ComponentPublicInstance | null) {
  if (element instanceof HTMLElement) pageElements.set(page.number, element)
  else pageElements.delete(page.number)
}

function queue(page: PreviewPage) {
  if (page.state !== 'idle') return
  page.state = 'queued'
  renderQueue.push(page)
}

function unqueue(page: PreviewPage) {
  const index = renderQueue.indexOf(page)
  if (index < 0) return
  renderQueue.splice(index, 1)
  page.state = 'idle'
}

function releaseFarPages() {
  while (drawnPages.length > MAX_DRAWN_PAGES) {
    const index = drawnPages.findIndex(page => !nearbyPages.has(page.number))
    if (index < 0) return
    const [page] = drawnPages.splice(index, 1)
    const canvas = pageElements.get(page.number)?.querySelector('canvas')
    if (canvas) {
      canvas.width = 0
      canvas.height = 0
    }
    page.state = 'idle'
  }
}

// One page is drawn at a time, top to bottom, so the page in view is never
// waiting behind several others.
function drawNext() {
  if (activeRender || !pdf) return
  renderQueue.sort((a, b) => a.number - b.number)
  const page = renderQueue.shift()
  if (!page) return
  const element = pageElements.get(page.number)
  const canvas = element?.querySelector('canvas')
  if (!element || !canvas) {
    page.state = 'idle'
    drawNext()
    return
  }
  page.state = 'rendering'
  const current = { page, render: pdf.renderPage(page.number, canvas, element.clientWidth || DEFAULT_PAGE_WIDTH) }
  activeRender = current
  current.render.promise.then(
    () => {
      if (activeRender !== current) return
      page.state = 'rendered'
      drawnPages.push(page)
      releaseFarPages()
    },
    error => {
      if (activeRender !== current) return
      page.state = isPdfPreviewAbort(error) ? 'idle' : 'failed'
    },
  ).finally(() => {
    if (activeRender !== current) return
    activeRender = null
    drawNext()
  })
}

function pageFor(target: Element): PreviewPage | undefined {
  const number = Number((target as HTMLElement).dataset.page)
  return pages.value.find(page => page.number === number)
}

function observePages() {
  if (typeof IntersectionObserver === 'undefined') {
    for (const page of pages.value) {
      nearbyPages.add(page.number)
      queue(page)
    }
    drawNext()
    return
  }
  observer = new IntersectionObserver(
    entries => {
      for (const entry of entries) {
        const page = pageFor(entry.target)
        if (!page) continue
        if (entry.isIntersecting) {
          nearbyPages.add(page.number)
          queue(page)
        } else {
          nearbyPages.delete(page.number)
          unqueue(page)
        }
      }
      drawNext()
    },
    // Pages start drawing a little before they scroll into view.
    { root: viewport.value, rootMargin: '400px 0px' },
  )
  for (const element of pageElements.values()) observer.observe(element)
}

function closePdf() {
  loadController?.abort()
  loadController = null
  observer?.disconnect()
  observer = null
  const render = activeRender?.render
  activeRender = null
  render?.cancel()
  renderQueue.length = 0
  drawnPages.length = 0
  nearbyPages.clear()
  pdf?.destroy()
  pdf = null
  pages.value = []
  pageCount.value = 0
  pdfStatus.value = 'idle'
}

async function loadPdf(blob: Blob) {
  const controller = new AbortController()
  loadController = controller
  pdfStatus.value = 'loading'
  try {
    const document = await openPdfPreview(blob, { signal: controller.signal })
    if (controller.signal.aborted) {
      document.destroy()
      return
    }
    loadController = null
    pdf = document
    pageCount.value = document.pageCount
    pages.value = document.pageSizes.map((size, index) => ({ number: index + 1, ...size, state: 'idle' }))
    pdfStatus.value = 'ready'
    await nextTick()
    if (pdf === document) observePages()
  } catch (error) {
    if (controller.signal.aborted || isPdfPreviewAbort(error)) return
    loadController = null
    pdfFailure.value = error instanceof PdfPreviewError ? error.reason : 'unavailable'
    pdfStatus.value = 'error'
  }
}

// The PDF is parsed each time the preview opens and released when it
// closes, so no worker or page bitmaps outlive the dialog.
watch(
  [() => props.open, () => (props.source.type === 'pdf' ? props.source.blob : null)],
  ([open, blob]) => {
    closePdf()
    if (open && blob) void loadPdf(blob)
  },
  { immediate: true },
)

watch([() => props.open, () => props.source], () => {
  zoomedWidth.value = null
  imageFailed.value = false
})

onBeforeUnmount(closePdf)
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent
      class="flex h-[92vh] w-[96vw] max-w-6xl flex-col gap-0 overflow-hidden p-0"
      data-testid="chat-media-preview"
      :data-preview-kind="source.type"
      :data-preview-status="status"
    >
      <!-- pr-12 keeps the actions clear of the dialog's own close button. -->
      <div class="flex min-w-0 items-center gap-2 border-b border-border py-3 pl-4 pr-12">
        <DialogTitle class="min-w-0 flex-1 truncate text-sm font-medium leading-6" :title="title">
          {{ title }}
        </DialogTitle>
        <DialogDescription class="sr-only">{{ t('chat.mediaPreview.description') }}</DialogDescription>
        <Button
          v-if="source.type === 'image' && !imageFailed"
          type="button"
          variant="ghost"
          size="sm"
          data-testid="chat-media-preview-zoom"
          @click="toggleZoom"
        >
          <ZoomOut v-if="zoomed" aria-hidden="true" />
          <ZoomIn v-else aria-hidden="true" />
          {{ zoomed ? t('chat.mediaPreview.fitToScreen') : t('chat.mediaPreview.zoomIn') }}
        </Button>
        <Button
          as="a"
          variant="outline"
          size="sm"
          :href="downloadUrl"
          :download="downloadName"
          data-testid="chat-media-preview-download"
        >
          <Download aria-hidden="true" />
          {{ t('chat.mediaPreview.download') }}
        </Button>
      </div>

      <div
        ref="viewport"
        class="min-h-0 flex-1 overflow-auto bg-muted/40"
        :aria-busy="status === 'loading' ? 'true' : undefined"
      >
        <div
          v-if="status === 'error'"
          role="alert"
          class="mx-auto flex h-full max-w-md flex-col items-center justify-center gap-3 p-8 text-center text-sm"
        >
          <AlertCircle class="h-6 w-6 text-muted-foreground" aria-hidden="true" />
          <p>{{ failureMessage }}</p>
          <Button as="a" size="sm" :href="downloadUrl" :download="downloadName">
            <Download aria-hidden="true" />
            {{ t('chat.mediaPreview.download') }}
          </Button>
        </div>

        <div v-else-if="source.type === 'image'" class="flex p-4" :class="zoomed ? 'min-h-full min-w-full w-max' : 'h-full w-full'">
          <!-- m-auto centres the image and still lets a zoomed one scroll to its edges. -->
          <img
            ref="image"
            :src="source.url"
            :alt="source.alt"
            draggable="false"
            class="m-auto select-none"
            :class="zoomed ? 'max-w-none cursor-zoom-out' : 'max-h-full max-w-full object-contain cursor-zoom-in'"
            :style="zoomedWidth !== null ? { width: `${zoomedWidth}px` } : undefined"
            @click="toggleZoom"
            @error="imageFailed = true"
          />
        </div>

        <div v-else-if="pdfStatus !== 'ready'" class="flex h-full items-center justify-center gap-2 text-sm text-muted-foreground">
          <Loader2 class="h-4 w-4 animate-spin" aria-hidden="true" />
          {{ t('chat.mediaPreview.loading') }}
        </div>

        <template v-else>
          <ol class="mx-auto flex w-full max-w-4xl flex-col gap-4 p-4" :aria-label="t('chat.mediaPreview.pages')">
            <li
              v-for="page in pages"
              :key="page.number"
              :ref="element => setPageElement(page, element)"
              :data-page="page.number"
              :data-page-state="page.state"
              data-testid="chat-media-preview-page"
              class="relative w-full overflow-hidden bg-white shadow-md"
              :style="{ aspectRatio: `${page.width} / ${page.height}` }"
            >
              <canvas
                class="block h-full w-full"
                role="img"
                :aria-label="t('chat.mediaPreview.pageLabel', { page: page.number, total: pageCount })"
              />
              <p
                v-if="page.state === 'failed'"
                class="absolute inset-0 flex items-center justify-center p-4 text-center text-sm text-slate-600"
              >
                {{ t('chat.mediaPreview.pageFailed') }}
              </p>
              <Loader2
                v-else-if="page.state !== 'rendered'"
                class="absolute left-1/2 top-1/2 h-5 w-5 -translate-x-1/2 -translate-y-1/2 animate-spin text-slate-400"
                aria-hidden="true"
              />
            </li>
          </ol>
          <div
            v-if="pageCount > pages.length"
            class="mx-auto mb-6 flex max-w-4xl flex-col items-center gap-3 px-4 text-center text-sm text-muted-foreground"
            data-testid="chat-media-preview-page-limit"
          >
            <p>{{ t('chat.mediaPreview.pageLimit', { shown: pages.length, total: pageCount }) }}</p>
            <Button as="a" variant="outline" size="sm" :href="downloadUrl" :download="downloadName">
              <Download aria-hidden="true" />
              {{ t('chat.mediaPreview.download') }}
            </Button>
          </div>
        </template>
      </div>
    </DialogContent>
  </Dialog>
</template>
