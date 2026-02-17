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

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('App', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('renders stats cards from /workers/stats', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        return jsonResponse(statsPayload)
      }
      if (url.startsWith('/api/v1/workers?')) {
        return jsonResponse(workersPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = mount(App)
    await flushPromises()

    expect(wrapper.text()).toContain('Execution Node Control Panel')
    expect(wrapper.text()).toContain('150')
    expect(wrapper.text()).toContain('120')
    expect(wrapper.text()).toContain('30')
    expect(wrapper.text()).toContain('10')
    expect(wrapper.text()).toContain('Heartbeat > 45s')
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/api/v1/workers/stats?stale_after_sec=30'),
      expect.objectContaining({ signal: expect.anything() }),
    )
    wrapper.unmount()
  })

  it('keeps latest filter results when prior request resolves later', async () => {
    const firstStatsReq = deferred<ReturnType<typeof jsonResponse>>()
    const firstListReq = deferred<ReturnType<typeof jsonResponse>>()

    const lateAllStats = {
      total: 2,
      online: 1,
      offline: 1,
      stale: 1,
      stale_after_sec: 30,
      generated_at: '2026-02-16T10:00:00Z',
    }

    const lateAllList = {
      items: [
        {
          node_id: 'node-all-late',
          node_name: 'all-late',
          executor_kind: 'docker',
          capabilities: [],
          labels: {},
          version: 'v0.1.0',
          status: 'offline',
          registered_at: '2026-02-16T09:00:00Z',
          last_seen_at: '2026-02-16T09:30:00Z',
        },
      ],
      total: 1,
      page: 1,
      page_size: 25,
    }

    const latestStats = {
      total: 1,
      online: 1,
      offline: 0,
      stale: 0,
      stale_after_sec: 30,
      generated_at: '2026-02-16T10:00:20Z',
    }

    const latestOnlineList = {
      items: [
        {
          node_id: 'node-online-latest',
          node_name: 'online-latest',
          executor_kind: 'docker',
          capabilities: [],
          labels: {},
          version: 'v0.1.0',
          status: 'online',
          registered_at: '2026-02-16T10:00:00Z',
          last_seen_at: '2026-02-16T10:00:19Z',
        },
      ],
      total: 1,
      page: 1,
      page_size: 25,
    }

    let statsCalls = 0
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (url.startsWith('/api/v1/workers/stats')) {
        statsCalls++
        if (statsCalls === 1) {
          return firstStatsReq.promise
        }
        return Promise.resolve(jsonResponse(latestStats))
      }
      if (url.includes('status=all')) {
        return firstListReq.promise
      }
      if (url.includes('status=online')) {
        return Promise.resolve(jsonResponse(latestOnlineList))
      }
      if (url.includes('status=offline')) {
        return Promise.resolve(jsonResponse({ ...latestOnlineList, items: [] }))
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = mount(App)
    await Promise.resolve()

    const tabButtons = wrapper.findAll('button.tab-btn')
    expect(tabButtons.length).toBeGreaterThan(1)
    await tabButtons[1]!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('online-latest')
    expect(wrapper.text()).not.toContain('all-late')

    firstStatsReq.resolve(jsonResponse(lateAllStats))
    firstListReq.resolve(jsonResponse(lateAllList))
    await flushPromises()

    expect(wrapper.text()).toContain('online-latest')
    expect(wrapper.text()).not.toContain('all-late')
    wrapper.unmount()
  })
})
