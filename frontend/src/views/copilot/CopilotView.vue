<script setup lang="ts">
import { computed, ref } from 'vue'
import {
  Bot,
  Check,
  Clipboard,
  FileSearch,
  ListChecks,
  Loader2,
  MessageSquareReply,
  ShieldAlert,
  Sparkles,
  Wand2,
} from 'lucide-vue-next'
import PageHeader from '@/components/shared/PageHeader.vue'
import ContactPicker from '@/components/shared/ContactPicker.vue'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { useAppToast } from '@/composables/useAppToast'
import { useAuthStore } from '@/stores/auth'
import { getErrorMessage, unwrapItemResponse } from '@/lib/api-utils'
import { copilotService, type CopilotRun } from '@/services/productSuite'

const toast = useAppToast()
const authStore = useAuthStore()
const contactId = ref('')
const taskType = ref<CopilotRun['task_type']>('reply')
const instruction = ref('')
const messageLimit = ref(20)
const generating = ref(false)
const accepting = ref(false)
const run = ref<CopilotRun | null>(null)
const editableResult = ref('')
const runIdempotencyKey = ref(crypto.randomUUID())
const canExecute = computed(() => authStore.hasPermission('copilot', 'execute'))
const canReadContacts = computed(() => authStore.hasPermission('contacts', 'read'))
const canReadConversations = computed(() => authStore.hasPermission('chat', 'read'))
const canReadAIContext = computed(() => authStore.hasPermission('chatbot.ai', 'read'))
const canRunGroundedCopilot = computed(
  () =>
    canExecute.value &&
    canReadContacts.value &&
    canReadConversations.value &&
    canReadAIContext.value,
)

const modes = [
  {
    key: 'reply' as const,
    label: 'Draft reply',
    description: 'A ready-to-edit customer response',
    icon: MessageSquareReply,
  },
  {
    key: 'summary' as const,
    label: 'Summarize',
    description: 'Turn the conversation into a brief',
    icon: FileSearch,
  },
  {
    key: 'qualify' as const,
    label: 'Qualify lead',
    description: 'Extract intent, fit and urgency',
    icon: Wand2,
  },
  {
    key: 'extract_actions' as const,
    label: 'Next actions',
    description: 'Suggest follow-ups for approval',
    icon: ListChecks,
  },
]

const activeMode = computed(() => modes.find((mode) => mode.key === taskType.value) ?? modes[0])

async function generate() {
  if (!canExecute.value) {
    toast.warning('You have read-only Copilot access')
    return
  }
  if (!contactId.value.trim()) {
    toast.warning('Choose a customer first')
    return
  }
  generating.value = true
  run.value = null
  editableResult.value = ''
  try {
    const response = await copilotService.run(contactId.value.trim(), taskType.value, {
      instruction: instruction.value.trim(),
      message_limit: messageLimit.value,
      idempotency_key: runIdempotencyKey.value,
    })
    run.value = unwrapItemResponse<CopilotRun>(response)
    editableResult.value =
      run.value.result_text ||
      (run.value.structured_result ? JSON.stringify(run.value.structured_result, null, 2) : '')
    runIdempotencyKey.value = crypto.randomUUID()
  } catch (error) {
    toast.error('AI Copilot could not generate a result', getErrorMessage(error))
  } finally {
    generating.value = false
  }
}

async function copyResult() {
  if (!editableResult.value) return
  await navigator.clipboard.writeText(editableResult.value)
  toast.success('Copied to clipboard')
}

async function acceptResult() {
  if (!run.value) return
  accepting.value = true
  try {
    await copilotService.feedback(run.value.id, {
      accepted: true,
      final_text: editableResult.value,
    })
    toast.success('Draft approved', 'It is still not sent; paste it into the conversation after your final review.')
  } catch (error) {
    toast.error('Approval was not saved', getErrorMessage(error))
  } finally {
    accepting.value = false
  }
}
</script>

