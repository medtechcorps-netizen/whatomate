<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { AlertTriangle, Loader2, Receipt, RefreshCw } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { useAppToast } from '@/composables/useAppToast'
import { getErrorMessage, unwrapResponse } from '@/lib/api-utils'
import { formatCurrencyMinorUnits } from '@/lib/currency'
import {
  commerceService,
  type CommerceInvoice,
  type CRMLead,
  type PackageDefinition,
} from '@/services/productSuite'

type InvoiceMode = 'custom' | 'package'

const props = withDefaults(
  defineProps<{
    open: boolean
    contact: { id: string; name: string } | null
    lead?: CRMLead | null
    canSellPackages?: boolean
    source?: string
  }>(),
  {
    lead: null,
    canSellPackages: false,
    source: 'customer_workspace',
  },
)

const emit = defineEmits<{
  'update:open': [value: boolean]
  created: [invoice: CommerceInvoice]
}>()

const CURRENCIES = ['MYR', 'SGD', 'USD']

const toast = useAppToast()
const saving = ref(false)
const attempted = ref(false)
const mode = ref<InvoiceMode>('custom')
const idempotencyKey = ref('')
const draft = ref({
  description: '',
  amount: '',
  currency: 'MYR',
  due_date: '',
  package_definition_id: '',
})

const packages = ref<PackageDefinition[]>([])
const packagesLoaded = ref(false)
const packagesLoading = ref(false)
const packagesError = ref('')

function todayInputValue() {
  const now = new Date()
  const local = new Date(now.getTime() - now.getTimezoneOffset() * 60_000)
  return local.toISOString().slice(0, 10)
}

const minDueDate = ref(todayInputValue())

function resetDraft() {
  const lead = props.lead
  draft.value = {
    description: lead?.title ?? '',
    amount: lead && lead.value_minor > 0 ? (lead.value_minor / 100).toFixed(2) : '',
    currency: lead?.currency || 'MYR',
    due_date: '',
    package_definition_id: '',
  }
  mode.value = 'custom'
  attempted.value = false
  minDueDate.value = todayInputValue()
  idempotencyKey.value = crypto.randomUUID()
}

watch(
  () => props.open,
  (isOpen) => {
    if (isOpen) resetDraft()
  },
  { immediate: true },
)

const description = computed(() => {
  const name = props.contact?.name || 'this customer'
  const leadTitle = props.lead?.title?.trim()
  return `For ${name}${leadTitle ? ` - ${leadTitle}` : ''}`
})

const activePackages = computed(() => packages.value.filter((item) => item.is_active !== false))
const selectedPackage = computed(
  () => activePackages.value.find((item) => item.id === draft.value.package_definition_id) ?? null,
)

function packageLabel(item: PackageDefinition) {
  return `${item.name} - ${formatCurrencyMinorUnits(item.currency || 'MYR', item.price_minor)}`
}

async function loadPackages() {
  if (packagesLoading.value) return
  packagesLoading.value = true
  packagesError.value = ''
  try {
    packages.value = await commerceService.allPackages({ active: true })
    packagesLoaded.value = true
  } catch (error) {
    packagesError.value = getErrorMessage(error, 'Packages could not be loaded')
  } finally {
    packagesLoading.value = false
  }
}

function chooseMode(next: InvoiceMode) {
  if (saving.value) return
  mode.value = next
  attempted.value = false
  if (next === 'package' && !packagesLoaded.value) void loadPackages()
}

const amountMinor = computed(() => Math.round(Number(draft.value.amount) * 100))

const errors = computed(() => {
  const result: { description?: string; amount?: string; package?: string; due?: string } = {}
  if (mode.value === 'custom') {
    if (!draft.value.description.trim()) result.description = 'Enter what this invoice is for.'
    const raw = String(draft.value.amount ?? '').trim()
    if (!raw) result.amount = 'Enter the amount.'
    else if (!Number.isFinite(amountMinor.value) || amountMinor.value <= 0)
      result.amount = 'Enter an amount greater than zero.'
  } else if (!selectedPackage.value) {
    result.package = 'Choose a package.'
  }
  if (draft.value.due_date && draft.value.due_date < minDueDate.value) {
    result.due = 'The due date cannot be in the past.'
  }
  return result
})

