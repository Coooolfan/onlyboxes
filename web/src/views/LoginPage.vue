<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { isInvalidCredentialsError } from '@/services/auth.api'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()
const route = useRoute()
const router = useRouter()

const loginUsername = ref('')
const loginPassword = ref('')
const loginErrorMessage = ref('')
const loginSubmitting = ref(false)

function resolveRedirect(): string {
  const redirect = route.query.redirect
  if (typeof redirect === 'string' && redirect.startsWith('/')) {
    return redirect
  }
  return authStore.homePath
}

async function submitLogin(): Promise<void> {
  if (loginSubmitting.value) {
    return
  }

  loginErrorMessage.value = ''
  if (loginUsername.value.trim() === '' || loginPassword.value === '') {
    loginErrorMessage.value = '请输入账号和密码。'
    return
  }

  loginSubmitting.value = true
  try {
    await authStore.login(loginUsername.value, loginPassword.value)
    await router.replace(resolveRedirect())
  } catch (error) {
    if (isInvalidCredentialsError(error)) {
      loginErrorMessage.value = '账号或密码错误'
    } else {
      loginErrorMessage.value = error instanceof Error ? error.message : '登录失败，请稍后重试。'
    }
  } finally {
    loginSubmitting.value = false
  }
}
</script>

<template>
  <section class="auth-panel">
    <p class="eyebrow">Onlyboxes / Console Login</p>
    <h1>Sign In to Control Panel</h1>
    <p class="subtitle">
      Use the dashboard username and password printed in the console startup logs.
    </p>

    <form class="login-form" @submit.prevent="submitLogin">
      <label class="field-label" for="dashboard-username">Username</label>
      <input
        id="dashboard-username"
        v-model="loginUsername"
        class="field-input"
        type="text"
        name="username"
        autocomplete="username"
        spellcheck="false"
      />

      <label class="field-label" for="dashboard-password">Password</label>
      <input
        id="dashboard-password"
        v-model="loginPassword"
        class="field-input"
        type="password"
        name="password"
        autocomplete="current-password"
      />

      <p v-if="loginErrorMessage" class="auth-error">{{ loginErrorMessage }}</p>

      <button class="primary-btn" type="submit" :disabled="loginSubmitting">
        {{ loginSubmitting ? 'Signing In...' : 'Sign In' }}
      </button>
    </form>
  </section>
</template>

<style scoped>
.auth-panel {
  width: min(440px, 100%);
  margin: 80px auto 0;
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

.login-form {
  margin-top: 24px;
  display: grid;
  gap: 16px;
}

.auth-error {
  margin: 0;
  border: 1px solid #fca5a5;
  border-radius: var(--radius);
  background: #fef2f2;
  color: #e00;
  padding: 10px 12px;
  font-size: 14px;
}
</style>
