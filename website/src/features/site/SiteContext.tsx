import { createContext, useCallback, useContext, useEffect, useState, type Dispatch, type ReactNode, type SetStateAction } from 'react'
import type { DocsLocale } from '../docs/registry'

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

function readStoredLocale(): DocsLocale | null {
  try {
    const value = localStorage.getItem(LOCALE_KEY)
    if (value === 'en' || value === 'zh-CN') return value
  } catch { /* ignore */ }
  return null
}

export function SiteProvider({ children }: { children: ReactNode }) {
  const [isDark, setIsDark] = useState(() =>
    window.matchMedia('(prefers-color-scheme: dark)').matches,
  )

  const [locale, setLocaleRaw] = useState<DocsLocale>(() =>
    readStoredLocale() ?? 'en',
  )

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const handler = (e: MediaQueryListEvent) => setIsDark(e.matches)
    mq.addEventListener('change', handler)
    return () => mq.removeEventListener('change', handler)
  }, [])

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
