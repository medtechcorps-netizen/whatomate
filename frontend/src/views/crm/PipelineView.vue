<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import draggable from 'vuedraggable'
import {
  ArrowRight,
  CalendarClock,
  Check,
  CircleDollarSign,
  Filter,
  GripVertical,
  Loader2,
  Plus,
  RefreshCw,
  Route,
  UserRound,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import ContactPicker from '@/components/shared/ContactPicker.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapListResponse } from '@/lib/api-utils'
import {
  crmService,
  type CRMLead,
  type FollowUpTask,
  type Pipeline,
  type PipelineStage,
} from '@/services/productSuite'

type BoardColumn = PipelineStage & { leads: CRMLead[] }

const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const saving = ref(false)
const pipelines = ref<Pipeline[]>([])
const selectedPipelineId = ref('')
const columns = ref<BoardColumn[]>([])
const tasks = ref<FollowUpTask[]>([])
const showCreate = ref(false)
const newLead = reactive({
  title: '',
  contact_id: '',
  value: '',
  currency: 'MYR',
})
const canReadTasks = computed(() => authStore.hasPermission('tasks', 'read'))
const canWriteTasks = computed(() => authStore.hasPermission('tasks', 'write'))
const canWriteLeads = computed(() => authStore.hasPermission('crm.leads', 'write'))
const canReadContacts = computed(() => authStore.hasPermission('contacts', 'read'))
const canCreateLeads = computed(() => canWriteLeads.value && canReadContacts.value)

const selectedPipeline = computed(() => pipelines.value.find((pipeline) => pipeline.id === selectedPipelineId.value))
const openLeadCount = computed(() =>
  columns.value.reduce(
    (total, column) => total + column.leads.filter((lead) => lead.status === 'open').length,
    0,
  ),
)
const openValues = computed(() =>
  currencyTotals(columns.value.flatMap((column) => column.leads).filter((lead) => lead.status === 'open')),
)
const overdueTasks = computed(() => {
  const now = Date.now()
  return tasks.value.filter((task) => task.status !== 'completed' && task.due_at && new Date(task.due_at).getTime() < now)
})

function rebuildBoard(leads: CRMLead[]) {
  const stages = selectedPipeline.value?.stages ?? []
  columns.value = [...stages]
    .sort((a, b) => a.display_order - b.display_order)
    .map((stage) => ({
      ...stage,
      leads: leads.filter((lead) => lead.stage_id === stage.id),
    }))
}

async function load() {
  loading.value = true
  try {
    const [pipelineResponse, taskResponse] = await Promise.all([
      crmService.pipelines(),
      canReadTasks.value
        ? crmService.allTasks({ status: 'open' })
        : Promise.resolve(null),
    ])
    pipelines.value = unwrapListResponse<Pipeline>(pipelineResponse, 'pipelines')
    tasks.value = taskResponse ?? []

    if (!selectedPipelineId.value) {
      selectedPipelineId.value = pipelines.value.find((item) => item.is_default)?.id ?? pipelines.value[0]?.id ?? ''
    }
    await loadLeads()
  } catch (error) {
    toast.error('Pipeline could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function loadLeads() {
  if (!selectedPipelineId.value) {
    columns.value = []
    return
  }
  const leads = await crmService.allLeads({ pipeline_id: selectedPipelineId.value })
  rebuildBoard(leads)
}

async function changePipeline() {
  loading.value = true
  try {
    await loadLeads()
  } finally {
    loading.value = false
  }
}

async function handleMove(event: any, stage: BoardColumn) {
  const lead = event?.added?.element as CRMLead | undefined
  if (!lead || lead.stage_id === stage.id) return

  const previousStage = lead.stage_id
  lead.stage_id = stage.id
  try {
    const response = await crmService.moveLead(lead.id, stage.id, lead.version)
    const updated = response.data?.data ?? response.data
    lead.version = updated?.version ?? lead.version + 1
    lead.status = updated?.status ?? lead.status
    toast.success(`Moved to ${stage.name}`)
  } catch (error) {
    lead.stage_id = previousStage
    toast.error('Lead was not moved', getErrorMessage(error))
    await loadLeads()
  }
}

async function createLead() {
  const firstOpenStage = columns.value.find((stage) => stage.kind === 'open')
  if (!newLead.title.trim() || !newLead.contact_id.trim() || !firstOpenStage) {
    toast.warning('Title, customer and an open stage are required')
    return
  }
  saving.value = true
  try {
    await crmService.createLead({
      title: newLead.title.trim(),
      contact_id: newLead.contact_id.trim(),
      pipeline_id: selectedPipelineId.value,
      stage_id: firstOpenStage.id,
      status: 'open',
      value_minor: Math.round(Number(newLead.value || 0) * 100),
      currency: newLead.currency,
    })
    newLead.title = ''
    newLead.contact_id = ''
    newLead.value = ''
    showCreate.value = false
    toast.success('Lead created')
    await loadLeads()
  } catch (error) {
    toast.error('Lead was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function completeTask(task: FollowUpTask) {
  try {
    await crmService.completeTask(task.id, task.version)
    task.status = 'completed'
    toast.success('Follow-up completed')
  } catch (error) {
    toast.error('Task was not completed', getErrorMessage(error))
  }
}

function formatMoney(amountMinor: number, currency = 'MYR') {
  return new Intl.NumberFormat('en-MY', {
    style: 'currency',
    currency,
    maximumFractionDigits: 0,
  }).format(amountMinor / 100)
}

function currencyTotals(leads: CRMLead[]) {
  const totals = new Map<string, number>()
  for (const lead of leads) {
    const currency = lead.currency || 'MYR'
    totals.set(currency, (totals.get(currency) ?? 0) + lead.value_minor)
  }
  return [...totals.entries()].sort(([left], [right]) => left.localeCompare(right))
}

function formatMoneyTotals(totals: Array<[string, number]>) {
  if (!totals.length) return '—'
  return totals.map(([currency, amount]) => formatMoney(amount, currency)).join(' · ')
}

function formatDue(value?: string) {
  if (!value) return 'No due date'
  return new Intl.DateTimeFormat('en-MY', { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' }).format(
    new Date(value),
  )
}

onMounted(load)
</script>

<template>
  <div class="flex h-full flex-col bg-[#08090a] light:bg-[#f6f7f8]">
    <PageHeader
      title="Revenue pipeline"
      description="Move every inquiry from first reply to a booked, retained customer."
      :icon="Route"
      icon-gradient="bg-gradient-to-br from-cyan-400 to-blue-600 shadow-cyan-500/20"
    >
      <template #actions>
        <div class="flex items-center gap-2">
          <select
            v-model="selectedPipelineId"
            class="h-9 rounded-md border border-white/10 bg-white/[0.04] px-3 text-sm text-white outline-none light:border-gray-200 light:bg-white light:text-gray-900"
            @change="changePipeline"
          >
            <option v-for="pipeline in pipelines" :key="pipeline.id" :value="pipeline.id">
              {{ pipeline.name }}
            </option>
          </select>
          <Button variant="outline" size="icon" title="Refresh pipeline" @click="load">
            <RefreshCw class="h-4 w-4" />
          </Button>
          <Button v-if="canCreateLeads" class="bg-cyan-400 text-black hover:bg-cyan-300" @click="showCreate = !showCreate">
            <Plus class="mr-2 h-4 w-4" />
            New lead
          </Button>
        </div>
      </template>
    </PageHeader>

    <div v-if="loading" class="flex flex-1 items-center justify-center">
      <Loader2 class="h-6 w-6 animate-spin text-cyan-300" />
    </div>

    <div v-else class="flex min-h-0 flex-1 flex-col">
      <div class="grid grid-cols-2 gap-px border-b border-white/[0.08] bg-white/[0.08] light:border-gray-200 light:bg-gray-200 md:grid-cols-4">
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-white">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-gray-500">Open leads</p>
          <p class="mt-1 text-xl font-semibold text-white light:text-gray-900">{{ openLeadCount }}</p>
        </div>
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-white">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-gray-500">Pipeline value</p>
          <p class="mt-1 text-base font-semibold text-white light:text-gray-900">{{ formatMoneyTotals(openValues) }}</p>
        </div>
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-white">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-gray-500">Open follow-ups</p>
          <p class="mt-1 text-xl font-semibold text-white light:text-gray-900">{{ tasks.filter((task) => task.status !== 'completed').length }}</p>
        </div>
        <div class="bg-[#0d0f10] px-5 py-3 light:bg-white">
          <p class="text-[10px] uppercase tracking-[0.18em] text-white/35 light:text-gray-500">Overdue</p>
          <p class="mt-1 text-xl font-semibold" :class="overdueTasks.length ? 'text-amber-300' : 'text-emerald-300'">
            {{ overdueTasks.length }}
          </p>
        </div>
      </div>

      <div
      v-if="showCreate && canCreateLeads"
        class="grid gap-3 border-b border-cyan-400/20 bg-cyan-400/[0.045] px-5 py-4 md:grid-cols-[1.2fr_1fr_.65fr_.45fr_auto]"
      >
        <Input v-model="newLead.title" placeholder="Lead title" />
        <ContactPicker v-model="newLead.contact_id" placeholder="Search customer" />
        <Input v-model="newLead.value" type="number" min="0" step="0.01" placeholder="Value" />
        <select
          v-model="newLead.currency"
          class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white light:border-gray-200 light:bg-white light:text-gray-900"
        >
          <option value="MYR">MYR</option>
          <option value="SGD">SGD</option>
          <option value="USD">USD</option>
        </select>
        <Button :disabled="saving" class="bg-cyan-400 text-black hover:bg-cyan-300" @click="createLead">
          <Loader2 v-if="saving" class="mr-2 h-4 w-4 animate-spin" />
          Create
        </Button>
      </div>

      <div class="grid min-h-0 flex-1 xl:grid-cols-[1fr_310px]">
        <div class="min-w-0 overflow-x-auto p-4 md:p-5">
          <div class="flex h-full min-w-max gap-3">
            <section
              v-for="column in columns"
              :key="column.id"
              class="flex w-[296px] shrink-0 flex-col overflow-hidden rounded-2xl border border-white/[0.08] bg-white/[0.02] light:border-gray-200 light:bg-white"
            >
              <header class="border-b border-white/[0.07] px-4 py-3 light:border-gray-100">
                <div class="flex items-center justify-between">
                  <div class="flex items-center gap-2">
                    <span class="h-2.5 w-2.5 rounded-full" :style="{ backgroundColor: column.color || '#67e8f9' }" />
                    <h3 class="text-sm font-semibold text-white light:text-gray-900">{{ column.name }}</h3>
                  </div>
                  <Badge variant="secondary" class="h-5 min-w-5 justify-center px-1.5 text-[10px]">{{ column.leads.length }}</Badge>
                </div>
                <div class="mt-2 flex items-center justify-between text-[11px] text-white/35 light:text-gray-500">
                  <span>{{ column.probability }}% probability</span>
                  <span>{{ formatMoneyTotals(currencyTotals(column.leads)) }}</span>
                </div>
              </header>

              <draggable
                v-model="column.leads"
                item-key="id"
                group="pipeline-leads"
                :disabled="!canWriteLeads"
                class="min-h-28 flex-1 space-y-2 overflow-y-auto p-2.5"
                ghost-class="opacity-30"
                drag-class="rotate-1"
                @change="handleMove($event, column)"
              >
                <template #item="{ element: lead }">
                  <article
                    class="cursor-grab rounded-xl border border-white/[0.07] bg-[#121416] p-3.5 shadow-lg shadow-black/10 transition hover:border-cyan-300/20 active:cursor-grabbing light:border-gray-200 light:bg-gray-50"
                  >
                    <div class="flex items-start gap-2">
                      <GripVertical class="mt-0.5 h-4 w-4 shrink-0 text-white/20 light:text-gray-300" />
                      <div class="min-w-0 flex-1">
                        <p class="line-clamp-2 text-sm font-medium leading-5 text-white light:text-gray-900">{{ lead.title }}</p>
                        <div class="mt-2 flex items-center gap-1.5 text-[11px] text-white/40 light:text-gray-500">
                          <UserRound class="h-3 w-3" />
                          <span class="truncate">{{ lead.contact?.profile_name || lead.contact?.phone_number || 'Contact' }}</span>
                        </div>
                      </div>
                    </div>
                    <div class="mt-3 flex items-center justify-between border-t border-white/[0.06] pt-2.5 light:border-gray-200">
                      <span class="flex items-center gap-1 text-xs font-medium text-emerald-300">
                        <CircleDollarSign class="h-3.5 w-3.5" />
                        {{ formatMoney(lead.value_minor, lead.currency) }}
                      </span>
                      <span v-if="lead.next_action_at" class="flex items-center gap-1 text-[10px] text-white/35 light:text-gray-500">
                        <CalendarClock class="h-3 w-3" />
                        {{ formatDue(lead.next_action_at) }}
                      </span>
                    </div>
                  </article>
                </template>
              </draggable>
            </section>
          </div>
        </div>

        <aside class="hidden overflow-y-auto border-l border-white/[0.08] bg-[#0b0c0d] p-4 light:border-gray-200 light:bg-white xl:block">
          <div class="mb-4 flex items-center justify-between">
            <div>
              <p class="text-xs font-semibold uppercase tracking-[0.16em] text-white/45 light:text-gray-500">Follow-up rail</p>
              <h3 class="mt-1 font-semibold text-white light:text-gray-900">What needs attention</h3>
            </div>
            <Filter class="h-4 w-4 text-white/30 light:text-gray-400" />
          </div>
          <div class="space-y-2">
            <article
              v-for="task in tasks.filter((item) => item.status !== 'completed')"
              :key="task.id"
              class="rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
            >
              <div class="flex items-start gap-2">
                <button
                  v-if="canWriteTasks"
                  class="mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full border border-white/15 text-transparent hover:border-emerald-300 hover:text-emerald-300 light:border-gray-300"
                  title="Complete task"
                  @click="completeTask(task)"
                >
                  <Check class="h-3 w-3" />
                </button>
                <div class="min-w-0">
                  <p class="text-xs font-medium leading-5 text-white light:text-gray-900">{{ task.title }}</p>
                  <p
                    class="mt-1 text-[10px]"
                    :class="
                      task.due_at && new Date(task.due_at).getTime() < Date.now()
                        ? 'text-amber-300'
                        : 'text-white/35 light:text-gray-500'
                    "
                  >
                    {{ formatDue(task.due_at) }}
                  </p>
                </div>
              </div>
            </article>
            <div v-if="!tasks.some((item) => item.status !== 'completed')" class="rounded-xl bg-emerald-400/[0.06] p-5 text-center">
              <Check class="mx-auto h-5 w-5 text-emerald-300" />
              <p class="mt-2 text-xs text-emerald-300">Follow-up queue is clear</p>
            </div>
          </div>
          <RouterLink v-if="canReadTasks" to="/crm/tasks">
            <Button variant="ghost" class="mt-4 w-full justify-between text-white/55 light:text-gray-600">
              View all tasks
              <ArrowRight class="h-4 w-4" />
            </Button>
          </RouterLink>
        </aside>
      </div>
    </div>
  </div>
</template>
