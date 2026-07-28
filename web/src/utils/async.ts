export function isAbortError(error: unknown): boolean {
  if (typeof DOMException !== 'undefined' && error instanceof DOMException) {
    return error.name === 'AbortError'
  }
  return error instanceof Error && error.name === 'AbortError'
}

export function toErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof Error && error.message.trim() !== '') {
    return error.message
  }
  return fallback
}

export interface RequestToken {
  signal: AbortSignal
  /** True when a newer request superseded this one, or when it was aborted. */
  isStale: () => boolean
  /** Releases the token; returns false when it was already superseded. */
  release: () => boolean
}

/**
 * Serializes overlapping loads for a single remote collection: every new request
 * aborts the previous one and stale responses are discarded instead of applied.
 */
export function createRequestGuard() {
  let serial = 0
  let active: AbortController | null = null

  function begin(): RequestToken {
    const current = ++serial
    active?.abort()
    const controller = new AbortController()
    active = controller

    return {
      signal: controller.signal,
      isStale: () => current !== serial || controller.signal.aborted,
      release: () => {
        if (active === controller) {
          active = null
        }
        return current === serial
      },
    }
  }

  function abort(): void {
    serial += 1
    active?.abort()
    active = null
  }

  return { begin, abort }
}
