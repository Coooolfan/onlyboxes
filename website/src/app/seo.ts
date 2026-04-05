import { getDocEntry, getDocHref, getDocsRootHref, getOtherDocsLocale, type DocsLocale } from '../features/docs/registry'
import { defaultDocsLocale, getLegacyRedirect, normalizePathname } from './routing'

const SITE_URL = 'https://onlybox.es'

interface AlternateLink {
  href: string
  hrefLang: string
}

export interface PageSeoData {
  alternates: AlternateLink[]
  canonical: string
  description: string
  lang: string
  robots?: string
  title: string
}

function toAbsoluteUrl(pathname: string) {
  return new URL(pathname, SITE_URL).toString()
}

function normalizeSlug(slug: string) {
  return slug.replace(/^\/+|\/+$/g, '')
}

function buildDocsAlternates(locale: DocsLocale, slug: string) {
  const currentPath = getDocHref(locale, slug)
  const otherLocale = getOtherDocsLocale(locale)
  const defaultHref = getDocEntry(defaultDocsLocale, slug)
    ? getDocHref(defaultDocsLocale, slug)
    : currentPath
  const links: AlternateLink[] = [
    {
      href: toAbsoluteUrl(currentPath),
      hrefLang: locale,
    },
  ]

  if (getDocEntry(otherLocale, slug)) {
    links.push({
      href: toAbsoluteUrl(getDocHref(otherLocale, slug)),
      hrefLang: otherLocale,
    })
  }

  links.push({
    href: toAbsoluteUrl(defaultHref),
    hrefLang: 'x-default',
  })

  return links
}

export function getDocsIndexSeo(locale: DocsLocale = defaultDocsLocale): PageSeoData {
  return {
    alternates: [
      { href: toAbsoluteUrl(getDocsRootHref('en')), hrefLang: 'en' },
      { href: toAbsoluteUrl(getDocsRootHref('zh-CN')), hrefLang: 'zh-CN' },
      { href: toAbsoluteUrl('/docs/'), hrefLang: 'x-default' },
    ],
    canonical: toAbsoluteUrl('/docs/'),
    description: locale === 'zh-CN'
      ? '选择文档语言以浏览 OnlyBoxes 的静态文档站点。'
      : 'Choose a documentation language to browse the static OnlyBoxes docs site.',
    lang: locale,
    title: 'OnlyBoxes Docs',
  }
}

export function getHomeSeo(locale: DocsLocale = defaultDocsLocale): PageSeoData {
  return {
    alternates: [],
    canonical: toAbsoluteUrl('/'),
    description: locale === 'zh-CN'
      ? 'OnlyBoxes 是一个面向个人与小型团队的自托管代码执行沙箱平台。'
      : 'OnlyBoxes is a self-hosted code execution sandbox platform for individuals and small teams.',
    lang: locale,
    title: locale === 'zh-CN'
      ? 'OnlyBoxes - 面向个人与小型团队的代码执行沙箱平台'
      : 'OnlyBoxes - Self-hosted Code Execution Sandbox Platform',
  }
}

export function getDocsSeo(pathname: string, locale: DocsLocale, slug: string): PageSeoData {
  const normalizedSlug = normalizeSlug(slug)
  const entry = getDocEntry(locale, normalizedSlug)

  if (entry === null) {
    return getNotFoundSeo('/404')
  }

  const canonicalPath = pathname === '/docs' ? getDocsRootHref(defaultDocsLocale) : getDocHref(locale, entry.meta.slug)

  return {
    alternates: buildDocsAlternates(locale, entry.meta.slug),
    canonical: toAbsoluteUrl(canonicalPath),
    description: entry.meta.description,
    lang: locale,
    title: `${entry.meta.title} | OnlyBoxes Docs`,
  }
}

export function getRedirectSeo(targetPath: string, locale: DocsLocale): PageSeoData {
  return {
    alternates: [],
    canonical: toAbsoluteUrl(targetPath),
    description: locale === 'zh-CN'
      ? '文档页面已迁移，正在跳转到最新地址。'
      : 'The documentation page has moved and is redirecting to the latest location.',
    lang: locale,
    robots: 'noindex',
    title: 'Redirecting | OnlyBoxes Docs',
  }
}

