<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  Activity,
  ArrowRight,
  Bot,
  Braces,
  CheckCircle2,
  CircleDashed,
  Clock3,
  CopyPlus,
  FileClock,
  Loader2,
  MoreHorizontal,
  PauseCircle,
  Plus,
  RefreshCw,
  Search,
  ShieldCheck,
  Trash2,
  Workflow,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import ConfirmDialog from '@/components/shared/ConfirmDialog.vue'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { useAppToast } from '@/composables/useAppToast'
import { getErrorMessage } from '@/lib/api-utils'
import { automationTemplates } from '@/lib/automationTemplates'
import {
  automationPolicyService,
  type AutomationPolicy,
  type AutomationPolicyStatus,
} from '@/services/automationPolicies'
import { useAuthStore } from '@/stores/auth'

type PolicyFilter = 'all' | Exclude<AutomationPolicyStatus, 'archived'>

const router = useRouter()
const toast = useAppToast()
const authStore = useAuthStore()
const policies = ref<AutomationPolicy[]>([])
const loading = ref(true)
const deleting = ref(false)
const search = ref('')
const filter = ref<PolicyFilter>('all')
const deleteTarget = ref<AutomationPolicy | null>(null)

const canWrite = computed(() => authStore.hasPermission('crm.automations', 'write'))
const canDelete = computed(() => authStore.hasPermission('crm.automations', 'delete'))

const statusCounts = computed(() => ({
  all: policies.value.length,
  active: policies.value.filter((policy) => policy.status === 'active').length,
  draft: policies.value.filter((policy) => policy.status === 'draft').length,
  paused: policies.value.filter((policy) => policy.status === 'paused').length,
}))

const visiblePolicies = computed(() => {
  const query = search.value.trim().toLowerCase()
  return policies.value.filter((policy) => {
    if (filter.value !== 'all' && policy.status !== filter.value) return false
    if (!query) return true
    return `${policy.name} ${policy.description ?? ''} ${policy.trigger_event_types?.join(' ') ?? ''}`
      .toLowerCase()
      .includes(query)
  })
})

function unwrapPolicies(response: any): AutomationPolicy[] {
  const data = response?.data
  const candidates = [
    data?.data?.automation_policies,
    data?.automation_policies,
    data?.data,
    data,
  ]
  return candidates.find(Array.isArray) ?? []
}

async function loadPolicies() {
  loading.value = true
  try {
    const response = await automationPolicyService.list({ limit: 100 })
    policies.value = unwrapPolicies(response)
  } catch (error) {
    toast.error('Automation policies could not be loaded', getErrorMessage(error))
  } finally {
    loading.value = false
  }
}

function useTemplate(key: string) {
  router.push({ path: '/crm/automations/new', query: { template: key } })
}

function duplicate(policy: AutomationPolicy) {
  router.push({
    path: '/crm/automations/new',
    query: { source: policy.id },
  })
}

async function confirmDelete() {
  if (!deleteTarget.value) return
  deleting.value = true
  try {
    await automationPolicyService.remove(deleteTarget.value.id, deleteTarget.value.version)
    policies.value = policies.value.filter((policy) => policy.id !== deleteTarget.value?.id)
    toast.success('Draft policy deleted')
    deleteTarget.value = null
  } catch (error) {
    toast.error('Policy was not deleted', getErrorMessage(error))
  } finally {
    deleting.value = false
  }
}

function formatRelative(value?: string) {
  if (!value) return 'Not saved yet'
  const timestamp = new Date(value).getTime()
  const diffMinutes = Math.max(0, Math.round((Date.now() - timestamp) / 60000))
  if (diffMinutes < 1) return 'Just now'
  if (diffMinutes < 60) return `${diffMinutes}m ago`
  const diffHours = Math.round(diffMinutes / 60)
  if (diffHours < 24) return `${diffHours}h ago`
  const diffDays = Math.round(diffHours / 24)
  if (diffDays < 7) return `${diffDays}d ago`
  return new Intl.DateTimeFormat('en-MY', {
    day: 'numeric',
    month: 'short',
    year: 'numeric',
  }).format(new Date(value))
}

