import { BrowserRouter } from 'react-router-dom'
import { WebsiteRoutes } from './app/routes'
import { SiteProvider } from './features/site/SiteContext'

function App() {
  return (
    <BrowserRouter>
      <SiteProvider>
        <WebsiteRoutes />
      </SiteProvider>
    </BrowserRouter>
  )
}

export default App
