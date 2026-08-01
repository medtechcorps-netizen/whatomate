<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { CalendarClock, CalendarOff, Clock3, Loader2, Pencil, Plus, RotateCcw, Trash2 } from 'lucide-vue-next'
import ConfirmDialog from '@/components/shared/ConfirmDialog.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useAppToast } from '@/composables/useAppToast'
import { getErrorMessage } from '@/lib/api-utils'
import {
  formatDateInTimeZone,
  formatDateTimeInputInTimeZone,
  zonedLocalDateTimeToUTC,
} from '@/lib/zonedDateTime'
import {
  bookingService,
  type AvailabilityRule,
  type BookingResource,
  type ResourceTimeOff,
} from '@/services/productSuite'

const props = defineProps<{
  resources: BookingResource[]
}>()

const toast = useAppToast()
const selectedResourceId = ref('')
const availabilityRules = ref<AvailabilityRule[]>([])
const timeOff = ref<ResourceTimeOff[]>([])
const loading = ref(false)
const savingRule = ref(false)
const savingTimeOff = ref(false)
const deleting = ref(false)
let loadRequestId = 0

const weekdays = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday']
const selectedResource = computed(
  () => props.resources.find((resource) => resource.id === selectedResourceId.value) ?? null,
)

const ruleDraft = reactive({
  id: '',
  weekday: 1,
  start_local_time: '09:00',
  end_local_time: '17:00',
  effective_from: '',
  effective_until: '',
  is_active: true,
  version: 0,
})

const timeOffDraft = reactive({
  id: '',
  starts_at: '',
  ends_at: '',
  reason: '',
  version: 0,
})

type PendingDelete = {
  type: 'availability' | 'time_off'
  resourceId: string
  id: string
  version: number
  label: string
}
const pendingDelete = ref<PendingDelete | null>(null)
const deleteOpen = computed({
  get: () => Boolean(pendingDelete.value),
  set: (value) => {
    if (!value && !deleting.value) pendingDelete.value = null
  },
})

function resetRuleDraft() {
  Object.assign(ruleDraft, {
    id: '',
    weekday: 1,
    start_local_time: '09:00',
    end_local_time: '17:00',
    effective_from: '',
    effective_until: '',
    is_active: true,
    version: 0,
  })
}

function resetTimeOffDraft() {
  Object.assign(timeOffDraft, {
    id: '',
    starts_at: '',
    ends_at: '',
    reason: '',
    version: 0,
  })
}

function formatZonedDateTimeInput(value?: string) {
  if (!value || !selectedResource.value) return ''
  return formatDateTimeInputInTimeZone(value, selectedResource.value.timezone)
}

function formatZonedDate(value?: string) {
  if (!value || !selectedResource.value) return ''
  return formatDateInTimeZone(value, selectedResource.value.timezone)
}

