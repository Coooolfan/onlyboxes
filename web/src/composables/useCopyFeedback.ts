import { computed, onBeforeUnmount, ref, type ComputedRef, type Ref } from 'vue'

import { writeTextToClipboard } from '@/utils/clipboard'

export type CopyStatus = 'idle' | 'copying' | 'copied' | 'failed'

export interface UseCopyFeedbackOptions {
  /** How long the copied/failed state stays visible. */
  resetDelayMs?: number
  fallbackErrorMessage?: string
}

export interface CopyFeedback {
  status: Ref<CopyStatus>
  /** Key of the most recently copied payload, useful for lists of snippets. */
  activeKey: Ref<string>
  busy: ComputedRef<boolean>
  copy: (value: string, key?: string) => Promise<boolean>
  statusFor: (key: string) => CopyStatus
  reset: () => void
}

/**
 * Shared clipboard interaction state: transient `copying -> copied/failed -> idle`
 * transitions with automatic timer cleanup.
 */
export function useCopyFeedback(options: UseCopyFeedbackOptions = {}): CopyFeedback {
  const resetDelayMs = options.resetDelayMs ?? 1500

  const status = ref<CopyStatus>('idle')
  const activeKey = ref('')

  let timer: ReturnType<typeof setTimeout> | null = null

  function clearTimer(): void {
    if (timer) {
      clearTimeout(timer)
      timer = null
    }
  }

  function reset(): void {
    clearTimer()
    status.value = 'idle'
    activeKey.value = ''
  }

  function scheduleReset(): void {
    clearTimer()
    timer = setTimeout(() => {
      status.value = 'idle'
      activeKey.value = ''
      timer = null
    }, resetDelayMs)
  }

  async function copy(value: string, key = 'default'): Promise<boolean> {
    const text = value.trim()
    if (!text || status.value === 'copying') {
      return false
    }

    clearTimer()
    activeKey.value = key
    status.value = 'copying'
    try {
      await writeTextToClipboard(text, {
        fallbackErrorMessage: options.fallbackErrorMessage,
      })
      status.value = 'copied'
      return true
    } catch {
      status.value = 'failed'
      return false
    } finally {
      scheduleReset()
    }
  }

  function statusFor(key: string): CopyStatus {
    return activeKey.value === key ? status.value : 'idle'
  }

  onBeforeUnmount(reset)

  return {
    status,
    activeKey,
    busy: computed(() => status.value === 'copying'),
    copy,
    statusFor,
    reset,
  }
}

export function copyStatusLabel(status: CopyStatus, idleLabel = 'Copy'): string {
  switch (status) {
    case 'copying':
      return 'Copying...'
    case 'copied':
      return 'Copied'
    case 'failed':
      return 'Copy Failed'
    default:
      return idleLabel
  }
}
