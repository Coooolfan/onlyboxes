import { createContext, useCallback, useContext, useEffect, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react'
import type { DocsLocale } from '../docs/registry'
import { resolveSiteLocale, shouldUseStoredLocale } from '../../app/routing'

const LOCALE_KEY = 'ob-locale'

interface SiteContextValue {
  isDark: boolean
  setIsDark: Dispatch<SetStateAction<boolean>>
  locale: DocsLocale
  setLocale: (locale: DocsLocale) => void
}

const SiteContext = createContext<SiteContextValue | null>(null)

export function useSiteContext() {
  const ctx = useContext(SiteContext)
  if (!ctx) throw new Error('useSiteContext must be used within SiteProvider')
  return ctx
}

function canUseBrowserApis() {
  return typeof window !== 'undefined'
}

function readStoredLocale(): DocsLocale | null {
  if (!canUseBrowserApis()) {
    return null
  }

  try {
    const value = localStorage.getItem(LOCALE_KEY)
    if (value === 'en' || value === 'zh-CN') return value
  } catch { /* ignore */ }
  return null
}

function readPreferredDarkMode() {
  if (!canUseBrowserApis() || typeof window.matchMedia !== 'function') {
    return false
  }

  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

export function SiteProvider({ children, pathname }: { children: ReactNode; pathname: string }) {
  const [isDark, setIsDark] = useState(false)
  const [locale, setLocaleRaw] = useState<DocsLocale>(() => resolveSiteLocale(pathname))

  useEffect(() => {
    setIsDark(readPreferredDarkMode())

    if (!canUseBrowserApis() || typeof window.matchMedia !== 'function') {
      return
    }

    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const handler = (e: MediaQueryListEvent) => setIsDark(e.matches)
    mq.addEventListener('change', handler)
    return () => mq.removeEventListener('change', handler)
  }, [])

  useEffect(() => {
    const routeLocale = resolveSiteLocale(pathname)

    if (shouldUseStoredLocale(pathname)) {
      setLocaleRaw(readStoredLocale() ?? routeLocale)
      return
    }

    setLocaleRaw(routeLocale)
  }, [pathname])

  const setLocale = useCallback((next: DocsLocale) => {
    setLocaleRaw(next)
    try { localStorage.setItem(LOCALE_KEY, next) } catch { /* ignore */ }
  }, [])

  return (
    <SiteContext.Provider value={{ isDark, setIsDark, locale, setLocale }}>
      {children}
    </SiteContext.Provider>
  )
}
