import { BrowserRouter } from 'react-router-dom'
import { WebsiteRoutes } from './app/routes'

function App() {
  return (
    <BrowserRouter>
      <WebsiteRoutes />
    </BrowserRouter>
  )
}

export default App
