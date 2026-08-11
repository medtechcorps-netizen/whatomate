<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";
import {
  AlertTriangle,
  CheckCircle2,
  Loader2,
  LockKeyhole,
  Send,
} from "lucide-vue-next";
import { Button } from "@/components/ui/button";
import { getErrorMessage, unwrapItemResponse } from "@/lib/api-utils";
import type { AttestedMetaReviewReplyEligibility } from "@/lib/metaReviewReply";
import {
  channelsService,
  type MetaMessengerReviewReplyResponse,
} from "@/services/productSuite";

const props = defineProps<{
  conversationId: string;
  eligibility: AttestedMetaReviewReplyEligibility;
}>();

const emit = defineEmits<{
  sent: [response: MetaMessengerReviewReplyResponse];
}>();

const text = ref("");
const manuallyConfirmed = ref(false);
const sending = ref(false);
const error = ref("");
const success = ref<MetaMessengerReviewReplyResponse["audit"] | null>(null);
const lastAttemptKey = ref("");
const attempted = ref(false);
const currentTime = ref(Date.now());
let expiryTimer: number | null = null;

const trimmedText = computed(() => text.value.trim());
const attestationCurrent = computed(
  () => Date.parse(props.eligibility.expires_at) > currentTime.value,
);
const remainingCharacters = computed(
  () => props.eligibility.constraints.max_length - text.value.length,
);
const canSend = computed(
  () =>
    !sending.value &&
    !attempted.value &&
    attestationCurrent.value &&
    manuallyConfirmed.value &&
    trimmedText.value.length > 0 &&
    text.value.length <= props.eligibility.constraints.max_length,
);

function scheduleExpiry() {
  if (expiryTimer !== null) window.clearTimeout(expiryTimer);
  currentTime.value = Date.now();
  const delay = Date.parse(props.eligibility.expires_at) - currentTime.value;
  if (delay <= 0) return;
  expiryTimer = window.setTimeout(
    () => {
      currentTime.value = Date.now();
      expiryTimer = null;
    },
    Math.min(delay + 1, 2_147_483_647),
  );
}

watch(
  () => [props.conversationId, props.eligibility.attestation_id],
  () => {
    text.value = "";
    manuallyConfirmed.value = false;
    sending.value = false;
    error.value = "";
    success.value = null;
    lastAttemptKey.value = "";
    attempted.value = false;
    scheduleExpiry();
  },
  { immediate: true },
);

onBeforeUnmount(() => {
  if (expiryTimer !== null) window.clearTimeout(expiryTimer);
});

function formatSentAt(value: string) {
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) return "time unavailable";
  return new Intl.DateTimeFormat("en-MY", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(parsed);
}

async function sendReviewReply() {
  if (!canSend.value) return;

  const attemptedText = trimmedText.value;
  const attemptKey = crypto.randomUUID();
  lastAttemptKey.value = attemptKey;
  attempted.value = true;
  sending.value = true;
  error.value = "";
  success.value = null;

  try {
    const response = await channelsService.sendMetaReviewReply(
      props.conversationId,
      {
        attestation_id: props.eligibility.attestation_id,
        idempotency_key: attemptKey,
        text: attemptedText,
        manual_confirmation: true,
      },
    );
    const result =
      unwrapItemResponse<MetaMessengerReviewReplyResponse>(response);
    if (
      !result.audit?.id ||
      result.audit.page_id !== props.eligibility.page_id ||
      result.audit.recipient_label !== props.eligibility.recipient_label ||
      result.message?.direction !== "outgoing"
    ) {
      throw new Error(
        "Meta returned an unexpected audit receipt. Do not send again until an operator verifies this attempt.",
      );
    }

    text.value = "";
    manuallyConfirmed.value = false;
    success.value = result.audit;
    emit("sent", result);
  } catch (cause) {
    error.value = getErrorMessage(
      cause,
      "The review reply was not confirmed. Do not retry until an operator checks the audit log.",
    );
  } finally {
    sending.value = false;
  }
}
</script>

