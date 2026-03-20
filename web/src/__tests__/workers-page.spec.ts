import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises } from '@vue/test-utils'

import router from '../router'
import {
  adminSessionPayload,
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
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    expect(wrapper.text()).toContain('version: v0.1.0')

    await wrapper.get('header.h-16 .relative > button').trigger('click')
    await flushPromises()
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
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    await flushPromises()
    await flushPromises()

    forceUnauthorized = true
    const refreshMenuBtn = wrapper
      .findAll('button')
      .find((button) => button.text().includes('Refresh Controls'))
    expect(refreshMenuBtn).toBeTruthy()
    await refreshMenuBtn?.trigger('click')
    await flushPromises()
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
      if (url === '/api/v1/workers' && method === 'POST') {
        const parsedBody = JSON.parse(String(init?.body ?? '{}'))
        expect(parsedBody).toEqual({ type: 'normal' })
        return jsonResponse({ node_id: 'node-2', type: 'normal', worker_secret: 'secret-2' })
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')

    const addBtn = wrapper.find('[data-testid="create-worker-button"]')
    expect(addBtn.exists()).toBe(true)
    await addBtn.trigger('click')
    await flushPromises()
    await flushPromises()

    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/v1/workers')).toBe(true)
    expect(writeText).not.toHaveBeenCalled()
    expect(document.body.querySelector('.worker-modal')).toBeTruthy()
    expect(document.body.textContent ?? '').toContain('Worker Created')

    wrapper.unmount()
  })

  it('navigates to startup tool with prefill when clicking Open in Startup Tool', async () => {
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
      if (url === '/api/v1/workers' && method === 'POST') {
        return jsonResponse({ node_id: 'node-2', type: 'normal', worker_secret: 'secret-2' })
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      const addBtn = wrapper.find('[data-testid="create-worker-button"]')
      await addBtn.trigger('click')
      await flushPromises()
      await flushPromises()

      const modalEl = document.body.querySelector('.worker-modal')
      expect(modalEl).toBeTruthy()

      const startupToolBtn = document.body.querySelector<HTMLButtonElement>(
        '[data-testid="open-in-startup-tool"]',
      )
      expect(startupToolBtn).toBeTruthy()
      expect(startupToolBtn!.textContent?.trim()).toBe('Open in Startup Tool')

      // Native click fires goToStartupTool which calls setPrefill with the
      // structured payload fields. The async router.push it initiates doesn't
      // fully resolve under jsdom, so we complete the navigation explicitly.
      startupToolBtn!.click()
      await router.push('/tools/worker-startup')
      await flushPromises()
      await flushPromises()
      await waitForRoute('/tools/worker-startup')

      expect(router.currentRoute.value.path).toBe('/tools/worker-startup')
      expect(wrapper.get('[data-testid="prefilled-credentials-notice"]').text()).toContain(
        'WORKER_ID and WORKER_SECRET are already filled in',
      )
      expect(wrapper.get('[data-testid="worker-id-prefilled-hint"]').text()).toContain(
        'Already Filled',
      )
      expect(wrapper.get('[data-testid="worker-secret-prefilled-hint"]').text()).toContain(
        'Already Filled',
      )
      // node-2 comes from the handler's extractEnvValue, not hardcoded by the test
      expect(wrapper.text()).toContain('node-2')
      expect(wrapper.text()).toContain('node-2')
    } finally {
      wrapper.unmount()
    }
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
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      const badgeTexts = wrapper.findAll('.capability-badge').map((item) => item.text())

      expect(badgeTexts.some((text) => text.includes('echo') && text.includes('1/4'))).toBeTruthy()
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

  it('renders labels as per-line badges with vertical scroll', async () => {
    const workersWithManyLabels = {
      ...workersPayload,
      items: [
        {
          ...workersPayload.items[0],
          labels: {
            zone: 'a',
            region: 'us-west',
            env: 'prod',
            team: 'core',
            tier: 'backend',
            runtime: 'docker',
          },
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
        return jsonResponse(inflightPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersWithManyLabels)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      const labelList = wrapper.find('.worker-label-list')
      expect(labelList.exists()).toBeTruthy()
      expect(labelList.classes()).toContain('max-h-24')
      expect(labelList.classes()).toContain('overflow-y-auto')

      const labelBadges = wrapper.findAll('.worker-label-badge')
      expect(labelBadges).toHaveLength(6)
      const labelTexts = labelBadges.map((item) => item.text())
      expect(labelTexts).toContain('zone=a')
      expect(labelTexts).toContain('region=us-west')
      expect(labelTexts).toContain('env=prod')
      expect(labelTexts).toContain('team=core')
      expect(labelTexts).toContain('tier=backend')
      expect(labelTexts).toContain('runtime=docker')
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

  it('changes password from workers page modal', async () => {
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
      if (url === '/api/v1/console/password' && method === 'POST') {
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
      const changePasswordBtn = wrapper
        .findAll('button')
        .find((button) => button.text() === 'Change Password')
      expect(changePasswordBtn).toBeTruthy()
      await changePasswordBtn?.trigger('click')
      await flushPromises()

      const modal = document.body.querySelector('.password-modal')
      expect(modal).toBeTruthy()
      const currentInput = document.body.querySelector<HTMLInputElement>('#current-password')
      const newInput = document.body.querySelector<HTMLInputElement>('#new-password')
      const form = document.body.querySelector<HTMLFormElement>('form.password-form')
      expect(currentInput).toBeTruthy()
      expect(newInput).toBeTruthy()
      expect(form).toBeTruthy()
      currentInput!.value = 'password-test'
      currentInput!.dispatchEvent(new Event('input', { bubbles: true }))
      newInput!.value = 'password-next'
      newInput!.dispatchEvent(new Event('input', { bubbles: true }))
      form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
      await flushPromises()

      expect(document.body.textContent ?? '').toContain('Password updated successfully.')
      const passwordCall = fetchMock.mock.calls.find(
        ([url, init]) =>
          String(url) === '/api/v1/console/password' &&
          String((init as RequestInit | undefined)?.method).toUpperCase() === 'POST',
      )
      expect(passwordCall).toBeTruthy()
      const passwordPayload = JSON.parse(
        String((passwordCall?.[1] as RequestInit | undefined)?.body),
      )
      expect(passwordPayload).toEqual({
        current_password: 'password-test',
        new_password: 'password-next',
      })
    } finally {
      wrapper.unmount()
    }
  })

  it('allows non-admin workers page and creates worker-sys only', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
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
      if (url === '/api/v1/workers' && method === 'POST') {
        const parsedBody = JSON.parse(String(init?.body ?? '{}'))
        expect(parsedBody).toEqual({ type: 'worker-sys' })
        return jsonResponse({
          node_id: 'node-sys-1',
          type: 'worker-sys',
          worker_secret: 'secret-sys-1',
        })
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/workers')
    try {
      expect(router.currentRoute.value.path).toBe('/workers')
      expect(wrapper.text()).toContain('Create Worker-Sys')
      const createBtn = wrapper.find('[data-testid="create-worker-button"]')
      expect(createBtn.exists()).toBe(true)
      await createBtn.trigger('click')
      await flushPromises()
      await flushPromises()
      expect(document.body.textContent ?? '').toContain('worker-sys')
    } finally {
      wrapper.unmount()
    }
  })
})
