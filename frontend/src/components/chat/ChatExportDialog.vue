<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Download, Loader2, X, FileArchive, CheckCircle2, AlertCircle } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { useAuthStore } from '@/stores/auth'
import { useOrganizationsStore } from '@/stores/organizations'
import { readLogicalSessionMarker } from '@/composables/useChatAttachments'
import { chatExportsService, chatExportError, exportDateBoundary, legacyExportAccount, type ChatExportConversation, type ChatExportFilters, type ChatExportPreview, type ChatExportRequest } from '@/services/chatExport'

const props = defineProps<{ filters: ChatExportFilters; initialAccount?: string | null }>()
const auth = useAuthStore()
const organizations = useOrganizationsStore()
const organizationID = computed(() => organizations.selectedOrgId || auth.organizationId || '')
const identity = computed(() => `${auth.user?.id || ''}:${organizationID.value}`)
const available = ref(false)
const open = ref(false)
const scope = ref<ChatExportRequest['scope']>('filtered')
const account = ref('')
const from = ref('')
const until = ref('')
const search = ref('')
const rows = ref<ChatExportConversation[]>([])
const accounts = ref<ChatExportPreview['accounts']>([])
const selected = ref<Record<string, ChatExportConversation>>({})
const next = ref<string | undefined>()
const summary = ref<ChatExportPreview | null>(null)
const loading = ref(false)
const busy = ref(false)
const received = ref(0)
const error = ref('')
const notice = ref('')
const maxSelected = ref(2000)
let generation = 0
let capabilityGeneration = 0
let previewController: AbortController | null = null
let capabilityController: AbortController | null = null
let downloadController: AbortController | null = null
let running: { id: string; organization: string } | null = null
let debounce: ReturnType<typeof setTimeout> | undefined
const selectedKeys = computed(() => Object.keys(selected.value))
const canDownload = computed(() => !busy.value && !loading.value && !!summary.value?.can_download && (scope.value !== 'selected' || selectedKeys.value.length > 0))

function request(forCandidates = false): ChatExportRequest {
  const selectedScope = scope.value === 'selected'
  return {
    ...(scope.value === 'filtered' || selectedScope ? props.filters : {}),
    scope: forCandidates && selectedScope ? 'filtered' : scope.value,
    selected: selectedScope && !forCandidates ? selectedKeys.value : undefined,
    search: forCandidates && selectedScope ? search.value || props.filters.search : scope.value === 'filtered' ? props.filters.search : undefined,
    account: account.value || undefined,
    from: exportDateBoundary(from.value), until: exportDateBoundary(until.value, true),
  }
}

async function refresh(append = false) {
  if (!open.value || busy.value) return
  const epoch = ++generation
  previewController?.abort()
  const controller = new AbortController(); previewController = controller
  loading.value = true; error.value = ''; notice.value = ''
  if (!append) { rows.value = []; next.value = undefined; summary.value = null }
  try {
    const candidate = request(true)
    if (append) candidate.after = next.value
    const result = await chatExportsService.preview(candidate, organizationID.value, controller.signal)
    if (epoch !== generation || !open.value) return
    rows.value = append ? [...rows.value, ...result.rows] : result.rows
    next.value = result.next
    accounts.value = result.accounts
    if (scope.value === 'selected') {
      const selectedPreview = selectedKeys.value.length ? await chatExportsService.preview(request(), organizationID.value, controller.signal) : null
      if (epoch !== generation || !open.value) return
      summary.value = selectedPreview
    } else summary.value = result
  } catch (failure) {
    if (epoch !== generation || controller.signal.aborted) return
    const message = await chatExportError(failure)
    if (epoch !== generation || controller.signal.aborted) return
    error.value = message
    summary.value = null
  } finally { if (epoch === generation) loading.value = false }
}

function scheduleRefresh() {
  if (debounce) clearTimeout(debounce)
  summary.value = null
  debounce = setTimeout(() => { void refresh() }, 200)
}

function toggle(row: ChatExportConversation) {
  const copy = { ...selected.value }
  if (copy[row.key]) delete copy[row.key]
  else {
    if (selectedKeys.value.length >= maxSelected.value) { error.value = 'This selection is too large. Use all matching conversations or a smaller date range.'; return }
    copy[row.key] = row
  }
  selected.value = copy
  scheduleRefresh()
}

async function cancelDownload(silent = false) {
  const active = running
  running = null; generation++
  downloadController?.abort(); downloadController = null
  busy.value = false; received.value = 0
  if (!active) return
  try {
    const result = await chatExportsService.cancel(active.id, active.organization)
    if (!silent && open.value) notice.value = result.cancelled ? 'Export cancelled. No archive was downloaded.' : 'Download stopped. Archive preparation had already finished.'
  }
  catch (failure) { if (!silent && open.value) error.value = await chatExportError(failure) }
}

