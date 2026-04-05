import { getDocHref, getDocsRootHref, listLocaleDocs, type DocsLocale } from '../features/docs/registry'

export const defaultDocsLocale: DocsLocale = 'en'

export const docsLegacyRedirects = [
  { from: '/en/docs/api-mcp-overview', to: getDocHref('en', 'console-api'), locale: 'en' as DocsLocale },
  { from: '/zh-CN/docs/api-mcp-overview', to: getDocHref('zh-CN', 'console-api'), locale: 'zh-CN' as DocsLocale },
] as const

export function normalizePathname(pathname: string) {
  if (pathname === '/') {
    return pathname
  }

  return pathname.replace(/\/+$/, '') || '/'
}

export function resolveSiteLocale(pathname: string): DocsLocale {
  const normalizedPathname = normalizePathname(pathname)

  if (normalizedPathname === '/zh-CN/docs' || normalizedPathname.startsWith('/zh-CN/docs/')) {
    return 'zh-CN'
  }

  if (normalizedPathname === '/en/docs' || normalizedPathname.startsWith('/en/docs/')) {
    return 'en'
  }

  if (normalizedPathname === '/docs') {
    return defaultDocsLocale
  }

  return defaultDocsLocale
}

export function shouldUseStoredLocale(pathname: string) {
  return normalizePathname(pathname) === '/'
}

export function getLegacyRedirect(pathname: string) {
  const normalizedPathname = normalizePathname(pathname)
  return docsLegacyRedirects.find((entry) => entry.from === normalizedPathname) ?? null
}

export function listPrerenderPaths() {
  const paths = new Set<string>(['/', '/docs', '/404'])

  for (const locale of ['en', 'zh-CN'] as const) {
    for (const entry of listLocaleDocs(locale)) {
      paths.add(entry.meta.slug ? getDocHref(locale, entry.meta.slug) : getDocsRootHref(locale))
    }
  }

  for (const redirect of docsLegacyRedirects) {
    paths.add(redirect.from)
  }

  return [...paths]
}
