<script setup lang="ts">
import { ref } from 'vue'

import type { TrustedTokenItem } from '@/types/workers'

defineProps<{
  tokens: TrustedTokenItem[]
  creatingToken: boolean
  deletingTokenId: string
  copyingTokenId: string
  copyButtonText: (tokenID: string) => string
  deleteButtonText: (tokenID: string) => string
}>()

const emit = defineEmits<{
  createToken: [payload: { name: string; token?: string }]
  copyToken: [tokenID: string]
  deleteToken: [tokenID: string]
}>()

const nameInput = ref('')
const tokenInput = ref('')

function submitCreateToken(): void {
  const name = nameInput.value.trim()
  if (!name) {
    return
  }

  const token = tokenInput.value.trim()
  emit('createToken', {
    name,
    token: token === '' ? undefined : token,
  })
}
</script>

<template>
  <section class="token-panel">
    <div class="token-panel-header">
      <h2>Trusted Tokens</h2>
      <p>Total: {{ tokens.length }}</p>
    </div>

    <form class="token-form" @submit.prevent="submitCreateToken">
      <label class="token-field">
        <span>Name</span>
        <input v-model="nameInput" type="text" maxlength="64" required placeholder="ci-prod" />
      </label>
      <label class="token-field">
        <span>Token (optional)</span>
        <input v-model="tokenInput" type="text" maxlength="256" placeholder="Leave blank to auto-generate" />
      </label>
      <button type="submit" class="ghost-btn small" :disabled="creatingToken || nameInput.trim() === ''">
        {{ creatingToken ? 'Creating...' : 'Add Token' }}
      </button>
    </form>

    <p v-if="tokens.length === 0" class="empty-hint">未配置，MCP 与受保护 HTTP 端点当前全部拒绝。</p>

    <ul v-else class="token-list">
      <li v-for="item in tokens" :key="item.id" class="token-item">
        <div class="token-content">
          <p class="token-name">{{ item.name }}</p>
          <code class="token-value">{{ item.token_masked }}</code>
        </div>

        <div class="token-actions">
          <button
            type="button"
            class="ghost-btn small"
            :disabled="copyingTokenId === item.id || deletingTokenId === item.id"
            @click="emit('copyToken', item.id)"
          >
            {{ copyButtonText(item.id) }}
          </button>
          <button
            type="button"
            class="ghost-btn small danger"
            :disabled="deletingTokenId === item.id"
            @click="emit('deleteToken', item.id)"
          >
            {{ deleteButtonText(item.id) }}
          </button>
        </div>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.token-panel {
  border: 1px solid var(--stroke);
  border-radius: 18px;
  background: var(--surface);
  box-shadow: var(--shadow);
  padding: 16px 18px;
  animation: rise-in 540ms ease-out;
}

.token-panel-header {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
}

h2 {
  margin: 0;
  font-size: 1.1rem;
}

.token-panel-header p {
  margin: 0;
  color: var(--text-secondary);
  font-size: 13px;
}

.token-form {
  margin: 14px 0 0;
  display: grid;
  gap: 10px;
}

.token-field {
  display: grid;
  gap: 6px;
}

.token-field span {
  color: var(--text-secondary);
  font-size: 12px;
}

.token-field input {
  border: 1px solid var(--stroke);
  border-radius: 10px;
  padding: 8px 10px;
  font-size: 13px;
}

.token-field input:focus {
  outline: none;
  border-color: #3f8cff;
  box-shadow: 0 0 0 3px rgba(63, 140, 255, 0.15);
}

.empty-hint {
  margin: 12px 0 0;
  color: var(--text-secondary);
  font-size: 13px;
}

.token-list {
  list-style: none;
  margin: 14px 0 0;
  padding: 0;
  display: grid;
  gap: 8px;
}

.token-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  border: 1px solid var(--stroke);
  background: #fafbfd;
  border-radius: 12px;
  padding: 10px 12px;
}

.token-content {
  min-width: 0;
}

.token-name {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
}

.token-value {
  font-family: 'IBM Plex Mono', monospace;
  font-size: 12px;
  color: #1b2230;
  word-break: break-all;
}

.token-actions {
  display: flex;
  gap: 8px;
}

@media (max-width: 620px) {
  .token-item {
    align-items: flex-start;
    flex-direction: column;
  }
}
</style>