async function download() {
  if (!canDownload.value || !organizationID.value) return
  previewController?.abort()
  const epoch = ++generation; const capturedIdentity = identity.value; const session = readLogicalSessionMarker()
  const controller = new AbortController(); downloadController = controller
  const id = crypto.randomUUID(); running = { id, organization: organizationID.value }
  busy.value = true; error.value = ''; notice.value = ''; received.value = 0
  try {
    const blob = await chatExportsService.download({ ...request(), request_id: id }, organizationID.value, controller.signal, loaded => { if (epoch === generation) received.value = loaded })
    if (epoch !== generation || controller.signal.aborted || !open.value || identity.value !== capturedIdentity || readLogicalSessionMarker() !== session) return
    const url = URL.createObjectURL(blob)
    const anchor = document.createElement('a'); anchor.href = url; anchor.download = `rereply-chats-${new Date().toISOString().slice(0, 10)}.zip`
    anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 1000)
    notice.value = 'Your complete archive is ready. Review the transcripts before adding them to AI Context.'
  } catch (failure) {
    if (epoch !== generation || controller.signal.aborted) return
    const message = await chatExportError(failure)
    if (epoch !== generation || controller.signal.aborted) return
    error.value = message
  } finally { if (epoch === generation) { running = null; downloadController = null; busy.value = false } }
}

watch(open, value => {
  if (value) {
    scope.value = 'filtered'; account.value = legacyExportAccount(props.initialAccount); from.value = ''; until.value = ''; search.value = ''; selected.value = {}; notice.value = ''; error.value = ''
    void refresh()
  } else {
    generation++; previewController?.abort(); loading.value = false
    if (debounce) clearTimeout(debounce)
    void cancelDownload(true)
  }
})
watch([scope, account, from, until, search], scheduleRefresh)
watch(() => JSON.stringify(props.filters), () => { if (open.value) { selected.value = {}; scheduleRefresh() } })
watch(identity, async () => {
  open.value = false; available.value = false
  capabilityController?.abort(); const controller = new AbortController(); capabilityController = controller
  const epoch = ++capabilityGeneration
  if (!organizationID.value || !auth.user?.id) return
  try {
    const result = await chatExportsService.capabilities(organizationID.value, controller.signal)
    if (epoch !== capabilityGeneration || controller.signal.aborted) return
    available.value = result.available; maxSelected.value = result.max_selected
  } catch { /* No export permission or an unavailable check keeps the control hidden. */ }
}, { immediate: true })
onBeforeUnmount(() => { generation++; capabilityGeneration++; previewController?.abort(); capabilityController?.abort(); if (debounce) clearTimeout(debounce); void cancelDownload(true) })
</script>

