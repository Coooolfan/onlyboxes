import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises } from '@vue/test-utils'

import router from '../router'
import {
  adminSessionPayload,
  defaultAccountsPayload,
  defaultTokensPayload,
  inflightPayload,
  jsonResponse,
  memberSessionPayload,
  mountApp,
  statsPayload,
  unauthorizedResponse,
  workersPayload,
} from './testkit'

describe('Auth Routing', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('bootstraps auth state from /api/v1/auth/session', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(wrapper.text()).toContain('Execution Node Control Panel')
    expect(wrapper.text()).toContain('Console v0.1.0')
    expect(wrapper.find('.console-footer-link').attributes('href')).toBe(
      'https://github.com/Coooolfan/onlyboxes',
    )
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/v1/auth/session')).toBe(true)

    wrapper.unmount()
  })

  it('allows admin /accounts access', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/accounts?')) {
        return jsonResponse(defaultAccountsPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')

    expect(router.currentRoute.value.path).toBe('/accounts')
    expect(wrapper.text()).toContain('Account Administration')

    wrapper.unmount()
  })

  it('allows non-admin /workers access', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(wrapper.text()).toContain('Execution Node Control Panel')

    wrapper.unmount()
  })

  it('allows non-admin /tools/worker-startup access', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url.startsWith('/api/v1/workers')) {
        throw new Error(`workers api should not be called on startup tool page: ${url}`)
      }
      if (url.startsWith('/api/v1/accounts')) {
        throw new Error(`accounts api should not be called on startup tool page: ${url}`)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/tools/worker-startup')

    expect(router.currentRoute.value.path).toBe('/tools/worker-startup')
    expect(wrapper.text()).toContain('Worker Startup Tool')

    wrapper.unmount()
  })

  it('redirects non-admin /accounts access to /tokens', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url.startsWith('/api/v1/accounts?')) {
        throw new Error(`accounts api should not be called for non-admin: ${url}`)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')

    expect(router.currentRoute.value.path).toBe('/tokens')
    expect(wrapper.text()).toContain('Trusted Token Management')

    wrapper.unmount()
  })

  it('routes non-admin login to /tokens', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return unauthorizedResponse()
      }
      if (url === '/api/v1/auth/login') {
        return jsonResponse({
          ...memberSessionPayload,
          authenticated: true,
        })
      }
      if (url === '/api/v1/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}, method=${String(init?.method ?? 'GET')}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/login')

    await wrapper.get('#dashboard-username').setValue('member-test')
    await wrapper.get('#dashboard-password').setValue('member-pass')
    await wrapper.get('form.login-form').trigger('submit.prevent')
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/tokens')
    expect(wrapper.text()).toContain('Trusted Token Management')
    expect(wrapper.text()).not.toContain('Execution Node Control Panel')

    const loginCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/v1/auth/login')
    expect(loginCall).toBeTruthy()

    wrapper.unmount()
  })

  it('redirects authenticated /login visits to role home', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/login')

    expect(router.currentRoute.value.path).toBe('/tokens')
    expect(wrapper.text()).toContain('Trusted Token Management')
    expect(wrapper.text()).not.toContain('Sign In to Control Panel')

    wrapper.unmount()
  })

  it('routes unauthenticated / to /login', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/')

    expect(router.currentRoute.value.path).toBe('/login')
    expect(wrapper.text()).toContain('Sign In to Control Panel')

    wrapper.unmount()
  })

  it('routes non-admin / to /tokens', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/')

    expect(router.currentRoute.value.path).toBe('/tokens')
    expect(wrapper.text()).toContain('Trusted Token Management')

    wrapper.unmount()
  })

  it('routes admin / to /workers', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/')

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(wrapper.text()).toContain('Execution Node Control Panel')

    wrapper.unmount()
  })

  it('routes unknown paths to role home without falling back to /', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/auth/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url.startsWith('/api/v1/workers')) {
        throw new Error(`workers api should not be called for non-admin: ${url}`)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/not-found')

    expect(router.currentRoute.value.path).toBe('/tokens')

    wrapper.unmount()
  })
})
