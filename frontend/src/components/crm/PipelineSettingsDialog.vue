<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Layers3, Loader2, Plus, Save, Trash2 } from 'lucide-vue-next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import { crmService, type Pipeline, type PipelineStage } from '@/services/productSuite'

type StageDraft = Pick<
  PipelineStage,
  'id' | 'name' | 'color' | 'display_order' | 'kind' | 'probability' | 'sla_hours' | 'is_active' | 'version'
>

const props = defineProps<{
  modelValue: boolean
  pipeline: Pipeline | null | undefined
  canDeleteStages?: boolean
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  saved: [pipelineId?: string]
}>()

const stageDrafts = ref<StageDraft[]>([])
const savingStageId = ref('')
const stageError = ref('')
const creatingStage = ref(false)
const creatingPipeline = ref(false)
const deleting = ref(false)
const pendingDelete = ref<StageDraft | null>(null)
const showNewPipeline = ref(false)
const newStage = ref({
  name: '',
  color: '#67e8f9',
  kind: 'open' as PipelineStage['kind'],
  probability: 25,
  sla_hours: 24,
})
const newPipeline = ref({ name: '', description: '', is_default: false })

const open = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value),
})

function syncDrafts() {
  stageDrafts.value = [...(props.pipeline?.stages ?? [])]
    .sort((left, right) => left.display_order - right.display_order)
    .map((stage) => ({
      id: stage.id,
      name: stage.name,
      color: stage.color || '#67e8f9',
      display_order: stage.display_order,
      kind: stage.kind,
      probability: stage.probability,
      sla_hours: stage.sla_hours ?? 0,
      is_active: stage.is_active ?? true,
      version: stage.version,
    }))
  stageError.value = ''
}

watch(
  [() => props.modelValue, () => props.pipeline],
  ([isOpen]) => {
    if (isOpen) syncDrafts()
  },
)

function stageIsValid(stage: Pick<StageDraft, 'name' | 'probability' | 'sla_hours'>) {
  return Boolean(
    stage.name.trim() &&
      stage.probability >= 0 &&
      stage.probability <= 100 &&
      stage.sla_hours >= 0,
  )
}

async function saveStage(stage: StageDraft) {
  if (!props.pipeline || !stageIsValid(stage)) {
    stageError.value = 'Stage name, probability (0–100), and a non-negative SLA are required.'
    return
  }

  stageError.value = ''
  savingStageId.value = stage.id
  try {
    const response = await crmService.updatePipelineStage(props.pipeline.id, stage.id, {
      version: stage.version,
      name: stage.name.trim(),
      color: stage.color,
      display_order: stage.display_order,
      kind: stage.kind,
      probability: Number(stage.probability),
      sla_hours: Number(stage.sla_hours),
      is_active: stage.is_active,
    })
    const updated = unwrapItemResponse<PipelineStage>(response)
    const index = stageDrafts.value.findIndex((item) => item.id === stage.id)
    if (index >= 0) {
      stageDrafts.value[index] = {
        ...stageDrafts.value[index],
        ...updated,
      }
    }
    emit('saved', props.pipeline.id)
  } catch (error) {
    stageError.value = getErrorMessage(error, 'Stage could not be saved')
  } finally {
    savingStageId.value = ''
  }
}

async function addStage() {
  if (!props.pipeline || !stageIsValid(newStage.value)) {
    stageError.value = 'Stage name, probability (0–100), and a non-negative SLA are required.'
    return
  }

  stageError.value = ''
  creatingStage.value = true
  try {
    const response = await crmService.createPipelineStage(props.pipeline.id, {
      name: newStage.value.name.trim(),
      color: newStage.value.color,
      display_order: stageDrafts.value.length,
      kind: newStage.value.kind,
      probability: Number(newStage.value.probability),
      sla_hours: Number(newStage.value.sla_hours),
      is_active: true,
    })
    const created = unwrapItemResponse<PipelineStage>(response)
    stageDrafts.value.push({
      id: created.id,
      name: created.name,
      color: created.color || '#67e8f9',
      display_order: created.display_order,
      kind: created.kind,
      probability: created.probability,
      sla_hours: created.sla_hours ?? 0,
      is_active: created.is_active ?? true,
      version: created.version,
    })
    newStage.value = {
      name: '',
      color: '#67e8f9',
      kind: 'open',
      probability: 25,
      sla_hours: 24,
    }
    emit('saved', props.pipeline.id)
  } catch (error) {
    stageError.value = getErrorMessage(error, 'Stage could not be added')
  } finally {
    creatingStage.value = false
  }
}

async function deleteStage() {
  if (!props.pipeline || !pendingDelete.value) return
  const stage = pendingDelete.value
  deleting.value = true
  stageError.value = ''
  try {
    await crmService.deletePipelineStage(props.pipeline.id, stage.id)
    stageDrafts.value = stageDrafts.value.filter((item) => item.id !== stage.id)
    pendingDelete.value = null
    emit('saved', props.pipeline.id)
  } catch (error) {
    stageError.value = getErrorMessage(error, 'Stage could not be deleted')
  } finally {
    deleting.value = false
  }
}

