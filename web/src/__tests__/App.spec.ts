import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises, mount } from '@vue/test-utils'
import { createPinia } from 'pinia'

import App from '../App.vue'
import router from '../router'

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

const trustedTokensPayload = {
  items: [
    {
      id: 'tok-1',
      name: 'ci-prod',
      token_masked: 'obx_****9fa1',
      created_at: '2026-02-16T10:00:00Z',
      updated_at: '2026-02-16T10:00:00Z',
    },
  ],
  total: 1,
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

function errorResponse(status: number, statusText: string, error: string) {
  return {
    ok: false,
    status,
    statusText,
    json: async () => ({ error }),
  }
}

async function mountApp(path: string) {
  const pinia = createPinia()
  const wrapper = mount(App, {
    global: {
      plugins: [pinia, router],
    },
  })

  await router.push('/login')
  await flushPromises()
  await router.push(path)
  await flushPromises()

  return wrapper
}

describe('App', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('redirects unauthenticated /workers access to login page', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/workers?')) {
        return unauthorizedResponse()
      }
      if (url === '/api/v1/console/tokens') {
        return unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    expect(wrapper.text()).toContain('Sign In to Control Panel')
    expect(router.currentRoute.value.path).toBe('/login')
    wrapper.unmount()
  })

  it('redirects authenticated /login access to workers page', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/login?redirect=/workers')

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(wrapper.text()).toContain('Execution Node Control Panel')
    wrapper.unmount()
  })

  it('logs in and renders dashboard', async () => {
    let authenticated = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
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
      if (url === '/api/v1/console/tokens') {
        return authenticated ? jsonResponse(trustedTokensPayload) : unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/login')

    await wrapper.get('#dashboard-username').setValue('admin-test')
    await wrapper.get('#dashboard-password').setValue('password-test')
    await wrapper.get('form.login-form').trigger('submit.prevent')
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(wrapper.text()).toContain('Execution Node Control Panel')
    expect(wrapper.text()).toContain('worker-1')

    const loginCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/v1/console/login')
    expect(loginCall).toBeTruthy()
    wrapper.unmount()
  })

  it('shows trusted tokens collapsed by default and expands to summary list', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    expect(wrapper.text()).toContain('Trusted Tokens')
    expect(wrapper.text()).toContain('列表已折叠')
    expect(wrapper.text()).not.toContain('ci-prod')

    const expandBtn = wrapper.findAll('button').find((button) => button.text() === 'Expand')
    expect(expandBtn).toBeTruthy()
    await expandBtn?.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('ci-prod')
    expect(wrapper.text()).toContain('obx_****9fa1')
    wrapper.unmount()
  })

  it('creates trusted token in modal and only exposes plaintext once', async () => {
    const writeText = vi.fn(async () => {})
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    let tokens = [...trustedTokensPayload.items]
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens' && !init?.method) {
        return jsonResponse({ items: tokens, total: tokens.length })
      }
      if (url === '/api/v1/console/tokens' && init?.method === 'POST') {
        tokens = [
          ...tokens,
          {
            id: 'tok-2',
            name: 'ci-staging',
            token_masked: 'manu****oken',
            created_at: '2026-02-16T10:01:00Z',
            updated_at: '2026-02-16T10:01:00Z',
          },
        ]
        return jsonResponse({
          id: 'tok-2',
          name: 'ci-staging',
          token: 'obx_plaintext_once',
          token_masked: 'manu****oken',
          generated: true,
          created_at: '2026-02-16T10:01:00Z',
          updated_at: '2026-02-16T10:01:00Z',
        })
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    const newTokenBtn = wrapper.findAll('button').find((button) => button.text() === 'New Token')
    expect(newTokenBtn).toBeTruthy()
    await newTokenBtn?.trigger('click')
    await flushPromises()

    const nameInput = wrapper.find('.token-modal input')
    expect(nameInput.exists()).toBe(true)
    await nameInput.setValue('ci-staging')

    const modalForm = wrapper.find('form.token-modal-form')
    expect(modalForm.exists()).toBe(true)
    await modalForm.trigger('submit.prevent')
    await flushPromises()

    expect(wrapper.text()).toContain('Token Created')
    expect(wrapper.text()).toContain('obx_plaintext_once')

    const createCall = fetchMock.mock.calls.find(
      ([url, init]) => String(url) === '/api/v1/console/tokens' && (init as RequestInit | undefined)?.method === 'POST',
    )
    expect(createCall).toBeTruthy()
    if (!createCall) {
      throw new Error('missing trusted token create call')
    }
    const requestBody = JSON.parse(String((createCall[1] as RequestInit | undefined)?.body ?? '{}'))
    expect(requestBody).toEqual({ name: 'ci-staging' })
    expect('token' in requestBody).toBe(false)

    const copyBtn = wrapper.findAll('button').find((button) => button.text() === 'Copy Token')
    expect(copyBtn).toBeTruthy()
    await copyBtn?.trigger('click')
    await flushPromises()
    expect(writeText).toHaveBeenCalledWith('obx_plaintext_once')

    const doneBtn = wrapper.findAll('button').find((button) => button.text() === 'Done')
    expect(doneBtn).toBeTruthy()
    await doneBtn?.trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('obx_plaintext_once')
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('/api/v1/console/tokens/tok-2/value'))).toBe(false)

    wrapper.unmount()
  })

  it('deletes trusted token and refreshes list', async () => {
    vi.stubGlobal('confirm', vi.fn(() => true))

    let tokens = [...trustedTokensPayload.items]
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse({ items: [], total: 0, page: 1, page_size: 25 })
      }
      if (url === '/api/v1/console/tokens' && !init?.method) {
        return jsonResponse({ items: tokens, total: tokens.length })
      }
      if (url === '/api/v1/console/tokens/tok-1' && init?.method === 'DELETE') {
        tokens = []
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    expect(wrapper.text()).not.toContain('ci-prod')

    const expandBtn = wrapper.findAll('button').find((button) => button.text() === 'Expand')
    expect(expandBtn).toBeTruthy()
    await expandBtn?.trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('ci-prod')

    const deleteBtn = wrapper.find('.token-panel .token-actions button')
    expect(deleteBtn.exists()).toBe(true)
    await deleteBtn.trigger('click')
    await flushPromises()

    expect(wrapper.text()).not.toContain('ci-prod')
    const deleteCall = fetchMock.mock.calls.find(
      ([url, init]) => String(url) === '/api/v1/console/tokens/tok-1' && (init as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCall).toBeTruthy()
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
      if (url === '/api/v1/console/tokens') {
        return authenticated ? jsonResponse(trustedTokensPayload) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/login' && init?.method === 'POST') {
        authenticated = true
        return jsonResponse({ authenticated: true })
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    expect(wrapper.text()).toContain('Execution Node Control Panel')

    const logoutBtn = wrapper.findAll('button').find((button) => button.text() === 'Logout')
    expect(logoutBtn).toBeTruthy()
    await logoutBtn?.trigger('click')
    await flushPromises()
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/login')

    const logoutCall = fetchMock.mock.calls.find(([url]) => String(url) === '/api/v1/console/logout')
    expect(logoutCall).toBeTruthy()

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
      if (url === '/api/v1/console/tokens') {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(trustedTokensPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    forceUnauthorized = true
    const refreshBtn = wrapper.findAll('button').find((button) => button.text() === 'Refresh Now')
    expect(refreshBtn).toBeTruthy()
    await refreshBtn?.trigger('click')
    await flushPromises()
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/login')

    wrapper.unmount()
  })

  it('removes per-worker startup command action', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    const copyBtn = wrapper.findAll('button').find((button) => button.text() === 'Copy Start Cmd')
    expect(copyBtn).toBeUndefined()
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/v1/workers/node-1/startup-command')).toBe(false)

    wrapper.unmount()
  })

  it('adds worker and shows modal before manual copy', async () => {
    const writeText = vi.fn(async () => {})
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    const createCommand =
      'WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=node-2 WORKER_SECRET=secret-2 WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 go run ./cmd/worker-docker'

    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      if (url === '/api/v1/workers') {
        return jsonResponse({ node_id: 'node-2', command: createCommand })
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    const addBtn = wrapper.findAll('button').find((button) => button.text() === 'Add Worker')
    expect(addBtn).toBeTruthy()
    await addBtn?.trigger('click')
    await flushPromises()
    await flushPromises()

    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/v1/workers')).toBe(true)
    expect(writeText).not.toHaveBeenCalled()
    expect(wrapper.find('.worker-modal').exists()).toBe(true)
    expect(wrapper.text()).toContain('Worker Created')

    const secretCodes = wrapper.findAll('.worker-meta code')
    expect(secretCodes.length).toBeGreaterThanOrEqual(2)
    expect(secretCodes[1]?.text()).toBe('********')

    const showBtn = wrapper.findAll('button').find((button) => button.text() === 'Show')
    expect(showBtn).toBeTruthy()
    await showBtn?.trigger('click')
    await flushPromises()

    const secretCodesAfterShow = wrapper.findAll('.worker-meta code')
    expect(secretCodesAfterShow[1]?.text()).toBe('secret-2')

    const copyBtn = wrapper.findAll('button').find((button) => button.text() === 'Copy Startup Command')
    expect(copyBtn).toBeTruthy()
    await copyBtn?.trigger('click')
    await flushPromises()

    expect(writeText).toHaveBeenCalledWith(createCommand)

    const doneBtn = wrapper.findAll('button').find((button) => button.text() === 'Done')
    expect(doneBtn).toBeTruthy()
    await doneBtn?.trigger('click')
    await flushPromises()

    expect(wrapper.find('.worker-modal').exists()).toBe(false)

    wrapper.unmount()
  })

  it('retries copy after modal copy failure', async () => {
    const writeText = vi.fn(async () => {})
    writeText.mockRejectedValueOnce(new Error('Clipboard API unavailable.'))
    writeText.mockResolvedValueOnce(undefined)
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    const createCommand =
      'WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=node-2 WORKER_SECRET=secret-2 WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 go run ./cmd/worker-docker'

    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      if (url === '/api/v1/workers') {
        return jsonResponse({ node_id: 'node-2', command: createCommand })
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    const addBtn = wrapper.findAll('button').find((button) => button.text() === 'Add Worker')
    expect(addBtn).toBeTruthy()
    await addBtn?.trigger('click')
    await flushPromises()
    await flushPromises()

    const copyBtn = wrapper.findAll('button').find((button) => button.text() === 'Copy Startup Command')
    expect(copyBtn).toBeTruthy()
    await copyBtn?.trigger('click')
    await flushPromises()

    expect(writeText).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('Copy Failed')

    const retryBtn = wrapper.findAll('button').find((button) => button.text() === 'Copy Failed')
    expect(retryBtn).toBeTruthy()
    await retryBtn?.trigger('click')
    await flushPromises()

    expect(writeText).toHaveBeenCalledTimes(2)
    expect(writeText).toHaveBeenLastCalledWith(createCommand)
    expect(wrapper.text()).toContain('Copied')

    wrapper.unmount()
  })

  it('deletes worker and refreshes list', async () => {
    vi.stubGlobal('confirm', vi.fn(() => true))

    let deleted = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return deleted
          ? jsonResponse({ items: [], total: 0, page: 1, page_size: 25 })
          : jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      if (url === '/api/v1/workers/node-1' && init?.method === 'DELETE') {
        deleted = true
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    const workerRow = wrapper.find('tbody tr')
    const deleteBtn = workerRow.findAll('button').find((button) => button.text() === 'Delete')
    expect(deleteBtn).toBeTruthy()
    await deleteBtn?.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('No workers found in current filter.')

    const deleteCall = fetchMock.mock.calls.find(
      ([url, init]) => String(url) === '/api/v1/workers/node-1' && (init as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCall).toBeTruthy()

    wrapper.unmount()
  })

  it('syncs status/page from query and back to URL', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(trustedTokensPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers?status=online&page=2')

    const workersCall = fetchMock.mock.calls.find(([url]) => String(url).startsWith('/api/v1/workers?status=online&page=2'))
    expect(workersCall).toBeTruthy()

    const allTab = wrapper.findAll('button').find((button) => button.text() === 'All')
    expect(allTab).toBeTruthy()
    await allTab?.trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(router.currentRoute.value.query.status).toBeUndefined()

    wrapper.unmount()
  })
})
