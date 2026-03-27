import { Navigate, Route, Routes, useLocation, useParams } from 'react-router-dom'
import HomePage from '../pages/HomePage'
import SiteNotFoundPage from '../pages/SiteNotFoundPage'
import { DocsPage } from '../features/docs/DocsPage'
import { getDocsRootHref, getPreferredDocsLocale, isDocsLocale } from '../features/docs/registry'

function DocsLanguageRedirect() {
  const location = useLocation()
  const preferredLocale = getPreferredDocsLocale(
    typeof navigator === 'undefined' ? 'en-US' : navigator.language,
  )

  return (
    <Navigate
      replace
      to={{
        pathname: getDocsRootHref(preferredLocale),
        search: location.search,
        hash: location.hash,
      }}
    />
  )
}

function LocaleDocsPage() {
  const { locale } = useParams()

  if (!locale || !isDocsLocale(locale)) {
    return <SiteNotFoundPage />
  }

  return <DocsPage locale={locale} />
}

export function WebsiteRoutes() {
  return (
    <Routes>
      <Route path="/" element={<HomePage />} />
      <Route path="/docs" element={<DocsLanguageRedirect />} />
      <Route path="/:locale/docs" element={<LocaleDocsPage />} />
      <Route path="/:locale/docs/*" element={<LocaleDocsPage />} />
      <Route path="*" element={<SiteNotFoundPage />} />
    </Routes>
  )
}
