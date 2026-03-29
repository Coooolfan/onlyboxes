import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it } from 'vitest'
import { WebsiteRoutes } from '../../../app/routes'
import { SiteProvider } from '../../site/SiteContext'
import {
  getAlternateDocHref,
  listLocaleDocs,
} from '../registry'

function mockNavigatorLanguage(language: string) {
  Object.defineProperty(window.navigator, 'language', {
    configurable: true,
    value: language,
  })
}

function renderRoutes(initialEntry: string) {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <SiteProvider>
        <WebsiteRoutes />
      </SiteProvider>
    </MemoryRouter>,
  )
}

describe('docs routing', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('redirects /docs to zh-CN docs for Chinese browsers', async () => {
    mockNavigatorLanguage('zh-CN')

    const view = renderRoutes('/docs')

    expect(await view.findByRole('heading', { name: 'OnlyBoxes 概览' })).toBeInTheDocument()
  })

  it('redirects /docs to English docs for non-Chinese browsers', async () => {
    mockNavigatorLanguage('en-US')

    const view = renderRoutes('/docs')

    expect(await view.findByRole('heading', { name: 'OnlyBoxes Overview' })).toBeInTheDocument()
  })

  it('keeps docs sorted by order within a locale', () => {
    const docs = listLocaleDocs('en').map((entry) => entry.meta.slug)

    expect(docs).toEqual([
      '',
      'architecture',
      'install',
      'console-config',
      'quick-start',
      'worker-docker',
      'worker-boxlite',
      'worker-sys',
      'console-api',
      'mcp-tools',
      'security-faq',
    ])
  })

  it('generates the language switch href for matching slugs', () => {
    expect(getAlternateDocHref('en', 'quick-start')).toBe('/zh-CN/docs/quick-start')
    expect(getAlternateDocHref('zh-CN', '')).toBe('/en/docs')
  })

  it('renders an in-docs 404 state for missing pages', async () => {
    const view = renderRoutes('/en/docs/not-a-real-page')

    expect(await view.findByRole('heading', { name: 'Document not found' })).toBeInTheDocument()
    expect(view.getByRole('link', { name: 'Browse docs home' })).toHaveAttribute('href', '/en/docs')
  })

  it('renders the language switcher for the current slug', async () => {
    const view = renderRoutes('/en/docs/quick-start')

    expect(await view.findByRole('heading', { name: 'Quick Start' })).toBeInTheDocument()
    expect(view.getByRole('link', { name: '简体中文' })).toHaveAttribute('href', '/zh-CN/docs/quick-start')
  })
})
