import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises } from '@vue/test-utils'

import router from '../router'
import {
  defaultTokensPayload,
  jsonResponse,
  memberSessionPayload,
  mountApp,
  noContentResponse,
  unauthorizedResponse,
  waitForRoute,
} from './testkit'

describe('Tokens Page', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('supports token CRUD on /tokens page', async () => {
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const writeText = vi.fn(async (_text: string) => {})
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    let tokens = defaultTokensPayload().items
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/console/tokens' && method === 'GET') {
        return jsonResponse({ items: tokens, total: tokens.length })
      }
      if (url === '/api/v1/console/tokens' && method === 'POST') {
        tokens = [
          ...tokens,
          {
            id: 'tok-2',
            name: 'ci-staging',
            token_masked: 'obx_****new2',
            created_at: '2026-02-16T10:01:00Z',
            updated_at: '2026-02-16T10:01:00Z',
          },
        ]
        return jsonResponse({
          id: 'tok-2',
          name: 'ci-staging',
          token: 'obx_plaintext_once',
          token_masked: 'obx_****new2',
          generated: true,
          created_at: '2026-02-16T10:01:00Z',
          updated_at: '2026-02-16T10:01:00Z',
        })
      }
      if (url === '/api/v1/console/tokens/tok-1' && method === 'DELETE') {
        tokens = tokens.filter((item) => item.id !== 'tok-1')
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}, method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/tokens')
    try {
      await waitForRoute('/tokens', 30)
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

      expect(wrapper.text()).toContain('obx_plaintext_once')
      expect(wrapper.findAll('.token-usage-label').map((node) => node.text())).toEqual([
        'claude code',
        'http header',
        'mcp json',
      ])

      const claudeUsageItem = wrapper
        .findAll('.token-usage-item')
        .find((node) => node.text().includes('claude code'))
      expect(claudeUsageItem).toBeTruthy()

      const claudeUsageToggle = claudeUsageItem?.find('.token-usage-trigger')
      expect(claudeUsageToggle?.exists()).toBe(true)
      expect(claudeUsageToggle?.text()).toContain('Expand')
      await claudeUsageToggle?.trigger('click')
      await flushPromises()

      expect(claudeUsageToggle?.text()).toContain('Collapse')
      const claudeCopyButton = claudeUsageItem
        ?.findAll('button')
        .find((button) => button.text() === 'Copy')
      expect(claudeCopyButton).toBeTruthy()
      await claudeCopyButton?.trigger('click')
      await flushPromises()
      expect(writeText).toHaveBeenCalledTimes(1)
      expect(String(writeText.mock.calls[0]?.[0] ?? '')).toContain(
        'claude mcp add --transport http onlyboxes',
      )

      const doneBtn = wrapper.findAll('button').find((button) => button.text() === 'Done')
      expect(doneBtn).toBeTruthy()
      await doneBtn?.trigger('click')
      await flushPromises()

      const expandBtn = wrapper.findAll('button').find((button) => button.text() === 'Expand')
      expect(expandBtn).toBeTruthy()
      await expandBtn?.trigger('click')
      await flushPromises()

      expect(wrapper.text()).toContain('ci-staging')
      expect(wrapper.text()).toContain('ci-prod')

      const deleteBtn = wrapper.find('.token-panel .token-actions button')
      expect(deleteBtn.exists()).toBe(true)
      await deleteBtn.trigger('click')
      await flushPromises()

      expect(wrapper.text()).not.toContain('ci-prod')
      expect(wrapper.text()).toContain('ci-staging')
    } finally {
      wrapper.unmount()
    }
  })

  it('returns to login when tokens refresh receives 401', async () => {
    let forceUnauthorized = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(defaultTokensPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/tokens')
    try {
      await waitForRoute('/tokens', 30)
      forceUnauthorized = true
      const refreshBtn = wrapper.findAll('button').find((button) => button.text() === 'Refresh')
      expect(refreshBtn).toBeTruthy()
      await refreshBtn?.trigger('click')
      await flushPromises()
      await flushPromises()
      await waitForRoute('/login', 40)

      expect(router.currentRoute.value.path).toBe('/login')
    } finally {
      wrapper.unmount()
    }
  })

  it('logs out from tokens page and returns to login', async () => {
    let authenticated = true
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return authenticated ? jsonResponse(memberSessionPayload) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/tokens') {
        return authenticated ? jsonResponse(defaultTokensPayload()) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/logout') {
        authenticated = false
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/tokens')
    try {
      await waitForRoute('/tokens', 30)
      const logoutBtn = wrapper.findAll('button').find((button) => button.text() === 'Logout')
      expect(logoutBtn).toBeTruthy()
      await logoutBtn?.trigger('click')
      await flushPromises()
      await flushPromises()
      await waitForRoute('/login', 40)

      expect(router.currentRoute.value.path).toBe('/login')
      expect(fetchMock.mock.calls.some(([url]) => String(url) === '/api/v1/console/logout')).toBe(
        true,
      )
    } finally {
      wrapper.unmount()
    }
  })
})
