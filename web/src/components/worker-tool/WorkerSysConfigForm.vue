<script setup lang="ts">
import { ref } from 'vue'

import SegmentedToggle from '@/components/worker-tool/SegmentedToggle.vue'
import type {
  WorkerCallTimeoutMode,
  WorkerSysStartupConfig,
  WorkerSysWhitelistMode,
} from '@/types/worker-startup-tool'

const props = defineProps<{
  config: WorkerSysStartupConfig
  autoCallTimeoutSec: number
  showPrefilledCredentialHint: boolean
  whitelistModeDescription: string
}>()

const showSecret = ref(false)

const callTimeoutOptions: Array<{ value: WorkerCallTimeoutMode; label: string }> = [
  { value: 'auto', label: 'Auto' },
  { value: 'manual', label: 'Manual' },
]

const whitelistModeOptions: Array<{ value: WorkerSysWhitelistMode; label: string }> = [
  { value: 'exact', label: 'exact' },
  { value: 'prefix', label: 'prefix' },
  { value: 'allow_all', label: 'allow_all' },
]

function handleCallTimeoutModeUpdate(value: string): void {
  if (value === 'auto' || value === 'manual') {
    props.config.callTimeoutMode = value
  }
}

function handleWhitelistModeUpdate(value: string): void {
  if (value === 'exact' || value === 'prefix' || value === 'allow_all') {
    props.config.computerUseCommandWhitelistMode = value
  }
}
</script>

