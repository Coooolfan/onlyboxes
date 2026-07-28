<script setup lang="ts">
import { computed, ref } from 'vue'

import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import AppModal from '@/components/ui/AppModal.vue'
import { changePasswordAPI } from '@/services/auth.api'
import { isUnauthorizedError } from '@/services/http'
import { redirectToLogin } from '@/stores/auth-redirect'
import { toErrorMessage } from '@/utils/async'

const emit = defineEmits<{ close: [] }>()

const currentPassword = ref('')
const newPassword = ref('')
const submitting = ref(false)
const errorMessage = ref('')
const successMessage = ref('')

const canSubmit = computed(
  () => currentPassword.value.trim() !== '' && newPassword.value.trim() !== '' && !submitting.value,
)

async function submit(): Promise<void> {
  if (submitting.value) {
    return
  }
  if (!canSubmit.value) {
    errorMessage.value = 'current password and new password are required'
    successMessage.value = ''
    return
  }

  submitting.value = true
  errorMessage.value = ''
  successMessage.value = ''

  try {
    await changePasswordAPI(currentPassword.value, newPassword.value)
    currentPassword.value = ''
    newPassword.value = ''
    successMessage.value = 'Password updated successfully.'
  } catch (error) {
    if (isUnauthorizedError(error)) {
      await redirectToLogin(() => emit('close'))
      return
    }
    errorMessage.value = toErrorMessage(error, 'Failed to change password.')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppModal
    title="Change Password"
    description="Existing sessions for this account are rotated after the change."
    size="sm"
    panel-class="password-modal"
    @close="emit('close')"
  >
    <form id="change-password-form" class="password-form grid gap-4" @submit.prevent="submit">
      <AppField label="Current Password">
        <AppInput
          id="current-password"
          v-model="currentPassword"
          type="password"
          autocomplete="current-password"
          required
          data-autofocus
        />
      </AppField>

      <AppField label="New Password">
        <AppInput
          id="new-password"
          v-model="newPassword"
          type="password"
          autocomplete="new-password"
          required
        />
      </AppField>

      <AppAlert v-if="errorMessage" tone="error" with-icon>{{ errorMessage }}</AppAlert>
      <AppAlert v-if="successMessage" tone="success" with-icon>{{ successMessage }}</AppAlert>
    </form>

    <template #footer>
      <AppButton size="sm" :disabled="submitting" @click="emit('close')">Cancel</AppButton>
      <AppButton
        variant="primary"
        size="sm"
        type="submit"
        form="change-password-form"
        :loading="submitting"
      >
        {{ submitting ? 'Saving...' : 'Save Password' }}
      </AppButton>
    </template>
  </AppModal>
</template>
