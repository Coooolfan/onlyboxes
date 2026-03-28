import { useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Box, Github, Languages, List, Moon, Sun, X } from 'lucide-react'
import {
  type DocsLocale,
  docsLocaleLabels,
  getAlternateDocHref,
  getDocEntry,
  getDocHref,
  getDocsRootHref,
  getOtherDocsLocale,
  listLocaleSections,
} from './registry'
import { mdxComponents } from './mdx-components'
import { useSiteContext } from '../site/SiteContext'

interface TocItem {
  id: string
  title: string
  level: 2 | 3
}

const docsCopy = {
  en: {
    docs: 'Docs',
    menu: 'Menu',
    onThisPage: 'On this page',
    languageSwitch: 'Language',
    documentNotFound: 'Document not found',
    documentNotFoundBody: 'The requested page is not available in this locale. Use the docs home or switch language.',
    browseDocs: 'Browse docs home',
    backHome: 'Back to home',
    github: 'GitHub',
  },
  'zh-CN': {
    docs: '文档',
    menu: '目录',
    onThisPage: '本页内容',
    languageSwitch: '语言',
    documentNotFound: '文档不存在',
    documentNotFoundBody: '当前语言下没有这个页面。你可以返回文档首页，或切换到另一种语言。',
    browseDocs: '返回文档首页',
    backHome: '返回首页',
    github: 'GitHub',
  },
} as const

function useTableOfContents(key: string) {
  const [items, setItems] = useState<TocItem[]>([])

  useEffect(() => {
    const frame = window.requestAnimationFrame(() => {
      const container = document.getElementById('docs-content')

      if (!container) {
        setItems([])
        return
      }

      const headings = [...container.querySelectorAll<HTMLHeadingElement>('h2[id], h3[id]')].map((heading) => {
        const level: TocItem['level'] = heading.tagName === 'H2' ? 2 : 3

        return {
          id: heading.id,
          title: heading.textContent?.trim() ?? '',
          level,
        }
      })

      setItems(headings.filter((heading) => heading.title))
    })

    return () => window.cancelAnimationFrame(frame)
  }, [key])

  return items
}

function DocsNavigation({
  locale,
  currentSlug,
  isDark,
  onNavigate,
}: {
  locale: DocsLocale
  currentSlug: string
  isDark: boolean
  onNavigate?: () => void
}) {
  const copy = docsCopy[locale]
  const sections = listLocaleSections(locale)

  return (
    <nav aria-label={copy.docs} className="space-y-6">
      {sections.map((section) => (
        <div key={section.title}>
          <p
            className={`mb-2 text-xs font-semibold tracking-[0.18em] uppercase transition-colors duration-300 ${
              isDark ? 'text-neutral-500' : 'text-neutral-400'
            }`}
          >
            {section.title}
          </p>
          <ul className="space-y-0.5">
            {section.docs.map((doc) => {
              const active = doc.meta.slug === currentSlug
              return (
                <li key={doc.meta.slug || '__root__'}>
                  <Link
                    to={getDocHref(locale, doc.meta.slug)}
                    onClick={onNavigate}
                    className={`block rounded-sm px-2.5 py-1.5 text-sm transition-colors duration-300 ${
                      active
                        ? isDark
                          ? 'bg-neutral-800 text-white'
                          : 'bg-neutral-100 text-neutral-950'
                        : isDark
                          ? 'text-neutral-400 hover:text-white'
                          : 'text-neutral-600 hover:text-neutral-950'
                    }`}
                  >
                    {doc.meta.title}
                  </Link>
                </li>
              )
            })}
          </ul>
        </div>
      ))}
    </nav>
  )
}