<template>
  <div class="h-full overflow-y-auto bg-[#070809] light:bg-[#f5f4f1]">
    <PageHeader
      title="AI Copilot"
      description="Human-reviewed drafts, summaries and next actions grounded in the current tenant."
      :icon="Sparkles"
      icon-gradient="bg-gradient-to-br from-emerald-400 to-teal-700 shadow-emerald-500/20"
    >
      <template #actions>
        <Badge variant="outline" class="gap-1.5 border-emerald-400/20 bg-emerald-400/[0.06] text-emerald-300">
          <ShieldAlert class="h-3.5 w-3.5" />
          Draft only · never auto-sends
        </Badge>
      </template>
    </PageHeader>

    <div class="mx-auto grid max-w-[1420px] gap-6 p-5 md:p-7 xl:grid-cols-[390px_1fr]">
      <aside class="space-y-5">
        <section class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-gray-200 light:bg-white">
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-emerald-300">1 · Choose an action</p>
          <div class="mt-4 grid gap-2">
            <button
              v-for="mode in modes"
              :key="mode.key"
              class="flex items-center gap-3 rounded-xl border p-3 text-left transition"
              :class="
                taskType === mode.key
                  ? 'border-emerald-300/30 bg-emerald-300/[0.08]'
                  : 'border-white/[0.07] bg-white/[0.015] hover:bg-white/[0.035] light:border-gray-200 light:bg-gray-50'
              "
              @click="taskType = mode.key"
            >
              <div
                class="flex h-9 w-9 items-center justify-center rounded-lg"
                :class="taskType === mode.key ? 'bg-emerald-300/15 text-emerald-200' : 'bg-white/[0.04] text-white/35 light:bg-gray-100 light:text-gray-500'"
              >
                <component :is="mode.icon" class="h-4 w-4" />
              </div>
              <div>
                <p class="text-sm font-medium text-white light:text-gray-900">{{ mode.label }}</p>
                <p class="mt-0.5 text-[11px] text-white/40 light:text-gray-500">{{ mode.description }}</p>
              </div>
            </button>
          </div>
        </section>

        <section class="rounded-2xl border border-white/[0.08] bg-white/[0.025] p-5 light:border-gray-200 light:bg-white">
          <p class="text-[10px] font-semibold uppercase tracking-[0.22em] text-emerald-300">2 · Ground the request</p>
          <label class="mt-4 block text-xs font-medium text-white/65 light:text-gray-600">Customer</label>
          <ContactPicker v-if="canReadContacts" v-model="contactId" class="mt-2" />
          <p v-if="!canRunGroundedCopilot" class="mt-2 rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-3 text-xs leading-5 text-amber-100/70">
            Contact, conversation, and AI-context read access are all required to run grounded Copilot. Existing run history remains read-only.
          </p>

          <label class="mt-4 block text-xs font-medium text-white/65 light:text-gray-600">Optional instruction</label>
          <textarea
            v-model="instruction"
            rows="5"
            class="mt-2 w-full resize-none rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm leading-5 text-white outline-none transition placeholder:text-white/20 focus:border-emerald-300/40 light:border-gray-200 light:bg-gray-50 light:text-gray-900"
            placeholder="For example: keep the tone warm, concise and invite them to book an assessment."
          />

          <label class="mt-4 flex items-center justify-between text-xs font-medium text-white/65 light:text-gray-600">
            <span>Recent messages</span>
            <span class="font-mono text-emerald-300">{{ messageLimit }}</span>
          </label>
          <input v-model.number="messageLimit" type="range" min="5" max="50" step="5" class="mt-2 w-full accent-emerald-400" />

          <Button
            class="mt-5 w-full bg-emerald-300 text-black hover:bg-emerald-200"
            :disabled="generating || !canRunGroundedCopilot"
            @click="generate"
          >
            <Loader2 v-if="generating" class="mr-2 h-4 w-4 animate-spin" />
            <Bot v-else class="mr-2 h-4 w-4" />
            Run {{ activeMode.label }}
          </Button>
        </section>

        <section class="rounded-2xl border border-amber-300/15 bg-amber-300/[0.035] p-4">
          <div class="flex gap-3">
            <ShieldAlert class="mt-0.5 h-4 w-4 shrink-0 text-amber-300" />
            <p class="text-xs leading-5 text-amber-100/65">
              Copilot is not a clinician. It must not diagnose, prescribe, or replace secure clinical records. Review every
              draft before using it.
            </p>
          </div>
        </section>
      </aside>

      <main
        class="relative min-h-[720px] overflow-hidden rounded-[26px] border border-white/[0.08] bg-[#101214] shadow-2xl shadow-black/25 light:border-gray-200 light:bg-white"
      >
        <div class="absolute -right-24 -top-24 h-72 w-72 rounded-full bg-emerald-300/[0.06] blur-3xl" />
        <header class="relative flex items-center justify-between border-b border-white/[0.07] px-6 py-4 light:border-gray-100">
          <div class="flex items-center gap-3">
            <div class="flex h-9 w-9 items-center justify-center rounded-xl bg-emerald-300/10 text-emerald-200">
              <component :is="activeMode.icon" class="h-4 w-4" />
            </div>
            <div>
              <h2 class="text-sm font-semibold text-white light:text-gray-900">{{ activeMode.label }}</h2>
              <p class="mt-0.5 text-[11px] text-white/35 light:text-gray-500">
                {{ run ? `AI-assisted · ${run.status}` : 'Waiting for a grounded request' }}
              </p>
            </div>
          </div>
          <div v-if="run" class="flex gap-2">
            <Button variant="outline" size="sm" :disabled="!editableResult" @click="copyResult">
              <Clipboard class="mr-2 h-3.5 w-3.5" />
              Copy
            </Button>
            <Button
              size="sm"
              class="bg-emerald-300 text-black hover:bg-emerald-200"
              :disabled="accepting || !editableResult || !canExecute"
              @click="acceptResult"
            >
              <Loader2 v-if="accepting" class="mr-2 h-3.5 w-3.5 animate-spin" />
              <Check v-else class="mr-2 h-3.5 w-3.5" />
              Approve draft
            </Button>
          </div>
        </header>

        <div v-if="generating" class="flex h-[620px] flex-col items-center justify-center">
          <div class="relative">
            <div class="absolute inset-0 animate-ping rounded-full bg-emerald-300/15" />
            <div class="relative flex h-14 w-14 items-center justify-center rounded-full border border-emerald-300/20 bg-emerald-300/[0.08]">
              <Sparkles class="h-5 w-5 animate-pulse text-emerald-200" />
            </div>
          </div>
          <p class="mt-5 text-sm font-medium text-white light:text-gray-900">AI Copilot is reviewing the authorized context</p>
          <p class="mt-1 text-xs text-white/35 light:text-gray-500">No draft will be sent automatically.</p>
        </div>

        <div v-else-if="run" class="relative p-6">
          <div
            v-if="run.safety_warnings?.length"
            class="mb-4 rounded-xl border border-amber-300/20 bg-amber-300/[0.05] p-3"
          >
            <p class="text-[10px] font-semibold uppercase tracking-[0.18em] text-amber-300">Review warnings</p>
            <ul class="mt-2 space-y-1">
              <li v-for="warning in run.safety_warnings" :key="warning" class="text-xs text-amber-100/65">• {{ warning }}</li>
            </ul>
          </div>

          <textarea
            v-model="editableResult"
            class="min-h-[470px] w-full resize-none rounded-2xl border border-white/[0.08] bg-black/15 p-5 text-[15px] leading-7 text-white outline-none transition focus:border-emerald-300/30 light:border-gray-200 light:bg-gray-50 light:text-gray-900"
          />

          <div class="mt-4 flex flex-wrap items-center gap-2">
            <span class="text-[10px] font-semibold uppercase tracking-[0.18em] text-white/30 light:text-gray-500">Sources</span>
            <Badge v-for="source in run.source_names ?? []" :key="source" variant="secondary">{{ source }}</Badge>
            <span v-if="!run.source_names?.length" class="text-xs text-white/30 light:text-gray-400">Conversation context only</span>
          </div>
        </div>

        <div v-else class="flex h-[650px] flex-col items-center justify-center px-8 text-center">
          <div class="flex h-16 w-16 items-center justify-center rounded-2xl border border-white/[0.08] bg-white/[0.025] text-white/25 light:border-gray-200 light:bg-gray-50 light:text-gray-400">
            <Sparkles class="h-6 w-6" />
          </div>
          <h2 class="mt-5 text-lg font-semibold text-white light:text-gray-900">A quiet place to think before replying</h2>
          <p class="mt-2 max-w-md text-sm leading-6 text-white/40 light:text-gray-500">
            Select an action and a contact. AI Copilot will use only records this user is authorized to read and return an editable,
            human-reviewed result.
          </p>
        </div>
      </main>
    </div>
  </div>
</template>
