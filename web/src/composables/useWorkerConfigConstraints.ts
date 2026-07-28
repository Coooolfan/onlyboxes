import { computed, watch, type ComputedRef } from 'vue'

import type { WorkerCallTimeoutMode } from '@/types/worker-startup-tool'

interface CallTimeoutConfig {
  callTimeoutMode: WorkerCallTimeoutMode
  callTimeoutSec: number
  heartbeatIntervalSec: number
}

interface TerminalLeaseConfig {
  terminalLeaseMinSec: number
  terminalLeaseMaxSec: number
  terminalLeaseDefaultSec: number
}

const defaultHeartbeatIntervalSec = 5

function toPositiveInt(value: number, fallback: number): number {
  const normalized = Number.isFinite(value) ? Math.floor(value) : Number.NaN
  return normalized > 0 ? normalized : fallback
}

/** Mirrors the worker-side default: ceil(2.5 x heartbeat interval). */
export function derivedCallTimeoutSec(heartbeatIntervalSec: number): number {
  const heartbeat = toPositiveInt(heartbeatIntervalSec, defaultHeartbeatIntervalSec)
  return Math.floor((heartbeat * 5 + 1) / 2)
}

/**
 * Keeps `callTimeoutSec` in sync with the heartbeat interval while the field is in
 * auto mode, and exposes the derived value for display.
 */
export function useCallTimeoutSync(config: CallTimeoutConfig): ComputedRef<number> {
  const autoTimeout = computed(() => derivedCallTimeoutSec(config.heartbeatIntervalSec))

  watch(
    () => [config.callTimeoutMode, autoTimeout.value] as const,
    ([mode, timeout]) => {
      if (mode === 'auto') {
        config.callTimeoutSec = timeout
      }
    },
    { immediate: true },
  )

  return autoTimeout
}

/**
 * Enforces `min <= default <= max` for terminal lease durations as the user edits
 * any of the three inputs.
 */
export function useTerminalLeaseClamp(config: TerminalLeaseConfig): void {
  watch(
    () =>
      [
        config.terminalLeaseMinSec,
        config.terminalLeaseMaxSec,
        config.terminalLeaseDefaultSec,
      ] as const,
    ([min, max, current]) => {
      const normalizedMin = Number.isFinite(min) ? Math.floor(min) : Number.NaN
      const normalizedMax = Number.isFinite(max) ? Math.floor(max) : Number.NaN
      if (normalizedMin <= 0 || normalizedMax <= 0) {
        return
      }

      if (normalizedMax < normalizedMin) {
        config.terminalLeaseMaxSec = normalizedMin
        return
      }

      const normalizedDefault = Number.isFinite(current) ? Math.floor(current) : Number.NaN
      if (!Number.isFinite(normalizedDefault)) {
        return
      }
      if (normalizedDefault < normalizedMin) {
        config.terminalLeaseDefaultSec = normalizedMin
        return
      }
      if (normalizedDefault > normalizedMax) {
        config.terminalLeaseDefaultSec = normalizedMax
      }
    },
  )
}
