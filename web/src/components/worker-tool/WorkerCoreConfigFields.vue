<script setup lang="ts">
import AppBadge from '@/components/ui/AppBadge.vue'
import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import AppSegmented from '@/components/ui/AppSegmented.vue'
import type { WorkerCallTimeoutMode, WorkerStartupBaseConfig } from '@/types/worker-startup-tool'

const props = withDefaults(
  defineProps<{
    config: WorkerStartupBaseConfig
    autoCallTimeoutSec: number
    showPrefilledCredentialHint: boolean
    binaryPathPlaceholder: string
    /** Temporary presets hide the optional operational fields. */
    showOptionalFields?: boolean
  }>(),
  { showOptionalFields: true },
)

const callTimeoutOptions = [
  { value: 'auto', label: 'Auto' },
  { value: 'manual', label: 'Manual' },
] as const satisfies ReadonlyArray<{ value: WorkerCallTimeoutMode; label: string }>

function handleCallTimeoutModeUpdate(value: string): void {
  if (value === 'auto' || value === 'manual') {
    props.config.callTimeoutMode = value
  }
}
</script>

<template>
  <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
    <AppField hint="Worker identity issued by console.">
      <template #label>
        <span>WORKER_ID</span>
        <AppBadge
          v-if="showPrefilledCredentialHint"
          tone="warning"
          pill
          data-testid="worker-id-prefilled-hint"
        >
          Already Filled
        </AppBadge>
      </template>
      <AppInput v-model="config.workerID" trim placeholder="worker-id" />
    </AppField>

    <AppField hint="One-time credential returned during worker creation.">
      <template #label>
        <span>WORKER_SECRET</span>
        <AppBadge
          v-if="showPrefilledCredentialHint"
          tone="warning"
          pill
          data-testid="worker-secret-prefilled-hint"
        >
          Already Filled
        </AppBadge>
      </template>
      <AppInput
        v-model="config.workerSecret"
        type="password"
        revealable
        trim
        placeholder="worker-secret"
      />
    </AppField>

    <AppField
      label="WORKER_CONSOLE_GRPC_TARGET"
      hint="Console gRPC endpoint in host:port format."
    >
      <AppInput v-model="config.consoleGRPCTarget" trim placeholder="127.0.0.1:50051" />
    </AppField>

    <AppField
      label="WORKER_CONSOLE_INSECURE"
      hint="Allow plaintext gRPC transport. Use only in trusted private networks."
    >
      <span class="ui-input flex items-center justify-between rounded-md border px-3 py-2">
        <span class="text-sm text-primary">Set to true</span>
        <input v-model="config.consoleInsecure" type="checkbox" class="ui-checkbox h-4 w-4 rounded" />
      </span>
    </AppField>

    <AppField
      v-if="showOptionalFields"
      label="WORKER_NODE_NAME"
      hint="Optional display name reported to console."
    >
      <AppInput v-model="config.nodeName" trim />
    </AppField>

    <div v-if="showOptionalFields" class="grid gap-1.5 md:col-span-2">
      <span class="text-sm text-secondary">WORKER_CALL_TIMEOUT_SEC</span>
      <span class="text-xs text-secondary">
        Auto mode follows worker default formula; manual mode overrides it.
      </span>
      <div class="grid gap-2">
        <AppSegmented
          :model-value="config.callTimeoutMode"
          :options="callTimeoutOptions"
          size="sm"
          aria-label="Call timeout mode"
          @update:model-value="handleCallTimeoutModeUpdate"
        />

        <p
          v-if="config.callTimeoutMode === 'auto'"
          class="ui-inset-surface m-0 rounded-md border px-3 py-2 text-sm text-primary"
        >
          Derived timeout: <strong>{{ autoCallTimeoutSec }}s</strong> (ceil(2.5 x heartbeat))
        </p>
        <AppField v-else hint="Manual timeout in seconds.">
          <AppInput v-model="config.callTimeoutSec" type="number" :min="1" width="220px" />
        </AppField>
      </div>
    </div>

    <AppField
      v-if="showOptionalFields"
      label="Worker Binary Path"
      hint="Executable path used in the final command line."
      span
    >
      <AppInput v-model="config.binaryPath" trim :placeholder="binaryPathPlaceholder" />
    </AppField>

    <slot />
  </div>
</template>