<template>
  <div class="grid gap-4">
    <h2 class="text-base font-semibold m-0">Core Configuration</h2>
    <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
      <label class="grid gap-1.5">
        <span class="flex items-center gap-2 text-sm text-secondary">
          <span>WORKER_ID</span>
          <span
            v-if="props.showPrefilledCredentialHint"
            data-testid="worker-id-prefilled-hint"
            class="ui-badge-warning inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium"
          >
            Already Filled
          </span>
        </span>
        <span class="text-xs text-secondary">Worker identity issued by console.</span>
        <input
          v-model.trim="props.config.workerID"
          type="text"
          class="ui-input rounded-md border px-3 py-2 text-sm"
          placeholder="worker-id"
        />
      </label>

      <label class="grid gap-1.5">
        <span class="flex items-center gap-2 text-sm text-secondary">
          <span>WORKER_SECRET</span>
          <span
            v-if="props.showPrefilledCredentialHint"
            data-testid="worker-secret-prefilled-hint"
            class="ui-badge-warning inline-flex items-center rounded-full border px-2 py-0.5 text-[11px] font-medium"
          >
            Already Filled
          </span>
        </span>
        <span class="text-xs text-secondary"
          >One-time credential returned during worker creation.</span
        >
        <div class="flex items-center gap-2">
          <input
            v-model.trim="props.config.workerSecret"
            :type="showSecret ? 'text' : 'password'"
            class="ui-input min-w-0 flex-1 rounded-md border px-3 py-2 text-sm"
            placeholder="worker-secret"
          />
          <button
            type="button"
            class="ui-btn-secondary h-[36px] rounded-md border px-3 text-xs transition-colors"
            @click="showSecret = !showSecret"
          >
            {{ showSecret ? 'Hide' : 'Show' }}
          </button>
        </div>
      </label>

      <label class="grid gap-1.5">
        <span class="text-sm text-secondary">WORKER_CONSOLE_GRPC_TARGET</span>
        <span class="text-xs text-secondary">Console gRPC endpoint in host:port format.</span>
        <input
          v-model.trim="props.config.consoleGRPCTarget"
          type="text"
          class="ui-input rounded-md border px-3 py-2 text-sm"
          placeholder="127.0.0.1:50051"
        />
      </label>

      <label class="grid gap-1.5">
        <span class="text-sm text-secondary">WORKER_CONSOLE_INSECURE</span>
        <span class="text-xs text-secondary">
          Allow plaintext gRPC transport. Use only in trusted private networks.
        </span>
        <div class="ui-input flex items-center justify-between rounded-md border px-3 py-2">
          <span class="text-sm text-primary">Set to true</span>
          <input
            v-model="props.config.consoleInsecure"
            type="checkbox"
            class="ui-checkbox h-4 w-4 rounded"
          />
        </div>
      </label>

      <label class="grid gap-1.5">
        <span class="text-sm text-secondary">WORKER_NODE_NAME</span>
        <span class="text-xs text-secondary">Optional display name reported to console.</span>
        <input
          v-model.trim="props.config.nodeName"
          type="text"
          class="ui-input rounded-md border px-3 py-2 text-sm"
        />
      </label>

      <div class="grid gap-1.5 md:col-span-2">
        <span class="text-sm text-secondary">WORKER_CALL_TIMEOUT_SEC</span>
        <span class="text-xs text-secondary">
          Auto mode follows worker default formula; manual mode overrides it.
        </span>
        <div class="grid gap-2">
          <SegmentedToggle
            :model-value="props.config.callTimeoutMode"
            :options="callTimeoutOptions"
            @update:model-value="handleCallTimeoutModeUpdate"
          />

          <p
            v-if="props.config.callTimeoutMode === 'auto'"
            class="ui-inset-surface m-0 text-sm text-primary rounded-md border px-3 py-2"
          >
            Derived timeout: <strong>{{ props.autoCallTimeoutSec }}s</strong> (ceil(2.5 x
            heartbeat))
          </p>
          <label v-else class="grid gap-1.5">
            <span class="text-xs text-secondary">Manual timeout in seconds.</span>
            <input
              v-model.number="props.config.callTimeoutSec"
              type="number"
              min="1"
              class="ui-input w-[220px] rounded-md border px-3 py-2 text-sm"
            />
          </label>
        </div>
      </div>

      <label class="grid gap-1.5 md:col-span-2">
        <span class="text-sm text-secondary">Worker Binary Path</span>
        <span class="text-xs text-secondary">Executable path used in the final command line.</span>
        <input
          v-model.trim="props.config.binaryPath"
          type="text"
          class="ui-input rounded-md border px-3 py-2 text-sm"
          placeholder="./onlyboxes-worker-sys"
        />
      </label>
    </div>

    <details data-testid="sys-advanced-section" class="rounded-md border border-stroke bg-surface">
      <summary class="cursor-pointer px-3 py-2 text-sm font-medium text-primary">
        Advanced Configuration
      </summary>
      <div class="grid grid-cols-1 gap-4 border-t border-stroke p-3 md:grid-cols-2">
        <label class="grid gap-1.5">
          <span class="text-sm text-secondary">WORKER_HEARTBEAT_INTERVAL_SEC</span>
          <span class="text-xs text-secondary"
            >Heartbeat interval in seconds; must be positive.</span
          >
          <input
            v-model.number="props.config.heartbeatIntervalSec"
            type="number"
            min="1"
            class="ui-input rounded-md border px-3 py-2 text-sm"
          />
        </label>

        <label class="grid gap-1.5">
          <span class="text-sm text-secondary">WORKER_HEARTBEAT_JITTER_PCT</span>
          <span class="text-xs text-secondary"
            >Random jitter percent applied to heartbeat scheduling.</span
          >
          <input
            v-model.number="props.config.heartbeatJitterPct"
            type="number"
            min="0"
            max="100"
            class="ui-input rounded-md border px-3 py-2 text-sm"
          />
        </label>

        <label class="grid gap-1.5">
          <span class="text-sm text-secondary">WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES</span>
          <span class="text-xs text-secondary"
            >Per-stream stdout/stderr truncation limit in bytes.</span
          >
          <input
            v-model.number="props.config.computerUseOutputLimitBytes"
            type="number"
            min="1"
            class="ui-input rounded-md border px-3 py-2 text-sm"
          />
        </label>

        <div class="grid gap-1.5">
          <span class="text-xs text-secondary">WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE</span>
          <p class="m-0 text-xs text-secondary">
            {{ props.whitelistModeDescription }}
          </p>
          <SegmentedToggle
            :model-value="props.config.computerUseCommandWhitelistMode"
            :options="whitelistModeOptions"
            @update:model-value="handleWhitelistModeUpdate"
          />
        </div>

        <label class="grid gap-1.5 md:col-span-2">
          <span class="text-sm text-secondary">
            WORKER_COMPUTER_USE_COMMAND_WHITELIST (one entry per line)
          </span>
          <span class="text-xs text-secondary">
            Serialized as JSON array. In exact/prefix mode, empty list blocks all commands.
          </span>
          <textarea
            v-model="props.config.computerUseCommandWhitelistText"
            data-testid="sys-whitelist-textarea"
            rows="3"
            :disabled="props.config.computerUseCommandWhitelistMode === 'allow_all'"
            class="ui-input rounded-md border px-3 py-2 text-sm disabled:cursor-not-allowed disabled:opacity-60"
            placeholder="echo&#10;time"
          ></textarea>
          <span
            v-if="props.config.computerUseCommandWhitelistMode === 'allow_all'"
            class="text-xs text-secondary"
          >
            Disabled because allow_all mode ignores whitelist entries.
          </span>
        </label>

        <label class="grid gap-1.5 md:col-span-2">
          <span class="text-sm text-secondary">
            WORKER_READ_IMAGE_ALLOWED_PATHS (one path per line)
          </span>
          <span class="text-xs text-secondary">
            Serialized as JSON array. Empty value means readImage is denied by default.
          </span>
          <textarea
            v-model="props.config.readImageAllowedPathsText"
            data-testid="sys-paths-textarea"
            rows="3"
            class="ui-input rounded-md border px-3 py-2 text-sm"
            placeholder="/data/images&#10;/tmp/a.png"
          ></textarea>
        </label>
      </div>
    </details>
  </div>
</template>
