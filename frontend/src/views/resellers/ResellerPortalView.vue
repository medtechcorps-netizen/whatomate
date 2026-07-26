<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import {
  Building2,
  Check,
  Gauge,
  Globe2,
  Mail,
  MessageSquareText,
  Palette,
  Plus,
  RefreshCw,
  ShieldCheck,
  Smartphone,
  Trash2,
  Users,
} from 'lucide-vue-next'
import { toast } from 'vue-sonner'
import { useAuthStore } from '@/stores/auth'
import {
  organizationsService,
  resellersService,
  type Reseller,
  type ResellerMember,
  type ResellerUsage,
} from '@/services/api'
import { getErrorMessage, unwrapItemResponse, unwrapListResponse } from '@/lib/api-utils'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

const authStore = useAuthStore()
const isPlatformOwner = computed(() => authStore.user?.is_super_admin === true)

const resellers = ref<Reseller[]>([])
const selectedId = ref('')
const usage = ref<ResellerUsage | null>(null)
const members = ref<ResellerMember[]>([])
const loading = ref(true)
const detailLoading = ref(false)
const activeTab = ref('portfolio')

const selected = computed(() => resellers.value.find(item => item.id === selectedId.value) ?? null)
const organizationCapacity = computed(() => {
  if (!usage.value?.max_organizations) return 0
  return Math.min(100, Math.round((usage.value.organization_count / usage.value.max_organizations) * 100))
})

const createOpen = ref(false)
const createSubmitting = ref(false)
const createForm = reactive({
  name: '',
  brand_name: '',
  workspace_name: '',
  support_email: '',
  plan: 'starter' as Reseller['plan'],
})

const organizationOpen = ref(false)
const organizationSubmitting = ref(false)
const organizationName = ref('')

const memberOpen = ref(false)
const memberSubmitting = ref(false)
const memberForm = reactive({
  email: '',
  role: 'admin' as ResellerMember['role'],
})

const brandingSubmitting = ref(false)
const brandForm = reactive({
  name: '',
  brand_name: '',
  logo_url: '',
  primary_color: '#0f766e',
  accent_color: '#f59e0b',
  support_email: '',
  custom_domain: '',
  status: 'active' as Reseller['status'],
  plan: 'starter' as Reseller['plan'],
  max_organizations: 10,
})

function hydrateBrandForm(reseller: Reseller | null) {
  if (!reseller) return
  brandForm.name = reseller.name
  brandForm.brand_name = reseller.brand_name || reseller.name
  brandForm.logo_url = reseller.logo_url || ''
  brandForm.primary_color = reseller.primary_color || '#0f766e'
  brandForm.accent_color = reseller.accent_color || '#f59e0b'
  brandForm.support_email = reseller.support_email || ''
  brandForm.custom_domain = reseller.custom_domain || ''
  brandForm.status = reseller.status
  brandForm.plan = reseller.plan
  brandForm.max_organizations = reseller.max_organizations
}

async function loadResellers(preferredId?: string) {
  loading.value = true
  try {
    const response = await resellersService.list()
    resellers.value = unwrapListResponse<Reseller>(response, 'resellers')
    const targetId = preferredId && resellers.value.some(item => item.id === preferredId)
      ? preferredId
      : selectedId.value && resellers.value.some(item => item.id === selectedId.value)
        ? selectedId.value
        : resellers.value[0]?.id || ''
    selectedId.value = targetId
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to load partner portfolios'))
  } finally {
    loading.value = false
  }
}

async function loadSelected() {
  if (!selectedId.value) {
    usage.value = null
    members.value = []
    return
  }
  detailLoading.value = true
  try {
    const [usageResponse, memberResponse] = await Promise.all([
      resellersService.usage(selectedId.value),
      resellersService.members(selectedId.value),
    ])
    usage.value = unwrapItemResponse<ResellerUsage>(usageResponse)
    members.value = unwrapListResponse<ResellerMember>(memberResponse, 'members')
    hydrateBrandForm(selected.value)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to load portfolio details'))
  } finally {
    detailLoading.value = false
  }
}

watch(selectedId, loadSelected)
onMounted(loadResellers)

