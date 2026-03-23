export type ThemeMode = 'light' | 'dark'

export const SYSTEM_THEME_MEDIA_QUERY = '(prefers-color-scheme: dark)'

type ThemeTargetWindow = Pick<Window, 'matchMedia'>
type ThemeRoot = Pick<HTMLElement, 'dataset' | 'style'>
type MediaQueryListener = (event: MediaQueryListEvent) => void
type MediaQueryListWithLegacyListeners = MediaQueryList & {
  addListener?: (listener: MediaQueryListener) => void
  removeListener?: (listener: MediaQueryListener) => void
}

export function resolveThemeMode(
  mediaQueryList: Pick<MediaQueryList, 'matches'> | null | undefined,
): ThemeMode {
  return mediaQueryList?.matches ? 'dark' : 'light'
}

export function getSystemThemeMediaQuery(
  targetWindow: ThemeTargetWindow | null | undefined = typeof window !== 'undefined'
    ? window
    : undefined,
): MediaQueryListWithLegacyListeners | null {
  if (!targetWindow?.matchMedia) {
    return null
  }

  return targetWindow.matchMedia(SYSTEM_THEME_MEDIA_QUERY) as MediaQueryListWithLegacyListeners
}

export function applyThemeMode(
  mode: ThemeMode,
  root: ThemeRoot | null | undefined = typeof document !== 'undefined'
    ? document.documentElement
    : undefined,
): void {
  if (!root) {
    return
  }

  root.dataset.theme = mode
  root.style.colorScheme = mode
}

export function syncThemeWithSystem(
  targetWindow: ThemeTargetWindow | null | undefined = typeof window !== 'undefined'
    ? window
    : undefined,
  root: ThemeRoot | null | undefined = typeof document !== 'undefined'
    ? document.documentElement
    : undefined,
): () => void {
  const mediaQueryList = getSystemThemeMediaQuery(targetWindow)
  applyThemeMode(resolveThemeMode(mediaQueryList), root)

  if (!mediaQueryList) {
    return () => {}
  }

  const handleChange: MediaQueryListener = (event) => {
    applyThemeMode(resolveThemeMode(event), root)
  }

  if (typeof mediaQueryList.addEventListener === 'function') {
    mediaQueryList.addEventListener('change', handleChange)
    return () => {
      mediaQueryList.removeEventListener?.('change', handleChange)
    }
  }

  mediaQueryList.addListener?.(handleChange)
  return () => {
    mediaQueryList.removeListener?.(handleChange)
  }
}