function eventLabel(policy: AutomationPolicy) {
  const values =
    policy.trigger_event_types?.length
      ? policy.trigger_event_types
      : []
  if (!values.length) return 'Trigger not configured'
  if (values.length === 1) return values[0]
  return `${values[0]} +${values.length - 1}`
}

function statusVariant(status: AutomationPolicyStatus) {
  if (status === 'active') return 'success'
  if (status === 'paused') return 'warning'
  return 'secondary'
}

onMounted(loadPolicies)
</script>

<template>
  <div class="automation-index h-full overflow-y-auto bg-[#090c0d] light:bg-[#f4f6f3]">
    <PageHeader
      title="Automation Studio"
      description="Turn customer moments into accountable work — safely, visibly, and without code."
      :icon="Workflow"
      icon-gradient="bg-gradient-to-br from-lime-300 to-emerald-600 shadow-lime-400/20"
    >
      <template #actions>
        <div class="flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            class="gap-2"
            :disabled="loading"
            aria-label="Refresh automation policies"
            @click="loadPolicies"
          >
            <RefreshCw class="h-3.5 w-3.5" :class="{ 'animate-spin': loading }" />
            <span class="hidden sm:inline">Refresh</span>
          </Button>
          <Button
            v-if="canWrite"
            size="sm"
            class="gap-2 bg-lime-300 text-[#12160d] hover:bg-lime-200"
            @click="router.push('/crm/automations/new')"
          >
            <Plus class="h-4 w-4" />
            New policy
          </Button>
        </div>
      </template>
    </PageHeader>

    <main data-testid="automation-policy-list" class="mx-auto max-w-[1500px] space-y-7 p-4 sm:p-6 lg:p-8">
      <section
        class="relative overflow-hidden rounded-[28px] border border-lime-200/10 bg-[#0d1210] p-5 shadow-2xl shadow-black/20 light:border-lime-900/10 light:bg-white sm:p-7"
        aria-labelledby="starter-policies-title"
      >
        <div
          class="pointer-events-none absolute -right-20 -top-32 h-72 w-72 rounded-full bg-lime-300/[0.07] blur-3xl"
          aria-hidden="true"
        />
        <div class="relative flex flex-col justify-between gap-4 md:flex-row md:items-end">
          <div>
            <div class="flex items-center gap-2 text-lime-200 light:text-lime-800">
              <ShieldCheck class="h-4 w-4" />
              <p class="text-[10px] font-bold uppercase tracking-[0.24em]">Safe starting points</p>
            </div>
            <h2 id="starter-policies-title" class="mt-2 text-2xl font-semibold tracking-tight text-white light:text-gray-950">
              Start with a care policy, not a blank canvas.
            </h2>
            <p class="mt-2 max-w-2xl text-sm leading-6 text-white/45 light:text-gray-600">
              Every starter creates internal follow-up tasks only. Customer messages stay locked until a future reviewed release.
            </p>
          </div>
          <div class="flex items-center gap-2 rounded-full border border-white/[0.08] bg-black/20 px-3 py-2 text-[11px] text-white/45 light:border-black/10 light:bg-gray-50 light:text-gray-600">
            <CheckCircle2 class="h-3.5 w-3.5 text-emerald-300 light:text-emerald-700" />
            Preview before publish
          </div>
        </div>

        <div class="relative mt-6 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <button
            v-for="template in automationTemplates"
            :key="template.key"
            :data-testid="`automation-template-${template.key}`"
            type="button"
            class="group min-h-[170px] cursor-pointer rounded-2xl border border-white/[0.08] bg-white/[0.025] p-4 text-left outline-none transition duration-200 hover:-translate-y-0.5 hover:border-lime-200/25 hover:bg-white/[0.05] focus-visible:ring-2 focus-visible:ring-lime-200 light:border-gray-200 light:bg-gray-50 light:hover:bg-white"
            :disabled="!canWrite"
            :aria-label="`Use ${template.name} starter policy`"
            @click="useTemplate(template.key)"
          >
            <div class="flex items-start justify-between gap-3">
              <span
                class="flex h-10 w-10 items-center justify-center rounded-xl border"
                :class="{
                  'border-cyan-300/15 bg-cyan-300/10 text-cyan-200 light:text-cyan-800': template.tone === 'cyan',
                  'border-amber-300/15 bg-amber-300/10 text-amber-200 light:text-amber-800': template.tone === 'amber',
                  'border-emerald-300/15 bg-emerald-300/10 text-emerald-200 light:text-emerald-800': template.tone === 'emerald',
                  'border-rose-300/15 bg-rose-300/10 text-rose-200 light:text-rose-800': template.tone === 'rose',
                }"
              >
                <component :is="template.icon" class="h-5 w-5" />
              </span>
              <ArrowRight class="h-4 w-4 text-white/20 transition group-hover:translate-x-0.5 group-hover:text-lime-200 light:text-gray-400" />
            </div>
            <h3 class="mt-4 text-sm font-semibold text-white light:text-gray-900">{{ template.name }}</h3>
            <p class="mt-1.5 line-clamp-2 text-xs leading-5 text-white/40 light:text-gray-600">
              {{ template.description }}
            </p>
            <p class="mt-3 flex items-center gap-1.5 text-[10px] font-semibold uppercase tracking-[0.14em] text-white/30 light:text-gray-500">
              <Activity class="h-3 w-3" />
              {{ template.eventLabel }}
            </p>
          </button>
        </div>
      </section>

      <section aria-labelledby="policies-title">
        <div class="mb-4 flex flex-col justify-between gap-4 lg:flex-row lg:items-end">
          <div>
            <p class="text-[10px] font-bold uppercase tracking-[0.22em] text-lime-200/70 light:text-lime-800">
              Policy library
            </p>
            <h2 id="policies-title" class="mt-1 text-xl font-semibold text-white light:text-gray-950">
              Your operating rules
            </h2>
          </div>

          <div class="flex flex-col gap-3 sm:flex-row sm:items-center">
            <div class="relative">
              <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-white/30 light:text-gray-400" />
              <input
                v-model="search"
                type="search"
                aria-label="Search automation policies"
                placeholder="Search policies"
                class="h-10 w-full rounded-xl border border-white/10 bg-white/[0.035] pl-9 pr-3 text-sm text-white outline-none transition placeholder:text-white/25 focus:border-lime-200/40 focus:ring-2 focus:ring-lime-200/10 light:border-gray-200 light:bg-white light:text-gray-900 sm:w-64"
              />
            </div>
            <div class="flex rounded-xl border border-white/[0.08] bg-black/20 p-1 light:border-gray-200 light:bg-white">
              <button
                v-for="item in (['all', 'active', 'draft', 'paused'] as PolicyFilter[])"
                :key="item"
                type="button"
                class="cursor-pointer rounded-lg px-3 py-1.5 text-xs font-medium capitalize outline-none transition focus-visible:ring-2 focus-visible:ring-lime-200"
                :class="filter === item
                  ? 'bg-white text-gray-950 shadow-sm'
                  : 'text-white/40 hover:text-white light:text-gray-500 light:hover:text-gray-900'"
                @click="filter = item"
              >
                {{ item }}
                <span class="ml-1 opacity-50">{{ statusCounts[item] }}</span>
              </button>
            </div>
          </div>
        </div>

        <div
          v-if="loading"
          class="flex min-h-64 items-center justify-center rounded-2xl border border-white/[0.08] bg-white/[0.02] light:border-gray-200 light:bg-white"
        >
          <Loader2 class="h-6 w-6 animate-spin text-lime-200 light:text-lime-800" />
        </div>

        <div
          v-else-if="!visiblePolicies.length"
          class="flex min-h-64 flex-col items-center justify-center rounded-2xl border border-dashed border-white/10 bg-white/[0.018] px-6 text-center light:border-gray-300 light:bg-white"
        >
          <span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-lime-200/[0.08] text-lime-200 light:bg-lime-100 light:text-lime-800">
            <Braces class="h-6 w-6" />
          </span>
          <h3 class="mt-4 font-semibold text-white light:text-gray-900">
            {{ policies.length ? 'No policies match this view' : 'No policies yet' }}
          </h3>
          <p class="mt-1 max-w-md text-sm text-white/40 light:text-gray-600">
            {{ policies.length
              ? 'Try a different status or search.'
              : 'Choose a safe starter above or create a policy from a blank canvas.' }}
          </p>
          <Button
            v-if="canWrite && !policies.length"
            data-testid="automation-create-blank"
            class="mt-5 gap-2 bg-lime-300 text-[#12160d] hover:bg-lime-200"
            @click="router.push('/crm/automations/new')"
          >
            <Plus class="h-4 w-4" />
            Create blank policy
          </Button>
        </div>

        <div v-else class="grid gap-3 md:grid-cols-2 2xl:grid-cols-3">
          <article
            v-for="policy in visiblePolicies"
            :key="policy.id"
            class="group relative overflow-hidden rounded-2xl border border-white/[0.08] bg-[#101315] p-5 transition duration-200 hover:border-lime-200/20 light:border-gray-200 light:bg-white"
          >
            <div class="flex items-start gap-3">
              <RouterLink
                :to="`/crm/automations/${policy.id}`"
                class="flex min-w-0 flex-1 items-start gap-3 rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-lime-200"
              >
                <span
                  class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-xl"
                  :class="policy.status === 'active'
                    ? 'bg-emerald-300/10 text-emerald-300 light:bg-emerald-100 light:text-emerald-700'
                    : policy.status === 'paused'
                      ? 'bg-amber-300/10 text-amber-300 light:bg-amber-100 light:text-amber-700'
                      : 'bg-white/[0.06] text-white/45 light:bg-gray-100 light:text-gray-600'"
                >
                  <Bot v-if="policy.status === 'active'" class="h-4 w-4" />
                  <PauseCircle v-else-if="policy.status === 'paused'" class="h-4 w-4" />
                  <CircleDashed v-else class="h-4 w-4" />
                </span>
                <span class="min-w-0">
                  <span class="flex flex-wrap items-center gap-2">
                    <span class="truncate text-sm font-semibold text-white light:text-gray-900">{{ policy.name }}</span>
                    <Badge :variant="statusVariant(policy.status)" class="capitalize">{{ policy.status }}</Badge>
                  </span>
                  <span class="mt-1.5 line-clamp-2 min-h-[36px] text-xs leading-[18px] text-white/40 light:text-gray-600">
                    {{ policy.description || 'No description yet.' }}
                  </span>
                </span>
              </RouterLink>

              <DropdownMenu>
                <DropdownMenuTrigger as-child>
                  <Button
                    variant="ghost"
                    size="icon"
                    class="h-8 w-8 shrink-0 text-white/40 light:text-gray-500"
                    :aria-label="`Actions for ${policy.name}`"
                  >
                    <MoreHorizontal class="h-4 w-4" />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem @click="router.push(`/crm/automations/${policy.id}`)">
                    <Workflow class="mr-2 h-4 w-4" />
                    Open builder
                  </DropdownMenuItem>
                  <DropdownMenuItem v-if="canWrite" @click="duplicate(policy)">
                    <CopyPlus class="mr-2 h-4 w-4" />
                    Duplicate
                  </DropdownMenuItem>
                  <template v-if="canDelete && policy.status === 'draft'">
                    <DropdownMenuSeparator />
                    <DropdownMenuItem
                      class="text-destructive focus:text-destructive"
                      @click="deleteTarget = policy"
                    >
                      <Trash2 class="mr-2 h-4 w-4" />
                      Delete draft
                    </DropdownMenuItem>
                  </template>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>

            <div class="mt-5 grid grid-cols-3 gap-px overflow-hidden rounded-xl border border-white/[0.06] bg-white/[0.06] light:border-gray-200 light:bg-gray-200">
              <div class="bg-[#0c0f11] px-3 py-2.5 light:bg-gray-50">
                <p class="text-[9px] uppercase tracking-wider text-white/25 light:text-gray-500">Trigger</p>
                <p class="mt-1 truncate text-[11px] font-medium text-cyan-200 light:text-cyan-800" :title="eventLabel(policy)">
                  {{ eventLabel(policy) }}
                </p>
              </div>
              <div class="bg-[#0c0f11] px-3 py-2.5 light:bg-gray-50">
                <p class="text-[9px] uppercase tracking-wider text-white/25 light:text-gray-500">Version</p>
                <p class="mt-1 text-[11px] font-medium text-white/65 light:text-gray-700">
                  {{ policy.active_version_number ? `Live v${policy.active_version_number}` : `Draft v${policy.version}` }}
                </p>
              </div>
              <div class="bg-[#0c0f11] px-3 py-2.5 light:bg-gray-50">
                <p class="text-[9px] uppercase tracking-wider text-white/25 light:text-gray-500">Steps</p>
                <p class="mt-1 text-[11px] font-medium text-white/65 light:text-gray-700">
                  {{ policy.graph?.nodes?.length ?? 0 }}
                </p>
              </div>
            </div>

            <div class="mt-4 flex items-center justify-between text-[10px] text-white/30 light:text-gray-500">
              <span class="flex items-center gap-1.5">
                <Clock3 class="h-3 w-3" />
                Updated {{ formatRelative(policy.updated_at) }}
              </span>
              <RouterLink
                :to="`/crm/automations/${policy.id}`"
                class="flex items-center gap-1 text-lime-200/70 transition hover:text-lime-200 light:text-lime-800"
              >
                {{ canWrite ? 'Edit' : 'View' }}
                <ArrowRight class="h-3 w-3" />
              </RouterLink>
            </div>
          </article>
        </div>
      </section>

      <section class="grid gap-3 md:grid-cols-3" aria-label="Automation safety commitments">
        <div class="rounded-2xl border border-white/[0.07] bg-white/[0.02] p-4 light:border-gray-200 light:bg-white">
          <ShieldCheck class="h-4 w-4 text-emerald-300 light:text-emerald-700" />
          <p class="mt-3 text-xs font-semibold text-white light:text-gray-900">Task-first execution</p>
          <p class="mt-1 text-[11px] leading-5 text-white/35 light:text-gray-600">Policies create accountable team work, never hidden outbound messages.</p>
        </div>
        <div class="rounded-2xl border border-white/[0.07] bg-white/[0.02] p-4 light:border-gray-200 light:bg-white">
          <FileClock class="h-4 w-4 text-violet-300 light:text-violet-700" />
          <p class="mt-3 text-xs font-semibold text-white light:text-gray-900">Immutable live versions</p>
          <p class="mt-1 text-[11px] leading-5 text-white/35 light:text-gray-600">Every publish freezes a version for audit and repeatable execution.</p>
        </div>
        <div class="rounded-2xl border border-white/[0.07] bg-white/[0.02] p-4 light:border-gray-200 light:bg-white">
          <Activity class="h-4 w-4 text-cyan-300 light:text-cyan-700" />
          <p class="mt-3 text-xs font-semibold text-white light:text-gray-900">Visible run history</p>
          <p class="mt-1 text-[11px] leading-5 text-white/35 light:text-gray-600">See which event matched, what waited, and which follow-up was created.</p>
        </div>
      </section>
    </main>

    <ConfirmDialog
      :open="Boolean(deleteTarget)"
      title="Delete draft policy?"
      :description="`“${deleteTarget?.name ?? 'This policy'}” will be permanently removed.`"
      confirm-label="Delete draft"
      variant="destructive"
      :is-submitting="deleting"
      @update:open="(value) => !value && (deleteTarget = null)"
      @confirm="confirmDelete"
    />
  </div>
</template>

<style scoped>
@media (prefers-reduced-motion: reduce) {
  .automation-index *,
  .automation-index *::before,
  .automation-index *::after {
    scroll-behavior: auto !important;
    transition-duration: 0.01ms !important;
    animation-duration: 0.01ms !important;
  }
}
</style>
