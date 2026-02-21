<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { useRouter } from 'vue-router'

import ErrorBanner from '@/components/common/ErrorBanner.vue'
import TrustedTokensPanel from '@/components/dashboard/TrustedTokensPanel.vue'
import { useAuthStore } from '@/stores/auth'
import { useTokensStore } from '@/stores/tokens'

const authStore = useAuthStore()
const tokensStore = useTokensStore()
const router = useRouter()

const refreshedAtText = computed(() => {
  if (!tokensStore.refreshedAt) {
    return 'never'
  }
  return tokensStore.formatDateTime(tokensStore.refreshedAt.toISOString())
})

async function handleLogout(): Promise<void> {
  await authStore.logout()
  tokensStore.teardown()
  tokensStore.reset()
  await router.replace('/login')
}

onMounted(async () => {
  await tokensStore.loadTokens()
})

onBeforeUnmount(() => {
  tokensStore.teardown()
})
</script>

<template>
  <main class="relative z-2 mx-auto w-[min(960px,100%)] grid gap-6">
    <header class="flex items-start justify-between gap-5 bg-surface border border-stroke rounded-lg p-8 shadow-card animate-rise-in max-[960px]:flex-col">
      <div>
        <p class="m-0 font-mono text-xs tracking-[0.05em] uppercase text-secondary">Onlyboxes / Token Console</p>
        <h1 class="mt-3 mb-2 text-2xl font-semibold leading-[1.2] tracking-[-0.02em]">Trusted Token Management</h1>
        <p class="m-0 text-secondary text-sm leading-normal">
          Account: <strong>{{ authStore.currentAccount?.username ?? '--' }}</strong>
        </p>
      </div>

      <div class="flex items-center gap-3 max-[960px]:w-full max-[960px]:flex-wrap">
        <button
          class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          :disabled="tokensStore.loading"
          @click="tokensStore.loadTokens"
        >
          {{ tokensStore.loading ? 'Refreshing...' : 'Refresh' }}
        </button>
        <button
          class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          @click="handleLogout"
        >
          Logout
        </button>
      </div>
    </header>

    <section class="border border-stroke rounded-lg bg-surface shadow-card overflow-hidden">
      <div class="flex justify-end items-center px-6 py-3 border-b border-stroke bg-surface-soft">
        <p class="m-0 text-secondary text-[13px]">
          Last refresh:
          <span class="text-primary font-medium">{{ refreshedAtText }}</span>
        </p>
      </div>

      <ErrorBanner
        v-if="tokensStore.errorMessage"
        :message="tokensStore.errorMessage"
        class="mx-6 mt-4"
      />

      <div class="p-6">
        <TrustedTokensPanel
          :tokens="tokensStore.trustedTokens"
          :creating-token="tokensStore.creatingTrustedToken"
          :deleting-token-id="tokensStore.deletingTrustedTokenID"
          :delete-button-text="tokensStore.trustedTokenDeleteButtonText"
          :create-token="tokensStore.createTrustedToken"
          :format-date-time="tokensStore.formatDateTime"
          @delete-token="tokensStore.deleteTrustedToken"
        />
      </div>
    </section>
  </main>
</template>
