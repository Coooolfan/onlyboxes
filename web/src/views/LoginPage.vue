<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import { isInvalidCredentialsError } from '@/services/auth.api'
import { useAuthStore } from '@/stores/auth'
import { toErrorMessage } from '@/utils/async'

const authStore = useAuthStore()
const route = useRoute()
const router = useRouter()

const username = ref('')
const password = ref('')
const errorMessage = ref('')
const submitting = ref(false)

const canSubmit = computed(() => username.value.trim() !== '' && password.value !== '')

function resolveRedirect(): string {
  const redirect = route.query.redirect
  if (typeof redirect === 'string' && redirect.startsWith('/')) {
    return redirect
  }
  return authStore.homePath
}

async function submitLogin(): Promise<void> {
  if (submitting.value) {
    return
  }

  errorMessage.value = ''
  if (!canSubmit.value) {
    errorMessage.value = 'Enter your username and password.'
    return
  }

  submitting.value = true
  try {
    await authStore.login(username.value, password.value)
    await router.replace(resolveRedirect())
  } catch (error) {
    errorMessage.value = isInvalidCredentialsError(error)
      ? 'Invalid username or password.'
      : toErrorMessage(error, 'Sign in failed. Try again later.')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section
    class="ui-rise w-[min(440px,100%)] rounded-lg border border-stroke bg-surface p-8 shadow-card max-[620px]:p-6"
  >
    <p class="m-0 font-mono text-xs tracking-[0.05em] text-secondary uppercase">
      Onlyboxes / Console Login
    </p>
    <h1 class="mt-3 mb-2 text-2xl leading-[1.2] font-semibold tracking-[-0.02em]">
      Sign In to Control Panel
    </h1>
    <p class="m-0 text-sm leading-normal text-secondary">
      Use the dashboard username and password printed in the console startup logs.
    </p>

    <form class="login-form mt-6 grid gap-4" @submit.prevent="submitLogin">
      <div class="grid gap-1.5">
        <label class="text-sm font-medium text-primary" for="dashboard-username">Username</label>
        <input
          id="dashboard-username"
          v-model="username"
          class="ui-input h-10 w-full rounded-md border px-3 text-sm"
          type="text"
          name="username"
          autocomplete="username"
          spellcheck="false"
        />
      </div>

      <div class="grid gap-1.5">
        <label class="text-sm font-medium text-primary" for="dashboard-password">Password</label>
        <input
          id="dashboard-password"
          v-model="password"
          class="ui-input h-10 w-full rounded-md border px-3 text-sm"
          type="password"
          name="password"
          autocomplete="current-password"
        />
      </div>

      <AppAlert v-if="errorMessage" tone="error" with-icon>{{ errorMessage }}</AppAlert>

      <AppButton variant="primary" type="submit" block :loading="submitting">
        {{ submitting ? 'Signing In...' : 'Sign In' }}
      </AppButton>
    </form>
  </section>
</template>