async function createReseller() {
  if (!createForm.name.trim()) {
    toast.error('Company name is required')
    return
  }
  createSubmitting.value = true
  try {
    const response = await resellersService.create({
      name: createForm.name.trim(),
      brand_name: createForm.brand_name.trim() || createForm.name.trim(),
      workspace_name: createForm.workspace_name.trim() || undefined,
      support_email: createForm.support_email.trim() || undefined,
      plan: createForm.plan,
    })
    const result = unwrapItemResponse<{ reseller: Reseller }>(response)
    createOpen.value = false
    Object.assign(createForm, {
      name: '',
      brand_name: '',
      workspace_name: '',
      support_email: '',
      plan: 'starter',
    })
    toast.success('Partner and private workspace created')
    await loadResellers(result.reseller.id)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to create partner'))
  } finally {
    createSubmitting.value = false
  }
}

async function createOrganization() {
  if (!selected.value || !organizationName.value.trim()) {
    toast.error('Business name is required')
    return
  }
  organizationSubmitting.value = true
  try {
    await organizationsService.create({
      name: organizationName.value.trim(),
      reseller_id: selected.value.id,
    })
    organizationOpen.value = false
    organizationName.value = ''
    toast.success('Customer workspace provisioned')
    await Promise.all([loadSelected(), loadResellers(selected.value.id)])
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to create customer workspace'))
  } finally {
    organizationSubmitting.value = false
  }
}

async function addMember() {
  if (!selected.value || !memberForm.email.trim()) {
    toast.error('Administrator email is required')
    return
  }
  memberSubmitting.value = true
  try {
    await resellersService.addMember(selected.value.id, {
      email: memberForm.email.trim(),
      role: memberForm.role,
    })
    memberOpen.value = false
    memberForm.email = ''
    memberForm.role = 'admin'
    toast.success('Partner administrator access synchronized')
    await loadSelected()
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to add administrator'))
  } finally {
    memberSubmitting.value = false
  }
}

async function removeMember(member: ResellerMember) {
  if (!selected.value) return
  if (!window.confirm(`Revoke ${member.full_name || member.email} from this entire portfolio?`)) return
  try {
    await resellersService.removeMember(selected.value.id, member.id)
    toast.success('Partner access revoked')
    await loadSelected()
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to revoke administrator'))
  }
}

async function saveBranding() {
  if (!selected.value) return
  brandingSubmitting.value = true
  try {
    const payload: Parameters<typeof resellersService.update>[1] = {
      name: brandForm.name.trim(),
      brand_name: brandForm.brand_name.trim(),
      logo_url: brandForm.logo_url.trim(),
      primary_color: brandForm.primary_color,
      accent_color: brandForm.accent_color,
      support_email: brandForm.support_email.trim(),
      custom_domain: brandForm.custom_domain.trim().toLowerCase(),
    }
    if (isPlatformOwner.value) {
      payload.status = brandForm.status
      payload.plan = brandForm.plan
      payload.max_organizations = Number(brandForm.max_organizations)
    }
    await resellersService.update(selected.value.id, payload)
    toast.success('Partner configuration saved')
    await loadResellers(selected.value.id)
  } catch (error) {
    toast.error(getErrorMessage(error, 'Unable to save partner configuration'))
  } finally {
    brandingSubmitting.value = false
  }
}

function formatNumber(value?: number) {
  return new Intl.NumberFormat().format(value ?? 0)
}
</script>