async function createPipeline() {
  if (!newPipeline.value.name.trim()) return
  creatingPipeline.value = true
  stageError.value = ''
  try {
    const response = await crmService.createPipeline({
      name: newPipeline.value.name.trim(),
      description: newPipeline.value.description.trim(),
      is_default: newPipeline.value.is_default,
      is_active: true,
      display_order: 0,
    })
    const created = unwrapItemResponse<Pipeline>(response)
    newPipeline.value = { name: '', description: '', is_default: false }
    showNewPipeline.value = false
    emit('saved', created.id)
  } catch (error) {
    stageError.value = getErrorMessage(error, 'Pipeline could not be created')
  } finally {
    creatingPipeline.value = false
  }
}
</script>

<template>
  <Dialog v-model:open="open">
    <DialogContent class="max-h-[92vh] w-[calc(100vw-1.5rem)] max-w-4xl overflow-y-auto border-white/10 bg-[#101318] p-0 text-white light:border-slate-200 light:bg-white light:text-slate-950">
      <DialogHeader class="border-b border-white/[0.08] px-5 py-5 pr-12 light:border-slate-200 sm:px-6">
        <div class="flex items-center gap-3">
          <span class="rounded-xl bg-cyan-300/10 p-2.5 text-cyan-200 light:text-cyan-700">
            <Layers3 class="h-5 w-5" />
          </span>
          <div>
            <DialogTitle>Configure pipeline</DialogTitle>
            <DialogDescription class="mt-1">
              Edit stage semantics carefully; lead outcomes follow the stage kind.
            </DialogDescription>
          </div>
        </div>
      </DialogHeader>

      <div class="space-y-5 px-4 py-5 sm:px-6">
        <div
          v-if="stageError"
          role="alert"
          class="flex items-start gap-2 rounded-xl border border-rose-300/20 bg-rose-300/[0.07] px-3 py-2.5 text-xs leading-5 text-rose-100 light:text-rose-800"
        >
          <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" />
          <span>{{ stageError }}</span>
        </div>

        <section v-if="pipeline" :aria-labelledby="`pipeline-stage-heading-${pipeline.id}`">
          <div class="flex flex-wrap items-end justify-between gap-3">
            <div>
              <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-cyan-300 light:text-cyan-700">Current pipeline</p>
              <h3 :id="`pipeline-stage-heading-${pipeline.id}`" class="mt-1 text-lg font-semibold">{{ pipeline.name }}</h3>
            </div>
            <Badge variant="outline">{{ stageDrafts.length }} stages</Badge>
          </div>

          <div class="mt-4 space-y-3">
            <form
              v-for="stage in stageDrafts"
              :key="stage.id"
              class="grid gap-3 rounded-2xl border border-white/[0.08] bg-white/[0.025] p-3 light:border-slate-200 light:bg-slate-50 md:grid-cols-[42px_minmax(150px,1fr)_110px_100px_100px_auto] md:items-end"
              @submit.prevent="saveStage(stage)"
            >
              <label class="block">
                <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Color</span>
                <input
                  v-model="stage.color"
                  type="color"
                  class="h-10 w-full cursor-pointer rounded-lg border border-white/10 bg-transparent p-1 light:border-slate-300"
                  :aria-label="`${stage.name} color`"
                />
              </label>
              <label class="block">
                <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Stage name</span>
                <input
                  v-model="stage.name"
                  required
                  maxlength="150"
                  class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white"
                />
              </label>
              <label class="block">
                <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Outcome</span>
                <select v-model="stage.kind" class="h-10 w-full rounded-lg border border-white/10 bg-[#15191f] px-2 text-xs outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white">
                  <option value="open">Open</option>
                  <option value="won">Won</option>
                  <option value="lost">Lost</option>
                </select>
              </label>
              <label class="block">
                <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Probability</span>
                <input v-model.number="stage.probability" type="number" min="0" max="100" required class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
              </label>
              <label class="block">
                <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">SLA hours</span>
                <input v-model.number="stage.sla_hours" type="number" min="0" max="87600" required class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
              </label>
              <div class="flex items-center justify-end gap-1">
                <Button type="submit" size="sm" class="gap-1.5" :disabled="savingStageId === stage.id">
                  <Loader2 v-if="savingStageId === stage.id" class="h-3.5 w-3.5 animate-spin" />
                  <Save v-else class="h-3.5 w-3.5" />
                  Save
                </Button>
                <Button
                  v-if="canDeleteStages"
                  type="button"
                  size="icon"
                  variant="ghost"
                  class="h-9 w-9 text-white/40 hover:text-rose-300 light:text-slate-500 light:hover:text-rose-700"
                  :disabled="stageDrafts.length <= 1"
                  :aria-label="`Delete ${stage.name} stage`"
                  :title="stageDrafts.length <= 1 ? 'A pipeline must keep at least one stage' : `Delete ${stage.name}`"
                  @click="pendingDelete = stage"
                >
                  <Trash2 class="h-4 w-4" />
                </Button>
              </div>
            </form>
          </div>

          <form class="mt-3 grid gap-3 rounded-2xl border border-dashed border-cyan-300/20 bg-cyan-300/[0.035] p-3 md:grid-cols-[42px_minmax(150px,1fr)_110px_100px_100px_auto] md:items-end" @submit.prevent="addStage">
            <label class="block">
              <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Color</span>
              <input v-model="newStage.color" type="color" aria-label="New stage color" class="h-10 w-full cursor-pointer rounded-lg border border-white/10 bg-transparent p-1 light:border-slate-300" />
            </label>
            <label class="block">
              <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">New stage</span>
              <input v-model="newStage.name" required maxlength="150" placeholder="e.g. Consultation" class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
            </label>
            <label class="block">
              <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Outcome</span>
              <select v-model="newStage.kind" class="h-10 w-full rounded-lg border border-white/10 bg-[#15191f] px-2 text-xs outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white">
                <option value="open">Open</option>
                <option value="won">Won</option>
                <option value="lost">Lost</option>
              </select>
            </label>
            <label class="block">
              <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">Probability</span>
              <input v-model.number="newStage.probability" type="number" min="0" max="100" required class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
            </label>
            <label class="block">
              <span class="mb-1 block text-[10px] text-white/40 light:text-slate-600">SLA hours</span>
              <input v-model.number="newStage.sla_hours" type="number" min="0" max="87600" required class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
            </label>
            <Button type="submit" variant="outline" class="gap-1.5" :disabled="creatingStage || !newStage.name.trim()">
              <Loader2 v-if="creatingStage" class="h-3.5 w-3.5 animate-spin" />
              <Plus v-else class="h-3.5 w-3.5" />
              Add stage
            </Button>
          </form>
        </section>

        <section class="rounded-2xl border border-white/[0.08] p-4 light:border-slate-200">
          <button type="button" class="flex w-full items-center justify-between rounded-lg text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300" :aria-expanded="showNewPipeline" @click="showNewPipeline = !showNewPipeline">
            <span>
              <span class="block text-sm font-semibold">Create another pipeline</span>
              <span class="mt-1 block text-xs text-white/40 light:text-slate-600">Starts with a safe open, won, and lost stage set.</span>
            </span>
            <Plus class="h-4 w-4 text-cyan-300 transition" :class="{ 'rotate-45': showNewPipeline }" />
          </button>
          <form v-if="showNewPipeline" class="mt-4 grid gap-3 md:grid-cols-2" @submit.prevent="createPipeline">
            <label class="block">
              <span class="mb-1 block text-xs text-white/50 light:text-slate-700">Pipeline name</span>
              <input v-model="newPipeline.name" required maxlength="150" class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
            </label>
            <label class="block">
              <span class="mb-1 block text-xs text-white/50 light:text-slate-700">Description</span>
              <input v-model="newPipeline.description" maxlength="4000" class="h-10 w-full rounded-lg border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" />
            </label>
            <label class="flex items-center gap-2 text-xs text-white/55 light:text-slate-700">
              <input v-model="newPipeline.is_default" type="checkbox" class="accent-cyan-400" />
              Make this the default pipeline
            </label>
            <Button type="submit" class="justify-self-stretch md:justify-self-end" :disabled="creatingPipeline || !newPipeline.name.trim()">
              <Loader2 v-if="creatingPipeline" class="mr-2 h-4 w-4 animate-spin" />
              Create pipeline
            </Button>
          </form>
        </section>
      </div>

      <DialogFooter class="border-t border-white/[0.08] px-5 py-4 light:border-slate-200 sm:px-6">
        <Button variant="outline" @click="open = false">Done</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <AlertDialog :open="Boolean(pendingDelete)" @update:open="(value) => !value && (pendingDelete = null)">
    <AlertDialogContent>
      <AlertDialogHeader>
        <AlertDialogTitle>Delete {{ pendingDelete?.name }}?</AlertDialogTitle>
        <AlertDialogDescription>
          This works only when no leads remain in the stage. The server will preserve the stage if it is still in use.
        </AlertDialogDescription>
      </AlertDialogHeader>
      <AlertDialogFooter>
        <AlertDialogCancel :disabled="deleting">Keep stage</AlertDialogCancel>
        <AlertDialogAction class="bg-rose-600 text-white hover:bg-rose-500" :disabled="deleting" @click="deleteStage">
          <Loader2 v-if="deleting" class="mr-2 h-4 w-4 animate-spin" />
          Delete stage
        </AlertDialogAction>
      </AlertDialogFooter>
    </AlertDialogContent>
  </AlertDialog>
</template>
