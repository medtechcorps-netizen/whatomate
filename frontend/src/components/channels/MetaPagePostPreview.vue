<script setup lang="ts">
import { computed, ref } from "vue";
import { ExternalLink, FileText, Loader2, RefreshCw } from "lucide-vue-next";
import { Button } from "@/components/ui/button";
import { getErrorMessage, unwrapItemResponse } from "@/lib/api-utils";
import { messengerReviewRelayReady } from "@/lib/channelAccountReadiness";
import {
  channelsService,
  type ChannelAccount,
  type MetaMessengerReviewPagePosts,
} from "@/services/productSuite";

const props = defineProps<{
  account: ChannelAccount;
}>();

const loading = ref(false);
const loaded = ref(false);
const error = ref("");
const preview = ref<MetaMessengerReviewPagePosts | null>(null);

const available = computed(() => messengerReviewRelayReady(props.account));

function formatDateTime(value: string) {
  const parsed = new Date(value);
  if (!Number.isFinite(parsed.getTime())) return "Time unavailable";
  return new Intl.DateTimeFormat("en-MY", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(parsed);
}

async function loadPosts() {
  if (!available.value || loading.value) return;
  loading.value = true;
  error.value = "";
  try {
    const response = await channelsService.reviewPagePosts(props.account.id);
    const result = unwrapItemResponse<MetaMessengerReviewPagePosts>(response);
    if (
      result.page_id !== props.account.external_account_id ||
      !Array.isArray(result.posts)
    ) {
      throw new Error("The Page preview did not match this connection");
    }
    preview.value = result;
    loaded.value = true;
  } catch (cause) {
    preview.value = null;
    loaded.value = false;
    error.value = getErrorMessage(
      cause,
      "Recent Facebook Page posts could not be loaded.",
    );
  } finally {
    loading.value = false;
  }
}
</script>

<template>
  <section
    v-if="available"
    data-testid="meta-page-post-preview"
    class="mt-4 overflow-hidden rounded-2xl border border-sky-300/15 bg-sky-300/[0.035] light:border-sky-200 light:bg-sky-50/60"
    aria-labelledby="meta-page-post-preview-title"
  >
    <div class="flex flex-wrap items-start justify-between gap-3 p-4">
      <div class="min-w-0 flex-1">
        <p
          class="text-[10px] font-bold uppercase tracking-[0.2em] text-sky-300 light:text-sky-800"
        >
          App Review evidence · pages_read_engagement
        </p>
        <h3
          id="meta-page-post-preview-title"
          class="mt-1 text-sm font-semibold text-white light:text-slate-950"
        >
          Recent Page-authored Facebook posts
        </h3>
        <p
          class="mt-1.5 max-w-xl text-[10px] leading-4 text-white/45 light:text-slate-600"
        >
          ReReply uses <code>pages_read_engagement</code> here only to read and
          display recent posts authored by this exact connected Page for Meta's
          reviewer. This action cannot publish, edit, or delete Page content.
        </p>
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        data-testid="meta-page-post-preview-load"
        :disabled="loading"
        @click="loadPosts"
      >
        <Loader2 v-if="loading" class="mr-1.5 h-3.5 w-3.5 animate-spin" />
        <RefreshCw v-else-if="loaded || error" class="mr-1.5 h-3.5 w-3.5" />
        <FileText v-else class="mr-1.5 h-3.5 w-3.5" />
        {{
          loading
            ? "Loading"
            : loaded || error
              ? "Reload posts"
              : "Load recent posts"
        }}
      </Button>
    </div>

    <div
      v-if="error"
      data-testid="meta-page-post-preview-error"
      class="border-t border-red-300/15 bg-red-300/[0.045] px-4 py-3 text-[10px] leading-4 text-red-100 light:border-red-200 light:bg-red-50 light:text-red-800"
      role="alert"
    >
      {{ error }} The connection remains inbound-only; no post was changed.
    </div>

    <div
      v-else-if="loaded && preview"
      data-testid="meta-page-post-preview-results"
      class="border-t border-white/[0.06] p-4 light:border-sky-200"
    >
      <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
        <p class="text-xs font-semibold text-white/75 light:text-slate-900">
          {{ preview.page_name }}
        </p>
        <p
          class="break-all font-mono text-[9px] text-white/35 light:text-slate-500"
        >
          Page ID {{ preview.page_id }}
        </p>
      </div>

      <p
        v-if="preview.posts.length === 0"
        data-testid="meta-page-post-preview-empty"
        class="rounded-xl border border-dashed border-white/[0.1] p-4 text-center text-[10px] text-white/40 light:border-slate-300 light:text-slate-600"
      >
        Meta returned no recent Page-authored posts for this Page.
      </p>
      <ol v-else class="space-y-2.5">
        <li
          v-for="post in preview.posts"
          :key="post.id"
          data-testid="meta-page-post-preview-item"
          class="rounded-xl border border-white/[0.07] bg-black/15 p-3 light:border-slate-200 light:bg-white"
        >
          <p
            class="whitespace-pre-wrap text-xs leading-5 text-white/75 light:text-slate-800"
          >
            {{ post.message || "This Page post has no text." }}
          </p>
          <div class="mt-2 flex flex-wrap items-center justify-between gap-2">
            <time
              :datetime="post.created_time"
              class="text-[9px] text-white/35 light:text-slate-500"
            >
              {{ formatDateTime(post.created_time) }}
            </time>
            <a
              :href="post.permalink_url"
              target="_blank"
              rel="noopener noreferrer"
              class="inline-flex items-center gap-1 text-[10px] font-semibold text-sky-300 hover:text-sky-200 light:text-sky-700 light:hover:text-sky-900"
            >
              View on Facebook
              <ExternalLink class="h-3 w-3" />
            </a>
          </div>
        </li>
      </ol>
    </div>
  </section>
</template>
