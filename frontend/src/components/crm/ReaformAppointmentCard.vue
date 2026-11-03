<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { CalendarClock, Loader2, RefreshCw } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { reaformService, type ReaformLeadState, type ReaformPatient, type ReaformSlot } from '@/services/reaform'

const props = withDefaults(defineProps<{ leadId: string; canBook?: boolean }>(), { canBook: true })
const emit = defineEmits<{ capability: [value: { leadId: string; status: 'loading' | 'enabled' | 'disabled' | 'unavailable' }] }>()
const cardElement = ref<HTMLElement | null>(null)
const state = ref<ReaformLeadState | null>(null)
const loading = ref(false)
const busy = ref(false)
const loadError = ref('')
const actionError = ref('')
const searchError = ref('')
const slotsError = ref('')
const pollError = ref('')
const query = ref('')
const patients = ref<ReaformPatient[]>([])
const searched = ref(false)
const searching = ref(false)
const selectedPatient = ref<ReaformPatient | null>(null)
const identityConfirmed = ref(false)
const date = ref('')
const slots = ref<ReaformSlot[]>([])
const slotsLoading = ref(false)
const slotsLoaded = ref(false)
const selectedSlot = ref('')
// A failed response may have followed a successful booking. Keep this card
// locked until the server reconciles its immutable per-lead command.
const uncertain = ref(false)
const retryingRejected = ref(false)
let generation = 0
let stateRevision = 0
let searchSequence = 0
let slotSequence = 0
let pollTimer: ReturnType<typeof setInterval> | undefined
let polling = false

const canWrite = computed(() => props.canBook && state.value?.can_write === true)
const canStartBooking = computed(() => canWrite.value && state.value?.sync_available !== false)
const appointment = computed(() => state.value?.link?.appointment ?? null)
const link = computed(() => state.value?.link ?? null)
const hasAttempt = computed(() => (Boolean(link.value) && !retryingRejected.value) || uncertain.value)
const slot = computed(() => slots.value.find((item) => slotKey(item) === selectedSlot.value))
const canSubmit = computed(() => canStartBooking.value && !busy.value && !hasAttempt.value &&
  Boolean(selectedPatient.value && identityConfirmed.value && slot.value && slot.value.date === date.value))
