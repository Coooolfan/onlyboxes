import { afterEach, describe, expect, it, vi } from 'vitest'

import { applyThemeMode, resolveThemeMode, syncThemeWithSystem } from '../theme/system-theme'

type ThemeListener = (event: MediaQueryListEvent) => void

function createModernThemeWindow(initialMatches: boolean) {
  let matches = initialMatches
  let listener: ThemeListener | null = null

  const mediaQueryList = {
    get matches() {
      return matches
    },
    addEventListener: vi.fn((_type: string, nextListener: ThemeListener) => {
      listener = nextListener
    }),
    removeEventListener: vi.fn((_type: string, nextListener: ThemeListener) => {
      if (listener === nextListener) {
        listener = null
      }
    }),
  } as unknown as MediaQueryList

  return {
    mediaQueryList,
    targetWindow: {
      matchMedia: vi.fn(() => mediaQueryList),
    },
    emit(nextMatches: boolean) {
      matches = nextMatches
      listener?.({ matches: nextMatches } as MediaQueryListEvent)
    },
  }
}

function createLegacyThemeWindow(initialMatches: boolean) {
  let matches = initialMatches
  let listener: ThemeListener | null = null

  const mediaQueryList = {
    get matches() {
      return matches
    },
    addListener: vi.fn((nextListener: ThemeListener) => {
      listener = nextListener
    }),
    removeListener: vi.fn((nextListener: ThemeListener) => {
      if (listener === nextListener) {
        listener = null
      }
    }),
  } as unknown as MediaQueryList

  return {
    mediaQueryList,
    targetWindow: {
      matchMedia: vi.fn(() => mediaQueryList),
    },
    emit(nextMatches: boolean) {
      matches = nextMatches
      listener?.({ matches: nextMatches } as MediaQueryListEvent)
    },
  }
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('system theme sync', () => {
  it('applies light mode when the system prefers light', () => {
    const root = document.createElement('div')
    const { targetWindow } = createModernThemeWindow(false)

    syncThemeWithSystem(targetWindow, root)

    expect(root.dataset.theme).toBe('light')
    expect(root.style.colorScheme).toBe('light')
  })

  it('applies dark mode initially and updates when the system theme changes', () => {
    const root = document.createElement('div')
    const themeWindow = createModernThemeWindow(true)

    const stopSync = syncThemeWithSystem(themeWindow.targetWindow, root)

    expect(root.dataset.theme).toBe('dark')
    expect(root.style.colorScheme).toBe('dark')
    expect(themeWindow.targetWindow.matchMedia).toHaveBeenCalledWith('(prefers-color-scheme: dark)')

    themeWindow.emit(false)
    expect(root.dataset.theme).toBe('light')
    expect(root.style.colorScheme).toBe('light')

    stopSync()
    expect(themeWindow.mediaQueryList.removeEventListener).toHaveBeenCalledTimes(1)
  })

  it('falls back to legacy media-query listeners when addEventListener is unavailable', () => {
    const root = document.createElement('div')
    const themeWindow = createLegacyThemeWindow(false)

    const stopSync = syncThemeWithSystem(themeWindow.targetWindow, root)

    expect(root.dataset.theme).toBe('light')
    expect(themeWindow.mediaQueryList.addListener).toHaveBeenCalledTimes(1)

    themeWindow.emit(true)
    expect(root.dataset.theme).toBe('dark')
    expect(root.style.colorScheme).toBe('dark')

    stopSync()
    expect(themeWindow.mediaQueryList.removeListener).toHaveBeenCalledTimes(1)
  })

  it('can resolve and apply theme mode directly', () => {
    const root = document.createElement('div')

    expect(resolveThemeMode(undefined)).toBe('light')
    expect(resolveThemeMode({ matches: true })).toBe('dark')

    applyThemeMode('dark', root)
    expect(root.dataset.theme).toBe('dark')
    expect(root.style.colorScheme).toBe('dark')
  })
})
