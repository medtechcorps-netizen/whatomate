<script setup lang="ts">
import { computed, type Component } from "vue";
import { RouterLink } from "vue-router";
import {
  Activity,
  ArrowUpRight,
  CheckCircle2,
  CircleDashed,
  Clock3,
  Info,
  Link2,
  Settings2,
  ShieldAlert,
} from "lucide-vue-next";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import type {
  IntegrationState,
  IntegrationStatus,
} from "@/services/integrations";
import type {
  IntegrationReadinessItem,
  IntegrationReadinessState,
} from "@/lib/integrationReadiness";

export interface ProviderCardDefinition {
  name: string;
  eyebrow: string;
  description: string;
  icon: Component;
  accent: string;
  glow: string;
  capabilities: string[];
  readOnly?: boolean;
  managePath?: string;
  manageLabel?: string;
  resourceLabel?: string;
  credentialSummary?: string;
}

const props = defineProps<{
  integration: IntegrationState;
  definition: ProviderCardDefinition;
  readiness: IntegrationReadinessItem[];
  busyAction?: "test" | "connect" | null;
  canWrite?: boolean;
}>();

const emit = defineEmits<{
  configure: [];
  test: [];
  connect: [];
}>();

const statusCopy: Record<
  IntegrationStatus,
  { label: string; icon: Component }
> = {
  connected: { label: "Connected", icon: CheckCircle2 },
  configured: { label: "Ready to connect", icon: CircleDashed },
  pending: { label: "Pending activation", icon: Clock3 },
  degraded: { label: "Needs attention", icon: ShieldAlert },
  approval_required: { label: "Approval required", icon: ShieldAlert },
  adapter_unavailable: { label: "Adapter pending", icon: ShieldAlert },
  disabled: { label: "Disabled", icon: CircleDashed },
  not_configured: { label: "Setup required", icon: CircleDashed },
};

const status = computed(
  () => statusCopy[props.integration.status] ?? statusCopy.not_configured,
);
const statusVariant = computed(() => {
  if (props.integration.status === "connected") return "success";
  if (props.integration.status === "pending") return "info";
  if (props.integration.status === "degraded") return "destructive";
  if (
    props.integration.status === "approval_required" ||
    props.integration.status === "adapter_unavailable"
  )
    return "warning";
  if (props.integration.status === "configured") return "info";
  return "secondary";
});
const notice = computed(() => {
  if (
    props.integration.connection.last_error ||
    props.integration.status === "degraded"
  ) {
    return {
      icon: ShieldAlert,
      classes:
        "border-red-500/15 bg-red-500/[0.06] text-red-300 light:border-red-200 light:bg-red-50 light:text-red-700",
    };
  }

  if (
    props.integration.status === "approval_required" ||
    props.integration.status === "adapter_unavailable"
  ) {
    return {
      icon: ShieldAlert,
      classes:
        "border-amber-400/20 bg-amber-400/[0.07] text-amber-200 light:border-amber-200 light:bg-amber-50 light:text-amber-800",
    };
  }

  return {
    icon: Info,
    classes:
      "border-sky-400/15 bg-sky-400/[0.06] text-sky-200 light:border-sky-200 light:bg-sky-50 light:text-sky-800",
  };
});
const configuredCredentialCount = computed(
  () =>
    Object.values(props.integration.credentials ?? {}).filter(
      (value) => value.configured,
    ).length,
);
const credentialCount = computed(
  () => Object.keys(props.integration.credentials ?? {}).length,
);
const credentialSource = computed(() => {
  const sources = [
    ...new Set(
      Object.values(props.integration.credentials ?? {})
        .filter((value) => value.configured && value.source)
        .map((value) => value.source),
    ),
  ];
  if (sources.length !== 1) return "";
  const labels = {
    platform: "Platform managed",
    workspace: "Workspace override",
    copilot: "Copilot settings",
    legacy_chatbot: "Legacy chatbot",
    legacy_account: "Legacy account fallback",
  };
  return labels[sources[0]!];
});
const readinessCopy: Record<
  IntegrationReadinessState,
  { label: string; icon: Component; classes: string }