const today = new Intl.DateTimeFormat('en-CA', { timeZone: 'Asia/Kuala_Lumpur', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date())

const appointmentStatus = computed(() => {
  const labels: Record<string, string> = {
    booked: 'Booked', scheduled: 'Booked', confirmed: 'Confirmed', checked_in: 'Arrived',
    in_consultation: 'In consultation', dispensing: 'At dispensary', billing: 'At cashier',
    completed: 'Visit completed', cancelled: 'Cancelled', no_show: 'Did not attend',
  }
  return labels[appointment.value?.status ?? ''] ?? 'Status unavailable'
})

const outcomeMessage = computed(() => {
  if (link.value?.error_code === 'patient_or_clinic_binding_changed') {
    return 'This lead’s patient or clinic link has changed. Ask a clinic administrator to review it before arranging another appointment.'
  }
  if (link.value?.state === 'pending' || link.value?.state === 'unknown' || uncertain.value) {
    return 'The booking has not been confirmed yet. Check its status before arranging another appointment.'
  }
  if (link.value?.state === 'rejected') return link.value.can_retry
    ? 'This appointment was not booked. Choose another available time or contact the clinic.'
    : 'This appointment could not be booked. Contact the clinic to arrange the visit.'
  if (link.value?.error_code) return 'The latest update could not be retrieved. The appointment below is the last confirmed information.'
  return ''
})

defineExpose({ focus: () => {
  cardElement.value?.scrollIntoView({ behavior: 'smooth', block: 'center' })
  cardElement.value?.focus({ preventScroll: true })
} })

function reportCapability() {
  emit('capability', { leadId: props.leadId, status: state.value ? (state.value.enabled ? 'enabled' : 'disabled') : 'unavailable' })
}

function slotKey(value: ReaformSlot) {
  return `${value.doctor_id}|${value.date}|${value.time}`
}

function patientIdentifier(value: ReaformPatient) {
  return value.masked_identifier?.trim() || `Patient reference …${value.id.slice(-8)}`
}

function displayDate(value: string) {
  const parsed = new Date(`${value}T12:00:00+08:00`)
  return Number.isNaN(parsed.getTime()) ? value : new Intl.DateTimeFormat('en-MY', {
    timeZone: 'Asia/Kuala_Lumpur', day: 'numeric', month: 'short', year: 'numeric',
  }).format(parsed)
}

function displayTimestamp(value?: string | null) {
  if (!value) return 'Not checked yet'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? 'Time unavailable' : new Intl.DateTimeFormat('en-MY', {
    timeZone: 'Asia/Kuala_Lumpur', day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit',
  }).format(parsed)
}

async function load() {
  const current = generation
  const revision = stateRevision
  const leadId = props.leadId
  loading.value = true
  loadError.value = ''
  try {
    const result = await reaformService.get(leadId)
    if (current !== generation || revision !== stateRevision) return
    state.value = result
    reportCapability()
  } catch {
    if (current === generation) {
      loadError.value = 'Clinic appointments could not be loaded. Try again.'
      reportCapability()
    }
  } finally {
    if (current === generation) loading.value = false
  }
}

async function searchPatients() {
  if (!canStartBooking.value || busy.value || hasAttempt.value || query.value.trim().length < 3) return
  const current = generation
  const sequence = ++searchSequence
  searching.value = true
  searchError.value = ''
  patients.value = []
  selectedPatient.value = null
  identityConfirmed.value = false
  searched.value = false
  try {
    const result = await reaformService.patients(query.value.trim())
    if (current !== generation || sequence !== searchSequence) return
    patients.value = result
    searched.value = true
  } catch {
    if (current === generation && sequence === searchSequence) searchError.value = 'Patient search is unavailable. Try again or contact the clinic.'
  } finally {
    if (current === generation && sequence === searchSequence) searching.value = false
  }
}

function selectPatient(patient: ReaformPatient) {
  selectedPatient.value = patient
  identityConfirmed.value = false
  actionError.value = ''
}

async function findSlots() {
  if (!canStartBooking.value || busy.value || hasAttempt.value || !identityConfirmed.value || !date.value) return
  const current = generation
  const sequence = ++slotSequence
  slotsLoading.value = true
  slotsError.value = ''
  slots.value = []
  selectedSlot.value = ''
  slotsLoaded.value = false
  try {
    const result = await reaformService.availability(date.value)
    if (current !== generation || sequence !== slotSequence) return
    slots.value = result.filter((item) => item.duration_minutes === 30 && item.date === date.value)
    slotsLoaded.value = true
  } catch {
    if (current === generation && sequence === slotSequence) slotsError.value = 'Available times could not be loaded. Try again.'
  } finally {
    if (current === generation && sequence === slotSequence) slotsLoading.value = false
  }
}

async function book() {
  if (!canSubmit.value || !selectedPatient.value || !slot.value) return
  const current = generation
  const leadId = props.leadId
  const request = {
    command_id: crypto.randomUUID(), patient_id: selectedPatient.value.id,
    doctor_id: slot.value.doctor_id, date: slot.value.date, time: slot.value.time,
    duration_minutes: 30 as const, identity_confirmed: true as const,
  }
  busy.value = true
  stateRevision++
  retryingRejected.value = false
  uncertain.value = true
  actionError.value = ''
  try {
    const result = await reaformService.book(leadId, request)
    if (current !== generation) return
    state.value = result
    reportCapability()
    uncertain.value = !result.link
  } catch {
    if (current === generation) actionError.value = 'We could not confirm the booking response. Use Check booking to find out whether it was saved.'
  } finally {
    if (current === generation) busy.value = false
  }
}

function chooseAnotherTime() {
  if (!canStartBooking.value || busy.value || uncertain.value || link.value?.state !== 'rejected' || !link.value.can_retry) return
  retryingRejected.value = true
  actionError.value = ''
  pollError.value = ''
  slots.value = []
  selectedSlot.value = ''
  slotsLoaded.value = false
}

async function pollStatus() {
  if (polling || busy.value || loading.value || !state.value?.enabled || !link.value || document.visibilityState === 'hidden') return
  const current = generation
  const revision = stateRevision
  const leadId = props.leadId
  polling = true
  try {
    const result = await reaformService.get(leadId)
    if (current !== generation || revision !== stateRevision || busy.value) return
    state.value = result
    reportCapability()
    if (result.link?.state !== 'rejected') retryingRejected.value = false
    pollError.value = ''
  } catch {
    if (current === generation) pollError.value = 'The latest update could not be loaded. The last confirmed appointment is still shown.'
  } finally {
    polling = false
  }
}

async function refresh() {
  if (busy.value || !canWrite.value) return
  const current = generation
  const leadId = props.leadId
  busy.value = true
  stateRevision++
  actionError.value = ''
  try {
    const result = await reaformService.refresh(leadId)
    if (current !== generation) return
    state.value = result
    reportCapability()
    pollError.value = ''
    // No row after a lost response is not proof that no appointment exists.
    if (result.link) uncertain.value = false
    else if (uncertain.value) actionError.value = 'The booking is still unconfirmed. Contact the clinic before arranging another appointment.'
  } catch {
    if (current === generation) actionError.value = 'The appointment could not be checked. Keep this booking and try Check booking again.'
  } finally {
    if (current === generation) busy.value = false
  }
}

watch(query, () => {
  searchSequence++
  searching.value = false
  searched.value = false
  patients.value = []
  selectedPatient.value = null
  identityConfirmed.value = false
})
watch([date, selectedPatient, identityConfirmed], () => {
  slotSequence++
  slotsLoading.value = false
  slotsLoaded.value = false
  slots.value = []
  selectedSlot.value = ''
  slotsError.value = ''
})
watch(() => props.leadId, () => {
  generation++
  stateRevision++
  searchSequence++
  slotSequence++
  state.value = null
  uncertain.value = false
  retryingRejected.value = false
  busy.value = false
  loadError.value = ''
  actionError.value = ''
  pollError.value = ''
  searchError.value = ''
  query.value = ''
  patients.value = []
  searched.value = false
  searching.value = false
  selectedPatient.value = null
  identityConfirmed.value = false
  date.value = ''
  slots.value = []
  selectedSlot.value = ''
  slotsLoaded.value = false
  slotsLoading.value = false
  slotsError.value = ''
  emit('capability', { leadId: props.leadId, status: 'loading' })
  if (props.leadId) void load()
}, { immediate: true })
onMounted(() => { pollTimer = setInterval(() => void pollStatus(), 60_000) })
onBeforeUnmount(() => {
  generation++; searchSequence++; slotSequence++
  if (pollTimer) clearInterval(pollTimer)
})
</script>

<template>
  <section
    v-if="state?.enabled || loadError"
    ref="cardElement"
    tabindex="-1"
    class="rounded-xl border border-cyan-300/20 bg-cyan-300/[0.035] p-3.5 text-sm light:border-cyan-200 light:bg-cyan-50/50"
    aria-label="Reaform clinic appointment"
    data-testid="reaform-appointment"
    :aria-busy="busy || loading"
  >
    <h3 class="flex items-center gap-2 text-sm font-semibold"><CalendarClock class="h-4 w-4" aria-hidden="true" />Clinic appointment</h3>
    <p v-if="state?.clinic_name" class="mt-1 text-xs text-white/60 light:text-slate-600">{{ state.clinic_name }} · Reaform360</p>

    <div v-if="loadError" class="mt-3" role="alert">
      <p class="text-xs text-rose-200 light:text-rose-800">{{ loadError }}</p>
      <Button type="button" variant="outline" class="mt-2" :disabled="loading" @click="load">Try again</Button>
    </div>
    <template v-else-if="state?.enabled">
      <p v-if="state.sync_available === false" role="status" class="mt-3 text-xs leading-5 text-amber-100 light:text-amber-900">The clinic connection is unavailable.<template v-if="appointment"> The last confirmed appointment is shown below.</template> Try again before arranging a new appointment.</p>
      <p v-if="actionError" role="alert" class="mt-3 text-xs leading-5 text-rose-200 light:text-rose-800">{{ actionError }}</p>
      <p v-if="pollError" role="status" class="mt-3 text-xs leading-5 text-amber-100 light:text-amber-900">{{ pollError }}</p>
      <div v-if="hasAttempt" class="mt-3 space-y-3" aria-live="polite">
        <p v-if="outcomeMessage" class="text-xs leading-5 text-amber-100 light:text-amber-900">{{ outcomeMessage }}</p>
        <div v-if="appointment" class="rounded-lg border border-white/10 p-3 light:border-slate-200" data-testid="reaform-booked">
          <p class="font-semibold">{{ appointmentStatus }}</p>
          <p class="mt-1 text-xs">{{ displayDate(appointment.date) }} · {{ appointment.time.slice(0, 5) }} MYT</p>
          <p class="mt-1 text-xs text-white/65 light:text-slate-700">{{ appointment.doctor_name || 'Doctor to be assigned' }}</p>
          <p class="mt-2 text-[11px] text-white/50 light:text-slate-600">Patient identity was confirmed when this appointment was requested.</p>
          <p v-if="appointment.checked_in_at" class="mt-1 text-xs">Arrived: {{ displayTimestamp(appointment.checked_in_at) }} MYT</p>
          <p v-if="appointment.completed_at" class="mt-1 text-xs">Completed: {{ displayTimestamp(appointment.completed_at) }} MYT</p>
          <p v-if="appointment.cancelled_at" class="mt-1 text-xs">Cancelled: {{ displayTimestamp(appointment.cancelled_at) }} MYT</p>
        </div>
        <p class="text-[11px] text-white/50 light:text-slate-600">Last updated: {{ displayTimestamp(link?.last_synced_at) }}<template v-if="link?.last_synced_at"> MYT</template></p>
        <Button v-if="canWrite" type="button" variant="outline" class="min-h-10 w-full" :disabled="busy" data-testid="reaform-refresh" @click="refresh">
          <Loader2 v-if="busy" class="mr-2 h-4 w-4 animate-spin" aria-hidden="true" />
          <RefreshCw v-else class="mr-2 h-4 w-4" aria-hidden="true" />
          {{ busy ? 'Checking appointment…' : 'Check booking' }}
        </Button>
        <Button v-if="canStartBooking && link?.state === 'rejected' && link.can_retry && !uncertain" type="button" class="min-h-10 w-full" :disabled="busy" data-testid="reaform-choose-another-time" @click="chooseAnotherTime">Choose another time</Button>
        <p class="text-[11px] leading-5 text-white/50 light:text-slate-600">Manage cancellations or changes in Reaform360. This status does not change the lead’s pipeline stage.</p>
      </div>
      <p v-else-if="!canWrite" class="mt-3 text-xs leading-5 text-white/60 light:text-slate-600">A team member with clinic booking access can arrange this appointment.</p>
      <Button v-else-if="state.sync_available === false" type="button" variant="outline" class="mt-3" :disabled="loading" @click="load">Check connection</Button>
      <div v-else class="mt-3 space-y-4">
        <p v-if="retryingRejected" role="status" class="text-xs leading-5 text-white/60 light:text-slate-600">The previous appointment was not booked. Confirm a new available time below.</p>
        <form class="space-y-2" @submit.prevent="searchPatients">
          <label class="block text-xs font-medium">
            1. Find the existing patient
            <input v-model="query" type="search" minlength="3" maxlength="80" required autocomplete="off" :disabled="busy" placeholder="Patient name or Reaform patient reference" class="mt-1.5 min-h-11 w-full rounded-lg border border-white/15 bg-black/20 px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" data-testid="reaform-patient-query" />
          </label>
          <p class="text-[11px] leading-5 text-white/50 light:text-slate-600">Use at least 3 characters. Check identity with the patient before booking.</p>
          <Button type="submit" variant="outline" class="min-h-10 w-full" :disabled="searching || query.trim().length < 3 || busy" data-testid="reaform-search">{{ searching ? 'Searching…' : 'Search patients' }}</Button>
          <p v-if="searchError" role="alert" class="text-xs text-rose-200 light:text-rose-800">{{ searchError }}</p>
          <p v-if="searched && !patients.length" class="text-xs leading-5 text-white/60 light:text-slate-600">No matching patient found. Ask the clinic to register the patient in Reaform360 first.</p>
        </form>
        <fieldset v-if="patients.length" class="space-y-2" :disabled="busy">
          <legend class="mb-2 text-xs font-medium">Choose the matching record</legend>
          <label v-for="patient in patients" :key="patient.id" class="flex min-h-12 cursor-pointer items-start gap-2 rounded-lg border border-white/10 p-2.5 light:border-slate-200">
            <input type="radio" :name="`reaform-patient-${leadId}`" :value="patient.id" :checked="selectedPatient?.id === patient.id" class="mt-1" @change="selectPatient(patient)" />
            <span><span class="block text-xs font-medium">{{ patient.display_name }}</span><span class="mt-0.5 block text-[11px] text-white/55 light:text-slate-600">{{ patientIdentifier(patient) }}</span></span>
          </label>
        </fieldset>
        <label v-if="selectedPatient" class="flex items-start gap-2 text-xs leading-5">
          <input v-model="identityConfirmed" type="checkbox" :disabled="busy" class="mt-1" data-testid="reaform-confirm-identity" />
          <span>I checked with the patient that <strong>{{ selectedPatient.display_name }}</strong> ({{ patientIdentifier(selectedPatient) }}) is their record.</span>
        </label>
        <form v-if="selectedPatient && identityConfirmed" class="space-y-2" @submit.prevent="findSlots">
          <label class="block text-xs font-medium">2. Choose a date (Malaysia time)
            <input v-model="date" type="date" :min="today" required :disabled="busy" class="mt-1.5 min-h-11 w-full rounded-lg border border-white/15 bg-black/20 px-3 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" data-testid="reaform-date" />
          </label>
          <Button type="submit" variant="outline" class="min-h-10 w-full" :disabled="slotsLoading || !date || busy" data-testid="reaform-find-times">{{ slotsLoading ? 'Finding times…' : 'Find available times' }}</Button>
          <p v-if="slotsError" role="alert" class="text-xs text-rose-200 light:text-rose-800">{{ slotsError }}</p>
          <p v-if="slotsLoaded && !slots.length" class="text-xs text-white/60 light:text-slate-600">No available times on this date. Choose another date or contact the clinic.</p>
        </form>
        <label v-if="slots.length" class="block text-xs font-medium">Available appointment
          <select v-model="selectedSlot" :disabled="busy" class="mt-1.5 min-h-11 w-full rounded-lg border border-white/15 bg-[#15191f] px-3 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 light:border-slate-300 light:bg-white" data-testid="reaform-slot">
            <option value="">Choose a time and doctor</option>
            <option v-for="available in slots" :key="slotKey(available)" :value="slotKey(available)">{{ available.time.slice(0, 5) }} · {{ available.doctor_name }} · 30 minutes</option>
          </select>
        </label>
        <div v-if="slot && selectedPatient && identityConfirmed" class="rounded-lg border border-cyan-300/20 p-3 light:border-cyan-200" data-testid="reaform-review">
          <h4 class="text-xs font-semibold">3. Confirm appointment</h4>
          <p class="mt-2 text-xs leading-5">{{ selectedPatient.display_name }}<br />{{ displayDate(slot.date) }} · {{ slot.time.slice(0, 5) }} MYT<br />{{ slot.doctor_name }} · 30 minutes</p>
          <p class="mt-2 text-[11px] leading-5 text-white/50 light:text-slate-600">Creates the appointment in Reaform360. Booking does not send a confirmation message.</p>
          <Button type="button" class="mt-3 min-h-11 w-full" :disabled="!canSubmit" data-testid="reaform-book" @click="book">{{ busy ? 'Booking…' : 'Book clinic appointment' }}</Button>
        </div>
      </div>
    </template>
  </section>
</template>
