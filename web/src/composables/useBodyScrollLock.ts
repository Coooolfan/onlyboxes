let lockCount = 0
let previousOverflow = ''
let previousPaddingRight = ''

/**
 * Prevents background scrolling while overlays are open.
 * Nested locks are reference-counted, so the last release restores the original styles.
 */
export function lockBodyScroll(): () => void {
  if (typeof document === 'undefined') {
    return () => {}
  }

  const body = document.body
  if (lockCount === 0) {
    previousOverflow = body.style.overflow
    previousPaddingRight = body.style.paddingRight
    const scrollbarWidth = window.innerWidth - document.documentElement.clientWidth
    if (scrollbarWidth > 0) {
      body.style.paddingRight = `${scrollbarWidth}px`
    }
    body.style.overflow = 'hidden'
  }
  lockCount += 1

  let released = false
  return () => {
    if (released) {
      return
    }
    released = true
    lockCount = Math.max(0, lockCount - 1)
    if (lockCount === 0) {
      body.style.overflow = previousOverflow
      body.style.paddingRight = previousPaddingRight
    }
  }
}
