<script setup lang="ts">
import { computed } from "vue";
import { AlertCircle, CheckCircle2 } from "lucide-vue-next";
import { Input } from "@/components/ui/input";
import {
  isManagedMessengerAccount,
  messengerRelayRegistryRecognized,
  metaAccountReadiness,
  type MetaAccountReadinessState,
} from "@/lib/channelAccountReadiness";
import type { ChannelAccount } from "@/services/productSuite";

const props = defineProps<{
  account: ChannelAccount;
  identityConfirmation: string;
  canManage: boolean;
}>();

const emit = defineEmits<{
  "update:identityConfirmation": [value: string];
}>();

const identityValue = computed({
  get: () => props.identityConfirmation,
  set: (value: string) => emit("update:identityConfirmation", value),
});

const readiness = computed(() =>
  metaAccountReadiness({
    ...props.account,
    config: {
      ...props.account.config,
      identity_confirmed_id: props.identityConfirmation.trim(),
    },
  }),
);

const selectedIdentityLabel = computed(() =>
  props.account.channel === "messenger"
    ? "Selected Facebook Page"
    : "Selected Instagram identity",
);
const externalIDLabel = computed(() =>
  props.account.channel === "messenger" ? "Page ID" : "Instagram Graph ID",
);

function formatDateTime(value?: string) {
  if (!value) return "Not observed";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "Unknown";
  return new Intl.DateTimeFormat("en-MY", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function readinessStateClasses(state: MetaAccountReadinessState) {
  if (state === "complete") {
    return "border-emerald-300/20 bg-emerald-300/[0.05] text-emerald-200 light:border-emerald-200 light:bg-emerald-50 light:text-emerald-800";
  }
  if (state === "blocked") {
    return "border-red-300/20 bg-red-300/[0.05] text-red-200 light:border-red-200 light:bg-red-50 light:text-red-800";
  }
  return "border-amber-300/20 bg-amber-300/[0.05] text-amber-100 light:border-amber-200 light:bg-amber-50 light:text-amber-800";
}
</script>

<template>
  <section
    data-testid="meta-account-readiness"
    class="mt-5 overflow-hidden rounded-2xl border border-white/[0.08] bg-[#0b0d0e] light:border-slate-200 light:bg-slate-50"
    aria-labelledby="meta-account-readiness-title"
  >
    <div
      class="border-b border-white/[0.07] bg-[linear-gradient(120deg,rgba(251,191,36,0.09),transparent_58%)] p-4 light:border-slate-200"
    >
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p
            class="text-[10px] font-bold uppercase tracking-[0.22em] text-amber-300 light:text-amber-800"
          >
            Organization go-live proof
          </p>
          <h3
            id="meta-account-readiness-title"
            class="mt-1 text-sm font-semibold text-white light:text-slate-950"
          >
            Five go-live checks before outbound
          </h3>
        </div>
        <span
          class="rounded-full border px-2.5 py-1 text-[10px] font-bold"
          :class="
            readiness.preflightReady
              ? 'border-emerald-300/25 bg-emerald-300/10 text-emerald-200 light:text-emerald-800'
              : 'border-amber-300/25 bg-amber-300/10 text-amber-100 light:text-amber-800'
          "
        >
          {{ readiness.completedCount }}/{{ readiness.requiredCount }} verified
        </span>
      </div>

      <div
        class="mt-4 grid gap-px overflow-hidden rounded-xl border border-white/[0.07] bg-white/[0.07] light:border-slate-200 light:bg-slate-200 sm:grid-cols-2"
      >
        <div class="bg-[#111416] p-3 light:bg-white">
          <p
            class="text-[9px] font-semibold uppercase tracking-[0.16em] text-white/30 light:text-slate-500"
          >
            {{ selectedIdentityLabel }}
          </p>
          <p
            class="mt-1 truncate text-xs font-semibold text-white light:text-slate-950"
          >
            {{ account.name }}
          </p>
          <p
            class="mt-1 break-all font-mono text-[10px] text-sky-300 light:text-sky-700"
          >
            {{ externalIDLabel }}: {{ account.external_account_id }}
          </p>
        </div>
        <div class="bg-[#111416] p-3 light:bg-white">
          <p
            class="text-[9px] font-semibold uppercase tracking-[0.16em] text-white/30 light:text-slate-500"
          >
            Server evidence
          </p>
          <p class="mt-1 text-[10px] text-white/55 light:text-slate-700">
            Relay test: {{ formatDateTime(account.last_health_check_at) }}
          </p>
          <p class="mt-1 text-[10px] text-white/55 light:text-slate-700">
            Provider proof:
            {{ account.meta_provider_proof_version || "Not proven" }}
          </p>
          <p class="mt-1 text-[10px] text-white/55 light:text-slate-700">
            Last proven incoming text DM:
            {{ formatDateTime(account.last_inbound_at) }}
          </p>
        </div>
      </div>

      <div
        v-if="isManagedMessengerAccount(account)"
        data-testid="messenger-relay-registry-state"
        class="mt-4 flex items-start gap-3 rounded-xl border p-3"
        :class="
          messengerRelayRegistryRecognized(account)
            ? 'border-emerald-300/20 bg-emerald-300/[0.05] light:border-emerald-200 light:bg-emerald-50'
            : 'border-amber-300/20 bg-amber-300/[0.05] light:border-amber-200 light:bg-amber-50'
        "
      >
        <CheckCircle2
          v-if="messengerRelayRegistryRecognized(account)"
          class="mt-0.5 h-4 w-4 shrink-0 text-emerald-300 light:text-emerald-700"
        />
        <AlertCircle
          v-else
          class="mt-0.5 h-4 w-4 shrink-0 text-amber-300 light:text-amber-700"
        />
        <div>
          <p class="text-xs font-semibold text-white/80 light:text-slate-900">
            {{
              messengerRelayRegistryRecognized(account)
                ? "Runtime relay registry recognized"
                : "Awaiting runtime relay registry"
            }}
          </p>
          <p
            class="mt-1 text-[10px] leading-4 text-white/38 light:text-slate-600"
          >
            {{
              messengerRelayRegistryRecognized(account)
                ? "This Facebook-authorized Page is registered. Test can now verify runtime permissions, relay health and webhook delivery."
                : "ReReply has saved the exact Facebook authorization, but Test and outbound approval remain locked until the protected runtime registry recognizes it."
            }}
          </p>
        </div>
      </div>

      <label v-else-if="canManage" class="mt-4 block">
        <span
          class="text-[11px] font-medium text-white/65 light:text-slate-700"
        >
          Retype the exact {{ externalIDLabel }}
        </span>
        <Input
          v-model="identityValue"
          data-testid="meta-account-identity-confirmation"
          class="mt-1.5 font-mono"
          :placeholder="account.external_account_id"
          autocomplete="off"
        />
        <span
          class="mt-1.5 block text-[10px] leading-4 text-white/35 light:text-slate-500"
        >
          This locks the connection to the immutable ID; it does not prove
          portfolio ownership. ReReply's deployment-held protected inventory
          separately authorizes the organization-to-asset mapping. The ID lock
          is audit recorded.
        </span>
      </label>
    </div>

    <ol class="divide-y divide-white/[0.06] light:divide-slate-200">
      <li
        v-for="(item, index) in readiness.items"
        :key="item.key"
        :data-testid="`meta-account-readiness-${item.key}`"
        class="grid gap-3 p-3.5 sm:grid-cols-[28px_minmax(0,1fr)_auto] sm:items-start"
      >
        <span
          class="flex h-7 w-7 items-center justify-center rounded-lg border text-[10px] font-bold"
          :class="readinessStateClasses(item.state)"
        >
          <CheckCircle2 v-if="item.state === 'complete'" class="h-3.5 w-3.5" />
          <AlertCircle v-else class="h-3.5 w-3.5" />
          <span class="sr-only">Check {{ index + 1 }}</span>
        </span>
        <div>
          <p class="text-xs font-semibold text-white/80 light:text-slate-900">
            {{ index + 1 }}. {{ item.label }}
          </p>
          <p
            class="mt-1 text-[10px] leading-4 text-white/38 light:text-slate-600"
          >
            {{ item.detail }}
          </p>
        </div>
        <span
          class="w-fit rounded-md border px-2 py-1 text-[9px] font-bold uppercase tracking-[0.12em]"
          :class="readinessStateClasses(item.state)"
        >
          {{ item.state }}
        </span>
      </li>
    </ol>

    <div
      v-if="readiness.hasLegacyOutboundApproval"
      class="border-t border-red-300/15 bg-red-300/[0.055] px-4 py-3 text-[10px] leading-4 text-red-100 light:border-red-200 light:bg-red-50 light:text-red-800"
      role="alert"
    >
      Outbound is currently live without all proof. Saving incomplete settings
      will disable it; complete the checklist and approve it again.
    </div>
  </section>
</template>
