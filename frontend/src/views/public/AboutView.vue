<script setup lang="ts">
import { onBeforeUnmount, watchEffect } from "vue";
import { RouterLink } from "vue-router";
import {
  ArrowRight,
  BarChart3,
  Bot,
  CalendarCheck2,
  Check,
  ChevronRight,
  Eye,
  MessageCircleMore,
  Search,
  ShieldCheck,
  Workflow,
} from "lucide-vue-next";
import ReReplyLogo from "@/components/brand/ReReplyLogo.vue";

const originalTitle = document.title;
let descriptionTag = document.querySelector<HTMLMetaElement>(
  'meta[name="description"]',
);
const originalDescription = descriptionTag?.content ?? "";

watchEffect(() => {
  document.title = "ReReply · Customer conversations, aligned";

  if (!descriptionTag) {
    descriptionTag = document.createElement("meta");
    descriptionTag.name = "description";
    document.head.appendChild(descriptionTag);
  }
  descriptionTag.content =
    "ReReply brings customer conversations, CRM workflows, bookings, automation and read-only search visibility into one tenant-aware workspace.";
});

onBeforeUnmount(() => {
  document.title = originalTitle;
  if (descriptionTag) {
    descriptionTag.content = originalDescription;
  }
});

const capabilities = [
  {
    number: "01",
    icon: MessageCircleMore,
    title: "Conversations with context",
    description:
      "Bring channels, customer history and team handover together so every reply begins with the full story.",
  },
  {
    number: "02",
    icon: Workflow,
    title: "Work that moves itself",
    description:
      "Turn follow-ups, bookings and lead stages into visible workflows with clear human ownership.",
  },
  {
    number: "03",
    icon: BarChart3,
    title: "Signals you can act on",
    description:
      "Read operational and search-performance signals in the same workspace where your team takes action.",
  },
];

const searchSignals = [
  "Clicks",
  "Impressions",
  "CTR",
  "Average position",
  "Queries",
  "Pages",
];
</script>