<template>
  <div class="min-h-full bg-[#08090a] light:bg-gray-50 text-white light:text-gray-900">
    <header class="border-b border-white/[0.08] light:border-gray-200 bg-[#0b0c0e]/95 light:bg-white/95">
      <div class="flex min-h-20 items-center gap-4 px-6 py-4">
        <div class="flex h-11 w-11 items-center justify-center rounded-xl border border-emerald-400/20 bg-emerald-400/10">
          <Building2 class="h-5 w-5 text-emerald-300 light:text-emerald-700" />
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <h1 class="text-xl font-semibold tracking-tight">Partner Console</h1>
            <Badge variant="outline">Control plane</Badge>
          </div>
          <p class="mt-0.5 text-sm text-white/45 light:text-gray-500">
            Isolated customer workspaces, partner access and white-label operations.
          </p>
        </div>
        <Button variant="outline" size="sm" :disabled="loading" @click="loadResellers(selectedId)">
          <RefreshCw class="mr-2 h-4 w-4" :class="{ 'animate-spin': loading }" />
          Refresh
        </Button>
        <Button v-if="isPlatformOwner" size="sm" @click="createOpen = true">
          <Plus class="mr-2 h-4 w-4" />
          New partner
        </Button>
      </div>
    </header>

    <main class="grid min-h-[calc(100vh-5rem)] grid-cols-1 lg:grid-cols-[300px_minmax(0,1fr)]">
      <aside class="border-b border-white/[0.08] light:border-gray-200 lg:border-b-0 lg:border-r">
        <div class="p-4">
          <p class="px-2 text-[11px] font-semibold uppercase tracking-[0.18em] text-white/35 light:text-gray-500">
            Portfolios · {{ resellers.length }}
          </p>
          <div v-if="loading" class="mt-4 space-y-2">
            <div v-for="index in 3" :key="index" class="h-20 animate-pulse rounded-xl bg-white/[0.04] light:bg-gray-100" />
          </div>
          <div v-else-if="resellers.length" class="mt-3 space-y-2">
            <button
              v-for="reseller in resellers"
              :key="reseller.id"
              class="group w-full rounded-xl border p-3 text-left transition"
              :class="selectedId === reseller.id
                ? 'border-emerald-400/35 bg-emerald-400/[0.08]'
                : 'border-transparent bg-white/[0.025] hover:border-white/10 hover:bg-white/[0.045] light:bg-white light:hover:border-gray-200'"
              @click="selectedId = reseller.id"
            >
              <div class="flex items-start gap-3">
                <div
                  class="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg text-sm font-bold text-white"
                  :style="{ backgroundColor: reseller.primary_color || '#0f766e' }"
                >
                  {{ (reseller.brand_name || reseller.name).slice(0, 1).toUpperCase() }}
                </div>
                <div class="min-w-0 flex-1">
                  <p class="truncate text-sm font-medium">{{ reseller.brand_name || reseller.name }}</p>
                  <div class="mt-1 flex items-center gap-2 text-xs text-white/40 light:text-gray-500">
                    <span>{{ reseller.organization_count }} workspace{{ reseller.organization_count === 1 ? '' : 's' }}</span>
                    <span>·</span>
                    <span class="capitalize">{{ reseller.plan }}</span>
                  </div>
                </div>
                <span
                  class="mt-1 h-2 w-2 rounded-full"
                  :class="reseller.status === 'active' ? 'bg-emerald-400' : 'bg-amber-400'"
                />
              </div>
            </button>
          </div>
          <div v-else class="mt-10 text-center text-sm text-white/40 light:text-gray-500">
            No partner portfolios yet.
          </div>
        </div>
      </aside>

      <section v-if="selected" class="min-w-0 p-5 md:p-7">
        <div
          class="relative overflow-hidden rounded-2xl border border-white/10 bg-[#101214] p-6 light:border-gray-200 light:bg-white"
          :style="{ '--brand-primary': selected.primary_color, '--brand-accent': selected.accent_color }"
        >
          <div class="absolute inset-y-0 left-0 w-1 bg-[var(--brand-primary)]" />
          <div class="absolute -right-12 -top-20 h-52 w-52 rounded-full bg-[var(--brand-primary)] opacity-[0.09] blur-3xl" />
          <div class="relative flex flex-col gap-4 md:flex-row md:items-center">
            <div
              class="flex h-14 w-14 shrink-0 items-center justify-center overflow-hidden rounded-xl border border-white/10 text-xl font-bold text-white"
              :style="{ backgroundColor: selected.primary_color || '#0f766e' }"
            >
              <img v-if="selected.logo_url" :src="selected.logo_url" :alt="selected.brand_name" class="h-full w-full object-contain p-1.5" />
              <span v-else>{{ (selected.brand_name || selected.name).slice(0, 1).toUpperCase() }}</span>
            </div>
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <h2 class="truncate text-2xl font-semibold tracking-tight">{{ selected.brand_name || selected.name }}</h2>
                <Badge :variant="selected.status === 'active' ? 'success' : 'warning'" class="capitalize">
                  {{ selected.status }}
                </Badge>
                <Badge variant="secondary" class="capitalize">{{ selected.plan }}</Badge>
              </div>
              <p class="mt-1 text-sm text-white/45 light:text-gray-500">
                {{ selected.custom_domain || `${selected.slug}.partner` }}
              </p>
            </div>
            <Button variant="outline" size="sm" @click="activeTab = 'brand'">
              <Palette class="mr-2 h-4 w-4" />
              Configure brand
            </Button>
          </div>
        </div>

        <div class="mt-5 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-blue-400/10 p-2.5"><Building2 class="h-5 w-5 text-blue-300 light:text-blue-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.organization_count) }}</p><p class="text-xs text-white/40 light:text-gray-500">Customer workspaces</p></div>
            </CardContent>
          </Card>
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-violet-400/10 p-2.5"><Users class="h-5 w-5 text-violet-300 light:text-violet-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.user_count) }}</p><p class="text-xs text-white/40 light:text-gray-500">Unique CRM users</p></div>
            </CardContent>
          </Card>
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-emerald-400/10 p-2.5"><Users class="h-5 w-5 text-emerald-300 light:text-emerald-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.contacts) }}</p><p class="text-xs text-white/40 light:text-gray-500">Contacts managed</p></div>
            </CardContent>
          </Card>
          <Card class="border-white/[0.08] bg-white/[0.025] light:border-gray-200 light:bg-white">
            <CardContent class="flex items-center gap-4 p-5">
              <div class="rounded-lg bg-amber-400/10 p-2.5"><MessageSquareText class="h-5 w-5 text-amber-300 light:text-amber-700" /></div>
              <div><p class="text-2xl font-semibold">{{ formatNumber(usage?.messages) }}</p><p class="text-xs text-white/40 light:text-gray-500">Messages processed</p></div>
            </CardContent>
          </Card>
        </div>

        <Tabs v-model="activeTab" class="mt-6">
          <TabsList class="bg-white/[0.04] light:bg-gray-100">
            <TabsTrigger value="portfolio">Portfolio</TabsTrigger>
            <TabsTrigger value="team">Administrators</TabsTrigger>
            <TabsTrigger value="brand">Brand & plan</TabsTrigger>
          </TabsList>

          <TabsContent value="portfolio" class="mt-4">
            <div class="grid gap-4 xl:grid-cols-[minmax(0,1fr)_280px]">
              <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader class="flex-row items-center justify-between space-y-0">
                  <div>
                    <CardTitle class="text-base">Customer workspaces</CardTitle>
                    <p class="mt-1 text-xs text-white/40 light:text-gray-500">Each business has its own CRM boundary and PostgreSQL RLS context.</p>
                  </div>
                  <Button size="sm" @click="organizationOpen = true"><Plus class="mr-2 h-4 w-4" />New workspace</Button>
                </CardHeader>
                <CardContent class="px-0 pb-0">
                  <div class="overflow-x-auto">
                    <table class="w-full text-sm">
                      <thead class="border-y border-white/[0.07] bg-white/[0.02] text-left text-[11px] uppercase tracking-wider text-white/35 light:border-gray-200 light:text-gray-500">
                        <tr><th class="px-6 py-3 font-medium">Business</th><th class="px-4 py-3 font-medium">Workspace ID</th><th class="px-6 py-3 text-right font-medium">Created</th></tr>
                      </thead>
                      <tbody>
                        <tr v-for="organization in usage?.organizations" :key="organization.id" class="border-b border-white/[0.06] last:border-0 light:border-gray-100">
                          <td class="px-6 py-4">
                            <p class="font-medium">{{ organization.name }}</p>
                            <p class="mt-0.5 text-xs text-white/35 light:text-gray-500">{{ organization.slug }}</p>
                          </td>
                          <td class="px-4 py-4 font-mono text-xs text-white/40 light:text-gray-500">{{ organization.id.slice(0, 8) }}…</td>
                          <td class="px-6 py-4 text-right text-xs text-white/40 light:text-gray-500">{{ new Date(organization.created_at).toLocaleDateString() }}</td>
                        </tr>
                        <tr v-if="!usage?.organizations.length"><td colspan="3" class="px-6 py-12 text-center text-white/35 light:text-gray-500">No customer workspaces yet.</td></tr>
                      </tbody>
                    </table>
                  </div>
                </CardContent>
              </Card>

              <div class="space-y-4">
                <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader><CardTitle class="flex items-center gap-2 text-sm"><Gauge class="h-4 w-4 text-emerald-300" />Plan capacity</CardTitle></CardHeader>
                  <CardContent>
                    <div class="flex items-end justify-between">
                      <p class="text-2xl font-semibold">{{ usage?.organization_count || 0 }}<span class="text-sm font-normal text-white/35"> / {{ usage?.max_organizations || 0 }}</span></p>
                      <p class="text-xs text-white/40 light:text-gray-500">{{ organizationCapacity }}%</p>
                    </div>
                    <Progress :model-value="organizationCapacity" class="mt-3" />
                    <p class="mt-3 text-xs leading-5 text-white/35 light:text-gray-500">Organization limits are enforced before provisioning. Platform owners can adjust the plan.</p>
                  </CardContent>
                </Card>
                <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                  <CardHeader><CardTitle class="flex items-center gap-2 text-sm"><Smartphone class="h-4 w-4 text-blue-300" />WhatsApp footprint</CardTitle></CardHeader>
                  <CardContent>
                    <p class="text-3xl font-semibold">{{ formatNumber(usage?.whatsapp_accounts) }}</p>
                    <p class="mt-1 text-xs text-white/40 light:text-gray-500">Connected business accounts</p>
                  </CardContent>
                </Card>
              </div>
            </div>
          </TabsContent>

          <TabsContent value="team" class="mt-4">
            <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
              <CardHeader class="flex-row items-center justify-between space-y-0">
                <div>
                  <CardTitle class="text-base">Partner administrators</CardTitle>
                  <p class="mt-1 text-xs text-white/40 light:text-gray-500">Access is inherited across this portfolio and revoked as one traceable unit.</p>
                </div>
                <Button size="sm" @click="memberOpen = true"><Plus class="mr-2 h-4 w-4" />Add administrator</Button>
              </CardHeader>
              <CardContent class="space-y-2">
                <div v-for="member in members" :key="member.id" class="flex items-center gap-3 rounded-xl border border-white/[0.07] bg-white/[0.02] p-4 light:border-gray-200 light:bg-gray-50">
                  <div class="flex h-10 w-10 items-center justify-center rounded-full bg-white/[0.07] text-sm font-semibold light:bg-white">
                    {{ (member.full_name || member.email).slice(0, 1).toUpperCase() }}
                  </div>
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm font-medium">{{ member.full_name || member.email }}</p>
                    <p class="truncate text-xs text-white/40 light:text-gray-500">{{ member.email }}</p>
                  </div>
                  <Badge :variant="member.role === 'owner' ? 'info' : 'secondary'" class="capitalize">{{ member.role }}</Badge>
                  <Button variant="ghost" size="icon" title="Revoke portfolio access" @click="removeMember(member)">
                    <Trash2 class="h-4 w-4 text-red-300" />
                  </Button>
                </div>
                <div v-if="!members.length" class="py-12 text-center">
                  <ShieldCheck class="mx-auto h-8 w-8 text-white/20 light:text-gray-300" />
                  <p class="mt-3 text-sm text-white/45 light:text-gray-500">No delegated administrators yet.</p>
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          <TabsContent value="brand" class="mt-4">
            <form class="grid gap-4 xl:grid-cols-[minmax(0,1fr)_320px]" @submit.prevent="saveBranding">
              <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader>
                  <CardTitle class="text-base">Partner identity</CardTitle>
                  <p class="text-xs text-white/40 light:text-gray-500">Configure the customer-facing brand and support identity.</p>
                </CardHeader>
                <CardContent class="grid gap-5 sm:grid-cols-2">
                  <div class="space-y-2"><Label for="partner-name">Legal / partner name</Label><Input id="partner-name" v-model="brandForm.name" /></div>
                  <div class="space-y-2"><Label for="brand-name">Display brand</Label><Input id="brand-name" v-model="brandForm.brand_name" /></div>
                  <div class="space-y-2 sm:col-span-2"><Label for="logo-url">Logo URL</Label><Input id="logo-url" v-model="brandForm.logo_url" placeholder="https://…" /></div>
                  <div class="space-y-2"><Label for="support-email">Support email</Label><Input id="support-email" v-model="brandForm.support_email" type="email" placeholder="support@example.com" /></div>
                  <div class="space-y-2"><Label for="custom-domain">Custom hostname</Label><Input id="custom-domain" v-model="brandForm.custom_domain" placeholder="crm.partner.com" /></div>
                  <div class="space-y-2"><Label for="primary-color">Primary color</Label><div class="flex gap-2"><Input id="primary-color" v-model="brandForm.primary_color" /><input v-model="brandForm.primary_color" type="color" class="h-10 w-12 rounded-md border border-white/10 bg-transparent p-1" /></div></div>
                  <div class="space-y-2"><Label for="accent-color">Accent color</Label><div class="flex gap-2"><Input id="accent-color" v-model="brandForm.accent_color" /><input v-model="brandForm.accent_color" type="color" class="h-10 w-12 rounded-md border border-white/10 bg-transparent p-1" /></div></div>
                  <template v-if="isPlatformOwner">
                    <div class="space-y-2"><Label for="partner-status">Status</Label><select id="partner-status" v-model="brandForm.status" class="control-select"><option value="active">Active</option><option value="suspended">Suspended</option></select></div>
                    <div class="space-y-2"><Label for="partner-plan">Plan</Label><select id="partner-plan" v-model="brandForm.plan" class="control-select"><option value="starter">Starter</option><option value="growth">Growth</option><option value="enterprise">Enterprise</option></select></div>
                    <div class="space-y-2"><Label for="org-limit">Organization limit</Label><Input id="org-limit" v-model.number="brandForm.max_organizations" type="number" min="1" /></div>
                  </template>
                  <div class="flex justify-end sm:col-span-2"><Button type="submit" :loading="brandingSubmitting"><Check class="mr-2 h-4 w-4" />Save configuration</Button></div>
                </CardContent>
              </Card>

              <Card class="border-white/[0.08] bg-[#0d0f11] light:border-gray-200 light:bg-white">
                <CardHeader><CardTitle class="text-base">Live preview</CardTitle></CardHeader>
                <CardContent>
                  <div class="overflow-hidden rounded-xl border border-white/10 bg-[#090a0b] light:border-gray-200 light:bg-gray-50">
                    <div class="h-1" :style="{ backgroundColor: brandForm.primary_color }" />
                    <div class="p-5">
                      <div class="flex items-center gap-3">
                        <div class="flex h-11 w-11 items-center justify-center rounded-lg text-lg font-semibold text-white" :style="{ backgroundColor: brandForm.primary_color }">
                          <img v-if="brandForm.logo_url" :src="brandForm.logo_url" class="h-full w-full object-contain p-1" />
                          <span v-else>{{ (brandForm.brand_name || brandForm.name || 'P').slice(0, 1).toUpperCase() }}</span>
                        </div>
                        <div><p class="font-semibold">{{ brandForm.brand_name || 'Partner brand' }}</p><p class="text-xs text-white/35 light:text-gray-500">WhatsApp CRM</p></div>
                      </div>
                      <div class="mt-6 space-y-2">
                        <div class="h-2 w-3/4 rounded bg-white/10 light:bg-gray-200" />
                        <div class="h-2 w-1/2 rounded bg-white/[0.06] light:bg-gray-100" />
                      </div>
                      <button type="button" class="mt-6 w-full rounded-lg px-3 py-2 text-sm font-medium text-white" :style="{ backgroundColor: brandForm.accent_color }">Open inbox</button>
                    </div>
                  </div>
                  <div class="mt-4 space-y-3 text-xs text-white/40 light:text-gray-500">
                    <p class="flex items-center gap-2"><Globe2 class="h-4 w-4" />{{ brandForm.custom_domain || 'Canonical domain not assigned' }}</p>
                    <p class="flex items-center gap-2"><Mail class="h-4 w-4" />{{ brandForm.support_email || 'Support identity not assigned' }}</p>
                  </div>
                </CardContent>
              </Card>
            </form>
          </TabsContent>
        </Tabs>
        <div v-if="detailLoading" class="pointer-events-none fixed inset-0 z-20 bg-black/5" />
      </section>

      <section v-else class="flex min-h-[60vh] items-center justify-center p-8 text-center">
        <div><Building2 class="mx-auto h-10 w-10 text-white/15 light:text-gray-300" /><h2 class="mt-4 font-medium">Select a partner portfolio</h2><p class="mt-1 text-sm text-white/40 light:text-gray-500">Portfolio controls will appear here.</p></div>
      </section>
    </main>

    <Dialog v-model:open="createOpen">
      <DialogContent>
        <DialogHeader><DialogTitle>Create partner</DialogTitle><DialogDescription>Creates an isolated reseller portfolio and a private administration workspace.</DialogDescription></DialogHeader>
        <div class="grid gap-4 py-2">
          <div class="space-y-2"><Label for="create-name">Company name</Label><Input id="create-name" v-model="createForm.name" placeholder="Example Health Technologies" /></div>
          <div class="space-y-2"><Label for="create-brand">Display brand</Label><Input id="create-brand" v-model="createForm.brand_name" placeholder="Example Health" /></div>
          <div class="space-y-2"><Label for="create-workspace">Private workspace name</Label><Input id="create-workspace" v-model="createForm.workspace_name" placeholder="Example Health HQ" /></div>
          <div class="space-y-2"><Label for="create-email">Support email</Label><Input id="create-email" v-model="createForm.support_email" type="email" /></div>
          <div class="space-y-2"><Label for="create-plan">Initial plan</Label><select id="create-plan" v-model="createForm.plan" class="control-select"><option value="starter">Starter · 10 organizations</option><option value="growth">Growth · 50 organizations</option><option value="enterprise">Enterprise · 1,000 organizations</option></select></div>
        </div>
        <DialogFooter><Button variant="outline" @click="createOpen = false">Cancel</Button><Button :loading="createSubmitting" @click="createReseller">Create partner</Button></DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="organizationOpen">
      <DialogContent>
        <DialogHeader><DialogTitle>Provision customer workspace</DialogTitle><DialogDescription>The new business receives its own CRM tenant, roles, settings and RLS boundary.</DialogDescription></DialogHeader>
        <div class="space-y-2 py-3"><Label for="organization-name">Business name</Label><Input id="organization-name" v-model="organizationName" placeholder="Serenity Wellness Clinic" @keyup.enter="createOrganization" /></div>
        <DialogFooter><Button variant="outline" @click="organizationOpen = false">Cancel</Button><Button :loading="organizationSubmitting" @click="createOrganization">Provision workspace</Button></DialogFooter>
      </DialogContent>
    </Dialog>

    <Dialog v-model:open="memberOpen">
      <DialogContent>
        <DialogHeader><DialogTitle>Add partner administrator</DialogTitle><DialogDescription>The user must already exist in a workspace. Their inherited access will cover every current and future organization in this portfolio.</DialogDescription></DialogHeader>
        <div class="grid gap-4 py-3">
          <div class="space-y-2"><Label for="member-email">Existing user email</Label><Input id="member-email" v-model="memberForm.email" type="email" placeholder="admin@partner.com" /></div>
          <div class="space-y-2"><Label for="member-role">Portfolio role</Label><select id="member-role" v-model="memberForm.role" class="control-select"><option value="admin">Administrator</option><option value="owner">Owner</option></select></div>
        </div>
        <DialogFooter><Button variant="outline" @click="memberOpen = false">Cancel</Button><Button :loading="memberSubmitting" @click="addMember">Grant portfolio access</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  </div>
</template>

<style scoped>
.control-select {
  @apply flex h-10 w-full rounded-md border border-white/10 bg-[#111315] px-3 py-2 text-sm text-white outline-none ring-offset-background transition focus:border-emerald-400/50 focus:ring-2 focus:ring-emerald-400/15;
}

:global(.light) .control-select {
  @apply border-gray-200 bg-white text-gray-900;
}
</style>
