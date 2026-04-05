import { SiteProvider } from '../features/site/SiteContext'
import { useLocation } from 'react-router-dom'
import { SeoManager } from './SeoManager'
import { WebsiteRoutes } from './routes'

export function WebsiteApp() {
  const location = useLocation()

  return (
    <SiteProvider pathname={location.pathname}>
      <SeoManager />
      <WebsiteRoutes />
    </SiteProvider>
  )
}
