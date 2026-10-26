<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { CalendarDays, Loader2, Plus, UserRound, X } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import { readSelectedOrganizationId } from '@/lib/browserIdentity'
import {
  bookingService,
  type BookingEvent,
  type BookingResource,
  type BookingService,
} from '@/services/productSuite'

const props = defineProps<{
  organization: string
  resources: BookingResource[]
  services: BookingService[]
  initialMode: 'person' | 'schedule'
  canManage: boolean
  canSchedule: boolean
}>()
const emit = defineEmits<{
  close: []
  resourceCreated: [resource: BookingResource]
  serviceCreated: [service: BookingService]
  scheduled: [event: BookingEvent]
  busy: [value: boolean]
}>()
const auth = useAuthStore()
const busy = ref(false)
const error = ref('')
const notice = ref('')
const mode = ref(props.initialMode)
const addingPerson = ref(
  props.initialMode === 'person' || (!props.resources.some((r) => r.is_active) && props.canManage),
)
const selectedResourceId = ref('')
const selectedServiceId = ref('')
const acknowledgedResources = ref<BookingResource[]>([])
const acknowledgedServices = ref<BookingService[]>([])
const person = reactive({
  name: '',
  kind: 'practitioner' as 'practitioner' | 'instructor' | 'staff',
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Kuala_Lumpur',
  location: '',
})
const service = reactive({
  name: '',
  kind: 'appointment' as BookingService['kind'],
  duration_minutes: 30,
  default_capacity: 1,
  price: '0',
  currency: 'MYR',
})
const today = new Date()
const date = ref(new Date(today.getTime() - today.getTimezoneOffset() * 60_000).toISOString().slice(0, 10))
const startTime = ref('09:00')
const capacity = ref(1)
const location = ref('')
let mounted = true
let contextGeneration = 0

const currentOrganization = () => readSelectedOrganizationId() || auth.organizationId || ''
const contextIsCurrent = (generation = contextGeneration) =>
  mounted &&
  generation === contextGeneration &&
  !!props.organization &&
  props.organization === currentOrganization()
const availableResources = computed(() => {
  const values = new Map(props.resources.map((resource) => [resource.id, resource]))
  // A completed create remains usable even if a background catalogue refresh fails.
  for (const resource of acknowledgedResources.value)
    if (!values.has(resource.id)) values.set(resource.id, resource)
  return [...values.values()].filter((resource) => resource.is_active)
})
const people = computed(() =>
  availableResources.value.filter((resource) =>
    ['practitioner', 'instructor', 'staff'].includes(resource.kind),
  ),
)
const otherResources = computed(() =>
  availableResources.value.filter((resource) => !people.value.includes(resource)),
)
const selectedResource = computed(() =>
  availableResources.value.find((resource) => resource.id === selectedResourceId.value),
)
const compatibleServices = computed(() => {
  if (!selectedResource.value) return []
  const values = new Map(props.services.map((item) => [item.id, item]))
  for (const item of acknowledgedServices.value) if (!values.has(item.id)) values.set(item.id, item)
  return [...values.values()].filter(
    (item) =>
      item.is_active && (!item.resource_ids?.length || item.resource_ids.includes(selectedResourceId.value)),
  )
})
const selectedService = computed(() =>
  compatibleServices.value.find((item) => item.id === selectedServiceId.value),
)
const canSaveSchedule = computed(
  () =>
    props.canSchedule &&
    !busy.value &&
    contextIsCurrent() &&
    !!selectedResource.value &&
    !!selectedService.value,
)

