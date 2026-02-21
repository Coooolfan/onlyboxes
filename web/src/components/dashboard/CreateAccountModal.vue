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
  <div class="fixed inset-0 z-1000 bg-black/40 backdrop-blur-xs flex items-center justify-center p-6" @click.self="closeModal">
    <div
      class="w-[min(480px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
      role="dialog"
      aria-modal="true"
      aria-labelledby="account-modal-title"
    >
      <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
        <h3 id="account-modal-title" class="m-0 text-xl font-semibold">Create Account</h3>
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          @click="closeModal"
        >
          Close
        </button>
      </div>

      <div class="p-6 grid gap-5">
        <p class="m-0 text-secondary text-sm leading-normal">
          Registration is enabled. New accounts are always non-admin.
        </p>

        <form class="grid gap-4" @submit.prevent="submitCreateAccount">
          <label class="grid gap-2">
            <span class="text-primary text-sm font-medium">Username</span>
            <input
              v-model="createAccountUsername"
              type="text"
              autocomplete="off"
              spellcheck="false"
              required
              class="border border-stroke rounded-default px-3 py-2.5 text-sm font-[inherit] transition-[border-color,box-shadow] duration-200 outline-none focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
            />
          </label>

          <label class="grid gap-2">
            <span class="text-primary text-sm font-medium">Password</span>
            <input
              v-model="createAccountPassword"
              type="password"
              autocomplete="new-password"
              required
              class="border border-stroke rounded-default px-3 py-2.5 text-sm font-[inherit] transition-[border-color,box-shadow] duration-200 outline-none focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
            />
          </label>

          <p v-if="createAccountError" class="m-0 border border-[#fca5a5] rounded-default bg-[#fef2f2] text-offline px-3 py-2.5 text-sm">{{ createAccountError }}</p>
          <p v-if="createAccountSuccess" class="m-0 border border-[#86efac] rounded-default bg-[#f0fdf4] text-[#166534] px-3 py-2.5 text-sm">{{ createAccountSuccess }}</p>
        </form>
      </div>

      <div class="flex justify-end gap-3 px-6 py-5 border-t border-stroke rounded-b-lg max-[600px]:flex-col-reverse max-[600px]:[&>button]:w-full">
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="creatingAccount"
          @click="closeModal"
        >
          Cancel
        </button>
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="creatingAccount"
          @click="submitCreateAccount"
        >
          {{ creatingAccount ? 'Creating...' : 'Create Account' }}
        </button>
      </div>
    </div>
  </div>
</template>
