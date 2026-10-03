<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  CalendarClock,
  Check,
  AlertCircle,
  Clock3,
  Loader2,
  MessageSquare,
  Pencil,
  Plus,
  RefreshCw,
  Target,
  User,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import TaskEditDialog from '@/components/crm/TaskEditDialog.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { useAppToast } from '@/composables/useAppToast'
import { getErrorMessage } from '@/lib/api-utils'
import { contactDisplayName } from '@/lib/contactAddress'
import { isFollowUpDueToday, isFollowUpOverdue } from '@/lib/crmFlow'
import { crmService, type FollowUpTask } from '@/services/productSuite'
import { useAuthStore } from '@/stores/auth'

type TaskFilter = 'open' | 'today' | 'overdue' | 'completed'

const TASK_FILTERS: Array<{ key: TaskFilter; label: string }> = [
  { key: 'open', label: 'Open' },
  { key: 'today', label: 'Today' },
  { key: 'overdue', label: 'Overdue' },
  { key: 'completed', label: 'Completed' },
]

const toast = useAppToast()
const authStore = useAuthStore()
const loading = ref(true)
const saving = ref(false)
const activeFilter = ref<TaskFilter>('open')
const tasks = ref<FollowUpTask[]>([])
const taskEditorOpen = ref(false)
const editingTask = ref<FollowUpTask | null>(null)
const draft = ref({
  title: '',
  description: '',
  priority: 'normal' as FollowUpTask['priority'],
  due_at: '',
})

const completingTaskId = ref<string | null>(null)

// Same calendar-day rule as the chat panel and pipeline (see crmFlow):
// overdue = due before today; due today = any time today, even if that time has passed.
const isTerminal = (status: FollowUpTask['status']) => ['completed', 'cancelled'].includes(status)
const isOverdue = (task: FollowUpTask) => !isTerminal(task.status) && isFollowUpOverdue(task.due_at)
const isDueToday = (task: FollowUpTask) => !isTerminal(task.status) && isFollowUpDueToday(task.due_at)

const visibleTasks = computed(() => {
  if (activeFilter.value === 'completed') return tasks.value.filter((task) => task.status === 'completed')
  if (activeFilter.value === 'today') return tasks.value.filter(isDueToday)
  if (activeFilter.value === 'overdue') return tasks.value.filter(isOverdue)
  return tasks.value.filter((task) => !isTerminal(task.status))
})

const openCount = computed(() => tasks.value.filter((task) => !isTerminal(task.status)).length)
const todayCount = computed(() => tasks.value.filter(isDueToday).length)
const overdueCount = computed(() => tasks.value.filter(isOverdue).length)
const metrics = computed(() => [
  { key: 'open' as const, label: 'Open', value: openCount.value, tone: 'text-violet-300 light:text-violet-700' },
  { key: 'today' as const, label: 'Due today', value: todayCount.value, tone: 'text-sky-300 light:text-sky-700' },
  { key: 'overdue' as const, label: 'Overdue', value: overdueCount.value, tone: 'text-rose-300 light:text-rose-700' },
])
const canWriteTasks = computed(() => authStore.hasPermission('tasks', 'write'))
const canOpenChat = computed(() => authStore.hasPermission('chat', 'read'))
const canOpenLead = computed(() => authStore.hasPermission('crm.leads', 'read'))

const emptyMessages = computed<Record<TaskFilter, { title: string; body: string }>>(() => ({
  open: {
    title: 'No open follow-ups',
    // Only staff who can add follow-ups are pointed at the form.
    body: canWriteTasks.value
      ? "Use Add a follow-up, or schedule one from a customer's chat."
      : 'Follow-ups your team schedules will show here.',
  },
  today: { title: 'Nothing due today', body: 'Follow-ups due today will show here.' },
  overdue: { title: 'Nothing overdue', body: 'Every follow-up is on time.' },
  completed: {
    title: 'No completed follow-ups yet',
    body: canWriteTasks.value
      ? 'Tick a follow-up when it is done and it moves here.'
      : 'Follow-ups your team finishes will show here.',
  },
}))

// GET /api/tasks returns the linked customer and lead with each follow-up.
function customerName(task: FollowUpTask): string {
  const contact = task.contact
  if (!contact) return task.contact_id ? 'Linked customer' : ''
  return contactDisplayName(contact)
}

function leadTitle(task: FollowUpTask): string {
  if (task.lead?.title?.trim()) return task.lead.title.trim()
  return task.lead_id ? 'Linked lead' : ''
}

