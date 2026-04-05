import { Link } from 'react-router-dom'
import { Languages } from 'lucide-react'
import { docsLocaleLabels, getDocsRootHref } from '../features/docs/registry'
import { useSiteContext } from '../features/site/SiteContext'

const copy = {
  en: {
    title: 'OnlyBoxes Docs',
    body: 'Choose a documentation language to browse the static docs site.',
    defaultLocale: 'Default',
    recommended: 'Recommended',
  },
  'zh-CN': {
    title: 'OnlyBoxes 文档',
    body: '请选择一种文档语言以浏览预生成的静态文档站点。',
    defaultLocale: '默认',
    recommended: '推荐',
  },
} as const

export default function DocsIndexPage() {
  const { isDark, locale } = useSiteContext()
  const pageCopy = copy[locale]

  return (
    <div
      className={`flex min-h-screen items-center justify-center px-6 transition-colors duration-300 ${
        isDark ? 'bg-black text-white selection:bg-neutral-800' : 'bg-white text-black selection:bg-neutral-200'
      }`}
    >
      <div
        className={`w-full max-w-3xl rounded-3xl border p-10 shadow-xl transition-colors duration-300 ${
          isDark ? 'border-neutral-900 bg-neutral-950' : 'border-neutral-200 bg-white'
        }`}
      >
        <div className="mb-6 flex items-center gap-3 text-sm font-semibold tracking-[0.18em] uppercase text-neutral-500">
          <Languages className="h-5 w-5" />
          Docs
        </div>
        <h1 className="mb-4 text-4xl font-semibold tracking-tight">{pageCopy.title}</h1>
        <p className={`max-w-2xl text-base leading-8 ${isDark ? 'text-neutral-400' : 'text-neutral-600'}`}>
          {pageCopy.body}
        </p>
        <div className="mt-10 grid gap-4 md:grid-cols-2">
          {(['en', 'zh-CN'] as const).map((docsLocale) => (
            <Link
              key={docsLocale}
              to={getDocsRootHref(docsLocale)}
              aria-label={docsLocaleLabels[docsLocale]}
              className={`rounded-2xl border px-6 py-5 transition-colors duration-300 ${
                isDark
                  ? 'border-neutral-800 hover:border-neutral-700 hover:bg-neutral-900'
                  : 'border-neutral-200 hover:border-neutral-300 hover:bg-neutral-50'
              }`}
            >
              <div className="flex items-center justify-between gap-4">
                <div>
                  <p className="text-lg font-semibold tracking-tight">{docsLocaleLabels[docsLocale]}</p>
                  <p className={`mt-2 text-sm ${isDark ? 'text-neutral-400' : 'text-neutral-500'}`}>
                    {docsLocale === 'en' ? pageCopy.defaultLocale : pageCopy.recommended}
                  </p>
                </div>
              </div>
            </Link>
          ))}
        </div>
      </div>
    </div>
  )
}