<template>
  <section
    v-if="attestationCurrent"
    data-testid="meta-review-reply-composer"
    class="overflow-hidden rounded-2xl border border-amber-300/20 bg-[linear-gradient(125deg,rgba(251,191,36,0.08),transparent_66%)] light:border-amber-300 light:bg-amber-50"
    aria-labelledby="meta-review-reply-title"
  >
    <div class="border-b border-amber-300/10 px-4 py-3 light:border-amber-200">
      <div class="flex items-start gap-3">
        <div
          class="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border border-amber-300/20 bg-amber-300/10 text-amber-200 light:text-amber-800"
        >
          <LockKeyhole class="h-4 w-4" />
        </div>
        <div class="min-w-0 flex-1">
          <p
            class="text-[9px] font-bold uppercase tracking-[0.2em] text-amber-200 light:text-amber-900"
          >
            App Review manual reply · pages_messaging
          </p>
          <h3
            id="meta-review-reply-title"
            class="mt-1 text-sm font-semibold text-white light:text-slate-950"
          >
            Send one reviewer-controlled text reply
          </h3>
          <p
            class="mt-1 text-[10px] leading-4 text-white/45 light:text-slate-700"
          >
            This short-lived server attestation permits only a manual text reply
            for this exact staging conversation. AI, media, mark-read,
            production outbound, and automatic retry stay disabled.
          </p>
        </div>
      </div>

      <dl
        class="mt-3 grid gap-px overflow-hidden rounded-lg border border-white/[0.07] bg-white/[0.07] light:border-amber-200 light:bg-amber-200 sm:grid-cols-2"
      >
        <div class="bg-[#121416] px-3 py-2 light:bg-white">
          <dt
            class="text-[8px] font-bold uppercase tracking-[0.16em] text-white/30 light:text-slate-500"
          >
            Exact Facebook Page
          </dt>
          <dd
            data-testid="meta-review-reply-page-id"
            class="mt-1 break-all font-mono text-[10px] text-amber-100 light:text-amber-900"
          >
            {{ eligibility.page_id }}
          </dd>
        </div>
        <div class="bg-[#121416] px-3 py-2 light:bg-white">
          <dt
            class="text-[8px] font-bold uppercase tracking-[0.16em] text-white/30 light:text-slate-500"
          >
            Masked Messenger recipient
          </dt>
          <dd
            data-testid="meta-review-reply-recipient-label"
            class="mt-1 break-all font-mono text-[10px] text-amber-100 light:text-amber-900"
          >
            {{ eligibility.recipient_label }}
          </dd>
        </div>
      </dl>
    </div>

    <div class="space-y-3 p-4">
      <label class="block">
        <span class="text-xs font-semibold text-white/75 light:text-slate-900">
          Plain-text review reply
        </span>
        <textarea
          v-model="text"
          data-testid="meta-review-reply-text"
          rows="3"
          :maxlength="eligibility.constraints.max_length"
          :disabled="sending || attempted"
          class="mt-1.5 min-h-20 w-full resize-y rounded-xl border border-white/10 bg-black/20 px-3 py-2.5 text-sm text-white outline-none placeholder:text-white/20 focus:border-amber-300/40 disabled:cursor-wait disabled:opacity-60 light:border-slate-400 light:bg-white light:text-slate-950 light:placeholder:text-slate-500"
          placeholder="Type the exact reply shown in your App Review steps..."
          autocomplete="off"
        />
        <span
          data-testid="meta-review-reply-character-count"
          class="mt-1 block text-right font-mono text-[9px] text-white/30 light:text-slate-500"
        >
          {{ remainingCharacters }} characters remaining
        </span>
      </label>

      <label
        class="flex cursor-pointer items-start gap-3 rounded-xl border border-amber-300/15 bg-amber-300/[0.04] p-3 light:border-amber-200 light:bg-white"
      >
        <input
          v-model="manuallyConfirmed"
          data-testid="meta-review-reply-confirmation"
          type="checkbox"
          :disabled="sending || attempted"
          class="mt-0.5 h-4 w-4 shrink-0 rounded border-white/20 accent-amber-300"
        />
        <span class="text-[10px] leading-4 text-white/55 light:text-slate-700">
          I confirm this is a manual Meta App Review reply to recipient
          <span class="font-mono text-amber-100 light:text-amber-900">{{
            eligibility.recipient_label
          }}</span
          >. ReReply must not generate, attach, mark read, or retry anything for
          me.
        </span>
      </label>

      <div class="flex flex-wrap items-center justify-between gap-3">
        <p
          class="max-w-md text-[9px] leading-4 text-white/30 light:text-slate-500"
        >
          One attestation permits one UI attempt. After any response or timeout,
          this control freezes; reopen the conversation to obtain a fresh server
          decision before taking another action.
        </p>
        <Button
          type="button"
          data-testid="meta-review-reply-send"
          class="min-w-40 bg-amber-300 text-slate-950 hover:bg-amber-200"
          :disabled="!canSend"
          @click="sendReviewReply"
        >
          <Loader2 v-if="sending" class="mr-2 h-4 w-4 animate-spin" />
          <Send v-else class="mr-2 h-4 w-4" />
          {{ sending ? "Sending once" : "Send review reply" }}
        </Button>
      </div>

      <div
        v-if="success"
        data-testid="meta-review-reply-success"
        class="flex items-start gap-2 rounded-xl border border-emerald-300/20 bg-emerald-300/[0.05] px-3 py-2.5 text-[10px] leading-4 text-emerald-100 light:border-emerald-200 light:bg-emerald-50 light:text-emerald-800"
        role="status"
      >
        <CheckCircle2 class="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <p>
          Sent once and audit-recorded {{ formatSentAt(success.sent_at) }}.
          Audit ID:
          <span class="break-all font-mono">{{ success.id }}</span>
        </p>
      </div>

      <div
        v-if="error"
        data-testid="meta-review-reply-error"
        class="flex items-start gap-2 rounded-xl border border-red-300/20 bg-red-300/[0.05] px-3 py-2.5 text-[10px] leading-4 text-red-100 light:border-red-200 light:bg-red-50 light:text-red-800"
        role="alert"
      >
        <AlertTriangle class="mt-0.5 h-3.5 w-3.5 shrink-0" />
        <p>
          {{ error }} Attempt key:
          <span class="break-all font-mono">{{ lastAttemptKey }}</span>
        </p>
      </div>
    </div>
  </section>
</template>
