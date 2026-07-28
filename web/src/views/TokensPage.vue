<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'

import PageHeader from '@/components/layout/PageHeader.vue'
import TrustedTokensPanel from '@/components/tokens/TrustedTokensPanel.vue'
import AppAlert from '@/components/ui/AppAlert.vue'
import { useRefreshedAtText } from '@/composables/useRefreshedAtText'
import { useAuthStore } from '@/stores/auth'
import { useTokensStore } from '@/stores/tokens'

const authStore = useAuthStore()
const tokensStore = useTokensStore()

const refreshedAtText = useRefreshedAtText(computed(() => tokensStore.refreshedAt))

onMounted(() => {
  void tokensStore.loadTokens()
})

onBeforeUnmount(() => {
  tokensStore.teardown()
})
</script>

<template>
  <div class="grid gap-6">
    <PageHeader
      eyebrow="Onlyboxes / Token Console"
      title="Trusted Token Management"
      :loading="tokensStore.loading"
      :refreshed-at-text="refreshedAtText"
      @refresh="tokensStore.loadTokens"
    >
      <template #subtitle>
        Account: <strong>{{ authStore.currentAccount?.username ?? '--' }}</strong>
      </template>
    </PageHeader>

    <AppAlert v-if="tokensStore.errorMessage" tone="error" with-icon>
      {{ tokensStore.errorMessage }}
    </AppAlert>

    <TrustedTokensPanel
      :tokens="tokensStore.trustedTokens"
      :deleting-token-id="tokensStore.deletingTrustedTokenID"
      @delete-token="tokensStore.deleteTrustedToken"
    />
  </div>
</template>