watch(
  availableResources,
  (values) => {
    if (selectedResourceId.value && !values.some((item) => item.id === selectedResourceId.value))
      selectedResourceId.value = ''
    if (!selectedResourceId.value && values.length === 1) selectedResourceId.value = values[0]!.id
  },
  { immediate: true },
)
watch(
  compatibleServices,
  (values) => {
    if (!values.some((item) => item.id === selectedServiceId.value))
      selectedServiceId.value = values.length === 1 ? values[0]!.id : ''
  },
  { immediate: true },
)
watch(
  selectedService,
  (value) => {
    if (value) capacity.value = value.default_capacity
  },
  { immediate: true },
)
watch(
  () => props.resources,
  (values) => {
    const ids = new Set(values.map((item) => item.id))
    acknowledgedResources.value = acknowledgedResources.value.filter((item) => !ids.has(item.id))
  },
)
watch(
  () => props.services,
  (values) => {
    const ids = new Set(values.map((item) => item.id))
    acknowledgedServices.value = acknowledgedServices.value.filter((item) => !ids.has(item.id))
  },
)
watch(
  () => auth.organizationId,
  () => {
    ++contextGeneration
    error.value = 'Your workspace changed. Close this form and reopen it in the current workspace.'
  },
)
watch(busy, (value) => emit('busy', value), { flush: 'sync' })
onBeforeUnmount(() => {
  mounted = false
  ++contextGeneration
})

