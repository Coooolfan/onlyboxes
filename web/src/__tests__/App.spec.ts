import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

import { flushPromises, mount } from '@vue/test-utils'
import App from '../App.vue'

const statsPayload = {
  total: 150,
  online: 120,
  offline: 30,
  stale: 10,
  stale_after_sec: 45,
  generated_at: '2026-02-16T10:00:10Z',
}

const workersPayload = {
  items: [
    {
      node_id: 'node-1',
      node_name: 'worker-1',
      executor_kind: 'docker',
      capabilities: [{ name: 'echo' }],
      labels: { zone: 'a' },
      version: 'v0.1.0',
      status: 'online',
      registered_at: '2026-02-16T10:00:00Z',
      last_seen_at: '2026-02-16T10:00:05Z',
    },
  ],
  total: 1,
  page: 1,
  page_size: 25,
}

function jsonResponse(payload: unknown) {
  return {
    ok: true,
    status: 200,
    statusText: 'OK',
    json: async () => payload,
  }
}

function noContentResponse() {
  return {
    ok: true,
    status: 204,
    statusText: 'No Content',
    json: async () => ({}),
  }
}

function unauthorizedResponse() {
  return {
    ok: false,
    status: 401,
    statusText: 'Unauthorized',
    json: async () => ({ error: 'authentication required' }),
  }
}

describe('App', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('shows login panel when dashboard APIs return 401', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/workers?')) {
        return unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = mount(App)
    await flushPromises()

    expect(wrapper.text()).toContain('Sign In to Control Panel')
    expect(wrapper.text()).not.toContain('Execution Node Control Panel')
    wrapper.unmount()
  })

  it('logs in and renders dashboard', async () => {
    let authenticated = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/v1/console/login') {
        authenticated = true
        return jsonResponse({ authenticated: true })
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return authenticated ? jsonResponse(statsPayload) : unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/workers?')) {
        return authenticated ? jsonResponse(workersPayload) : unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = mount(App)
    await flushPromises()

    await wrapper.get('#dashboard-username').setValue('admin-test')
    await wrapper.get('#dashboard-password').setValue('password-test')
    await wrapper.get('form.login-form').trigger('submit.prevent')
    await flushPromises()

    expect(wrapper.text()).toContain('Execution Node Control Panel')
    expect(wrapper.text()).toContain('worker-1')

    const loginCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/v1/console/login')
    expect(loginCall).toBeTruthy()
    expect(loginCall?.[1]).toEqual(expect.objectContaining({ credentials: 'same-origin' }))

    const workersCall = fetchMock.mock.calls.find(([url]) => String(url).startsWith('/api/v1/workers?'))
    expect(workersCall?.[1]).toEqual(expect.objectContaining({ credentials: 'same-origin' }))

    wrapper.unmount()
  })

  it('logs out and returns to login panel', async () => {
    let authenticated = true
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url === '/api/v1/console/logout') {
        authenticated = false
        return noContentResponse()
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return authenticated ? jsonResponse(statsPayload) : unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/workers?')) {
        return authenticated ? jsonResponse(workersPayload) : unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = mount(App)
    await flushPromises()

    expect(wrapper.text()).toContain('Execution Node Control Panel')

    const logoutBtn = wrapper.findAll('button').find((button) => button.text() === 'Logout')
    expect(logoutBtn).toBeTruthy()
    await logoutBtn?.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Sign In to Control Panel')

    const logoutCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/v1/console/logout')
    expect(logoutCall).toBeTruthy()
    expect(logoutCall?.[1]).toEqual(expect.objectContaining({ credentials: 'same-origin' }))

    wrapper.unmount()
  })

  it('returns to login when refresh receives 401', async () => {
    let forceUnauthorized = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(workersPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = mount(App)
    await flushPromises()

    expect(wrapper.text()).toContain('Execution Node Control Panel')

    forceUnauthorized = true
    const refreshBtn = wrapper.findAll('button').find((button) => button.text() === 'Refresh Now')
    expect(refreshBtn).toBeTruthy()
    await refreshBtn?.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('Sign In to Control Panel')
    expect(wrapper.text()).not.toContain('API 401')

    wrapper.unmount()
  })
})
