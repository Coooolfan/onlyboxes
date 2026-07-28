<script setup lang="ts">
import WorkerAdvancedSection from '@/components/worker-tool/WorkerAdvancedSection.vue'
import WorkerCoreConfigFields from '@/components/worker-tool/WorkerCoreConfigFields.vue'
import WorkerFieldGrid from '@/components/worker-tool/WorkerFieldGrid.vue'
import { dockerAdvancedFields } from '@/components/worker-tool/workerFieldSpecs'
import type { WorkerDockerStartupConfig } from '@/types/worker-startup-tool'

const props = defineProps<{
  config: WorkerDockerStartupConfig
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
      binary-path-placeholder="./onlyboxes-worker-docker"
    />

    <WorkerAdvancedSection test-id="docker-advanced-section">
      <WorkerFieldGrid :config="props.config" :fields="dockerAdvancedFields" />
    </WorkerAdvancedSection>
  </div>
</template>