function guard(permission: boolean) {
  if (busy.value) return false
  if (!contextIsCurrent() || !permission) {
    error.value = 'Your workspace or permission changed. Close this form and reopen it before saving.'
    return false
  }
  error.value = ''
  return true
}
function startAddingPerson() {
  if (!guard(props.canManage)) return
  addingPerson.value = true
  person.name = ''
  notice.value = ''
}
async function addPerson() {
  if (!guard(props.canManage)) return
  if (!person.name.trim() || !person.timezone.trim()) {
    error.value = 'Enter a name and a valid timezone.'
    return
  }
  const generation = contextGeneration
  busy.value = true
  try {
    const response = await bookingService.createResource(
      {
        name: person.name.trim(),
        kind: person.kind,
        timezone: person.timezone.trim(),
        location: person.location.trim(),
        is_active: true,
      },
      props.organization,
    )
    if (!contextIsCurrent(generation)) return
    const created = unwrapItemResponse<BookingResource>(response)
    acknowledgedResources.value.push(created)
    selectedResourceId.value = created.id
    location.value = created.location || ''
    addingPerson.value = false
    // Keep the acknowledged ID for every later service/schedule retry.
    notice.value = created.name + ' was added. You can use this person for future schedules.'
    emit('resourceCreated', created)
    if (props.canSchedule) mode.value = 'schedule'
  } catch (cause) {
    if (contextIsCurrent(generation)) error.value = getErrorMessage(cause, 'Person could not be added.')
  } finally {
    busy.value = false
  }
}
async function addService() {
  if (!guard(props.canManage) || !selectedResource.value) return
  const price = Math.round(Number(service.price) * 100)
  if (
    !service.name.trim() ||
    !Number.isInteger(service.duration_minutes) ||
    service.duration_minutes < 1 ||
    service.duration_minutes > 1440 ||
    !Number.isInteger(service.default_capacity) ||
    service.default_capacity < 1 ||
    service.default_capacity > 10000 ||
    !Number.isFinite(price) ||
    price < 0
  ) {
    error.value = 'Enter a service name, duration, capacity and a valid price.'
    return
  }
  const generation = contextGeneration
  const resourceId = selectedResourceId.value
  busy.value = true
  try {
    const response = await bookingService.createService(
      {
        name: service.name.trim(),
        kind: service.kind,
        duration_minutes: service.duration_minutes,
        default_capacity: service.default_capacity,
        price_minor: price,
        currency: service.currency,
        resource_ids: [resourceId],
        is_active: true,
      },
      props.organization,
    )
    if (!contextIsCurrent(generation)) return
    const created = unwrapItemResponse<BookingService>(response)
    acknowledgedServices.value.push(created)
    selectedServiceId.value = created.id
    capacity.value = created.default_capacity
    notice.value = created.name + ' was added for ' + selectedResource.value?.name + '.'
    emit('serviceCreated', created)
  } catch (cause) {
    if (contextIsCurrent(generation))
      error.value = getErrorMessage(cause, 'Service could not be added. Your person is already saved.')
  } finally {
    busy.value = false
  }
}
async function saveSchedule() {
  if (!guard(props.canSchedule)) return
  const resource = selectedResource.value
  const offering = selectedService.value
  const start = new Date(date.value + 'T' + startTime.value + ':00Z')
  if (
    !resource ||
    !offering ||
    !date.value ||
    !startTime.value ||
    !Number.isFinite(start.getTime()) ||
    !Number.isInteger(Number(capacity.value)) ||
    Number(capacity.value) < 1 ||
    Number(capacity.value) > 10000
  ) {
    error.value = 'Choose a person, a compatible service, a date, a start time and a valid capacity.'
    return
  }
  const generation = contextGeneration
  busy.value = true
  try {
    const response = await bookingService.createEvent(
      {
        service_id: offering.id,
        resource_id: resource.id,
        local_starts_at: date.value + 'T' + startTime.value,
        local_ends_at: new Date(start.getTime() + offering.duration_minutes * 60_000)
          .toISOString()
          .slice(0, 16),
        timezone: resource.timezone,
        capacity: Number(capacity.value),
        status: 'scheduled',
        location: location.value.trim(),
      },
      props.organization,
    )
    if (!contextIsCurrent(generation)) return
    emit('scheduled', unwrapItemResponse<BookingEvent>(response))
  } catch (cause) {
    if (contextIsCurrent(generation))
      error.value = getErrorMessage(
        cause,
        'Schedule could not be saved. Your person and service are preserved.',
      )
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <section
    aria-label="People and scheduling"
    class="shrink-0 border-b border-fuchsia-400/20 bg-fuchsia-400/[0.04] px-5 py-5"
  >
    <div class="mx-auto max-w-5xl">
      <div class="mb-4 flex items-start justify-between gap-3">
        <div>
          <p class="flex items-center gap-2 text-xs font-semibold text-fuchsia-200 light:text-fuchsia-800">
            <UserRound class="h-4 w-4" /> People & schedules
          </p>
          <h2 class="mt-1 text-lg font-semibold text-white light:text-gray-900">
            {{ mode === 'person' ? 'Add someone to your team' : 'Who is this schedule for?' }}
          </h2>
          <p class="mt-1 text-sm text-white/60 light:text-gray-600">
            Choose a doctor, instructor or staff member, then set their time.
          </p>
        </div>
        <Button
          type="button"
          variant="ghost"
          aria-label="Close scheduling form"
          :disabled="busy"
          @click="emit('close')"
          ><X class="h-4 w-4"
        /></Button>
      </div>
      <p
        v-if="error"
        role="alert"
        class="mb-4 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
      >
        {{ error }}
      </p>
      <p
        v-if="notice"
        role="status"
        class="mb-4 rounded-lg border border-emerald-500/20 bg-emerald-500/5 p-3 text-sm text-emerald-200 light:text-emerald-800"
      >
        {{ notice }}
      </p>

      <form
        v-if="addingPerson && canManage"
        aria-label="Add person"
        class="mb-4 rounded-xl border border-white/10 bg-black/15 p-4 light:border-gray-200 light:bg-white"
        @submit.prevent="addPerson"
      >
        <fieldset :disabled="busy" class="grid gap-4 sm:grid-cols-2">
          <label class="grid gap-1.5 text-sm"
            >Name
            <Input
              v-model="person.name"
              name="person-name"
              required
              maxlength="255"
              autocomplete="name"
              placeholder="e.g. Dr Aina"
            />
          </label>
          <label class="grid gap-1.5 text-sm"
            >Role
            <select
              v-model="person.kind"
              name="person-role"
              class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 light:border-gray-200 light:bg-white"
            >
              <option value="practitioner">Doctor / practitioner</option>
              <option value="instructor">Instructor</option>
              <option value="staff">Staff</option>
            </select>
          </label>
          <details class="sm:col-span-2">
            <summary class="cursor-pointer text-sm text-white/60 light:text-gray-600">
              Optional details · timezone and location
            </summary>
            <div class="mt-3 grid gap-3 sm:grid-cols-2">
              <label class="grid gap-1.5 text-sm"
                >Timezone<Input v-model="person.timezone" name="person-timezone" required maxlength="100"
              /></label>
              <label class="grid gap-1.5 text-sm"
                >Location<Input
                  v-model="person.location"
                  name="person-location"
                  maxlength="255"
                  placeholder="Optional"
              /></label>
            </div>
          </details>
          <div class="flex flex-wrap gap-2 sm:col-span-2">
            <Button
              type="submit"
              :disabled="busy"
              class="min-h-[44px] bg-fuchsia-400 text-black hover:bg-fuchsia-300"
              ><Loader2 v-if="busy" class="mr-2 h-4 w-4 animate-spin" />Add person</Button
            >
            <Button
              v-if="mode === 'schedule'"
              type="button"
              variant="outline"
              :disabled="busy"
              @click="addingPerson = false"
              >Choose existing person</Button
            >
          </div>
        </fieldset>
      </form>

      <Button
        v-if="mode === 'person' && !addingPerson && canManage"
        type="button"
        variant="outline"
        :disabled="busy"
        @click="startAddingPerson"
        >Add another person</Button
      >

      <div v-if="mode === 'schedule' && !addingPerson" class="grid gap-4 lg:grid-cols-[1fr_1.15fr]">
        <div
          class="space-y-4 rounded-xl border border-white/10 bg-black/15 p-4 light:border-gray-200 light:bg-white"
        >
          <label class="grid gap-1.5 text-sm font-medium"
            >Person
            <select
              v-model="selectedResourceId"
              name="schedule-person"
              :disabled="busy"
              class="h-11 rounded-md border border-white/10 bg-[#0d0f10] px-3 font-normal light:border-gray-200 light:bg-white"
            >
              <option value="">Choose a person</option>
              <optgroup label="People">
                <option v-for="person in people" :key="person.id" :value="person.id">
                  {{ person.name }} ·
                  {{ person.kind === 'practitioner' ? 'Doctor / practitioner' : person.kind }}
                </option>
              </optgroup>
              <optgroup v-if="otherResources.length" label="Rooms and equipment">
                <option v-for="resource in otherResources" :key="resource.id" :value="resource.id">
                  {{ resource.name }} · {{ resource.kind }}
                </option>
              </optgroup>
            </select>
          </label>
          <Button
            v-if="canManage"
            type="button"
            variant="outline"
            :disabled="busy"
            class="min-h-[44px]"
            @click="startAddingPerson"
            ><Plus class="mr-2 h-4 w-4" />Add doctor / instructor / staff</Button
          >
          <p v-else class="text-xs leading-5 text-white/60 light:text-gray-600">
            You can schedule existing people and services. Adding people or services requires booking setup
            permission.
          </p>
          <template v-if="selectedResource">
            <label class="grid gap-1.5 text-sm font-medium"
              >Service for {{ selectedResource.name }}
              <select
                v-model="selectedServiceId"
                name="schedule-service"
                :disabled="busy || !compatibleServices.length"
                class="h-11 rounded-md border border-white/10 bg-[#0d0f10] px-3 font-normal light:border-gray-200 light:bg-white"
              >
                <option value="">
                  {{ compatibleServices.length ? 'Choose a service' : 'No compatible service yet' }}
                </option>
                <option v-for="item in compatibleServices" :key="item.id" :value="item.id">
                  {{ item.name }} · {{ item.duration_minutes }} min
                </option>
              </select>
            </label>
            <p v-if="compatibleServices.length === 1" class="text-xs text-white/50 light:text-gray-600">
              The one available service is selected for you.
            </p>
            <form
              v-if="!compatibleServices.length && canManage"
              aria-label="Add service for person"
              class="grid gap-3 border-t border-white/10 pt-4 light:border-gray-200"
              @submit.prevent="addService"
            >
              <fieldset :disabled="busy" class="grid gap-3 sm:grid-cols-2">
                <p class="text-sm text-white/70 light:text-gray-700 sm:col-span-2">
                  Add a service for {{ selectedResource.name }} to finish scheduling.
                </p>
                <label class="grid gap-1 text-sm sm:col-span-2"
                  >Service name<Input
                    v-model="service.name"
                    name="inline-service-name"
                    required
                    maxlength="255"
                    placeholder="e.g. Consultation"
                /></label>
                <label class="grid gap-1 text-sm"
                  >Type<select
                    v-model="service.kind"
                    name="inline-service-kind"
                    class="h-10 rounded-md border border-white/10 bg-[#0d0f10] px-3 light:border-gray-200 light:bg-white"
                  >
                    <option value="appointment">Appointment</option>
                    <option value="class">Class</option>
                  </select></label
                >
                <label class="grid gap-1 text-sm"
                  >Duration (minutes)<Input
                    v-model.number="service.duration_minutes"
                    name="inline-service-duration"
                    type="number"
                    min="1"
                    max="1440"
                    required
                /></label>
                <label class="grid gap-1 text-sm"
                  >Capacity<Input
                    v-model.number="service.default_capacity"
                    name="inline-service-capacity"
                    type="number"
                    min="1"
                    max="10000"
                    required
                /></label>
                <label class="grid gap-1 text-sm"
                  >Price (MYR)<Input
                    v-model="service.price"
                    name="inline-service-price"
                    type="number"
                    min="0"
                    step="0.01"
                    required
                /></label>
                <p class="text-xs text-white/50 light:text-gray-600 sm:col-span-2">
                  This new service is available only for
                  {{ selectedResource.name }}. More options are in Advanced setup.
                </p>
                <Button type="submit" :disabled="busy" variant="outline" class="min-h-[44px] sm:col-span-2"
                  >Add service</Button
                >
              </fieldset>
            </form>
            <p v-else-if="!compatibleServices.length" class="text-sm text-amber-200 light:text-amber-800">
              No active service is available for this person. Ask someone with booking setup permission to add
              one.
            </p>
          </template>
          <p v-else-if="!availableResources.length" class="text-sm text-white/60 light:text-gray-600">
            No people have been added yet.
          </p>
        </div>

        <form
          aria-label="Schedule details"
          class="rounded-xl border border-white/10 bg-black/15 p-4 light:border-gray-200 light:bg-white"
          @submit.prevent="saveSchedule"
        >
          <fieldset :disabled="busy" class="grid gap-4 sm:grid-cols-2">
            <div class="sm:col-span-2">
              <h3 class="flex items-center gap-2 text-sm font-semibold">
                <CalendarDays class="h-4 w-4" />Set the time
              </h3>
              <p class="mt-1 text-xs text-white/60 light:text-gray-600">
                {{
                  selectedResource
                    ? 'Times use ' +
                      selectedResource.timezone +
                      '. Availability, time off and existing bookings still apply.'
                    : 'Choose a person and service to save a schedule.'
                }}
              </p>
            </div>
            <label class="grid gap-1.5 text-sm"
              >Date<Input v-model="date" name="schedule-date" type="date" required
            /></label>
            <label class="grid gap-1.5 text-sm"
              >Start time<Input v-model="startTime" name="schedule-time" type="time" required
            /></label>
            <label class="grid gap-1.5 text-sm"
              >Capacity<Input
                v-model.number="capacity"
                name="schedule-capacity"
                type="number"
                min="1"
                max="10000"
                required
            /></label>
            <label class="grid gap-1.5 text-sm"
              >Location<Input
                v-model="location"
                name="schedule-location"
                maxlength="255"
                placeholder="Optional"
            /></label>
            <p v-if="selectedService" class="text-xs text-white/60 light:text-gray-600 sm:col-span-2">
              {{ selectedService.name }} · {{ selectedService.duration_minutes }} minutes
            </p>
            <Button
              type="submit"
              :disabled="!canSaveSchedule"
              class="min-h-[44px] bg-fuchsia-400 text-black hover:bg-fuchsia-300 sm:col-span-2"
              ><Loader2 v-if="busy" class="mr-2 h-4 w-4 animate-spin" />Save schedule</Button
            >
          </fieldset>
        </form>
      </div>
    </div>
  </section>
</template>