function formatTimeOff(value: string) {
  if (!selectedResource.value) return value
  return new Intl.DateTimeFormat('en-MY', {
    timeZone: selectedResource.value.timezone,
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(value))
}

async function loadSchedule() {
  const resourceId = selectedResourceId.value
  if (!resourceId) {
    availabilityRules.value = []
    timeOff.value = []
    return
  }
  const requestId = ++loadRequestId
  availabilityRules.value = []
  timeOff.value = []
  loading.value = true
  try {
    const [rules, blocks] = await Promise.all([
      bookingService.allAvailabilityRules(resourceId),
      bookingService.allTimeOff(resourceId),
    ])
    if (requestId !== loadRequestId || resourceId !== selectedResourceId.value) return
    availabilityRules.value = rules.sort(
      (left, right) => left.weekday - right.weekday || left.start_local_time.localeCompare(right.start_local_time),
    )
    timeOff.value = blocks.sort(
      (left, right) => new Date(left.starts_at).getTime() - new Date(right.starts_at).getTime(),
    )
  } catch (error) {
    if (requestId === loadRequestId) {
      toast.error('Availability could not be loaded', getErrorMessage(error))
    }
  } finally {
    if (requestId === loadRequestId) loading.value = false
  }
}

watch(selectedResourceId, () => {
  resetRuleDraft()
  resetTimeOffDraft()
  void loadSchedule()
})
watch(
  () => props.resources.map((resource) => resource.id).join(','),
  () => {
    if (!props.resources.some((resource) => resource.id === selectedResourceId.value)) {
      selectedResourceId.value = props.resources[0]?.id ?? ''
    } else if (selectedResourceId.value) {
      void loadSchedule()
    }
  },
  { immediate: true },
)

function editRule(rule: AvailabilityRule) {
  Object.assign(ruleDraft, {
    id: rule.id,
    weekday: rule.weekday,
    start_local_time: rule.start_local_time.slice(0, 5),
    end_local_time: rule.end_local_time.slice(0, 5),
    effective_from: rule.effective_from_date || formatZonedDate(rule.effective_from),
    effective_until: rule.effective_until_date || formatZonedDate(rule.effective_until),
    is_active: rule.is_active,
    version: rule.version,
  })
}

async function saveRule() {
  if (!selectedResourceId.value) return
  if (ruleDraft.start_local_time >= ruleDraft.end_local_time) {
    toast.warning('Availability end time must be later than its start time')
    return
  }
  if (ruleDraft.effective_from && ruleDraft.effective_until && ruleDraft.effective_from > ruleDraft.effective_until) {
    toast.warning('Availability end date must be on or after its start date')
    return
  }

  savingRule.value = true
  const payload = {
    weekday: Number(ruleDraft.weekday),
    start_local_time: ruleDraft.start_local_time,
    end_local_time: ruleDraft.end_local_time,
    effective_from: ruleDraft.effective_from || null,
    effective_until: ruleDraft.effective_until || null,
    is_active: ruleDraft.is_active,
  }
  try {
    if (ruleDraft.id) {
      await bookingService.updateAvailabilityRule(selectedResourceId.value, ruleDraft.id, {
        ...payload,
        version: ruleDraft.version,
      })
      toast.success('Availability window updated')
    } else {
      await bookingService.createAvailabilityRule(selectedResourceId.value, payload)
      toast.success('Availability window added')
    }
    resetRuleDraft()
    await loadSchedule()
  } catch (error) {
    toast.error('Availability was not saved', getErrorMessage(error))
  } finally {
    savingRule.value = false
  }
}

function editTimeOff(block: ResourceTimeOff) {
  Object.assign(timeOffDraft, {
    id: block.id,
    starts_at: formatZonedDateTimeInput(block.starts_at),
    ends_at: formatZonedDateTimeInput(block.ends_at),
    reason: block.reason ?? '',
    version: block.version,
  })
}

async function saveTimeOff() {
  if (!selectedResource.value || !timeOffDraft.starts_at || !timeOffDraft.ends_at) {
    toast.warning('Time-off start and end are required')
    return
  }

  let startsAt = ''
  let endsAt = ''
  try {
    startsAt = zonedLocalDateTimeToUTC(timeOffDraft.starts_at, selectedResource.value.timezone)
    endsAt = zonedLocalDateTimeToUTC(timeOffDraft.ends_at, selectedResource.value.timezone)
  } catch (error) {
    toast.warning(getErrorMessage(error, 'Choose valid local times'))
    return
  }
  if (new Date(startsAt).getTime() >= new Date(endsAt).getTime()) {
    toast.warning('Time off must end after it starts')
    return
  }

  savingTimeOff.value = true
  const payload = {
    starts_at: startsAt,
    ends_at: endsAt,
    reason: timeOffDraft.reason.trim() || undefined,
  }
  try {
    if (timeOffDraft.id) {
      await bookingService.updateTimeOff(selectedResource.value.id, timeOffDraft.id, {
        ...payload,
        version: timeOffDraft.version,
      })
      toast.success('Time off updated')
    } else {
      await bookingService.createTimeOff(selectedResource.value.id, payload)
      toast.success('Time off added')
    }
    resetTimeOffDraft()
    await loadSchedule()
  } catch (error) {
    toast.error('Time off was not saved', getErrorMessage(error))
  } finally {
    savingTimeOff.value = false
  }
}

async function confirmDelete() {
  if (!pendingDelete.value) return
  deleting.value = true
  const target = pendingDelete.value
  try {
    if (target.type === 'availability') {
      await bookingService.deleteAvailabilityRule(target.resourceId, target.id, target.version)
      toast.success('Availability window deleted')
      if (ruleDraft.id === target.id) resetRuleDraft()
    } else {
      await bookingService.deleteTimeOff(target.resourceId, target.id, target.version)
      toast.success('Time off deleted')
      if (timeOffDraft.id === target.id) resetTimeOffDraft()
    }
    pendingDelete.value = null
    await loadSchedule()
  } catch (error) {
    toast.error('Schedule rule was not deleted', getErrorMessage(error))
  } finally {
    deleting.value = false
  }
}
</script>

<template>
  <section
    data-testid="booking-availability-manager"
    aria-labelledby="booking-availability-title"
    :aria-busy="loading"
    class="rounded-2xl border border-amber-300/15 bg-gradient-to-br from-amber-300/[0.055] via-transparent to-cyan-300/[0.035] p-4 xl:col-span-2"
  >
    <header
      class="flex flex-col gap-3 border-b border-white/[0.07] pb-4 light:border-gray-200 sm:flex-row sm:items-end sm:justify-between"
    >
      <div class="flex items-start gap-3">
        <span class="rounded-xl bg-amber-300/10 p-2.5 text-amber-200 light:text-amber-700">
          <CalendarOff class="h-5 w-5" />
        </span>
        <div>
          <div class="flex flex-wrap items-center gap-2">
            <h3 id="booking-availability-title" class="text-sm font-semibold text-white light:text-gray-900">
              Recurring availability & time off
            </h3>
            <Badge
              variant="outline"
              class="border-emerald-300/25 text-[9px] uppercase tracking-wider text-emerald-200 light:text-emerald-800"
            >
              Enforced
            </Badge>
          </div>
          <p class="mt-1 max-w-2xl text-xs leading-5 text-white/40 light:text-gray-600">
            Scheduled events must fit an active weekly window and cannot overlap time off. Empty weekly hours leave a
            resource unrestricted for backward compatibility.
          </p>
        </div>
      </div>
      <label class="min-w-64">
        <span class="mb-1 block text-[10px] font-semibold uppercase tracking-[0.16em] text-white/40 light:text-gray-600"
          >Resource</span
        >
        <select
          v-model="selectedResourceId"
          aria-label="Availability resource"
          class="h-10 w-full rounded-md border border-white/10 bg-[#0d0f10] px-3 text-sm text-white outline-none focus-visible:ring-2 focus-visible:ring-amber-300 light:border-gray-300 light:bg-white light:text-gray-900"
        >
          <option v-for="resource in resources" :key="resource.id" :value="resource.id">
            {{ resource.name }} · {{ resource.timezone }}
          </option>
        </select>
      </label>
    </header>

    <div v-if="!resources.length" class="py-10 text-center text-sm text-white/40 light:text-gray-600">
      Add a schedulable resource before defining hours.
    </div>
    <div v-else-if="loading" role="status" class="flex min-h-48 items-center justify-center">
      <Loader2 class="h-5 w-5 animate-spin text-amber-200" />
      <span class="sr-only">Loading availability for {{ selectedResource?.name }}</span>
    </div>
    <div v-else class="mt-4 grid gap-4 2xl:grid-cols-2">
      <article class="rounded-2xl border border-white/[0.08] bg-black/15 p-4 light:border-gray-200 light:bg-white">
        <div class="flex items-start justify-between gap-3">
          <div>
            <p class="flex items-center gap-2 text-sm font-semibold text-white light:text-gray-900">
              <Clock3 class="h-4 w-4 text-cyan-300 light:text-cyan-700" />
              Weekly windows
            </p>
            <p class="mt-1 text-[11px] leading-5 text-white/35 light:text-gray-500">
              Times use {{ selectedResource?.timezone }}. Overnight windows are intentionally split across days.
            </p>
          </div>
          <Button v-if="ruleDraft.id" type="button" size="sm" variant="ghost" @click="resetRuleDraft">
            <RotateCcw class="mr-1.5 h-3.5 w-3.5" />New
          </Button>
        </div>

        <form class="mt-4 grid gap-3 sm:grid-cols-2" @submit.prevent="saveRule">
          <label class="block sm:col-span-2">
            <span class="field-label">Weekday</span>
            <select v-model.number="ruleDraft.weekday" aria-label="Availability weekday" class="schedule-control">
              <option v-for="(day, index) in weekdays" :key="day" :value="index">
                {{ day }}
              </option>
            </select>
          </label>
          <label class="block"
            ><span class="field-label">Opens</span
            ><Input v-model="ruleDraft.start_local_time" aria-label="Availability opens" type="time" required
          /></label>
          <label class="block"
            ><span class="field-label">Closes</span
            ><Input v-model="ruleDraft.end_local_time" aria-label="Availability closes" type="time" required
          /></label>
          <label class="block"
            ><span class="field-label">Effective from</span
            ><Input v-model="ruleDraft.effective_from" aria-label="Availability effective from" type="date"
          /></label>
          <label class="block"
            ><span class="field-label">Effective until</span
            ><Input v-model="ruleDraft.effective_until" aria-label="Availability effective until" type="date"
          /></label>
          <label class="flex items-start gap-2 text-xs leading-5 text-white/50 light:text-gray-600 sm:col-span-2">
            <input v-model="ruleDraft.is_active" type="checkbox" class="mt-1 accent-cyan-400" />
            Enforce this window when schedules are created or changed.
          </label>
          <Button type="submit" class="bg-cyan-400 text-black hover:bg-cyan-300 sm:col-span-2" :disabled="savingRule">
            <Loader2 v-if="savingRule" class="mr-2 h-4 w-4 animate-spin" />
            <Plus v-else class="mr-2 h-4 w-4" />
            {{ ruleDraft.id ? 'Update window' : 'Add window' }}
          </Button>
        </form>

        <div class="mt-4 space-y-2 border-t border-white/[0.07] pt-4 light:border-gray-200">
          <div
            v-for="rule in availabilityRules"
            :key="rule.id"
            class="flex items-center gap-3 rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
          >
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <p class="text-xs font-semibold text-white light:text-gray-900">
                  {{ weekdays[rule.weekday] }} · {{ rule.start_local_time.slice(0, 5) }}–{{
                    rule.end_local_time.slice(0, 5)
                  }}
                </p>
                <Badge :variant="rule.is_active ? 'success' : 'secondary'">{{
                  rule.is_active ? 'Active' : 'Paused'
                }}</Badge>
              </div>
              <p class="mt-1 text-[10px] text-white/35 light:text-gray-500">
                {{
                  rule.effective_from_date ||
                  (rule.effective_from ? formatZonedDate(rule.effective_from) : 'No start limit')
                }}
                →
                {{
                  rule.effective_until_date ||
                  (rule.effective_until ? formatZonedDate(rule.effective_until) : 'No end limit')
                }}
              </p>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              :aria-label="`Edit ${weekdays[rule.weekday]} availability`"
              @click="editRule(rule)"
              ><Pencil class="h-3.5 w-3.5"
            /></Button>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              :aria-label="`Delete ${weekdays[rule.weekday]} availability`"
              @click="
                pendingDelete = {
                  type: 'availability',
                  resourceId: selectedResourceId,
                  id: rule.id,
                  version: rule.version,
                  label: `${weekdays[rule.weekday]} ${rule.start_local_time.slice(0, 5)}–${rule.end_local_time.slice(0, 5)}`,
                }
              "
              ><Trash2 class="h-3.5 w-3.5 text-rose-300 light:text-rose-700"
            /></Button>
          </div>
          <p
            v-if="!availabilityRules.length"
            class="rounded-xl border border-dashed border-white/[0.08] p-4 text-center text-xs text-white/35 light:border-gray-300 light:text-gray-500"
          >
            No weekly windows yet; this resource is currently unrestricted.
          </p>
        </div>
      </article>

      <article class="rounded-2xl border border-white/[0.08] bg-black/15 p-4 light:border-gray-200 light:bg-white">
        <div class="flex items-start justify-between gap-3">
          <div>
            <p class="flex items-center gap-2 text-sm font-semibold text-white light:text-gray-900">
              <CalendarClock class="h-4 w-4 text-amber-300 light:text-amber-700" />
              Time off
            </p>
            <p class="mt-1 text-[11px] leading-5 text-white/35 light:text-gray-500">
              Blocks are absolute instants entered and displayed in
              {{ selectedResource?.timezone }}.
            </p>
          </div>
          <Button v-if="timeOffDraft.id" type="button" size="sm" variant="ghost" @click="resetTimeOffDraft">
            <RotateCcw class="mr-1.5 h-3.5 w-3.5" />New
          </Button>
        </div>

        <form class="mt-4 grid gap-3 sm:grid-cols-2" @submit.prevent="saveTimeOff">
          <label class="block"
            ><span class="field-label">Starts</span
            ><Input v-model="timeOffDraft.starts_at" aria-label="Time off starts" type="datetime-local" required
          /></label>
          <label class="block"
            ><span class="field-label">Ends</span
            ><Input v-model="timeOffDraft.ends_at" aria-label="Time off ends" type="datetime-local" required
          /></label>
          <label class="block sm:col-span-2"
            ><span class="field-label">Reason</span
            ><Input
              v-model="timeOffDraft.reason"
              aria-label="Time off reason"
              maxlength="2000"
              placeholder="Annual leave, room maintenance…"
          /></label>
          <Button
            type="submit"
            class="bg-amber-300 text-black hover:bg-amber-200 sm:col-span-2"
            :disabled="savingTimeOff"
          >
            <Loader2 v-if="savingTimeOff" class="mr-2 h-4 w-4 animate-spin" />
            <Plus v-else class="mr-2 h-4 w-4" />
            {{ timeOffDraft.id ? 'Update time off' : 'Add time off' }}
          </Button>
        </form>

        <div class="mt-4 space-y-2 border-t border-white/[0.07] pt-4 light:border-gray-200">
          <div
            v-for="block in timeOff"
            :key="block.id"
            class="flex items-center gap-3 rounded-xl border border-white/[0.07] bg-white/[0.025] p-3 light:border-gray-200 light:bg-gray-50"
          >
            <div class="min-w-0 flex-1">
              <p class="text-xs font-semibold text-white light:text-gray-900">
                {{ block.reason || 'Unavailable' }}
              </p>
              <p class="mt-1 text-[10px] leading-4 text-white/35 light:text-gray-500">
                {{ formatTimeOff(block.starts_at) }} →
                {{ formatTimeOff(block.ends_at) }}
              </p>
            </div>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              :aria-label="`Edit time off ${block.reason || 'Unavailable'}`"
              @click="editTimeOff(block)"
              ><Pencil class="h-3.5 w-3.5"
            /></Button>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              :aria-label="`Delete time off ${block.reason || 'Unavailable'}`"
              @click="
                pendingDelete = {
                  type: 'time_off',
                  resourceId: selectedResourceId,
                  id: block.id,
                  version: block.version,
                  label: block.reason || 'Unavailable block',
                }
              "
              ><Trash2 class="h-3.5 w-3.5 text-rose-300 light:text-rose-700"
            /></Button>
          </div>
          <p
            v-if="!timeOff.length"
            class="rounded-xl border border-dashed border-white/[0.08] p-4 text-center text-xs text-white/35 light:border-gray-300 light:text-gray-500"
          >
            No time-off blocks for this resource.
          </p>
        </div>
      </article>
    </div>
  </section>

  <ConfirmDialog
    v-model:open="deleteOpen"
    title="Delete this schedule rule?"
    :description="`${pendingDelete?.label || 'This rule'} will stop affecting future schedule checks. Existing events are not deleted.`"
    confirm-label="Delete rule"
    cancel-label="Keep rule"
    variant="destructive"
    :is-submitting="deleting"
    @confirm="confirmDelete"
  />
</template>

<style scoped>
.field-label {
  display: block;
  margin-bottom: 0.375rem;
  font-size: 0.6875rem;
  font-weight: 500;
  color: rgb(255 255 255 / 0.48);
}

:global(.light) .field-label {
  color: rgb(75 85 99);
}

.schedule-control {
  height: 2.5rem;
  width: 100%;
  border-radius: 0.375rem;
  border: 1px solid rgb(255 255 255 / 0.1);
  background: #0d0f10;
  padding: 0 0.75rem;
  font-size: 0.875rem;
  color: white;
  outline: none;
}

.schedule-control:focus-visible {
  box-shadow: 0 0 0 2px rgb(103 232 249);
}

:global(.light) .schedule-control {
  border-color: rgb(209 213 219);
  background: white;
  color: rgb(17 24 39);
}
</style>
