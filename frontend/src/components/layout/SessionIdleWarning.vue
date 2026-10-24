<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { useSessionIdle } from '@/composables/useSessionIdle'

const { t } = useI18n()
const { warningOpen, countdown, staySignedIn, signOutNow } = useSessionIdle()

// Escape dismisses the warning the same way "Stay signed in" does.
function onOpenChange(open: boolean) {
  if (!open && warningOpen.value) void staySignedIn()
}
</script>

<template>
  <AlertDialog :open="warningOpen" @update:open="onOpenChange">
    <AlertDialogContent class="max-w-md">
      <AlertDialogHeader data-testid="session-idle-warning">
        <AlertDialogTitle>{{ t('sessionSecurity.idleWarningTitle') }}</AlertDialogTitle>
        <AlertDialogDescription>
          {{ t('sessionSecurity.idleWarningBody', { time: countdown }) }}
        </AlertDialogDescription>
      </AlertDialogHeader>
      <AlertDialogFooter>
        <Button
          type="button"
          variant="outline"
          data-testid="session-idle-sign-out"
          @click="signOutNow"
        >
          {{ t('sessionSecurity.signOut') }}
        </Button>
        <Button
          type="button"
          data-testid="session-idle-stay"
          @click="staySignedIn"
        >
          {{ t('sessionSecurity.staySignedIn') }}
        </Button>
      </AlertDialogFooter>
    </AlertDialogContent>
  </AlertDialog>
</template>