function DocsTableOfContents({ locale, items, isDark }: { locale: DocsLocale; items: TocItem[]; isDark: boolean }) {
  const copy = docsCopy[locale]

  if (!items.length) {
    return null
  }

  return (
    <div>
      <p
        className={`mb-3 text-xs font-semibold tracking-[0.18em] uppercase transition-colors duration-300 ${
          isDark ? 'text-neutral-500' : 'text-neutral-400'
        }`}
      >
        {copy.onThisPage}
      </p>
      <ul className="space-y-1.5">
        {items.map((item) => (
          <li key={item.id}>
            <a
              href={`#${item.id}`}
              className={`block text-sm transition-colors duration-300 ${
                isDark ? 'text-neutral-500 hover:text-white' : 'text-neutral-500 hover:text-neutral-950'
              } ${item.level === 3 ? 'pl-3' : ''}`}
            >
              {item.title}
            </a>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function DocsPage({ locale }: { locale: DocsLocale }) {
  const params = useParams()
  const currentSlug = (params['*'] ?? '').replace(/^\/+|\/+$/g, '')
  const entry = getDocEntry(locale, currentSlug)
  const copy = docsCopy[locale]
  const [isMenuOpen, setIsMenuOpen] = useState(false)
  const tocItems = useTableOfContents(`${locale}:${currentSlug}:${entry?.meta.title ?? '404'}`)
  const targetLocale = locale === 'en' ? 'zh-CN' : 'en'

  const { isDark, setIsDark, setLocale: setSiteLocale } = useSiteContext()

  useEffect(() => {
    setSiteLocale(locale)
  }, [locale, setSiteLocale])

  useEffect(() => {
    const title = entry ? `${entry.meta.title} | OnlyBoxes Docs` : `404 | OnlyBoxes Docs`
    document.title = title
  }, [entry])

  useEffect(() => {
    setIsMenuOpen(false)
  }, [locale, currentSlug])

  const alternateHref = useMemo(() => {
    if (!entry) {
      return getDocsRootHref(getOtherDocsLocale(locale))
    }

    return getAlternateDocHref(locale, entry.meta.slug)
  }, [entry, locale])

  const CurrentDoc = entry?.Component

  const headerBtnClass = `inline-flex items-center gap-2 rounded px-3 py-2 text-sm font-medium transition-colors duration-300 ${
    isDark
      ? 'text-neutral-400 hover:text-white'
      : 'text-neutral-500 hover:text-black'
  }`

  return (
    <div
      data-docs-theme={isDark ? 'dark' : 'light'}
      className={`min-h-screen transition-colors duration-300 ${
        isDark ? 'bg-black text-white selection:bg-neutral-800' : 'bg-white text-black selection:bg-neutral-200'
      }`}
    >
      <header
        className={`sticky top-0 z-40 border-b backdrop-blur-sm transition-colors duration-300 ${
          isDark ? 'border-neutral-900 bg-black/90' : 'border-neutral-100 bg-white/90'
        }`}
      >
        <div className="mx-auto flex max-w-7xl items-center justify-between gap-4 px-4 py-4 sm:px-6 lg:px-8">
          <div className="flex items-center gap-4">
            <Link
              to="/"
              className={`flex items-center gap-2 text-sm font-semibold tracking-tight transition-colors duration-300 ${
                isDark ? 'text-white' : 'text-black'
              }`}
            >
              <Box className={`h-5 w-5 transition-colors duration-300 ${isDark ? 'text-white' : 'text-black'}`} />
              <span>OnlyBoxes</span>
            </Link>
            <div
              className={`hidden h-5 w-px transition-colors duration-300 md:block ${
                isDark ? 'bg-neutral-800' : 'bg-neutral-200'
              }`}
            />
            <span
              className={`hidden text-sm transition-colors duration-300 md:block ${
                isDark ? 'text-neutral-500' : 'text-neutral-500'
              }`}
            >
              {copy.docs}
            </span>
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setIsDark((v) => !v)}
              className={`rounded p-2 transition-colors duration-300 ${
                isDark ? 'text-neutral-400 hover:text-white' : 'text-neutral-500 hover:text-black'
              }`}
              aria-label="Toggle theme"
            >
              {isDark ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
            </button>
            <Link
              to={alternateHref}
              aria-label={docsLocaleLabels[targetLocale]}
              className={headerBtnClass}
            >
              <Languages className="h-4 w-4" />
              <span className="hidden sm:inline">{docsLocaleLabels[targetLocale]}</span>
              <span className="sm:hidden">{copy.languageSwitch}</span>
            </Link>
            <a
              href="https://github.com/Coooolfan/onlyboxes"
              target="_blank"
              rel="noreferrer"
              className={`hidden sm:inline-flex ${headerBtnClass}`}
            >
              <Github className="h-4 w-4" />
              {copy.github}
            </a>
            <button
              type="button"
              onClick={() => setIsMenuOpen(true)}
              className={`lg:hidden ${headerBtnClass}`}
            >
              <List className="h-4 w-4" />
              {copy.menu}
            </button>
          </div>
        </div>
      </header>

      <div className="mx-auto flex max-w-7xl gap-8 px-4 py-8 sm:px-6 lg:px-8">
        <aside className="sticky top-24 hidden h-[calc(100vh-7rem)] w-56 shrink-0 overflow-y-auto pr-4 lg:block">
          <DocsNavigation locale={locale} currentSlug={entry?.meta.slug ?? currentSlug} isDark={isDark} />
        </aside>

        <main
          className={`min-w-0 flex-1 border-l pl-8 transition-colors duration-300 ${
            isDark ? 'border-neutral-800' : 'border-neutral-200'
          }`}
        >
          <div className="px-2 py-2 sm:px-4">
            {CurrentDoc ? (
              <article id="docs-content">
                <CurrentDoc components={mdxComponents} />
              </article>
            ) : (
              <article id="docs-content">
                <p
                  className={`mb-2 text-xs font-semibold tracking-[0.2em] uppercase transition-colors duration-300 ${
                    isDark ? 'text-neutral-500' : 'text-neutral-400'
                  }`}
                >
                  404
                </p>
                <h1
                  className={`mb-4 text-4xl font-semibold tracking-tight transition-colors duration-300 ${
                    isDark ? 'text-white' : 'text-neutral-950'
                  }`}
                >
                  {copy.documentNotFound}
                </h1>
                <p
                  className={`max-w-2xl text-base leading-8 transition-colors duration-300 ${
                    isDark ? 'text-neutral-400' : 'text-neutral-700'
                  }`}
                >
                  {copy.documentNotFoundBody}
                </p>
                <div className="mt-8 flex flex-col gap-3 sm:flex-row">
                  <Link
                    to={getDocsRootHref(locale)}
                    className={`inline-flex items-center justify-center rounded px-6 py-3 font-medium transition-colors ${
                      isDark ? 'bg-white text-black hover:bg-neutral-200' : 'bg-black text-white hover:bg-neutral-800'
                    }`}
                  >
                    {copy.browseDocs}
                  </Link>
                  <Link
                    to="/"
                    className={`inline-flex items-center justify-center rounded border px-6 py-3 font-medium transition-colors ${
                      isDark
                        ? 'border-neutral-700 text-white hover:bg-neutral-900'
                        : 'border-neutral-200 text-black hover:border-neutral-300 hover:bg-neutral-50'
                    }`}
                  >
                    {copy.backHome}
                  </Link>
                </div>
              </article>
            )}
          </div>

        </main>

        <aside
          className={`sticky top-24 hidden h-[calc(100vh-7rem)] w-52 shrink-0 overflow-y-auto border-l pl-6 transition-colors duration-300 xl:block ${
            isDark ? 'border-neutral-800' : 'border-neutral-200'
          }`}
        >
          <DocsTableOfContents locale={locale} items={tocItems} isDark={isDark} />
        </aside>
      </div>

      {isMenuOpen ? (
        <div className="fixed inset-0 z-50 bg-neutral-950/50 backdrop-blur-sm lg:hidden">
          <div
            className={`ml-auto h-full w-full max-w-sm p-6 shadow-2xl transition-colors duration-300 ${
              isDark ? 'bg-black' : 'bg-white'
            }`}
          >
            <div className="mb-6 flex items-center justify-between">
              <p
                className={`text-sm font-semibold tracking-[0.18em] uppercase transition-colors duration-300 ${
                  isDark ? 'text-neutral-500' : 'text-neutral-400'
                }`}
              >
                {copy.menu}
              </p>
              <button
                type="button"
                onClick={() => setIsMenuOpen(false)}
                className={`rounded p-2 transition-colors duration-300 ${
                  isDark
                    ? 'text-neutral-400 hover:text-white'
                    : 'text-neutral-500 hover:text-black'
                }`}
                aria-label="Close menu"
              >
                <X className="h-4 w-4" />
              </button>
            </div>
            <DocsNavigation
              locale={locale}
              currentSlug={entry?.meta.slug ?? currentSlug}
              isDark={isDark}
              onNavigate={() => setIsMenuOpen(false)}
            />
          </div>
        </div>
      ) : null}
    </div>
  )
}
