<script setup lang="ts">
import AppButton from '@/components/ui/AppButton.vue'
import AppModal from '@/components/ui/AppModal.vue'
import { useConfirmHost } from '@/composables/useConfirm'

const { pending, accept, dismiss } = useConfirmHost()
</script>

<template>
  <AppModal
    v-if="pending"
    :title="pending.title"
    size="sm"
    panel-class="confirm-dialog"
    @close="dismiss"
  >
    <p class="m-0 text-sm leading-normal text-secondary">{{ pending.message }}</p>
    <code
      v-if="pending.detail"
      class="ui-code-inline block rounded-default border px-3 py-2 font-mono text-[13px] break-all"
      >{{ pending.detail }}</code
    >

    <template #footer>
      <AppButton size="sm" data-testid="confirm-dialog-cancel" @click="dismiss">
        {{ pending.cancelLabel ?? 'Cancel' }}
      </AppButton>
      <AppButton
        size="sm"
        :variant="pending.destructive ? 'danger' : 'primary'"
        data-testid="confirm-dialog-accept"
        data-autofocus
        @click="accept"
      >
        {{ pending.confirmLabel ?? 'Confirm' }}
      </AppButton>
    </template>
  </AppModal>
</template>