const hasErrors = computed(() => Object.keys(errors.value).length > 0)

function dueAtIso() {
  if (!draft.value.due_date) return undefined
  const date = new Date(`${draft.value.due_date}T23:59:59`)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

function invoiceMetadata() {
  const metadata: Record<string, unknown> = { source: props.source }
  if (props.lead?.id) metadata.lead_id = props.lead.id
  return metadata
}

function setOpen(value: boolean) {
  if (!value && saving.value) return
  emit('update:open', value)
}

async function submit() {
  if (saving.value || !props.contact?.id) return
  attempted.value = true
  if (hasErrors.value) return
  if (!idempotencyKey.value) idempotencyKey.value = crypto.randomUUID()

  saving.value = true
  try {
    let invoice: CommerceInvoice | undefined
    if (mode.value === 'package' && props.canSellPackages) {
      const response = await commerceService.sellPackage({
        contact_id: props.contact.id,
        package_definition_id: draft.value.package_definition_id,
        due_at: dueAtIso(),
        idempotency_key: idempotencyKey.value,
        metadata: invoiceMetadata(),
      })
      invoice = unwrapResponse<{ invoice?: CommerceInvoice }>(response)?.invoice
    } else {
      const response = await commerceService.createInvoice({
        contact_id: props.contact.id,
        currency: draft.value.currency,
        due_at: dueAtIso(),
        idempotency_key: idempotencyKey.value,
        lines: [
          {
            description: draft.value.description.trim(),
            quantity: 1,
            unit_amount_minor: amountMinor.value,
          },
        ],
        metadata: invoiceMetadata(),
      })
      invoice = unwrapResponse<CommerceInvoice>(response)
    }
    if (!invoice?.id) throw new Error('The server did not return the new invoice.')
    toast.success(`Invoice ${invoice.invoice_number} created`)
    emit('created', invoice)
    emit('update:open', false)
  } catch (error) {
    toast.error('Invoice was not created', getErrorMessage(error))
  } finally {
    saving.value = false
  }
}

const inputClass =
  'h-11 w-full rounded-xl border border-white/10 bg-black/20 px-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 disabled:opacity-60 light:border-slate-300 light:bg-white'
const toggleBase =
  'flex min-h-10 flex-1 items-center justify-center rounded-lg px-3 text-sm font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-cyan-300 disabled:opacity-60'
</script>

<template>
  <Dialog :open="open" @update:open="setOpen">
    <DialogContent
      data-testid="invoice-quick-dialog"
      class="max-h-[92vh] w-[calc(100vw-1.5rem)] max-w-lg overflow-y-auto border-white/10 bg-[#111419] text-white light:border-slate-200 light:bg-white light:text-slate-950"
    >
      <DialogHeader>
        <DialogTitle class="flex items-center gap-2">
          <Receipt class="h-4 w-4 text-cyan-300 light:text-cyan-700" aria-hidden="true" />
          Create invoice
        </DialogTitle>
        <DialogDescription>{{ description }}</DialogDescription>
      </DialogHeader>

      <form id="invoice-quick-form" class="space-y-4" novalidate @submit.prevent="submit">
        <fieldset :disabled="saving" class="contents">
          <div
            v-if="canSellPackages"
            role="group"
            aria-label="Invoice type"
            class="flex gap-1 rounded-xl border border-white/10 bg-black/20 p-1 light:border-slate-200 light:bg-slate-100"
          >
            <button
              type="button"
              :aria-pressed="mode === 'custom'"
              :class="[
                toggleBase,
                mode === 'custom'
                  ? 'bg-cyan-400 text-black'
                  : 'text-white/65 hover:text-white light:text-slate-600 light:hover:text-slate-950',
              ]"
              @click="chooseMode('custom')"
            >
              Custom amount
            </button>
            <button
              type="button"
              :aria-pressed="mode === 'package'"
              :class="[
                toggleBase,
                mode === 'package'
                  ? 'bg-cyan-400 text-black'
                  : 'text-white/65 hover:text-white light:text-slate-600 light:hover:text-slate-950',
              ]"
              @click="chooseMode('package')"
            >
              Package
            </button>
          </div>

          <template v-if="mode === 'custom' || !canSellPackages">
            <label class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Description</span>
              <input
                v-model="draft.description"
                maxlength="255"
                :aria-invalid="attempted && errors.description ? 'true' : undefined"
                :class="inputClass"
                placeholder="What is this invoice for?"
              />
              <span v-if="attempted && errors.description" class="mt-1 block text-xs text-rose-300 light:text-rose-700">
                {{ errors.description }}
              </span>
            </label>
            <div class="grid gap-3 sm:grid-cols-[1fr_8rem]">
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Amount</span>
                <input
                  v-model="draft.amount"
                  type="number"
                  min="0.01"
                  step="0.01"
                  inputmode="decimal"
                  placeholder="0.00"
                  :aria-invalid="attempted && errors.amount ? 'true' : undefined"
                  :class="inputClass"
                />
                <span v-if="attempted && errors.amount" class="mt-1 block text-xs text-rose-300 light:text-rose-700">
                  {{ errors.amount }}
                </span>
              </label>
              <label class="block">
                <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Currency</span>
                <select v-model="draft.currency" :class="[inputClass, 'bg-[#15191f]']">
                  <option v-for="currency in CURRENCIES" :key="currency" :value="currency">{{ currency }}</option>
                </select>
              </label>
            </div>
          </template>

          <template v-else>
            <div
              v-if="packagesLoading"
              class="flex items-center gap-2 rounded-xl border border-white/10 px-3 py-3 text-xs text-white/55 light:border-slate-200 light:text-slate-600"
              aria-live="polite"
            >
              <Loader2 class="h-4 w-4 animate-spin" />
              Loading packages
            </div>
            <div
              v-else-if="packagesError"
              role="alert"
              class="flex items-start justify-between gap-3 rounded-xl border border-rose-300/20 bg-rose-300/[0.07] p-3 text-xs leading-5 text-rose-100 light:text-rose-800"
            >
              <span class="flex items-start gap-2">
                <AlertTriangle class="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
                {{ packagesError }}
              </span>
              <Button type="button" size="sm" variant="outline" class="gap-1.5" @click="loadPackages">
                <RefreshCw class="h-3.5 w-3.5" />
                Try again
              </Button>
            </div>
            <p
              v-else-if="packagesLoaded && !activePackages.length"
              class="rounded-xl border border-white/10 p-3 text-xs text-white/55 light:border-slate-200 light:text-slate-600"
            >
              No active packages yet. Use a custom amount instead.
            </p>
            <label v-else class="block">
              <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">Package</span>
              <select
                v-model="draft.package_definition_id"
                :aria-invalid="attempted && errors.package ? 'true' : undefined"
                :class="[inputClass, 'bg-[#15191f]']"
              >
                <option value="" disabled>Choose a package</option>
                <option v-for="item in activePackages" :key="item.id" :value="item.id">
                  {{ packageLabel(item) }}
                </option>
              </select>
              <span v-if="attempted && errors.package" class="mt-1 block text-xs text-rose-300 light:text-rose-700">
                {{ errors.package }}
              </span>
            </label>
          </template>

          <label class="block">
            <span class="mb-1.5 block text-xs font-medium text-white/60 light:text-slate-700">
              Due date <span class="font-normal text-white/40 light:text-slate-500">(optional)</span>
            </span>
            <input
              v-model="draft.due_date"
              type="date"
              :min="minDueDate"
              :aria-invalid="attempted && errors.due ? 'true' : undefined"
              :class="[inputClass, 'bg-[#15191f]']"
            />
            <span v-if="attempted && errors.due" class="mt-1 block text-xs text-rose-300 light:text-rose-700">
              {{ errors.due }}
            </span>
          </label>
        </fieldset>
      </form>

      <DialogFooter class="gap-2">
        <Button variant="outline" :disabled="saving" @click="setOpen(false)">Cancel</Button>
        <Button
          type="submit"
          form="invoice-quick-form"
          data-testid="invoice-quick-submit"
          class="gap-2 bg-cyan-400 text-black hover:bg-cyan-300"
          :disabled="saving || !contact?.id || (mode === 'package' && canSellPackages && packagesLoading)"
        >
          <Loader2 v-if="saving" class="h-4 w-4 animate-spin" />
          <Receipt v-else class="h-4 w-4" />
          Create invoice
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
