<script setup lang="ts">
import { ref } from 'vue'

import { changePasswordAPI } from '@/services/auth.api'
import { isUnauthorizedError } from '@/services/http'
import { redirectToLogin } from '@/stores/auth-redirect'

const emit = defineEmits<{
  close: []
}>()

const currentPassword = ref('')
const newPassword = ref('')
const changingPassword = ref(false)
const changePasswordError = ref('')
const changePasswordSuccess = ref('')

async function submitChangePassword(): Promise<void> {
  if (changingPassword.value) {
    return
  }

  const currentPasswordValue = currentPassword.value
  const newPasswordValue = newPassword.value
  if (!currentPasswordValue.trim() || !newPasswordValue.trim()) {
    changePasswordError.value = 'current password and new password are required'
    changePasswordSuccess.value = ''
    return
  }

  changingPassword.value = true
  changePasswordError.value = ''
  changePasswordSuccess.value = ''

  try {
    await changePasswordAPI(currentPasswordValue, newPasswordValue)
    currentPassword.value = ''
    newPassword.value = ''
    changePasswordSuccess.value = 'Password updated successfully.'
  } catch (error) {
    if (isUnauthorizedError(error)) {
      await redirectToLogin(() => {
        closeModal()
      })
      return
    }
    changePasswordError.value =
      error instanceof Error ? error.message : 'Failed to change password.'
  } finally {
    changingPassword.value = false
  }
}

function closeModal(): void {
  emit('close')
}
</script>

<template>
  <Teleport to="body">
    <div
      class="fixed inset-0 z-1000 bg-black/40 backdrop-blur-xs flex items-center justify-center p-6"
      @click.self="closeModal"
    >
      <div
        class="password-modal w-[min(480px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
        role="dialog"
        aria-modal="true"
        aria-labelledby="change-password-modal-title"
      >
        <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
          <h3 id="change-password-modal-title" class="m-0 text-xl font-semibold">
            Change Password
          </h3>
        </div>

        <div class="p-6 grid gap-5">
          <p class="m-0 text-secondary text-sm leading-normal">
            Update your console account password. Existing sessions for this account will be
            rotated.
          </p>

          <form
            id="change-password-form"
            class="password-form grid gap-4"
            @submit.prevent="submitChangePassword"
          >
            <label class="grid gap-2">
              <span class="text-primary text-sm font-medium">Current Password</span>
              <input
                id="current-password"
                v-model="currentPassword"
                type="password"
                autocomplete="current-password"
                required
                class="border border-stroke rounded-default px-3 py-2.5 text-sm font-[inherit] transition-[border-color,box-shadow] duration-200 outline-none focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
              />
            </label>

            <label class="grid gap-2">
              <span class="text-primary text-sm font-medium">New Password</span>
              <input
                id="new-password"
                v-model="newPassword"
                type="password"
                autocomplete="new-password"
                required
                class="border border-stroke rounded-default px-3 py-2.5 text-sm font-[inherit] transition-[border-color,box-shadow] duration-200 outline-none focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
              />
            </label>

            <p
              v-if="changePasswordError"
              class="m-0 border border-[#fca5a5] rounded-default bg-[#fef2f2] text-offline px-3 py-2.5 text-sm"
            >
              {{ changePasswordError }}
            </p>
            <p
              v-if="changePasswordSuccess"
              class="m-0 border border-[#86efac] rounded-default bg-[#f0fdf4] text-[#166534] px-3 py-2.5 text-sm"
            >
              {{ changePasswordSuccess }}
            </p>
          </form>
        </div>

        <div
          class="flex justify-end gap-3 px-6 py-5 border-t border-stroke rounded-b-lg max-[600px]:flex-col-reverse max-[600px]:[&>button]:w-full"
        >
          <button
            type="button"
            class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
            :disabled="changingPassword"
            @click="closeModal"
          >
            Cancel
          </button>
          <button
            type="submit"
            form="change-password-form"
            class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
            :disabled="changingPassword"
          >
            {{ changingPassword ? 'Saving...' : 'Save Password' }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
