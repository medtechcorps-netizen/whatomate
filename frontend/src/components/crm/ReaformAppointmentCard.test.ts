/** @vitest-environment happy-dom */
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ReaformAppointmentCard from './ReaformAppointmentCard.vue'
import type { ReaformLeadState } from '@/services/reaform'

const service = vi.hoisted(() => ({ get: vi.fn(), patients: vi.fn(), availability: vi.fn(), book: vi.fn(), refresh: vi.fn() }))
vi.mock('@/services/reaform', () => ({ reaformService: service }))

const patient = { id: 'patient-1', display_name: 'Synthetic Patient', masked_identifier: '•••• 4321' }
const available = { doctor_id: 'doctor-1', doctor_name: 'Dr Example', date: '2026-12-20', time: '10:30', duration_minutes: 30 }
function state(overrides: Partial<ReaformLeadState> = {}): ReaformLeadState {
  return { enabled: true, clinic_name: 'Pilot clinic', can_write: true, link: null, ...overrides }
}
function linked(status = 'booked'): ReaformLeadState {
  return state({ link: {
    command_id: 'command-1', patient_id: patient.id, state: 'linked',
    last_synced_at: '2026-11-03T02:00:00Z',
    appointment: { id: 'appointment-1', status, date: available.date, time: available.time, doctor_id: available.doctor_id, doctor_name: available.doctor_name, version: '1' },
  } })
}

const wrappers: Array<{ unmount: () => void }> = []
function mountCard(props: { leadId?: string; canBook?: boolean } = {}) {
  const wrapper = mount(ReaformAppointmentCard, { props: { leadId: 'lead-1', ...props }, attachTo: document.body })
  wrappers.push(wrapper)
  return wrapper
}
async function choosePatient(wrapper: ReturnType<typeof mountCard>) {
  await wrapper.get('[data-testid="reaform-patient-query"]').setValue('Synthetic')
  await wrapper.get('[data-testid="reaform-search"]').trigger('click')
  await flushPromises()
  await wrapper.get('input[type="radio"]').setValue(true)
  await wrapper.get('[data-testid="reaform-confirm-identity"]').setValue(true)
}
async function prepareBooking(wrapper: ReturnType<typeof mountCard>) {
  await flushPromises()
  await choosePatient(wrapper)
  await wrapper.get('[data-testid="reaform-date"]').setValue(available.date)
  await wrapper.get('[data-testid="reaform-find-times"]').trigger('click')
  await flushPromises()
  await wrapper.get('[data-testid="reaform-slot"]').setValue('doctor-1|2026-12-20|10:30')
}

beforeEach(() => {
  service.get.mockReset().mockResolvedValue(state())
  service.patients.mockReset().mockResolvedValue([patient])
  service.availability.mockReset().mockResolvedValue([available])
  service.book.mockReset().mockResolvedValue(linked())
  service.refresh.mockReset().mockResolvedValue(linked())
})
afterEach(() => {
  wrappers.splice(0).forEach((wrapper) => wrapper.unmount())
  document.body.innerHTML = ''
  vi.useRealTimers()
})

