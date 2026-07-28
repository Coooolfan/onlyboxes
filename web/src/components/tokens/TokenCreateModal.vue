<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'

import TokenQuickSetup from '@/components/tokens/TokenQuickSetup.vue'
import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import AppModal from '@/components/ui/AppModal.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import DefinitionRow from '@/components/ui/DefinitionRow.vue'
import { useCopyFeedback } from '@/composables/useCopyFeedback'
import { useMCPSnippets } from '@/composables/useMCPSnippets'
import { useTokensStore } from '@/stores/tokens'
import { toErrorMessage } from '@/utils/async'
import type { TrustedTokenCreateResponse } from '@/types/workers'

const emit = defineEmits<{ close: [] }>()

const tokensStore = useTokensStore()

const name = ref('')
const errorMessage = ref('')
const createdToken = ref<TrustedTokenCreateResponse | null>(null)

const tokenValue = computed(() => createdToken.value?.token ?? '')
const snippets = useMCPSnippets(tokenValue)
const copyFeedback = useCopyFeedback({ fallbackErrorMessage: 'Failed to copy token.' })

const canSubmit = computed(() => name.value.trim() !== '' && !tokensStore.creatingTrustedToken)

async function submit(): Promise<void> {
  if (!canSubmit.value) {
    return
  }

  errorMessage.value = ''
  try {
    createdToken.value = await tokensStore.createTrustedToken(name.value)
  } catch (error) {
    errorMessage.value = toErrorMessage(error, 'Failed to create trusted token.')
  }
}

function close(): void {
  createdToken.value = null
  copyFeedback.reset()
  emit('close')
}

onBeforeUnmount(() => {
  createdToken.value = null
})
</script>

<template>
  <AppModal
    :title="createdToken ? 'Token Created' : 'New Trusted Token'"
    panel-class="token-modal"
    @close="close"
  >
    <template v-if="!createdToken">
      <p class="m-0 text-sm leading-normal text-secondary">
        The plaintext token is shown only once after creation and cannot be viewed again after
        closing this dialog.
      </p>

      <form id="create-token-form" class="token-modal-form grid gap-4" @submit.prevent="submit">
        <AppField label="Name">
          <AppInput v-model="name" :maxlength="64" placeholder="ci-prod" required data-autofocus />
        </AppField>

        <AppAlert v-if="errorMessage" tone="error" with-icon>{{ errorMessage }}</AppAlert>
      </form>
    </template>

    <template v-else>
      <p class="m-0 text-sm leading-normal text-secondary">
        This is the only time the plaintext token is shown. Copy and store it securely now.
      </p>

      <code
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap"
        >{{ createdToken.token }}</code
      >

      <div class="grid gap-3">
        <DefinitionRow label="Name" :value="createdToken.name" label-width="64px" :code="false" />
        <DefinitionRow label="ID" :value="createdToken.id" label-width="64px" />
        <DefinitionRow label="Masked" :value="createdToken.token_masked" label-width="64px" />
      </div>

      <TokenQuickSetup :snippets="snippets" />
    </template>

    <template #footer>
      <template v-if="!createdToken">
        <AppButton size="sm" :disabled="tokensStore.creatingTrustedToken" @click="close">
          Cancel
        </AppButton>
        <AppButton
          variant="primary"
          size="sm"
          type="submit"
          form="create-token-form"
          :disabled="!canSubmit"
          :loading="tokensStore.creatingTrustedToken"
        >
          {{ tokensStore.creatingTrustedToken ? 'Creating...' : 'Create Token' }}
        </AppButton>
      </template>
      <template v-else>
        <CopyButton
          label="Copy Token"
          :status="copyFeedback.statusFor('token')"
          @copy="copyFeedback.copy(createdToken!.token, 'token')"
        />
        <AppButton variant="primary" size="sm" @click="close">Done</AppButton>
      </template>
    </template>
  </AppModal>
</template>
