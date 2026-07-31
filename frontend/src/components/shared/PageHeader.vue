<script setup lang="ts">
import { Button } from '@/components/ui/button'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { ArrowLeft } from 'lucide-vue-next'
import type { Component } from 'vue'

defineProps<{
  title: string
  description?: string
  icon?: Component
  iconGradient?: string
  backLink?: string
  breadcrumbs?: Array<{ label: string; href?: string }>
  compactActions?: boolean
}>()
</script>

<template>
  <header class="shrink-0 border-b border-white/[0.08] light:border-slate-300 bg-[#0a0a0b]/95 light:bg-slate-50/95 backdrop-blur">
    <div class="flex min-h-16 flex-wrap items-center gap-y-3 px-4 py-3 sm:px-6">
      <RouterLink v-if="backLink" :to="backLink">
        <Button variant="ghost" size="icon" class="mr-1 sm:mr-3">
          <ArrowLeft class="h-5 w-5" />
        </Button>
      </RouterLink>
      <div
        v-if="icon"
        class="mr-3 flex h-9 w-9 shrink-0 items-center justify-center rounded-xl shadow-lg sm:h-8 sm:w-8 sm:rounded-lg"
        :class="iconGradient || 'bg-gradient-to-br from-blue-500 to-indigo-600 shadow-blue-500/20'"
      >
        <component :is="icon" class="h-4 w-4 text-white" />
      </div>
      <div class="min-w-0 flex-1">
        <h1 class="truncate text-lg font-semibold text-white light:text-slate-950 sm:text-xl">{{ title }}</h1>
        <template v-if="breadcrumbs?.length">
          <Breadcrumb>
            <BreadcrumbList>
              <template v-for="(crumb, index) in breadcrumbs" :key="index">
                <BreadcrumbItem>
                  <BreadcrumbLink v-if="crumb.href" :href="crumb.href">
                    {{ crumb.label }}
                  </BreadcrumbLink>
                  <BreadcrumbPage v-else>{{ crumb.label }}</BreadcrumbPage>
                </BreadcrumbItem>
                <BreadcrumbSeparator v-if="index < breadcrumbs.length - 1" />
              </template>
            </BreadcrumbList>
          </Breadcrumb>
        </template>
        <p v-else-if="description" class="mt-0.5 hidden truncate text-sm text-white/50 light:text-slate-600 sm:block">
          {{ description }}
        </p>
      </div>
      <div
        v-if="$slots.actions"
        :class="[
          'flex min-w-0 flex-wrap items-center gap-2',
          compactActions
            ? 'ml-auto w-auto shrink-0'
            : 'w-full sm:ml-auto sm:w-auto sm:shrink-0',
        ]"
      >
        <slot name="actions" />
      </div>
    </div>
  </header>
</template>