export function getNotFoundSeo(pathname = '/404'): PageSeoData {
  return {
    alternates: [],
    canonical: toAbsoluteUrl(pathname),
    description: 'The requested page does not exist on the OnlyBoxes website.',
    lang: defaultDocsLocale,
    robots: 'noindex',
    title: '404 | OnlyBoxes',
  }
}

export function getSeoForPath(pathname: string): PageSeoData {
  const normalizedPathname = normalizePathname(pathname)

  if (normalizedPathname === '/') {
    return getHomeSeo(defaultDocsLocale)
  }

  const redirect = getLegacyRedirect(normalizedPathname)

  if (redirect) {
    return getRedirectSeo(redirect.to, redirect.locale)
  }

  if (normalizedPathname === '/docs') {
    return getDocsIndexSeo(defaultDocsLocale)
  }

  const docsMatch = normalizedPathname.match(/^\/(en|zh-CN)\/docs(?:\/(.*))?$/)

  if (docsMatch) {
    const locale = docsMatch[1] as DocsLocale
    const slug = docsMatch[2] ?? ''

    if (getDocEntry(locale, slug) === null) {
      return getNotFoundSeo('/404')
    }

    return getDocsSeo(normalizedPathname, locale, slug)
  }

  return getNotFoundSeo(normalizedPathname === '/404' ? '/404' : normalizedPathname)
}

function upsertMeta(name: string, content: string) {
  let element = document.head.querySelector<HTMLMetaElement>(`meta[data-ob-seo="${name}"]`)

  if (element === null && name === 'description') {
    element = document.head.querySelector<HTMLMetaElement>('meta[name="description"]')
    element?.setAttribute('data-ob-seo', name)
  }

  if (element === null && name === 'robots') {
    element = document.head.querySelector<HTMLMetaElement>('meta[name="robots"]')
    element?.setAttribute('data-ob-seo', name)
  }

  if (element === null) {
    element = document.createElement('meta')
    element.setAttribute('data-ob-seo', name)
    element.setAttribute('name', name)
    document.head.append(element)
  }

  element.setAttribute('content', content)
}

function upsertCanonical(href: string) {
  let element = document.head.querySelector<HTMLLinkElement>('link[data-ob-seo="canonical"]')

  if (element === null) {
    element = document.head.querySelector<HTMLLinkElement>('link[rel="canonical"]')
    element?.setAttribute('data-ob-seo', 'canonical')
  }

  if (element === null) {
    element = document.createElement('link')
    element.setAttribute('data-ob-seo', 'canonical')
    element.setAttribute('rel', 'canonical')
    document.head.append(element)
  }

  element.setAttribute('href', href)
}

export function applySeoToDocument(seo: PageSeoData) {
  document.title = seo.title
  document.documentElement.lang = seo.lang
  upsertMeta('description', seo.description)
  upsertCanonical(seo.canonical)

  const robots = document.head.querySelector<HTMLMetaElement>('meta[data-ob-seo="robots"]')

  if (seo.robots) {
    if (robots === null) {
      const element = document.createElement('meta')
      element.setAttribute('data-ob-seo', 'robots')
      element.setAttribute('name', 'robots')
      element.setAttribute('content', seo.robots)
      document.head.append(element)
    } else {
      robots.setAttribute('content', seo.robots)
    }
  } else if (robots !== null) {
    robots.remove()
  }

  for (const element of document.head.querySelectorAll('link[data-ob-seo="alternate"]')) {
    element.remove()
  }

  for (const alternate of seo.alternates) {
    const element = document.createElement('link')
    element.setAttribute('data-ob-seo', 'alternate')
    element.setAttribute('rel', 'alternate')
    element.setAttribute('hreflang', alternate.hrefLang)
    element.setAttribute('href', alternate.href)
    document.head.append(element)
  }
}
