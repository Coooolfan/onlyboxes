<script setup lang="ts">
import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import WorkerAdvancedSection from '@/components/worker-tool/WorkerAdvancedSection.vue'
import WorkerCoreConfigFields from '@/components/worker-tool/WorkerCoreConfigFields.vue'
import WorkerFieldGrid from '@/components/worker-tool/WorkerFieldGrid.vue'
import { boxliteAdvancedFields } from '@/components/worker-tool/workerFieldSpecs'
import type { WorkerBoxliteStartupConfig } from '@/types/worker-startup-tool'

const props = defineProps<{
  config: WorkerBoxliteStartupConfig
  autoCallTimeoutSec: number
  showPrefilledCredentialHint: boolean
}>()
</script>

<template>
  <div class="grid gap-4">
    <h2 class="m-0 text-base font-semibold">Core Configuration</h2>

    <WorkerCoreConfigFields
      :config="props.config"
      :auto-call-timeout-sec="props.autoCallTimeoutSec"
      :show-prefilled-credential-hint="props.showPrefilledCredentialHint"
      binary-path-placeholder="./onlyboxes-worker-boxlite"
    >
      <AppField
        label="WORKER_BOXLITE_HOME"
        hint="Optional home directory for local Boxlite state."
        span
      >
        <AppInput v-model="props.config.boxliteHome" trim placeholder="/var/lib/onlyboxes/boxlite" />
      </AppField>
    </WorkerCoreConfigFields>

    <WorkerAdvancedSection test-id="boxlite-advanced-section">
      <WorkerFieldGrid :config="props.config" :fields="boxliteAdvancedFields" />
    </WorkerAdvancedSection>
  </div>
</template>
