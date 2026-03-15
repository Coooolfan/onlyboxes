import { shallowRef } from 'vue'

import type { WorkerStartupKind } from '@/types/worker-startup-tool'

export interface WorkerStartupPrefill {
  workerKind: WorkerStartupKind
  workerID: string
  workerSecret: string
  consoleGRPCTarget?: string
}

const pending = shallowRef<WorkerStartupPrefill | null>(null)

export function setPrefill(data: WorkerStartupPrefill): void {
  pending.value = data
}

export function consumePrefill(): WorkerStartupPrefill | null {
  const data = pending.value
  pending.value = null
  return data
}