describe('ReaformAppointmentCard', () => {
  it('stays hidden when the pilot is disabled and never searches patients automatically', async () => {
    service.get.mockResolvedValue(state({ enabled: false }))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.find('[data-testid="reaform-appointment"]').exists()).toBe(false)
    expect(service.patients).not.toHaveBeenCalled()
    expect(service.availability).not.toHaveBeenCalled()
  })

  it('requires patient choice and identity confirmation before loading times and booking', async () => {
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.find('[data-testid="reaform-date"]').exists()).toBe(false)
    await wrapper.get('[data-testid="reaform-patient-query"]').setValue('Synthetic')
    await wrapper.get('[data-testid="reaform-search"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="reaform-date"]').exists()).toBe(false)
    await wrapper.get('input[type="radio"]').setValue(true)
    expect(wrapper.find('[data-testid="reaform-date"]').exists()).toBe(false)
    await wrapper.get('[data-testid="reaform-confirm-identity"]').setValue(true)
    await wrapper.get('[data-testid="reaform-date"]').setValue(available.date)
    await wrapper.get('[data-testid="reaform-find-times"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-testid="reaform-slot"]').setValue('doctor-1|2026-12-20|10:30')
    expect(wrapper.get('[data-testid="reaform-review"]').text()).toContain(patient.display_name)
    await wrapper.get('[data-testid="reaform-book"]').trigger('click')
    await flushPromises()
    expect(service.book).toHaveBeenCalledOnce()
    expect(service.book).toHaveBeenCalledWith('lead-1', {
      command_id: expect.stringMatching(/^[0-9a-f-]{36}$/), patient_id: 'patient-1', doctor_id: 'doctor-1',
      date: '2026-12-20', time: '10:30', duration_minutes: 30, identity_confirmed: true,
    })
    expect(wrapper.get('[data-testid="reaform-booked"]').text()).toContain('Booked')
    expect(wrapper.find('[data-testid="reaform-search"]').exists()).toBe(false)
  })

  it('removes the confirmed identity and slot when the search or selected date changes', async () => {
    const wrapper = mountCard()
    await prepareBooking(wrapper)
    await wrapper.get('[data-testid="reaform-date"]').setValue('2026-12-21')
    expect(wrapper.find('[data-testid="reaform-book"]').exists()).toBe(false)
    await wrapper.get('[data-testid="reaform-patient-query"]').setValue('Different person')
    expect(wrapper.find('[data-testid="reaform-confirm-identity"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reaform-date"]').exists()).toBe(false)
    expect(service.book).not.toHaveBeenCalled()
  })

  it('keeps an uncertain booking locked and checks its existing request rather than creating another', async () => {
    service.book.mockRejectedValue(new Error('network unavailable'))
    service.refresh.mockResolvedValueOnce(state())
    const wrapper = mountCard()
    await prepareBooking(wrapper)
    await wrapper.get('[data-testid="reaform-book"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('could not confirm the booking response')
    expect(wrapper.find('[data-testid="reaform-book"]').exists()).toBe(false)
    await wrapper.get('[data-testid="reaform-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('still unconfirmed')
    expect(wrapper.find('[data-testid="reaform-search"]').exists()).toBe(false)
    await wrapper.get('[data-testid="reaform-refresh"]').trigger('click')
    await flushPromises()
    expect(service.refresh).toHaveBeenCalledWith('lead-1')
    expect(service.book).toHaveBeenCalledOnce()
    expect(wrapper.get('[data-testid="reaform-booked"]').text()).toContain('Booked')
  })

  it('prevents repeated submission while a booking is in progress', async () => {
    let resolve!: (value: ReaformLeadState) => void
    service.book.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountCard()
    await prepareBooking(wrapper)
    const button = wrapper.get('[data-testid="reaform-book"]')
    await button.trigger('click')
    await button.trigger('click')
    expect(service.book).toHaveBeenCalledOnce()
    resolve(linked())
    await flushPromises()
  })

  it('shows attendance, cancellation and no-show updates without changing the CRM stage', async () => {
    service.get.mockResolvedValue(linked('checked_in'))
    service.refresh.mockResolvedValueOnce(linked('no_show')).mockResolvedValueOnce(linked('cancelled'))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.text()).toContain('Arrived')
    expect(wrapper.text()).toContain('Last updated:')
    await wrapper.get('[data-testid="reaform-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Did not attend')
    await wrapper.get('[data-testid="reaform-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Cancelled')
    expect(service.book).not.toHaveBeenCalled()
  })

  it('shows an authoritative unassigned doctor without retaining the old doctor name', async () => {
    const unassigned = linked()
    unassigned.link!.appointment!.doctor_id = null
    unassigned.link!.appointment!.doctor_name = null
    service.get.mockResolvedValue(unassigned)
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.get('[data-testid="reaform-booked"]').text()).toContain('Doctor to be assigned')
    expect(wrapper.text()).not.toContain('Dr Example')
  })

  it('keeps the last confirmed appointment visible on refresh failure', async () => {
    service.get.mockResolvedValue(linked('completed'))
    service.refresh.mockRejectedValue(new Error('outage'))
    const wrapper = mountCard()
    await flushPromises()
    await wrapper.get('[data-testid="reaform-refresh"]').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('Visit completed')
    expect(wrapper.text()).toContain('could not be checked')
  })

  it('offers a new reviewed booking only after a definitive rejection', async () => {
    service.get.mockResolvedValue(state({ link: { command_id: 'command-1', patient_id: 'patient-1', state: 'rejected', can_retry: true, appointment: null, error_code: 'slot_unavailable' } }))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.text()).toContain('was not booked')
    expect(wrapper.find('[data-testid="reaform-book"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reaform-search"]').exists()).toBe(false)
    await wrapper.get('[data-testid="reaform-choose-another-time"]').trigger('click')
    await prepareBooking(wrapper)
    await wrapper.get('[data-testid="reaform-book"]').trigger('click')
    await flushPromises()
    expect(service.book).toHaveBeenCalledOnce()
    expect(service.book.mock.calls[0]![1].command_id).not.toBe('command-1')
  })

  it('updates the open appointment from stored status every minute and stops after unmount', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    service.get.mockResolvedValueOnce(linked()).mockResolvedValue(linked('checked_in'))
    const pollingCard = mountCard()
    await flushPromises()
    expect(pollingCard.text()).toContain('Booked')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(pollingCard.text()).toContain('Arrived')
    expect(service.refresh).not.toHaveBeenCalled()
    pollingCard.unmount()
    const calls = service.get.mock.calls.length
    await vi.advanceTimersByTimeAsync(120_000)
    expect(service.get).toHaveBeenCalledTimes(calls)
  })

  it('does not offer a new request without explicit server confirmation that retry is safe', async () => {
    service.get.mockResolvedValue(state({ link: { command_id: 'command-1', patient_id: 'patient-1', state: 'rejected', can_retry: false, appointment: null } }))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.text()).toContain('Contact the clinic')
    expect(wrapper.find('[data-testid="reaform-choose-another-time"]').exists()).toBe(false)
  })

  it('explains a changed patient or clinic link without offering a new booking', async () => {
    service.get.mockResolvedValue(state({ can_write: false, link: { command_id: 'command-1', patient_id: '', state: 'unknown', appointment: null, error_code: 'patient_or_clinic_binding_changed' } }))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.text()).toContain('patient or clinic link has changed')
    expect(wrapper.find('[data-testid="reaform-search"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reaform-refresh"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reaform-choose-another-time"]').exists()).toBe(false)
  })

  it('honours server and parent booking permissions', async () => {
    service.get.mockResolvedValue(state({ can_write: false }))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.text()).toContain('team member with clinic booking access')
    expect(wrapper.find('[data-testid="reaform-search"]').exists()).toBe(false)
    service.get.mockResolvedValue(state())
    const parentDenied = mountCard({ canBook: false })
    await flushPromises()
    expect(parentDenied.find('[data-testid="reaform-search"]').exists()).toBe(false)
  })

  it('retains the last appointment during a clinic outage and disables new searches', async () => {
    service.get.mockResolvedValue({ ...linked(), sync_available: false })
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.text()).toContain('clinic connection is unavailable')
    expect(wrapper.get('[data-testid="reaform-booked"]').text()).toContain('Booked')
    expect(wrapper.find('[data-testid="reaform-refresh"]').exists()).toBe(true)
    service.get.mockResolvedValue(state({ sync_available: false }))
    const unlinked = mountCard({ leadId: 'lead-2' })
    await flushPromises()
    expect(unlinked.find('[data-testid="reaform-search"]').exists()).toBe(false)
    expect(unlinked.text()).toContain('Check connection')
  })

  it('distinguishes an unavailable connection from a disabled pilot and supports retry', async () => {
    service.get.mockRejectedValueOnce(new Error('unavailable'))
    const wrapper = mountCard()
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('could not be loaded')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('Pilot clinic')
  })

  it('does not display results from an earlier patient query', async () => {
    let resolve!: (value: typeof patient[]) => void
    service.patients.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountCard()
    await flushPromises()
    await wrapper.get('[data-testid="reaform-patient-query"]').setValue('Synthetic')
    await wrapper.get('[data-testid="reaform-search"]').trigger('click')
    await wrapper.get('[data-testid="reaform-patient-query"]').setValue('Different')
    resolve([patient])
    await flushPromises()
    expect(wrapper.text()).not.toContain(patient.display_name)
    expect(wrapper.find('input[type="radio"]').exists()).toBe(false)
  })

  it('does not offer a slot response after the chosen date has changed', async () => {
    let resolve!: (value: typeof available[]) => void
    service.availability.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountCard()
    await flushPromises()
    await choosePatient(wrapper)
    await wrapper.get('[data-testid="reaform-date"]').setValue(available.date)
    await wrapper.get('[data-testid="reaform-find-times"]').trigger('click')
    await wrapper.get('[data-testid="reaform-date"]').setValue('2026-12-21')
    resolve([available])
    await flushPromises()
    expect(wrapper.find('[data-testid="reaform-slot"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reaform-book"]').exists()).toBe(false)
  })

  it('does not overwrite a freshly checked appointment with an older automatic update', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let resolve!: (value: ReaformLeadState) => void
    service.get.mockResolvedValueOnce(linked()).mockReturnValueOnce(new Promise((done) => { resolve = done }))
    service.refresh.mockResolvedValue(linked('checked_in'))
    const wrapper = mountCard()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(60_000)
    await wrapper.get('[data-testid="reaform-refresh"]').trigger('click')
    await flushPromises()
    resolve(linked())
    await flushPromises()
    expect(wrapper.get('[data-testid="reaform-booked"]').text()).toContain('Arrived')
  })

  it('discards a previous lead’s in-flight booking response after the selected lead changes', async () => {
    let resolve!: (value: ReaformLeadState) => void
    service.book.mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mountCard()
    await prepareBooking(wrapper)
    await wrapper.get('[data-testid="reaform-book"]').trigger('click')
    await wrapper.setProps({ leadId: 'lead-2' })
    await flushPromises()
    resolve(linked())
    await flushPromises()
    expect(service.get).toHaveBeenLastCalledWith('lead-2')
    expect(wrapper.find('[data-testid="reaform-booked"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="reaform-patient-query"]').element).toHaveProperty('value', '')
  })
})
