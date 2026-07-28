<script setup lang="ts">
import { computed, ref } from 'vue'

import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import AppModal from '@/components/ui/AppModal.vue'
import { createAccountAPI } from '@/services/auth.api'
import { toErrorMessage } from '@/utils/async'

const emit = defineEmits<{
  close: []
  created: []
}>()

const username = ref('')
const password = ref('')
const submitting = ref(false)
const errorMessage = ref('')
const successMessage = ref('')

const canSubmit = computed(
  () => username.value.trim() !== '' && password.value !== '' && !submitting.value,
)

async function submit(): Promise<void> {
  if (!canSubmit.value) {
    errorMessage.value = 'username and password are required'
    successMessage.value = ''
    return
  }

  errorMessage.value = ''
  successMessage.value = ''
  submitting.value = true
  try {
    const payload = await createAccountAPI(username.value.trim(), password.value)
    username.value = ''
    password.value = ''
    successMessage.value = `Created account ${payload.account.username}`
    emit('created')
  } catch (error) {
    errorMessage.value = toErrorMessage(error, 'Failed to create account.')
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppModal
    title="Create Account"
    description="Registration is enabled. New accounts are always non-admin."
    size="sm"
    panel-class="account-modal"
    @close="emit('close')"
  >
    <form id="create-account-form" class="account-form grid gap-4" @submit.prevent="submit">
      <AppField label="Username">
        <AppInput
          v-model="username"
          autocomplete="off"
          :spellcheck="false"
          required
          data-autofocus
        />
      </AppField>

      <AppField label="Password">
        <AppInput v-model="password" type="password" autocomplete="new-password" required />
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
        form="create-account-form"
        :loading="submitting"
      >
        {{ submitting ? 'Creating...' : 'Create Account' }}
      </AppButton>
    </template>
  </AppModal>
</template>
