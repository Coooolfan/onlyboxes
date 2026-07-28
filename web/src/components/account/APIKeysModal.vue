<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppEmptyState from '@/components/ui/AppEmptyState.vue'
import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import AppModal from '@/components/ui/AppModal.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import DefinitionRow from '@/components/ui/DefinitionRow.vue'
import { useCopyFeedback } from '@/composables/useCopyFeedback'
import { useAPIKeysStore } from '@/stores/apiKeys'
import { toErrorMessage } from '@/utils/async'
import { formatDateTime } from '@/utils/datetime'
import type { APIKeyCreateResponse } from '@/types/workers'

type Stage = 'list' | 'create-form' | 'create-success'

const emit = defineEmits<{ close: [] }>()

const apiKeysStore = useAPIKeysStore()
const copyFeedback = useCopyFeedback({ fallbackErrorMessage: 'Failed to copy API key.' })

const stage = ref<Stage>('list')
const name = ref('')
const formError = ref('')
const createdAPIKey = ref<APIKeyCreateResponse | null>(null)

const dialogTitle = computed(() => {
  switch (stage.value) {
    case 'create-form':
      return 'New API Key'
    case 'create-success':
      return 'API Key Created'
    default:
      return 'API Keys'
  }
})

const canSubmit = computed(() => name.value.trim() !== '' && !apiKeysStore.creatingAPIKey)

function clearSensitiveKey(): void {
  createdAPIKey.value = null
  copyFeedback.reset()
}

function openCreateForm(): void {
  stage.value = 'create-form'
  name.value = ''
  formError.value = ''
  clearSensitiveKey()
}

function returnToList(): void {
  stage.value = 'list'
  name.value = ''
  formError.value = ''
  clearSensitiveKey()
}

async function submit(): Promise<void> {
  if (!canSubmit.value) {
    formError.value = 'name is required'
    return
  }

  formError.value = ''
  try {
    createdAPIKey.value = await apiKeysStore.createAPIKey(name.value)
    stage.value = 'create-success'
  } catch (error) {
    formError.value = toErrorMessage(error, 'Failed to create API key.')
  }
}

onMounted(() => {
  void apiKeysStore.loadAPIKeys()
})

onBeforeUnmount(() => {
  clearSensitiveKey()
  apiKeysStore.teardown()
})
</script>

<template>
  <AppModal
    :title="dialogTitle"
    description="Manage dashboard Bearer keys."
    size="xl"
    panel-class="api-keys-modal"
    @close="emit('close')"
  >
    <AppAlert
      v-if="apiKeysStore.errorMessage && stage !== 'create-form'"
      tone="error"
      with-icon
    >
      {{ apiKeysStore.errorMessage }}
    </AppAlert>

    <template v-if="stage === 'list'">
      <div class="flex items-center justify-between gap-4 max-[700px]:flex-col max-[700px]:items-start">
        <p class="m-0 text-sm text-secondary">Total: {{ apiKeysStore.apiKeys.length }}</p>
        <AppButton
          variant="primary"
          size="sm"
          icon="plus"
          :disabled="apiKeysStore.loading"
          @click="openCreateForm"
        >
          New API Key
        </AppButton>
      </div>

      <p
        v-if="apiKeysStore.loading"
        class="m-0 rounded-default border border-dashed border-stroke px-4 py-3 text-sm text-secondary"
      >
        Loading API keys...
      </p>

      <AppEmptyState
        v-else-if="apiKeysStore.apiKeys.length === 0"
        icon="key"
        dashed
        title="No API keys created yet"
        description="Create a key to authenticate dashboard automation with a Bearer token."
      />

      <ul v-else class="m-0 grid list-none gap-3 p-0">
        <li
          v-for="item in apiKeysStore.apiKeys"
          :key="item.id"
          class="rounded-lg border border-stroke bg-surface-soft/40 px-4 py-4 transition-[border-color] duration-200 hover:border-stroke-hover"
        >
          <div class="flex items-start justify-between gap-4 max-[700px]:flex-col">
            <div class="grid min-w-0 gap-2">
              <p class="m-0 text-[15px] font-semibold text-primary">{{ item.name }}</p>
              <DefinitionRow label="Key" :value="item.key_masked" label-width="64px" />
              <DefinitionRow
                label="Created"
                :value="formatDateTime(item.created_at)"
                label-width="64px"
                :code="false"
              />
            </div>

            <div class="api-key-actions shrink-0">
              <AppButton
                variant="danger"
                size="sm"
                icon="trash"
                :loading="apiKeysStore.deletingAPIKeyID === item.id"
                @click="apiKeysStore.deleteAPIKey(item.id)"
              >
                {{ apiKeysStore.deletingAPIKeyID === item.id ? 'Deleting...' : 'Delete' }}
              </AppButton>
            </div>
          </div>
        </li>
      </ul>
    </template>

    <template v-else-if="stage === 'create-form'">
      <p class="m-0 text-sm leading-normal text-secondary">
        Create a long-lived Bearer token for dashboard automation. The plaintext key is shown once.
      </p>

      <form id="create-api-key-form" class="grid gap-4" @submit.prevent="submit">
        <AppField label="Name">
          <AppInput v-model="name" :maxlength="64" autocomplete="off" required data-autofocus />
        </AppField>

        <AppAlert v-if="formError" tone="error" with-icon>{{ formError }}</AppAlert>
      </form>
    </template>

    <template v-else>
      <p class="m-0 text-sm leading-normal text-secondary">
        Copy this API key now. It will not be shown again after you close this dialog.
      </p>

      <div class="grid gap-3 rounded-default border border-stroke bg-surface-soft/40 px-4 py-4">
        <DefinitionRow
          label="Name"
          :value="createdAPIKey?.name"
          label-width="64px"
          :code="false"
        />
        <code
          class="ui-code-block api-key-value block rounded-default border p-3 font-mono text-xs leading-relaxed break-all whitespace-pre-wrap"
          >{{ createdAPIKey?.key }}</code
        >
        <div class="flex justify-end">
          <CopyButton
            variant="primary"
            label="Copy Key"
            :status="copyFeedback.statusFor('api-key')"
            @copy="copyFeedback.copy(createdAPIKey?.key ?? '', 'api-key')"
          />
        </div>
      </div>
    </template>

    <template #footer>
      <template v-if="stage === 'create-form'">
        <AppButton size="sm" :disabled="apiKeysStore.creatingAPIKey" @click="returnToList">
          Cancel
        </AppButton>
        <AppButton
          variant="primary"
          size="sm"
          type="submit"
          form="create-api-key-form"
          :loading="apiKeysStore.creatingAPIKey"
        >
          {{ apiKeysStore.creatingAPIKey ? 'Creating...' : 'Create API Key' }}
        </AppButton>
      </template>
      <template v-else-if="stage === 'create-success'">
        <AppButton size="sm" @click="returnToList">Back to Keys</AppButton>
        <AppButton variant="primary" size="sm" @click="emit('close')">Close</AppButton>
      </template>
      <template v-else>
        <AppButton size="sm" @click="emit('close')">Close</AppButton>
      </template>
    </template>
  </AppModal>
</template>
