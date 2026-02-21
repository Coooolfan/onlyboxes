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
  <section class="w-[min(440px,100%)] mx-auto mt-20 bg-surface border border-stroke rounded-lg p-8 shadow-card animate-rise-in">
    <p class="m-0 font-mono text-xs tracking-[0.05em] uppercase text-secondary">Onlyboxes / Console Login</p>
    <h1 class="mt-3 mb-2 text-2xl font-semibold leading-[1.2] tracking-[-0.02em]">Sign In to Control Panel</h1>
    <p class="m-0 text-secondary text-sm leading-normal">
      Use the dashboard username and password printed in the console startup logs.
    </p>

    <form class="mt-6 grid gap-4" @submit.prevent="submitLogin">
      <label class="text-sm font-medium text-primary mb-1.5 block" for="dashboard-username">Username</label>
      <input
        id="dashboard-username"
        v-model="loginUsername"
        class="w-full border border-stroke rounded-md bg-white text-primary px-3 py-2 text-sm font-[inherit] outline-none transition-[border-color,box-shadow] duration-200 h-10 focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
        type="text"
        name="username"
        autocomplete="username"
        spellcheck="false"
      />

      <label class="text-sm font-medium text-primary mb-1.5 block" for="dashboard-password">Password</label>
      <input
        id="dashboard-password"
        v-model="loginPassword"
        class="w-full border border-stroke rounded-md bg-white text-primary px-3 py-2 text-sm font-[inherit] outline-none transition-[border-color,box-shadow] duration-200 h-10 focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
        type="password"
        name="password"
        autocomplete="current-password"
      />

      <p v-if="loginErrorMessage" class="m-0 border border-[#fca5a5] rounded-default bg-[#fef2f2] text-offline px-3 py-2.5 text-sm">{{ loginErrorMessage }}</p>

      <button
        class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
        type="submit"
        :disabled="loginSubmitting"
      >
        {{ loginSubmitting ? 'Signing In...' : 'Sign In' }}
      </button>
    </form>
  </section>
</template>