function chatLink(task: FollowUpTask) {
  const contactId = task.contact?.id || task.contact_id
  return contactId ? `/chat/${encodeURIComponent(contactId)}` : null
}

function leadLink(task: FollowUpTask) {
  const leadId = task.lead?.id || task.lead_id
  if (!leadId) return null
  const pipelineId = task.lead?.pipeline_id
  const query: Record<string, string> = pipelineId ? { pipeline: pipelineId, lead: leadId } : { lead: leadId }
  return { path: '/crm/pipeline', query }
}

function formatDue(value?: string) {
  if (!value) return 'No deadline'
  return new Intl.DateTimeFormat('en-MY', {
    weekday: 'short',
    day: 'numeric',
    month: 'short',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(value))
}

async function load() {
  loading.value = true
  try {
    tasks.value = await crmService.allTasks()
  } catch (error) {
    toast.error('Follow-ups could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

async function createTask() {
  if (!draft.value.title.trim()) return
  saving.value = true
  try {
    await crmService.createTask({
      title: draft.value.title.trim(),
      description: draft.value.description.trim(),
      priority: draft.value.priority,
      due_at: draft.value.due_at ? new Date(draft.value.due_at).toISOString() : undefined,
    })
    draft.value = { title: '', description: '', priority: 'normal', due_at: '' }
    toast.success('Follow-up scheduled')
    await load()
  } catch (error) {
    toast.error('Follow-up was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

async function complete(task: FollowUpTask) {
  // One request at a time: repeat clicks while it runs are ignored.
  if (completingTaskId.value || task.status === 'completed') return
  completingTaskId.value = task.id
  try {
    await crmService.completeTask(task.id, task.version)
    toast.success('Follow-up completed')
    await load()
  } catch (error) {
    toast.error('Follow-up was not updated', getErrorMessage(error))
  } finally {
    completingTaskId.value = null
  }
}

function openTaskEditor(task: FollowUpTask) {
  editingTask.value = task
  taskEditorOpen.value = true
}

async function handleTaskSaved() {
  toast.success('Follow-up updated')
  await load()
}

onMounted(load)
</script>

<template>
  <div class="h-full overflow-y-auto bg-[#090b0e] light:bg-[#f5f6f3]">
    <PageHeader
      title="Follow-ups"
      description="Every call or message your team owes a customer, in one list."
      :icon="CalendarClock"
      icon-gradient="bg-gradient-to-br from-violet-500 to-fuchsia-600 shadow-violet-500/20"
    >
      <template #actions>
        <Button variant="outline" size="sm" class="gap-2" :disabled="loading" @click="load">
          <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
          Refresh
        </Button>
      </template>
    </PageHeader>

    <main
      class="mx-auto grid max-w-[1480px] gap-6 p-5 md:p-7"
      :class="canWriteTasks ? 'xl:grid-cols-[minmax(0,1fr)_390px]' : 'xl:grid-cols-1'"
    >
      <section class="min-w-0">
        <div class="mb-5 grid gap-3 sm:grid-cols-3">
          <button
            v-for="metric in metrics"
            :key="metric.key"
            type="button"
            data-testid="task-metric"
            class="rounded-2xl border p-4 text-left transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300"
            :class="activeFilter === metric.key
              ? 'border-violet-300/40 bg-violet-400/[0.08] light:border-violet-300 light:bg-violet-50'
              : 'border-white/[0.08] bg-white/[0.03] hover:border-white/20 light:border-black/10 light:bg-white light:hover:border-black/20'"
            :aria-pressed="activeFilter === metric.key"
            @click="activeFilter = metric.key"
          >
            <p class="text-[10px] font-semibold uppercase tracking-[0.2em] text-white/35 light:text-gray-500">
              {{ metric.label }}
            </p>
            <p class="mt-2 text-3xl font-semibold tracking-tight" :class="metric.tone">{{ metric.value }}</p>
          </button>
        </div>

        <div class="overflow-hidden rounded-2xl border border-white/[0.08] bg-[#111419] light:border-black/10 light:bg-white">
          <div class="flex flex-wrap items-center justify-between gap-3 border-b border-white/[0.07] px-5 py-4 light:border-black/10">
            <div>
              <p class="font-semibold text-white light:text-gray-900">Team follow-ups</p>
              <p class="mt-0.5 text-xs text-white/40 light:text-gray-500">Tick a follow-up when it is done.</p>
            </div>
            <div class="flex flex-wrap rounded-lg bg-black/20 p-1 light:bg-gray-100">
              <button
                v-for="filter in TASK_FILTERS"
                :key="filter.key"
                type="button"
                class="rounded-md px-3 py-1.5 text-xs font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300"
                :class="activeFilter === filter.key ? 'bg-white text-gray-950 shadow-sm' : 'text-white/45 hover:text-white light:text-gray-500 light:hover:text-gray-900'"
                :aria-pressed="activeFilter === filter.key"
                @click="activeFilter = filter.key"
              >
                {{ filter.label }}
              </button>
            </div>
          </div>

          <div v-if="loading" class="flex h-72 items-center justify-center">
            <Loader2 class="h-6 w-6 animate-spin text-violet-300" />
          </div>
          <div v-else-if="!visibleTasks.length" class="flex h-72 flex-col items-center justify-center px-6 text-center">
            <div class="rounded-2xl bg-emerald-400/10 p-4 text-emerald-300">
              <Check class="h-7 w-7" />
            </div>
            <p class="mt-4 font-medium text-white light:text-gray-900">{{ emptyMessages[activeFilter].title }}</p>
            <p class="mt-1 max-w-sm text-sm text-white/40 light:text-gray-500">
              {{ emptyMessages[activeFilter].body }}
            </p>
          </div>
          <div v-else class="divide-y divide-white/[0.06] light:divide-black/[0.06]">
            <article
              v-for="task in visibleTasks"
              :key="task.id"
              data-testid="follow-up-row"
              class="group flex gap-4 px-5 py-4"
            >
              <button
                v-if="canWriteTasks"
                type="button"
                data-testid="follow-up-complete"
                class="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full border transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300 disabled:cursor-default"
                :class="task.status === 'completed'
                  ? 'border-emerald-400/30 bg-emerald-400/15 text-emerald-300'
                  : completingTaskId === task.id
                    ? 'border-emerald-300 text-emerald-300'
                    : 'border-white/15 text-transparent hover:border-emerald-300 hover:text-emerald-300 light:border-black/20'"
                :disabled="task.status === 'completed' || completingTaskId === task.id"
                :aria-busy="completingTaskId === task.id ? 'true' : undefined"
                :aria-label="`Mark ${task.title} as done`"
                @click="complete(task)"
              >
                <Loader2 v-if="completingTaskId === task.id" class="h-3.5 w-3.5 animate-spin" />
                <Check v-else class="h-3.5 w-3.5" />
              </button>
              <div
                v-else
                class="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full border"
                :class="task.status === 'completed'
                  ? 'border-emerald-400/30 bg-emerald-400/15 text-emerald-300'
                  : 'border-white/15 text-transparent light:border-black/20'"
                aria-hidden="true"
              >
                <Check class="h-3.5 w-3.5" />
              </div>
              <div class="min-w-0 flex-1">
                <div class="flex flex-wrap items-start justify-between gap-2">
                  <div>
                    <p
                      class="text-sm font-medium text-white light:text-gray-900"
                      :class="{ 'line-through opacity-45': task.status === 'completed' }"
                    >
                      {{ task.title }}
                    </p>
                    <p v-if="task.description" class="mt-1 line-clamp-2 text-xs leading-5 text-white/40 light:text-gray-500">
                      {{ task.description }}
                    </p>
                  </div>
                  <div class="flex items-center gap-1.5">
                    <Badge variant="outline" class="capitalize" :class="{ 'border-rose-400/30 text-rose-300': task.priority === 'urgent' }">
                      {{ task.priority }}
                    </Badge>
                    <Button
                      v-if="canWriteTasks"
                      type="button"
                      variant="ghost"
                      size="icon"
                      class="h-8 w-8 text-white/35 hover:text-violet-200 light:text-gray-500 light:hover:text-violet-700"
                      :aria-label="`Edit ${task.title}`"
                      @click="openTaskEditor(task)"
                    >
                      <Pencil class="h-3.5 w-3.5" />
                    </Button>
                  </div>
                </div>
                <div class="mt-3 flex flex-wrap items-center gap-3 text-[11px] text-white/35 light:text-gray-500">
                  <span class="flex items-center gap-1.5" :class="{ 'text-rose-300': isOverdue(task) }">
                    <AlertCircle v-if="isOverdue(task)" class="h-3.5 w-3.5" />
                    <Clock3 v-else class="h-3.5 w-3.5" />
                    {{ isOverdue(task) ? 'Overdue · ' : '' }}{{ formatDue(task.due_at) }}
                  </span>
                  <span
                    v-if="customerName(task)"
                    data-testid="follow-up-customer"
                    class="flex min-w-0 items-center gap-1.5 text-white/60 light:text-gray-700"
                  >
                    <User class="h-3.5 w-3.5 shrink-0" />
                    <span class="truncate">{{ customerName(task) }}</span>
                  </span>
                  <span
                    v-if="leadTitle(task)"
                    data-testid="follow-up-lead"
                    class="flex min-w-0 items-center gap-1.5 text-white/60 light:text-gray-700"
                  >
                    <Target class="h-3.5 w-3.5 shrink-0" />
                    <span class="truncate">Lead: {{ leadTitle(task) }}</span>
                  </span>
                </div>
                <div
                  v-if="(canOpenChat && chatLink(task)) || (canOpenLead && leadLink(task))"
                  class="mt-2 flex flex-wrap items-center gap-2"
                >
                  <RouterLink
                    v-if="canOpenChat && chatLink(task)"
                    :to="chatLink(task)!"
                    data-testid="follow-up-open-chat"
                    class="inline-flex min-h-8 items-center gap-1.5 rounded-md border border-white/10 px-2.5 text-xs font-medium text-white/70 transition hover:border-violet-300/40 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-black/10 light:text-gray-700 light:hover:text-gray-950"
                  >
                    <MessageSquare class="h-3.5 w-3.5" />
                    Open chat
                  </RouterLink>
                  <RouterLink
                    v-if="canOpenLead && leadLink(task)"
                    :to="leadLink(task)!"
                    data-testid="follow-up-open-lead"
                    class="inline-flex min-h-8 items-center gap-1.5 rounded-md border border-white/10 px-2.5 text-xs font-medium text-white/70 transition hover:border-violet-300/40 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-black/10 light:text-gray-700 light:hover:text-gray-950"
                  >
                    <Target class="h-3.5 w-3.5" />
                    Open lead
                  </RouterLink>
                </div>
              </div>
            </article>
          </div>
        </div>
      </section>

      <aside
        v-if="canWriteTasks"
        class="h-fit rounded-[24px] border border-violet-300/15 bg-gradient-to-b from-violet-400/[0.09] to-white/[0.025] p-5 light:border-violet-200 light:from-violet-50 light:to-white"
      >
        <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-violet-300 light:text-violet-700">New follow-up</p>
        <h2 class="mt-2 text-xl font-semibold tracking-tight text-white light:text-gray-950">Add a follow-up</h2>
        <p class="mt-2 text-xs leading-5 text-white/45 light:text-gray-600">
          Follow-ups added from a customer's chat are linked to that customer and lead automatically.
        </p>

        <form class="mt-6 space-y-4" @submit.prevent="createTask">
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">What needs to happen?</span>
            <input
              v-model="draft.title"
              required
              maxlength="255"
              placeholder="Call after first consultation"
              class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm text-white outline-none transition placeholder:text-white/25 focus:border-violet-300/50 light:border-gray-200 light:bg-white light:text-gray-900"
            />
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Notes</span>
            <textarea
              v-model="draft.description"
              rows="3"
              maxlength="2000"
              placeholder="Useful context for the teammate handling this…"
              class="w-full resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none transition placeholder:text-white/25 focus:border-violet-300/50 light:border-gray-200 light:bg-white light:text-gray-900"
            />
          </label>
          <div class="grid grid-cols-2 gap-3">
            <label>
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Priority</span>
              <select
                v-model="draft.priority"
                class="h-11 w-full rounded-xl border border-white/10 bg-[#15171d] px-3 text-sm text-white outline-none focus:border-violet-300/50 light:border-gray-200 light:bg-white light:text-gray-900"
              >
                <option value="low">Low</option>
                <option value="normal">Normal</option>
                <option value="high">High</option>
                <option value="urgent">Urgent</option>
              </select>
            </label>
            <label>
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-gray-700">Due</span>
              <input
                v-model="draft.due_at"
                type="datetime-local"
                class="h-11 w-full rounded-xl border border-white/10 bg-[#15171d] px-3 text-xs text-white outline-none focus:border-violet-300/50 light:border-gray-200 light:bg-white light:text-gray-900"
              />
            </label>
          </div>
          <Button type="submit" class="h-11 w-full gap-2 bg-violet-500 text-white hover:bg-violet-400" :disabled="saving || !draft.title.trim()">
            <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
            <Plus v-else class="h-4 w-4" />
            Schedule follow-up
          </Button>
        </form>
      </aside>
    </main>

    <TaskEditDialog
      v-model="taskEditorOpen"
      :task="editingTask"
      @saved="handleTaskSaved"
    />
  </div>
</template>