> = {
  ready: {
    label: "Ready",
    icon: CheckCircle2,
    classes: "text-emerald-400 light:text-emerald-700",
  },
  prepared: {
    label: "Prepared",
    icon: Clock3,
    classes: "text-violet-300 light:text-violet-700",
  },
  missing: {
    label: "Missing",
    icon: CircleDashed,
    classes: "text-white/30 light:text-gray-500",
  },
  blocked: {
    label: "Blocked",
    icon: ShieldAlert,
    classes: "text-amber-300 light:text-amber-700",
  },
  managed: {
    label: "Managed",
    icon: Settings2,
    classes: "text-sky-300 light:text-sky-700",
  },
};
const readinessAttentionCount = computed(
  () =>
    props.readiness.filter((item) =>
      ["missing", "blocked"].includes(item.state),
    ).length,
);

function formatRelative(value?: string) {
  if (!value) return "Not checked yet";
  const timestamp = new Date(value).getTime();
  if (!Number.isFinite(timestamp)) return "Unknown";
  const minutes = Math.max(0, Math.floor((Date.now() - timestamp) / 60_000));
  if (minutes < 1) return "Just now";
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}
</script>

<template>
  <article
    class="integration-card group relative flex min-h-[470px] flex-col overflow-hidden rounded-2xl border border-white/[0.08] bg-[#101011]/90 p-5 light:border-gray-200 light:bg-white"
    :aria-label="`${definition.name} integration: ${status.label}`"
  >
    <div
      class="pointer-events-none absolute -right-12 -top-16 h-40 w-40 rounded-full opacity-20 blur-3xl transition-opacity duration-500 group-hover:opacity-35"
      :style="{ background: definition.glow }"
    />
    <div
      class="pointer-events-none absolute inset-x-0 top-0 h-px opacity-70"
      :style="{
        background: `linear-gradient(90deg, transparent, ${definition.accent}, transparent)`,
      }"
    />

    <div class="relative flex items-start justify-between gap-4">
      <div class="flex min-w-0 items-center gap-3">
        <div
          class="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl border border-white/10 shadow-lg light:border-black/5"
          :style="{ background: definition.glow }"
        >
          <component
            :is="definition.icon"
            class="h-5 w-5 text-white"
            aria-hidden="true"
          />
        </div>
        <div class="min-w-0">
          <p
            class="text-[10px] font-semibold uppercase tracking-[0.18em] text-white/35 light:text-gray-400"
          >
            {{ definition.eyebrow }}
          </p>
          <h2
            class="truncate text-base font-semibold text-white light:text-gray-950"
          >
            {{ integration.display_name || definition.name }}
          </h2>
        </div>
      </div>
      <Badge
        :variant="statusVariant"
        class="shrink-0 gap-1.5 whitespace-nowrap"
      >
        <component :is="status.icon" class="h-3 w-3" aria-hidden="true" />
        {{ status.label }}
      </Badge>
    </div>

    <p
      class="relative mt-4 min-h-[42px] text-sm leading-6 text-white/50 light:text-gray-600"
    >
      {{ definition.description }}
    </p>

    <div class="relative mt-4 flex flex-wrap gap-1.5" aria-label="Capabilities">
      <span
        v-for="capability in definition.capabilities"
        :key="capability"
        class="rounded-md border border-white/[0.07] bg-white/[0.035] px-2 py-1 text-[11px] font-medium text-white/45 light:border-gray-200 light:bg-gray-50 light:text-gray-600"
      >
        {{ capability }}
      </span>
    </div>

    <div
      class="relative mt-5 grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-white/[0.07] bg-white/[0.07] light:border-gray-200 light:bg-gray-200"
    >
      <div class="bg-[#111112] px-3 py-2.5 light:bg-white">
        <div
          class="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-white/30 light:text-gray-400"
        >
          <Activity class="h-3 w-3" aria-hidden="true" />
          {{ definition.resourceLabel || "Accounts" }}
        </div>
        <p class="mt-1 text-sm font-semibold text-white/80 light:text-gray-800">
          {{ integration.connection.active_count }} /
          {{ integration.connection.account_count }} active
        </p>
      </div>
      <div class="bg-[#111112] px-3 py-2.5 light:bg-white">
        <div
          class="flex items-center gap-1.5 text-[10px] uppercase tracking-wider text-white/30 light:text-gray-400"
        >
          <Clock3 class="h-3 w-3" aria-hidden="true" />
          Health check
        </div>
        <p class="mt-1 text-sm font-semibold text-white/80 light:text-gray-800">
          {{ formatRelative(integration.connection.last_health_check_at) }}
        </p>
      </div>
    </div>

    <section
      class="relative mt-4"
      :data-testid="`integration-readiness-${integration.provider}`"
      :aria-label="`${definition.name} setup requirements`"
    >
      <div class="flex items-center justify-between gap-3">
        <p
          class="text-[10px] font-semibold uppercase tracking-[0.16em] text-white/35 light:text-gray-500"
        >
          Setup preflight
        </p>
        <span class="text-[10px] text-white/25 light:text-gray-400">
          {{
            readinessAttentionCount
              ? `${readinessAttentionCount} detected issue${readinessAttentionCount === 1 ? "" : "s"}`
              : "No detected blockers"
          }}
        </span>
      </div>
      <div
        class="mt-2 overflow-hidden rounded-xl border border-white/[0.07] light:border-gray-200"
      >
        <div
          v-for="item in readiness"
          :key="item.key"
          :data-testid="`integration-readiness-${integration.provider}-${item.key}`"
          class="border-b border-white/[0.06] px-3 py-2 last:border-b-0 light:border-gray-100"
        >
          <div class="flex items-center justify-between gap-3">
            <div class="flex min-w-0 items-center gap-2">
              <component
                :is="readinessCopy[item.state].icon"
                class="h-3.5 w-3.5 shrink-0"
                :class="readinessCopy[item.state].classes"
                aria-hidden="true"
              />
              <span
                class="truncate text-[11px] font-medium text-white/65 light:text-gray-700"
              >
                {{ item.label }}
              </span>
            </div>
            <span
              class="shrink-0 text-[9px] font-semibold uppercase tracking-wider"
              :class="readinessCopy[item.state].classes"
            >
              {{ readinessCopy[item.state].label }}
            </span>
          </div>
          <p
            v-if="
              !['ready', 'prepared'].includes(item.state) &&
              (item.state !== 'managed' ||
                definition.readOnly ||
                integration.read_only)
            "
            class="mt-1 pl-[22px] text-[10px] leading-4 text-white/30 light:text-gray-500"
          >
            {{ item.detail }}
          </p>
        </div>
      </div>
    </section>

    <div
      v-if="integration.connection.last_error || integration.message"
      class="relative mt-3 flex items-start gap-2 rounded-lg border px-3 py-2 text-xs"
      :class="notice.classes"
      role="status"
    >
      <component
        :is="notice.icon"
        class="mt-0.5 h-3.5 w-3.5 shrink-0"
        aria-hidden="true"
      />
      <span class="line-clamp-2">{{
        integration.connection.last_error || integration.message
      }}</span>
    </div>

    <div class="relative mt-auto flex items-center justify-between gap-3 pt-5">
      <span
        class="inline-flex items-center gap-1.5 text-xs text-white/35 light:text-gray-500"
      >
        <Link2 class="h-3.5 w-3.5" aria-hidden="true" />
        <template v-if="credentialCount">
          {{
            credentialSource ||
            `${configuredCredentialCount}/${credentialCount} secrets stored`
          }}
        </template>
        <template v-else>
          {{ definition.credentialSummary || "Managed channel" }}
        </template>
      </span>

      <RouterLink
        v-if="
          (definition.readOnly || integration.read_only) &&
          definition.managePath
        "
        :to="definition.managePath"
      >
        <Button variant="outline" size="sm">
          {{ definition.manageLabel || "Manage" }}
          <ArrowUpRight class="h-3.5 w-3.5" aria-hidden="true" />
        </Button>
      </RouterLink>
      <Button
        v-else
        :data-testid="`integration-configure-${integration.provider}`"
        variant="outline"
        size="sm"
        @click="emit('configure')"
      >
        <Settings2 class="h-3.5 w-3.5" aria-hidden="true" />
        {{ canWrite === false ? "View details" : "Configure" }}
      </Button>
    </div>
  </article>
</template>

<style scoped>
.integration-card {
  box-shadow:
    0 18px 45px -32px rgba(0, 0, 0, 0.9),
    inset 0 1px 0 rgba(255, 255, 255, 0.025);
  transition:
    border-color 220ms ease,
    transform 220ms ease,
    box-shadow 220ms ease;
}

.integration-card:hover {
  transform: translateY(-2px);
  border-color: rgba(203, 212, 154, 0.2);
  box-shadow:
    0 22px 55px -34px rgba(0, 0, 0, 0.95),
    0 0 0 1px rgba(203, 212, 154, 0.035),
    inset 0 1px 0 rgba(255, 255, 255, 0.04);
}

.light .integration-card {
  box-shadow: 0 16px 40px -30px rgba(17, 24, 39, 0.35);
}

@media (prefers-reduced-motion: reduce) {
  .integration-card {
    transition: none;
  }

  .integration-card:hover {
    transform: none;
  }
}
</style>
