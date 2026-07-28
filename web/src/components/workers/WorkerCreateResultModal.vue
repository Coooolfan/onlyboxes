<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'

import AppButton from '@/components/ui/AppButton.vue'
import AppIcon from '@/components/ui/AppIcon.vue'
import AppModal from '@/components/ui/AppModal.vue'
import DefinitionRow from '@/components/ui/DefinitionRow.vue'
import { setPrefill } from '@/composables/useWorkerStartupPrefill'
import { maskSecret } from '@/utils/secret'
import type { WorkerStartupKind } from '@/types/worker-startup-tool'
import type { WorkerStartupCommandResponse, WorkerType } from '@/types/workers'

const workerTypeToStartupKind: Record<WorkerType, WorkerStartupKind> = {
  normal: 'worker-docker',
  'worker-sys': 'worker-sys',
}

const props = defineProps<{
  payload: WorkerStartupCommandResponse | null
}>()

const emit = defineEmits<{ close: [] }>()

const router = useRouter()
const secretVisible = ref(false)

const workerSecret = computed(() => props.payload?.worker_secret ?? '')
const workerSecretDisplay = computed(() => {
  if (!workerSecret.value) {
    return 'Unavailable'
  }
  return secretVisible.value ? workerSecret.value : maskSecret(workerSecret.value)
})

async function goToStartupTool(): Promise<void> {
  if (!props.payload) {
    return
  }
  setPrefill({
    workerKind: workerTypeToStartupKind[props.payload.type] ?? 'worker-docker',
    workerID: props.payload.node_id,
    workerSecret: workerSecret.value,
  })
  const failure = await router.push('/tools/worker-startup')
  if (!failure) {
    closeModal()
  }
}

function closeModal(): void {
  secretVisible.value = false
  emit('close')
}

watch(
  () => props.payload,
  () => {
    secretVisible.value = false
  },
)
</script>

<template>
  <AppModal
    :open="payload !== null"
    title="Worker Created"
    size="lg"
    panel-class="worker-modal"
    @close="closeModal"
  >
    <p class="m-0 text-sm leading-normal text-secondary">
      Startup details are returned only once at creation time. Copy and store them securely now.
    </p>

    <div v-if="payload" class="grid gap-3">
      <DefinitionRow label="Node ID" :value="payload.node_id" />
      <DefinitionRow label="Worker Type" :value="payload.type" />
      <DefinitionRow label="Worker Secret" :value="workerSecretDisplay">
        <template #action>
          <AppButton
            v-if="workerSecret"
            size="sm"
            :icon="secretVisible ? 'eye-off' : 'eye'"
            @click="secretVisible = !secretVisible"
          >
            {{ secretVisible ? 'Hide' : 'Show' }}
          </AppButton>
        </template>
      </DefinitionRow>
    </div>

    <div class="ui-inset-surface rounded-xl border p-5">
      <div class="flex items-start gap-4">
        <div class="ui-strong-surface mt-0.5 shrink-0 rounded-lg border p-2.5 text-secondary">
          <AppIcon name="terminal" :size="20" />
        </div>
        <div class="flex-1">
          <h3 class="m-0 mb-1 text-sm font-semibold text-primary">Quick Startup</h3>
          <p class="m-0 mb-3.5 text-[13px] leading-normal text-secondary">
            Use the startup tool to quickly configure and generate a startup script for this worker.
          </p>
          <AppButton
            icon="external-link"
            data-testid="open-in-startup-tool"
            @click="goToStartupTool"
          >
            Open in Startup Tool with Id and Secret
          </AppButton>
        </div>
      </div>
    </div>

    <template #footer>
      <AppButton variant="primary" size="sm" @click="closeModal">Done</AppButton>
    </template>
  </AppModal>
</template>