<template>
  <div class="public-shell min-h-screen overflow-hidden text-[#eff0e8]">
    <a href="#main-content" class="skip-link">Skip to content</a>
    <div
      class="public-grid pointer-events-none fixed inset-0 opacity-70"
      aria-hidden="true"
    />
    <div
      class="public-glow public-glow-top pointer-events-none fixed -right-60 -top-72 h-[48rem] w-[48rem] rounded-full"
      aria-hidden="true"
    />
    <div
      class="public-glow public-glow-bottom pointer-events-none fixed -bottom-80 -left-60 h-[42rem] w-[42rem] rounded-full"
      aria-hidden="true"
    />

    <header
      class="sticky top-0 z-50 border-b border-white/[0.07] bg-[#0b0e09]/85 backdrop-blur-2xl"
    >
      <div
        class="mx-auto flex h-[72px] max-w-[1220px] items-center justify-between px-5 sm:px-8"
      >
        <RouterLink to="/about" aria-label="ReReply home">
          <ReReplyLogo size="md" tone="light" tagline />
        </RouterLink>

        <nav
          aria-label="Primary"
          class="hidden items-center gap-7 text-xs font-semibold text-white/48 md:flex"
        >
          <a href="#product" class="transition hover:text-[#d9dfa9]">Product</a>
          <a href="#integrations" class="transition hover:text-[#d9dfa9]"
            >Integrations</a
          >
          <a href="#trust" class="transition hover:text-[#d9dfa9]">Trust</a>
        </nav>

        <RouterLink
          to="/login"
          class="group inline-flex items-center gap-2 rounded-full border border-[#cbd49a]/25 bg-[#cbd49a]/[0.07] px-4 py-2 text-xs font-bold uppercase tracking-[0.13em] text-[#dfe5b6] transition hover:border-[#cbd49a]/45 hover:bg-[#cbd49a]/[0.12]"
        >
          Sign in
          <ArrowRight
            class="h-3.5 w-3.5 transition-transform group-hover:translate-x-0.5"
          />
        </RouterLink>
      </div>
    </header>

    <main id="main-content" class="relative z-10">
      <section
        class="mx-auto grid max-w-[1220px] gap-14 px-5 pb-20 pt-16 sm:px-8 sm:pb-28 sm:pt-24 lg:grid-cols-[minmax(0,1.08fr)_minmax(410px,0.92fr)] lg:items-center lg:gap-16 lg:pt-28"
      >
        <div class="public-reveal">
          <div
            class="inline-flex items-center gap-2 rounded-full border border-[#cbd49a]/20 bg-[#cbd49a]/[0.06] px-3 py-1.5 text-[10px] font-bold uppercase tracking-[0.23em] text-[#cbd49a]"
          >
            <span
              class="h-1.5 w-1.5 rounded-full bg-[#cbd49a] shadow-[0_0_12px_rgba(203,212,154,0.8)]"
            />
            Customer operations, composed
          </div>
          <h1
            class="public-title mt-8 max-w-[760px] text-[clamp(3.7rem,8.5vw,7.5rem)] leading-[0.86] tracking-[-0.072em] text-[#f4f5ec]"
          >
            Every signal.<br />
            <span class="text-[#cbd49a]">One reply.</span>
          </h1>
          <p
            class="mt-8 max-w-[650px] text-base leading-8 text-white/56 sm:text-lg"
          >
            ReReply brings customer conversations, CRM workflows, bookings,
            automation and performance insight into one focused workspace—so
            teams can act with context, not tab fatigue.
          </p>
          <div class="mt-9 flex flex-wrap gap-3">
            <RouterLink
              to="/login"
              class="group inline-flex items-center gap-2 rounded-full bg-[#dce2ad] px-5 py-3 text-sm font-semibold text-[#272c19] transition hover:-translate-y-0.5 hover:bg-[#edf1cb]"
            >
              Open ReReply
              <ArrowRight
                class="h-4 w-4 transition-transform group-hover:translate-x-0.5"
              />
            </RouterLink>
            <a
              href="#integrations"
              class="inline-flex items-center gap-2 rounded-full border border-white/12 bg-white/[0.035] px-5 py-3 text-sm font-semibold text-white/70 transition hover:border-white/20 hover:bg-white/[0.06] hover:text-white"
            >
              Explore integrations
              <ChevronRight class="h-4 w-4" />
            </a>
          </div>
        </div>

        <div
          class="public-reveal public-reveal-delay relative"
          aria-label="ReReply workspace overview"
        >
          <div
            class="absolute -inset-8 rounded-full bg-[#aeb77a]/[0.08] blur-3xl"
            aria-hidden="true"
          />
          <div
            class="workspace-frame relative overflow-hidden rounded-[30px] border border-white/[0.1] bg-[#11160f]/90 p-4 shadow-[0_45px_120px_rgba(0,0,0,0.5)] sm:p-5"
          >
            <div
              class="flex items-center justify-between border-b border-white/[0.07] pb-4"
            >
              <div class="flex items-center gap-2">
                <span class="h-2 w-2 rounded-full bg-[#cbd49a]" />
                <span
                  class="text-[10px] font-bold uppercase tracking-[0.2em] text-white/45"
                  >Live workspace</span
                >
              </div>
              <span
                class="rounded-full border border-white/[0.08] px-2.5 py-1 text-[9px] font-semibold uppercase tracking-[0.13em] text-white/35"
                >Tenant aware</span
              >
            </div>

            <div
              class="mt-4 grid grid-cols-[108px_1fr] gap-3 sm:grid-cols-[128px_1fr]"
            >
              <div
                class="rounded-2xl border border-white/[0.07] bg-black/20 p-3"
              >
                <div
                  class="mb-4 flex h-8 w-8 items-center justify-center rounded-xl bg-[#cbd49a]/10 text-[#cbd49a]"
                >
                  <MessageCircleMore class="h-4 w-4" />
                </div>
                <div class="space-y-2.5">
                  <div class="h-2 w-4/5 rounded-full bg-[#cbd49a]/35" />
                  <div class="h-2 w-3/5 rounded-full bg-white/10" />
                  <div class="h-2 w-2/3 rounded-full bg-white/10" />
                  <div class="h-2 w-1/2 rounded-full bg-white/10" />
                </div>
              </div>

              <div class="space-y-3">
                <div
                  class="rounded-2xl border border-[#cbd49a]/15 bg-[#cbd49a]/[0.045] p-4"
                >
                  <div class="flex items-start justify-between gap-3">
                    <div>
                      <p
                        class="text-[9px] font-bold uppercase tracking-[0.18em] text-[#cbd49a]/70"
                      >
                        Search visibility
                      </p>
                      <p class="mt-1 text-sm font-semibold text-[#f0f1e8]">
                        Verified website performance
                      </p>
                    </div>
                    <Search class="h-4 w-4 text-[#cbd49a]" />
                  </div>
                  <div
                    class="mt-5 flex h-20 items-end gap-1.5"
                    aria-hidden="true"
                  >
                    <span
                      v-for="height in [28, 44, 35, 62, 54, 76, 66, 88, 72, 94]"
                      :key="height"
                      class="flex-1 rounded-t-sm bg-gradient-to-t from-[#707849] to-[#dce2ad]"
                      :style="{ height: `${height}%` }"
                    />
                  </div>
                </div>

                <div class="grid grid-cols-2 gap-3">
                  <div
                    class="rounded-2xl border border-white/[0.07] bg-black/20 p-3.5"
                  >
                    <CalendarCheck2 class="h-4 w-4 text-[#cbd49a]/80" />
                    <p
                      class="mt-4 text-[9px] uppercase tracking-[0.15em] text-white/32"
                    >
                      Follow-up
                    </p>
                    <p class="mt-1 text-xs font-semibold text-white/75">
                      Owner assigned
                    </p>
                  </div>
                  <div
                    class="rounded-2xl border border-white/[0.07] bg-black/20 p-3.5"
                  >
                    <Bot class="h-4 w-4 text-[#cbd49a]/80" />
                    <p
                      class="mt-4 text-[9px] uppercase tracking-[0.15em] text-white/32"
                    >
                      Copilot
                    </p>
                    <p class="mt-1 text-xs font-semibold text-white/75">
                      Human reviewed
                    </p>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </section>

      <section class="border-y border-white/[0.07] bg-black/10">
        <div
          class="mx-auto grid max-w-[1220px] divide-y divide-white/[0.07] px-5 sm:grid-cols-3 sm:divide-x sm:divide-y-0 sm:px-8"
        >
          <div class="py-7 sm:px-6 sm:first:pl-0">
            <p
              class="text-[10px] font-bold uppercase tracking-[0.2em] text-[#cbd49a]/65"
            >
              Access
            </p>
            <p class="mt-2 text-sm font-medium text-white/70">
              Role-based and tenant isolated
            </p>
          </div>
          <div class="py-7 sm:px-6">
            <p
              class="text-[10px] font-bold uppercase tracking-[0.2em] text-[#cbd49a]/65"
            >
              Control
            </p>
            <p class="mt-2 text-sm font-medium text-white/70">
              Connections remain user directed
            </p>
          </div>
          <div class="py-7 sm:px-6 sm:last:pr-0">
            <p
              class="text-[10px] font-bold uppercase tracking-[0.2em] text-[#cbd49a]/65"
            >
              Visibility
            </p>
            <p class="mt-2 text-sm font-medium text-white/70">
              Read-only Google search insight
            </p>
          </div>
        </div>
      </section>

      <section
        id="product"
        class="mx-auto max-w-[1220px] scroll-mt-28 px-5 py-20 sm:px-8 sm:py-28"
      >
        <div class="grid gap-8 lg:grid-cols-[0.72fr_1.28fr] lg:gap-16">
          <div>
            <p
              class="text-[10px] font-bold uppercase tracking-[0.23em] text-[#cbd49a]/70"
            >
              The operating rhythm
            </p>
            <h2
              class="public-heading mt-5 max-w-md text-[clamp(2.5rem,5vw,4.6rem)] leading-[0.95] tracking-[-0.055em] text-[#f1f2e9]"
            >
              Less switching.<br />More knowing.
            </h2>
          </div>
          <div
            class="grid gap-px overflow-hidden rounded-[28px] border border-white/[0.08] bg-white/[0.08]"
          >
            <article
              v-for="capability in capabilities"
              :key="capability.number"
              class="group grid gap-5 bg-[#0d110b] p-6 transition hover:bg-[#11160f] sm:grid-cols-[54px_1fr_auto] sm:items-center sm:p-8"
            >
              <span class="public-heading text-2xl text-[#cbd49a]/38">{{
                capability.number
              }}</span>
              <div>
                <h3
                  class="text-lg font-semibold tracking-[-0.025em] text-[#eff0e8]"
                >
                  {{ capability.title }}
                </h3>
                <p class="mt-2 max-w-xl text-sm leading-6 text-white/45">
                  {{ capability.description }}
                </p>
              </div>
              <component
                :is="capability.icon"
                class="hidden h-5 w-5 text-[#cbd49a]/45 transition group-hover:text-[#cbd49a] sm:block"
              />
            </article>
          </div>
        </div>
      </section>

      <section
        id="integrations"
        class="scroll-mt-24 border-y border-white/[0.07] bg-[#0f130d]/78"
      >
        <div class="mx-auto max-w-[1220px] px-5 py-20 sm:px-8 sm:py-28">
          <div
            class="flex flex-col gap-7 sm:flex-row sm:items-end sm:justify-between"
          >
            <div>
              <p
                class="text-[10px] font-bold uppercase tracking-[0.23em] text-[#cbd49a]/70"
              >
                Connected, deliberately
              </p>
              <h2
                class="public-heading mt-5 max-w-2xl text-[clamp(2.7rem,5.5vw,5rem)] leading-[0.94] tracking-[-0.058em] text-[#f1f2e9]"
              >
                Bring the signal.<br />Keep the control.
              </h2>
            </div>
            <p class="max-w-md text-sm leading-7 text-white/46">
              ReReply integrations are enabled by authorised workspace users,
              scoped to the work they need and removable when they no longer
              serve the team.
            </p>
          </div>

          <div class="mt-12 grid gap-5 lg:grid-cols-[1.35fr_0.65fr]">
            <article
              class="relative overflow-hidden rounded-[30px] border border-[#cbd49a]/20 bg-[#cbd49a]/[0.055] p-6 sm:p-9"
            >
              <div
                class="absolute -right-20 -top-24 h-72 w-72 rounded-full bg-[#cbd49a]/10 blur-3xl"
                aria-hidden="true"
              />
              <div class="relative">
                <div class="flex flex-wrap items-start justify-between gap-5">
                  <div class="flex items-center gap-4">
                    <span
                      class="flex h-12 w-12 items-center justify-center rounded-2xl border border-[#cbd49a]/20 bg-black/20 text-[#d9dfa9]"
                    >
                      <Search class="h-5 w-5" />
                    </span>
                    <div>
                      <p
                        class="text-[10px] font-bold uppercase tracking-[0.18em] text-[#cbd49a]/65"
                      >
                        Read-only integration
                      </p>
                      <h3
                        class="mt-1 text-xl font-semibold tracking-[-0.03em] text-[#f2f3eb]"
                      >
                        Google Search Console
                      </h3>
                    </div>
                  </div>
                  <span
                    class="inline-flex items-center gap-1.5 rounded-full border border-[#cbd49a]/20 bg-black/15 px-3 py-1.5 text-[10px] font-bold uppercase tracking-[0.14em] text-[#d9dfa9]"
                  >
                    <Eye class="h-3.5 w-3.5" />
                    No write access
                  </span>
                </div>

                <p class="mt-7 max-w-2xl text-base leading-7 text-white/56">
                  An authorised user can connect verified website properties and
                  view how those sites appear in Google Search. ReReply requests
                  only the read-only Search Console permission.
                </p>

                <div class="mt-7 flex flex-wrap gap-2">
                  <span
                    v-for="signal in searchSignals"
                    :key="signal"
                    class="rounded-full border border-white/[0.09] bg-black/15 px-3 py-1.5 text-xs font-medium text-white/55"
                  >
                    {{ signal }}
                  </span>
                </div>

                <div
                  class="mt-8 grid gap-3 border-t border-white/[0.08] pt-6 sm:grid-cols-3"
                >
                  <div class="flex gap-2.5 text-xs leading-5 text-white/48">
                    <Check class="mt-0.5 h-3.5 w-3.5 shrink-0 text-[#cbd49a]" />
                    Select only verified properties
                  </div>
                  <div class="flex gap-2.5 text-xs leading-5 text-white/48">
                    <Check class="mt-0.5 h-3.5 w-3.5 shrink-0 text-[#cbd49a]" />
                    Encrypted OAuth credential
                  </div>
                  <div class="flex gap-2.5 text-xs leading-5 text-white/48">
                    <Check class="mt-0.5 h-3.5 w-3.5 shrink-0 text-[#cbd49a]" />
                    Disconnect whenever needed
                  </div>
                </div>
              </div>
            </article>

            <div class="grid gap-5">
              <article
                class="rounded-[26px] border border-white/[0.08] bg-black/15 p-6"
              >
                <MessageCircleMore class="h-5 w-5 text-[#cbd49a]" />
                <h3
                  class="mt-7 text-lg font-semibold tracking-[-0.025em] text-[#eff0e8]"
                >
                  WhatsApp Business
                </h3>
                <p class="mt-2 text-sm leading-6 text-white/43">
                  Coordinate customer conversations, templates and operational
                  handover from a governed workspace.
                </p>
              </article>
              <article
                class="rounded-[26px] border border-white/[0.08] bg-black/15 p-6"
              >
                <Bot class="h-5 w-5 text-[#cbd49a]" />
                <h3
                  class="mt-7 text-lg font-semibold tracking-[-0.025em] text-[#eff0e8]"
                >
                  Optional AI assistance
                </h3>
                <p class="mt-2 text-sm leading-6 text-white/43">
                  Support drafting and workflow tasks while keeping people
                  responsible for review and decisions.
                </p>
              </article>
            </div>
          </div>
        </div>
      </section>

      <section
        id="trust"
        class="mx-auto max-w-[1220px] scroll-mt-28 px-5 py-20 sm:px-8 sm:py-28"
      >
        <div
          class="trust-panel relative overflow-hidden rounded-[32px] border border-white/[0.09] p-7 sm:p-12"
        >
          <div
            class="absolute -right-20 -top-24 h-80 w-80 rounded-full bg-[#cbd49a]/[0.08] blur-3xl"
            aria-hidden="true"
          />
          <div
            class="relative grid gap-10 lg:grid-cols-[0.8fr_1.2fr] lg:items-end"
          >
            <div>
              <div
                class="flex h-11 w-11 items-center justify-center rounded-2xl border border-[#cbd49a]/20 bg-[#cbd49a]/[0.07] text-[#cbd49a]"
              >
                <ShieldCheck class="h-5 w-5" />
              </div>
              <h2
                class="public-heading mt-7 text-[clamp(2.5rem,5vw,4.5rem)] leading-[0.94] tracking-[-0.055em] text-[#f1f2e9]"
              >
                Trust is part of the workflow.
              </h2>
            </div>
            <div>
              <p class="max-w-2xl text-base leading-8 text-white/52">
                ReReply separates customer organisations, limits access by role
                and keeps integration choices in the hands of authorised users.
                Our public policies explain what data is used, why it is needed
                and how to disconnect or request deletion.
              </p>
              <div class="mt-7 flex flex-wrap gap-3">
                <RouterLink
                  to="/privacy"
                  class="inline-flex items-center gap-2 rounded-full border border-white/12 bg-white/[0.035] px-4 py-2.5 text-sm font-semibold text-[#dfe5b6] transition hover:border-[#cbd49a]/30 hover:bg-[#cbd49a]/[0.06]"
                >
                  Privacy Policy
                  <ChevronRight class="h-4 w-4" />
                </RouterLink>
                <RouterLink
                  to="/terms"
                  class="inline-flex items-center gap-2 rounded-full border border-white/12 bg-white/[0.035] px-4 py-2.5 text-sm font-semibold text-[#dfe5b6] transition hover:border-[#cbd49a]/30 hover:bg-[#cbd49a]/[0.06]"
                >
                  Terms of Service
                  <ChevronRight class="h-4 w-4" />
                </RouterLink>
                <RouterLink
                  to="/data-deletion"
                  class="inline-flex items-center gap-2 rounded-full px-4 py-2.5 text-sm font-semibold text-white/48 transition hover:text-[#dfe5b6]"
                >
                  Data deletion
                </RouterLink>
              </div>
            </div>
          </div>
        </div>
      </section>
    </main>

    <footer class="relative z-10 border-t border-white/[0.07]">
      <div
        class="mx-auto flex max-w-[1220px] flex-col gap-7 px-5 py-10 text-xs text-white/35 sm:flex-row sm:items-center sm:justify-between sm:px-8"
      >
        <div>
          <ReReplyLogo size="sm" tone="light" />
          <p class="mt-3">Conversations, aligned. Decisions, informed.</p>
        </div>
        <nav aria-label="Footer" class="flex flex-wrap gap-x-5 gap-y-3">
          <RouterLink
            to="/about"
            class="text-white/45 transition hover:text-[#cbd49a]"
            >About</RouterLink
          >
          <RouterLink
            to="/privacy"
            class="text-white/45 transition hover:text-[#cbd49a]"
            >Privacy</RouterLink
          >
          <RouterLink
            to="/terms"
            class="text-white/45 transition hover:text-[#cbd49a]"
            >Terms</RouterLink
          >
          <RouterLink
            to="/data-deletion"
            class="text-white/45 transition hover:text-[#cbd49a]"
            >Data deletion</RouterLink
          >
          <a
            href="mailto:medtechcorps@gmail.com"
            class="text-white/45 transition hover:text-[#cbd49a]"
            >Contact</a
          >
        </nav>
      </div>
    </footer>
  </div>
