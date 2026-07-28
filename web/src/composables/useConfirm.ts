import { ref, shallowRef } from 'vue'

export interface ConfirmRequest {
  title: string
  message: string
  confirmLabel?: string
  cancelLabel?: string
  /** Renders the confirm action in the destructive style. */
  destructive?: boolean
  /** Optional monospace detail line, e.g. the resource identifier. */
  detail?: string
}

interface PendingConfirm extends ConfirmRequest {
  resolve: (confirmed: boolean) => void
}

const pending = shallowRef<PendingConfirm | null>(null)
const busy = ref(false)

/**
 * Application-level confirmation dialog. Replaces `window.confirm` so destructive
 * actions share the console styling and stay testable.
 *
 * `ConfirmDialogHost` must be mounted once near the application root.
 */
export function requestConfirm(request: ConfirmRequest): Promise<boolean> {
  pending.value?.resolve(false)

  return new Promise<boolean>((resolve) => {
    pending.value = { ...request, resolve }
  })
}

export function useConfirmHost() {
  function settle(confirmed: boolean): void {
    const current = pending.value
    if (!current) {
      return
    }
    pending.value = null
    busy.value = false
    current.resolve(confirmed)
  }

  return {
    pending,
    busy,
    accept: () => settle(true),
    dismiss: () => settle(false),
  }
}
