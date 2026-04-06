import { fireEvent, render, waitFor } from '@testing-library/react'
import { MemoryRouter, useLocation, useNavigate } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { WebsiteApp } from '../../../app/WebsiteApp'
import {
  getAlternateDocHref,
  listLocaleDocs,
} from '../registry'

function RouteTestProbe() {
  const location = useLocation()
  const navigate = useNavigate()

  return (
    <>
      <output data-testid="location">{location.pathname}</output>
      <button type="button" onClick={() => navigate('/en/docs/not-a-real-page')}>
        Open missing doc
      </button>
    </>
  )
}

function renderRoutes(initialEntry: string, withProbe = false) {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <WebsiteApp />
      {withProbe ? <RouteTestProbe /> : null}
    </MemoryRouter>,
  )
}

describe('docs routing', () => {
  it('renders a static docs language index at /docs', async () => {
    const view = renderRoutes('/docs')

    expect(await view.findByRole('heading', { name: 'OnlyBoxes Docs' })).toBeInTheDocument()
    expect(view.getByRole('link', { name: 'English' })).toHaveAttribute('href', '/en/docs/')
    expect(view.getByRole('link', { name: '简体中文' })).toHaveAttribute('href', '/zh-CN/docs/')
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
    expect(getAlternateDocHref('en', 'quick-start')).toBe('/zh-CN/docs/quick-start/')
    expect(getAlternateDocHref('zh-CN', '')).toBe('/en/docs/')
  })

  it('redirects missing docs pages to the site-wide 404 route', async () => {
    const view = renderRoutes('/en/docs/not-a-real-page', true)

    expect(await view.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
    await waitFor(() => expect(view.getByTestId('location')).toHaveTextContent('/404'))
  })

  it('renders the language switcher for the current slug', async () => {
    const view = renderRoutes('/en/docs/quick-start')

    expect(await view.findByRole('heading', { name: 'Quick Start' })).toBeInTheDocument()
    expect(view.getByRole('link', { name: '简体中文' })).toHaveAttribute('href', '/zh-CN/docs/quick-start/')
  })

  it('highlights every toc section whose content is currently visible', async () => {
    const rects = new WeakMap<Element, DOMRect>()
    const defaultRect = new DOMRect(0, 0, 0, 0)
    const getBoundingClientRectSpy = vi
      .spyOn(HTMLElement.prototype, 'getBoundingClientRect')
      .mockImplementation(function mockRect(this: HTMLElement) {
        return rects.get(this) ?? defaultRect
      })

    Object.defineProperty(window, 'innerHeight', {
      configurable: true,
      value: 700,
      writable: true,
    })
    Object.defineProperty(window, 'scrollY', {
      configurable: true,
      value: 0,
      writable: true,
    })

    const view = renderRoutes('/en/docs/quick-start')

    expect(await view.findByRole('heading', { name: 'Quick Start' })).toBeInTheDocument()

    const content = document.getElementById('docs-content')
    const headings = [...document.querySelectorAll<HTMLElement>('#docs-content h2[id], #docs-content h3[id]')]

    expect(content).not.toBeNull()
    expect(headings.length).toBeGreaterThan(3)

    rects.set(content!, new DOMRect(0, 0, 960, 2200))
    headings.forEach((heading, index) => {
      rects.set(heading, new DOMRect(0, 80 + index * 240, 960, 40))
    })

    fireEvent.scroll(window)

    await waitFor(() => {
      expect(view.getByRole('link', { name: '1. Start the control node' })).toHaveAttribute('data-active', 'true')
      expect(view.getByRole('link', { name: '2. Sign in' })).toHaveAttribute('data-active', 'true')
      expect(view.getByRole('link', { name: '3. Create a worker' })).toHaveAttribute('data-active', 'true')
    })

    expect(view.getByRole('link', { name: '4. Start the worker' })).toHaveAttribute('data-active', 'false')

    getBoundingClientRectSpy.mockRestore()
  })

  it('keeps client-side navigation to missing docs consistent with direct visits', async () => {
    const view = renderRoutes('/en/docs/quick-start', true)

    expect(await view.findByRole('heading', { name: 'Quick Start' })).toBeInTheDocument()
    fireEvent.click(view.getByRole('button', { name: 'Open missing doc' }))

    expect(await view.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
    await waitFor(() => expect(view.getByTestId('location')).toHaveTextContent('/404'))
  })
})
