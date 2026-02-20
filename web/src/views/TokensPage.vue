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
  <main class="tokens-content">
    <header class="tokens-header">
      <div>
        <p class="eyebrow">Onlyboxes / Token Console</p>
        <h1>Trusted Token Management</h1>
        <p class="subtitle">
          Account: <strong>{{ authStore.currentAccount?.username ?? '--' }}</strong>
        </p>
      </div>

      <div class="header-actions">
        <button
          class="primary-btn"
          type="button"
          :disabled="tokensStore.loading"
          @click="tokensStore.loadTokens"
        >
          {{ tokensStore.loading ? 'Refreshing...' : 'Refresh' }}
        </button>
        <button class="ghost-btn" type="button" @click="handleLogout">Logout</button>
      </div>
    </header>

    <section class="board-panel">
      <div class="panel-topbar">
        <p class="panel-meta">
          Last refresh:
          <span>{{ refreshedAtText }}</span>
        </p>
      </div>

      <ErrorBanner
        v-if="tokensStore.errorMessage"
        :message="tokensStore.errorMessage"
        class="panel-error"
      />

      <div class="panel-body">
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

<style scoped>
.tokens-content {
  position: relative;
  z-index: 2;
  margin: 0 auto;
  width: min(960px, 100%);
  display: grid;
  gap: 24px;
}

.tokens-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  background: var(--surface);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-lg);
  padding: 32px;
  box-shadow: var(--shadow);
  animation: rise-in 500ms ease-out;
}

.eyebrow {
  margin: 0;
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--text-secondary);
}

h1 {
  margin: 12px 0 8px;
  font-size: 24px;
  font-weight: 600;
  line-height: 1.2;
  letter-spacing: -0.02em;
}

.subtitle {
  margin: 0;
  color: var(--text-secondary);
  font-size: 14px;
  line-height: 1.5;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}

.board-panel {
  border: 1px solid var(--stroke);
  border-radius: var(--radius-lg);
  background: var(--surface);
  box-shadow: var(--shadow);
  overflow: hidden;
}

.panel-topbar {
  display: flex;
  justify-content: flex-end;
  align-items: center;
  padding: 12px 24px;
  border-bottom: 1px solid var(--stroke);
  background: var(--surface-soft);
}

.panel-meta {
  margin: 0;
  color: var(--text-secondary);
  font-size: 13px;
}

.panel-meta span {
  color: var(--text-primary);
  font-weight: 500;
}

.panel-error {
  margin: 16px 24px 0;
}

.panel-body {
  padding: 24px;
}

@media (max-width: 960px) {
  .tokens-header {
    flex-direction: column;
  }

  .header-actions {
    width: 100%;
    flex-wrap: wrap;
  }
}
</style>
