import { Link } from 'react-router-dom'
import { Box } from 'lucide-react'

function SiteNotFoundPage() {
  return (
    <div className="flex min-h-screen items-center justify-center bg-neutral-950 px-6 text-neutral-50">
      <div className="w-full max-w-xl rounded-3xl border border-white/10 bg-white/5 p-10 shadow-2xl shadow-black/30 backdrop-blur">
        <div className="mb-6 flex items-center gap-3 text-sm font-semibold tracking-[0.2em] uppercase text-neutral-400">
          <Box className="h-5 w-5" />
          OnlyBoxes
        </div>
        <p className="mb-2 text-sm font-medium tracking-[0.18em] uppercase text-neutral-500">404</p>
        <h1 className="mb-4 text-4xl font-semibold tracking-tight text-white">Page not found</h1>
        <p className="mb-8 max-w-lg text-base leading-7 text-neutral-300">
          The requested page does not exist in the website. Use the homepage or jump into the docs entry point.
        </p>
        <div className="flex flex-col gap-3 sm:flex-row">
          <Link
            to="/"
            className="inline-flex items-center justify-center rounded-xl bg-white px-5 py-3 font-medium text-neutral-950 transition hover:bg-neutral-200"
          >
            Back to home
          </Link>
          <Link
            to="/docs"
            className="inline-flex items-center justify-center rounded-xl border border-white/15 px-5 py-3 font-medium text-white transition hover:bg-white/8"
          >
            Open docs
          </Link>
        </div>
      </div>
    </div>
  )
}

export default SiteNotFoundPage
