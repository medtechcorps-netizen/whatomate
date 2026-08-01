<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Ban, Loader2, Save } from 'lucide-vue-next'
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
import { getErrorMessage } from '@/lib/api-utils'
import { crmService, type FollowUpTask } from '@/services/productSuite'

const props = defineProps<{
  modelValue: boolean
  task: FollowUpTask | null
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  saved: []
}>()

const saving = ref(false)
const cancelling = ref(false)
const cancelOpen = ref(false)
const errorMessage = ref('')
const draft = ref({
  title: '',
  description: '',
  priority: 'normal' as FollowUpTask['priority'],
  status: 'open' as 'open' | 'in_progress',
  due_at: '',
})

const open = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value),
})

const isActive = computed(() => Boolean(props.task && !['completed', 'cancelled'].includes(props.task.status)))

function toLocalDateTime(value?: string) {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 16)
}

function resetDraft() {
  if (!props.task) return
  draft.value = {
    title: props.task.title,
    description: props.task.description ?? '',
    priority: props.task.priority,
    status: props.task.status === 'in_progress' ? 'in_progress' : 'open',
    due_at: toLocalDateTime(props.task.due_at),
  }
  errorMessage.value = ''
  cancelOpen.value = false
}

watch(
  [() => props.modelValue, () => props.task],
  ([isOpen]) => {
    if (isOpen) resetDraft()
  },
)

async function save() {
  if (!props.task || !draft.value.title.trim()) return
  saving.value = true
  errorMessage.value = ''
  try {
    await crmService.updateTask(props.task.id, {
      version: props.task.version,
      title: draft.value.title.trim(),
      description: draft.value.description.trim(),
      priority: draft.value.priority,
      status: isActive.value ? draft.value.status : undefined,
      due_at: draft.value.due_at ? new Date(draft.value.due_at).toISOString() : undefined,
      clear_due_at: !draft.value.due_at,
    })
    emit('saved')
    open.value = false
  } catch (error) {
    errorMessage.value = getErrorMessage(error, 'Follow-up could not be saved')
  } finally {
    saving.value = false
  }
}

async function cancelTask() {
  if (!props.task || !isActive.value) return
  cancelling.value = true
  errorMessage.value = ''
  try {
    await crmService.updateTask(props.task.id, {
      version: props.task.version,
      status: 'cancelled',
    })
    cancelOpen.value = false
    emit('saved')
    open.value = false
  } catch (error) {
    errorMessage.value = getErrorMessage(error, 'Follow-up could not be cancelled')
  } finally {
    cancelling.value = false
  }
}
</script>

<template>
  <Dialog v-model:open="open">
    <DialogContent class="w-[calc(100vw-1.5rem)] max-w-xl border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950">
      <DialogHeader>
        <DialogTitle>Edit follow-up</DialogTitle>
        <DialogDescription>
          Keep the next action current. Every save is protected by the task version you opened.
        </DialogDescription>
      </DialogHeader>

      <form v-if="task" id="task-edit-form" class="space-y-4" @submit.prevent="save">
        <div
          v-if="errorMessage"
          role="alert"
          class="flex items-start gap-2 rounded-xl border border-rose-300/20 bg-rose-300/[0.07] p-3 text-xs leading-5 text-rose-100 light:text-rose-800"
        >
          <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" />
          <span>{{ errorMessage }}</span>
        </div>

        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Task</span>
          <input
            v-model="draft.title"
            required
            maxlength="255"
            autofocus
            class="h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-slate-300 light:bg-white"
          />
        </label>
        <label class="block">
          <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Notes</span>
          <textarea
            v-model="draft.description"
            rows="4"
            maxlength="5000"
            class="w-full resize-y rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-slate-300 light:bg-white"
          />
        </label>
        <div class="grid gap-3 sm:grid-cols-3">
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Priority</span>
            <select v-model="draft.priority" class="h-11 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-slate-300 light:bg-white">
              <option value="low">Low</option>
              <option value="normal">Normal</option>
              <option value="high">High</option>
              <option value="urgent">Urgent</option>
            </select>
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">State</span>
            <select v-if="isActive" v-model="draft.status" class="h-11 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-slate-300 light:bg-white">
              <option value="open">Open</option>
              <option value="in_progress">In progress</option>
            </select>
            <div v-else class="flex h-11 items-center rounded-xl border border-white/10 bg-white/[0.03] px-3 text-sm capitalize text-white/45 light:border-slate-300 light:text-slate-600">
              {{ task.status.replace('_', ' ') }}
            </div>
          </label>
          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Due</span>
            <input v-model="draft.due_at" type="datetime-local" class="h-11 w-full rounded-xl border border-white/10 bg-[#15191f] px-3 text-xs outline-none focus-visible:ring-2 focus-visible:ring-violet-300 light:border-slate-300 light:bg-white" />
          </label>
        </div>
      </form>

      <DialogFooter class="gap-2 sm:justify-between">
        <Button v-if="isActive" variant="ghost" class="gap-2 text-rose-300 hover:bg-rose-300/10 hover:text-rose-200 light:text-rose-700" :disabled="saving" @click="cancelOpen = true">
          <Ban class="h-4 w-4" />
          Cancel follow-up
        </Button>
        <div class="flex flex-col-reverse gap-2 sm:ml-auto sm:flex-row">
          <Button variant="outline" :disabled="saving" @click="open = false">Close</Button>
          <Button type="submit" form="task-edit-form" class="gap-2 bg-violet-500 text-white hover:bg-violet-400" :disabled="saving || !draft.title.trim()">
            <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
            <Save v-else class="h-4 w-4" />
            Save changes
          </Button>
        </div>
      </DialogFooter>
    </DialogContent>
  </Dialog>

  <AlertDialog v-model:open="cancelOpen">
    <AlertDialogContent>
      <AlertDialogHeader>
        <AlertDialogTitle>Cancel this follow-up?</AlertDialogTitle>
        <AlertDialogDescription>
          It will leave the active queue but remain in the audit trail. This does not delete the task.
        </AlertDialogDescription>
      </AlertDialogHeader>
      <AlertDialogFooter>
        <AlertDialogCancel :disabled="cancelling">Keep active</AlertDialogCancel>
        <AlertDialogAction class="bg-rose-600 text-white hover:bg-rose-500" :disabled="cancelling" @click="cancelTask">
          <Loader2 v-if="cancelling" class="mr-2 h-4 w-4 animate-spin" />
          Cancel follow-up
        </AlertDialogAction>
      </AlertDialogFooter>
    </AlertDialogContent>
  </AlertDialog>
</template>