</template>

<style scoped>
.public-shell {
  background:
    radial-gradient(
      circle at 76% 8%,
      rgba(111, 121, 67, 0.12),
      transparent 30rem
    ),
    linear-gradient(180deg, #0a0d08 0%, #0d110b 44%, #090b08 100%);
}

.public-grid {
  background-image:
    linear-gradient(rgba(203, 212, 154, 0.024) 1px, transparent 1px),
    linear-gradient(90deg, rgba(203, 212, 154, 0.024) 1px, transparent 1px);
  background-size: 58px 58px;
  mask-image: linear-gradient(to bottom, black 0%, transparent 72%);
}

.public-glow {
  background: rgba(107, 117, 62, 0.17);
  filter: blur(120px);
}

.public-glow-bottom {
  background: rgba(203, 212, 154, 0.065);
}

.public-title,
.public-heading {
  font-family: Georgia, "Times New Roman", serif;
  font-weight: 500;
}

.workspace-frame::after {
  background: linear-gradient(
    90deg,
    transparent,
    rgba(220, 226, 173, 0.38),
    transparent
  );
  content: "";
  height: 1px;
  left: 14%;
  position: absolute;
  right: 14%;
  top: 0;
}

.trust-panel {
  background:
    linear-gradient(118deg, rgba(203, 212, 154, 0.055), transparent 55%),
    rgba(13, 17, 11, 0.88);
}

.public-reveal {
  animation: public-reveal 700ms cubic-bezier(0.2, 0.72, 0.2, 1) both;
}

.public-reveal-delay {
  animation-delay: 120ms;
}

@keyframes public-reveal {
  from {
    opacity: 0;
    transform: translateY(18px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}

@media (prefers-reduced-motion: reduce) {
  .public-reveal {
    animation: none;
  }
}
</style>