<template>
  <Button v-if="available" variant="ghost" size="icon" aria-label="Export chats" title="Export chats" class="h-8 w-8 shrink-0" @click="open = true">
    <Download class="h-4 w-4" />
  </Button>
  <Dialog v-model:open="open">
    <DialogContent class="sm:max-w-3xl max-h-[90vh] overflow-y-auto">
      <DialogHeader>
        <div class="mb-2 flex items-center gap-2 text-emerald-500"><FileArchive class="h-5 w-5" /><span class="text-xs font-semibold uppercase tracking-widest">Conversation archive</span></div>
        <DialogTitle class="text-xl">Export chats</DialogTitle>
        <DialogDescription>Readable transcripts and structured messages, ready to review for AI Context. Exporting does not mark chats as read or change your AI settings.</DialogDescription>
      </DialogHeader>
      <div class="grid gap-5 sm:grid-cols-[220px_1fr]">
        <div class="space-y-4">
          <fieldset :disabled="busy" class="space-y-2">
            <legend class="mb-2 text-sm font-medium">Conversations</legend>
            <label v-for="option in [{ value: 'filtered', title: 'All matching chats', note: 'Uses your current chat filters' }, { value: 'selected', title: 'Choose conversations', note: 'Select across every result page' }, { value: 'all', title: 'All accessible chats', note: 'Every conversation you can access' }]" :key="option.value" class="flex cursor-pointer gap-2 rounded-lg border p-3" :class="scope === option.value ? 'border-emerald-500 bg-emerald-500/5' : 'border-border'">
              <input v-model="scope" type="radio" name="export-scope" :value="option.value" class="mt-0.5 accent-emerald-600" />
              <span><span class="block text-sm font-medium">{{ option.title }}</span><span class="mt-1 block text-xs text-muted-foreground">{{ option.note }}</span></span>
            </label>
          </fieldset>
          <label class="block space-y-1 text-sm"><span>Account</span><select v-model="account" :disabled="busy" class="h-9 w-full rounded-md border border-input bg-background px-2"><option value="">All accounts</option><option v-for="item in accounts" :key="item.key" :value="item.key">{{ item.name }} · {{ item.channel }}</option></select></label>
          <label class="block space-y-1 text-sm"><span>From</span><Input v-model="from" type="date" :disabled="busy" aria-label="Export start date" /></label>
          <label class="block space-y-1 text-sm"><span>Through</span><Input v-model="until" type="date" :disabled="busy" aria-label="Export end date" /></label>
          <p class="text-xs leading-relaxed text-muted-foreground">Dates use your local time zone. Leave dates empty for all available history. Very large exports may need a smaller date range.</p>
        </div>
        <div class="flex min-h-72 flex-col rounded-xl border border-border bg-muted/20">
          <div class="border-b border-border p-4">
            <p class="text-xs font-medium uppercase tracking-wider text-muted-foreground">Export preview</p>
            <div class="mt-2 flex items-baseline gap-2"><span class="text-3xl font-semibold tabular-nums">{{ summary?.messages?.toLocaleString() ?? '—' }}</span><span class="text-sm text-muted-foreground">messages in {{ summary?.conversations?.toLocaleString() ?? (scope === 'selected' ? selectedKeys.length : '—') }} conversations</span></div>
            <p v-if="summary" class="mt-2 text-xs text-muted-foreground">Count checked {{ new Date(summary.as_of).toLocaleTimeString() }}. Download uses a fresh snapshot; new arrivals after it starts are excluded.</p>
          </div>
          <div v-if="scope === 'selected'" class="space-y-2 border-b border-border p-3">
            <Input v-model="search" :disabled="busy" placeholder="Find a conversation" aria-label="Find conversations to export" />
            <div v-if="selectedKeys.length" class="flex max-h-20 flex-wrap gap-1 overflow-y-auto"><button v-for="item in selected" :key="item.key" type="button" :disabled="busy" class="flex items-center gap-1 rounded-full border border-emerald-500/30 bg-emerald-500/10 px-2 py-1 text-xs" :aria-label="`Remove ${item.name} from export`" @click="toggle(item)">{{ item.name }}<X class="h-3 w-3" /></button></div>
          </div>
          <div class="max-h-60 flex-1 overflow-y-auto p-2" :aria-busy="loading">
            <label v-for="row in rows" :key="row.key" class="flex items-start gap-3 rounded-lg px-2 py-3" :class="scope === 'selected' ? 'cursor-pointer hover:bg-muted' : ''">
              <input v-if="scope === 'selected'" type="checkbox" :checked="!!selected[row.key]" :disabled="busy" :aria-label="`Export ${row.name}, ${row.account}`" class="mt-1 accent-emerald-600" @change="toggle(row)" />
              <span class="min-w-0 flex-1"><span class="block truncate text-sm font-medium">{{ row.name }}</span><span class="block truncate text-xs text-muted-foreground">{{ row.account }} · {{ row.channel }}</span></span><span class="text-xs tabular-nums text-muted-foreground">{{ row.messages.toLocaleString() }}</span>
            </label>
            <p v-if="!loading && rows.length === 0 && !error" class="p-5 text-center text-sm text-muted-foreground">No conversations match this scope.</p>
            <div v-if="loading" role="status" class="flex justify-center gap-2 p-4 text-sm text-muted-foreground"><Loader2 class="h-4 w-4 animate-spin" />Checking conversations…</div>
            <Button v-if="next && !loading" variant="ghost" class="w-full" :disabled="busy" @click="refresh(true)">Load more conversations</Button>
          </div>
          <p class="border-t border-border px-4 py-3 text-xs text-muted-foreground">The archive includes every matching page. Attachments are described; media files and private storage links are not included.</p>
        </div>
      </div>
      <div v-if="error || summary?.limit_reason" role="alert" class="flex gap-2 rounded-lg bg-destructive/10 p-3 text-sm text-destructive"><AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />{{ error || summary?.limit_reason }}</div>
      <div v-if="notice" role="status" class="flex gap-2 rounded-lg bg-emerald-500/10 p-3 text-sm"><CheckCircle2 class="mt-0.5 h-4 w-4 shrink-0 text-emerald-500" />{{ notice }}</div>
      <DialogFooter class="items-center gap-2">
        <p v-if="busy" role="status" class="mr-auto flex items-center gap-2 text-sm text-muted-foreground"><Loader2 class="h-4 w-4 animate-spin" />{{ received ? 'Downloading complete archive…' : 'Preparing your complete archive…' }}</p>
        <Button v-if="busy" variant="outline" @click="cancelDownload()">Cancel export</Button>
        <template v-else><Button variant="outline" @click="open = false">Close</Button><Button :disabled="!canDownload" @click="download"><Download class="mr-2 h-4 w-4" />Download ZIP</Button></template>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
