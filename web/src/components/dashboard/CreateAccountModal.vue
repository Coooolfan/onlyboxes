<script setup lang="ts">
import { ref } from 'vue'
import { createAccountAPI } from '@/services/auth.api'

const emit = defineEmits<{
  close: []
}>()

const createAccountUsername = ref('')
const createAccountPassword = ref('')
const creatingAccount = ref(false)
const createAccountError = ref('')
const createAccountSuccess = ref('')

async function submitCreateAccount(): Promise<void> {
  if (creatingAccount.value) {
    return
  }
  const username = createAccountUsername.value.trim()
  const password = createAccountPassword.value
  if (!username || !password) {
    createAccountError.value = 'username and password are required'
    createAccountSuccess.value = ''
    return
  }

  createAccountError.value = ''
  createAccountSuccess.value = ''
  creatingAccount.value = true
  try {
    const payload = await createAccountAPI(username, password)
    createAccountUsername.value = ''
    createAccountPassword.value = ''
    createAccountSuccess.value = `Created account ${payload.account.username}`
  } catch (error) {
    createAccountError.value = error instanceof Error ? error.message : 'Failed to create account.'
  } finally {
    creatingAccount.value = false
  }
}

function closeModal(): void {
  emit('close')
}
</script>

<template>
  <div class="account-modal-backdrop" @click.self="closeModal">
    <div
      class="account-modal"
      role="dialog"
      aria-modal="true"
      aria-labelledby="account-modal-title"
    >
      <div class="account-modal-header">
        <h3 id="account-modal-title">Create Account</h3>
        <button type="button" class="ghost-btn small" @click="closeModal">Close</button>
      </div>

      <div class="account-modal-content">
        <p class="account-modal-note">
          Registration is enabled. New accounts are always non-admin.
        </p>

        <form class="account-form" @submit.prevent="submitCreateAccount">
          <label class="account-field">
            <span>Username</span>
            <input
              v-model="createAccountUsername"
              type="text"
              autocomplete="off"
              spellcheck="false"
              required
            />
          </label>

          <label class="account-field">
            <span>Password</span>
            <input
              v-model="createAccountPassword"
              type="password"
              autocomplete="new-password"
              required
            />
          </label>

          <p v-if="createAccountError" class="account-error">{{ createAccountError }}</p>
          <p v-if="createAccountSuccess" class="account-success">{{ createAccountSuccess }}</p>

          <div class="account-modal-actions">
            <button
              type="button"
              class="ghost-btn small"
              :disabled="creatingAccount"
              @click="closeModal"
            >
              Cancel
            </button>
            <button type="submit" class="primary-btn small" :disabled="creatingAccount">
              {{ creatingAccount ? 'Creating...' : 'Create Account' }}
            </button>
          </div>
        </form>
      </div>
    </div>
  </div>
</template>

<style scoped>
.account-modal-backdrop {
  position: fixed;
  inset: 0;
  z-index: 1000;
  background: rgba(0, 0, 0, 0.4);
  backdrop-filter: blur(4px);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}

.account-modal {
  width: min(480px, 100%);
  border-radius: var(--radius-lg);
  border: 1px solid var(--stroke);
  background: var(--surface);
  box-shadow: var(--shadow-modal);
  display: flex;
  flex-direction: column;
}

.account-modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 20px 24px;
  border-bottom: 1px solid var(--stroke);
}

.account-modal-header h3 {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
}

.account-modal-content {
  padding: 24px;
  display: grid;
  gap: 20px;
}

.account-modal-note {
  margin: 0;
  color: var(--text-secondary);
  font-size: 14px;
  line-height: 1.5;
}

.account-form {
  display: grid;
  gap: 16px;
}

.account-field {
  display: grid;
  gap: 8px;
}

.account-field span {
  color: var(--text-primary);
  font-size: 14px;
  font-weight: 500;
}

.account-field input {
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  padding: 10px 12px;
  font-size: 14px;
  font-family: inherit;
  transition:
    border-color 0.2s ease,
    box-shadow 0.2s ease;
}

.account-field input:focus {
  outline: none;
  border-color: var(--text-secondary);
  box-shadow: 0 0 0 1px var(--text-secondary);
}

.account-error {
  margin: 0;
  border: 1px solid #fca5a5;
  border-radius: var(--radius);
  background: #fef2f2;
  color: #e00;
  padding: 10px 12px;
  font-size: 14px;
}

.account-success {
  margin: 0;
  border: 1px solid #86efac;
  border-radius: var(--radius);
  background: #f0fdf4;
  color: #166534;
  padding: 10px 12px;
  font-size: 14px;
}

.account-modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 12px;
  padding-top: 20px;
}

@media (max-width: 600px) {
  .account-modal-actions {
    flex-direction: column-reverse;
  }

  .account-modal-actions button {
    width: 100%;
  }
}
</style>
