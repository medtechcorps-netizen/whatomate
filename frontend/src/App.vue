<script setup lang="ts">
import { RouterView } from 'vue-router'
import { Toaster } from 'vue-sonner'
import { TooltipProvider } from '@/components/ui/tooltip'
import { useColorMode } from '@/composables/useColorMode'
import { useWindowFileDropGuard } from '@/composables/useChatDropZone'

// Initialize color mode early
const { colorMode } = useColorMode()

// A file dropped outside a drop zone must never make the browser open it
// (navigating away and losing unsent drafts).
useWindowFileDropGuard()
</script>

<template>
  <TooltipProvider>
    <div class="min-h-screen bg-background font-sans antialiased">
      <RouterView />
      <Toaster position="top-right" richColors :theme="colorMode" offset="72px" />
    </div>
  </TooltipProvider>
</template>
