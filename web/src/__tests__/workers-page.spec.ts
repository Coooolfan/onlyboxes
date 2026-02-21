import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises } from '@vue/test-utils'

import router from '../router'
import {
  adminSessionPayload,
  defaultTokensPayload,
  inflightPayload,
  jsonResponse,
  memberSessionPayload,
  mountApp,
  noContentResponse,
  statsPayload,
  unauthorizedResponse,
  waitForRoute,
  workersPayload,
} from './testkit'

describe('Workers Page', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('logs out and returns to login panel', async () => {
    let authenticated = true
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return authenticated ? jsonResponse(adminSessionPayload) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/logout') {
        authenticated = false
        return noContentResponse()
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return authenticated ? jsonResponse(statsPayload) : unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return authenticated ? jsonResponse(inflightPayload) : unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/workers?')) {
        return authenticated ? jsonResponse(workersPayload) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/tokens') {
        return authenticated ? jsonResponse(defaultTokensPayload()) : unauthorizedResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    const logoutBtn = wrapper.findAll('button').find((button) => button.text() === 'Logout')
    expect(logoutBtn).toBeTruthy()
    await logoutBtn?.trigger('click')
    await flushPromises()
    await flushPromises()
    await waitForRoute('/login')

    expect(router.currentRoute.value.path).toBe('/login')
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/v1/console/logout')).toBe(
      true,
    )

    wrapper.unmount()
  })

  it('returns to login when refresh receives 401', async () => {
    let forceUnauthorized = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(defaultTokensPayload())
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
    await waitForRoute('/login')

    expect(router.currentRoute.value.path).toBe('/login')

    wrapper.unmount()
  })

  it('adds worker and shows modal before manual copy', async () => {
    const writeText = vi.fn(async () => {})
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    const createCommand =
      'WORKER_CONSOLE_GRPC_TARGET=127.0.0.1:50051 WORKER_ID=node-2 WORKER_SECRET=secret-2 WORKER_HEARTBEAT_INTERVAL_SEC=5 WORKER_HEARTBEAT_JITTER_PCT=20 ./path-to-binary'

    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url === '/api/v1/workers' && method === 'POST') {
        return jsonResponse({ node_id: 'node-2', command: createCommand })
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
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

    wrapper.unmount()
  })

  it('deletes worker and refreshes list', async () => {
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )

    let deleted = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return deleted
          ? jsonResponse({ items: [], total: 0, page: 1, page_size: 25 })
          : jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url === '/api/v1/workers/node-1' && method === 'DELETE') {
        deleted = true
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
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
      ([url, init]) =>
        String(url) === '/api/v1/workers/node-1' &&
        String((init as RequestInit | undefined)?.method).toUpperCase() === 'DELETE',
    )
    expect(deleteCall).toBeTruthy()

    wrapper.unmount()
  })

  it('matches capability inflight status with case-insensitive names', async () => {
    const workersWithMixedCaseCaps = {
      ...workersPayload,
      items: [
        {
          ...workersPayload.items[0],
          capabilities: [
            { name: 'echo' },
            { name: 'pythonExec' },
            { name: 'terminalExec' },
            { name: 'terminalResource' },
          ],
        },
      ],
    }

    const inflightWithNormalizedCaps = {
      ...inflightPayload,
      workers: [
        {
          node_id: 'node-1',
          capabilities: [
            { name: 'echo', inflight: 1, max_inflight: 4 },
            { name: 'pythonexec', inflight: 0, max_inflight: 4 },
            { name: 'terminalexec', inflight: 2, max_inflight: 4 },
            { name: 'terminalresource', inflight: 3, max_inflight: 6 },
          ],
        },
      ],
    }

    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightWithNormalizedCaps)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersWithMixedCaseCaps)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      const badgeTexts = wrapper.findAll('.capability-badge').map((item) => item.text())

      expect(
        badgeTexts.some((text) => text.includes('echo') && text.includes('1/4')),
      ).toBeTruthy()
      expect(
        badgeTexts.some((text) => text.includes('pythonExec') && text.includes('0/4')),
      ).toBeTruthy()
      expect(
        badgeTexts.some((text) => text.includes('terminalExec') && text.includes('2/4')),
      ).toBeTruthy()
      expect(
        badgeTexts.some((text) => text.includes('terminalResource') && text.includes('3/6')),
      ).toBeTruthy()
    } finally {
      wrapper.unmount()
    }
  })

  it('syncs status/page from query and back to URL', async () => {
    const pagedWorkersPayload = {
      ...workersPayload,
      total: 100,
      page: 2,
    }

    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?status=online&page=2')) {
        return jsonResponse(pagedWorkersPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers?status=online&page=2')

    const workersCall = fetchMock.mock.calls.find(([url]) =>
      String(url).startsWith('/api/v1/workers?status=online&page=2'),
    )
    expect(workersCall).toBeTruthy()

    const allTab = wrapper.findAll('button').find((button) => button.text() === 'All')
    expect(allTab).toBeTruthy()
    await allTab?.trigger('click')
    await flushPromises()

    expect(router.currentRoute.value.path).toBe('/workers')
    expect(router.currentRoute.value.query.status).toBeUndefined()
    expect(router.currentRoute.value.query.page).toBeUndefined()

    wrapper.unmount()
  })

  it('shows create-account panel when registration is enabled and submits form', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url === '/api/v1/console/register' && method === 'POST') {
        return jsonResponse({
          account: {
            account_id: 'acc-member-new',
            username: 'member-new',
            is_admin: false,
          },
          created_at: '2026-02-20T00:00:00Z',
          updated_at: '2026-02-20T00:00:00Z',
        })
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      const openCreateAccountBtn = wrapper
        .findAll('button')
        .find(
          (button) => button.text() === 'Create Account' && button.classes().includes('ghost-btn'),
        )
      expect(openCreateAccountBtn).toBeTruthy()
      await openCreateAccountBtn?.trigger('click')
      await flushPromises()

      const createAccountModal = wrapper.find('.account-modal')
      expect(createAccountModal.exists()).toBe(true)

      const createAccountNameInput = createAccountModal.find('input[type="text"]')
      const createAccountPasswordInput = createAccountModal.find('input[type="password"]')
      expect(createAccountNameInput.exists()).toBe(true)
      expect(createAccountPasswordInput.exists()).toBe(true)
      await createAccountNameInput.setValue('member-new')
      await createAccountPasswordInput.setValue('member-pass')
      await createAccountModal.get('form.account-form').trigger('submit.prevent')
      await flushPromises()

      expect(wrapper.text()).toContain('Created account member-new')
      expect((createAccountNameInput.element as HTMLInputElement).value).toBe('')
      expect((createAccountPasswordInput.element as HTMLInputElement).value).toBe('')
      const registerCall = fetchMock.mock.calls.find(
        ([url, init]) =>
          String(url) === '/api/v1/console/register' &&
          String((init as RequestInit | undefined)?.method).toUpperCase() === 'POST',
      )
      expect(registerCall).toBeTruthy()
    } finally {
      wrapper.unmount()
    }
  })

  it('hides create-account panel when registration is disabled', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse({
          ...adminSessionPayload,
          registration_enabled: false,
        })
      }
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers/inflight')) {
        return jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    expect(wrapper.find('#create-account-username').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Create Account')

    wrapper.unmount()
  })

  it('redirects non-admin /workers access to /tokens', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url.startsWith('/api/v1/workers')) {
        throw new Error(`workers api should not be called for non-admin: ${url}`)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      await waitForRoute('/tokens')
      expect(router.currentRoute.value.path).toBe('/tokens')
      expect(wrapper.text()).toContain('Trusted Token Management')
    } finally {
      wrapper.unmount()
    }
  })
})
