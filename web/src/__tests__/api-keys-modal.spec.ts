import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises } from '@vue/test-utils'

import router from '../router'
import {
  confirmDestructiveAction,
  defaultAPIKeysPayload,
  defaultTokensPayload,
  jsonResponse,
  memberSessionPayload,
  mountApp,
  noContentResponse,
  unauthorizedResponse,
  waitForRoute,
} from './testkit'
import { useAPIKeysStore } from '@/stores/apiKeys'

describe('API Keys Modal', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('supports api key create, copy, delete, and logout reset', async () => {
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )
    const writeText = vi.fn(async (_text: string) => {})
    Object.defineProperty(window.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    })

    let authenticated = true
    let apiKeys = defaultAPIKeysPayload().items
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return authenticated ? jsonResponse(memberSessionPayload) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/tokens') {
        return authenticated ? jsonResponse(defaultTokensPayload()) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/api-keys' && method === 'GET') {
        return authenticated ? jsonResponse({ items: apiKeys, total: apiKeys.length }) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/api-keys' && method === 'POST') {
        apiKeys = [
          ...apiKeys,
          {
            id: 'apik-2',
            name: 'ci-staging',
            key_masked: 'obxk****new2',
            created_at: '2026-02-16T10:01:00Z',
            updated_at: '2026-02-16T10:01:00Z',
          },
        ]
        return jsonResponse({
          id: 'apik-2',
          name: 'ci-staging',
          key: 'obxk_plaintext_once',
          key_masked: 'obxk****new2',
          created_at: '2026-02-16T10:01:00Z',
          updated_at: '2026-02-16T10:01:00Z',
        })
      }
      if (url === '/api/v1/console/api-keys/apik-1' && method === 'DELETE') {
        apiKeys = apiKeys.filter((item) => item.id !== 'apik-1')
        return noContentResponse()
      }
      if (url === '/api/v1/console/logout') {
        authenticated = false
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}, method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/tokens')
    try {
      await waitForRoute('/tokens', 30)

      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
      const apiKeysButton = wrapper.findAll('button').find((button) => button.text() === 'API Keys')
      expect(apiKeysButton).toBeTruthy()
      await apiKeysButton?.trigger('click')
      await flushPromises()
      await flushPromises()

      expect(document.body.textContent ?? '').toContain('ci-prod')

      const newKeyButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'New API Key',
      )
      expect(newKeyButton).toBeTruthy()
      newKeyButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()

      const nameInput = document.body.querySelector<HTMLInputElement>('.api-keys-modal input')
      expect(nameInput).toBeTruthy()
      nameInput!.value = 'ci-staging'
      nameInput!.dispatchEvent(new window.Event('input', { bubbles: true }))
      nameInput!.dispatchEvent(new window.Event('change', { bubbles: true }))
      await flushPromises()

      const createButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Create API Key',
      )
      expect(createButton).toBeTruthy()
      createButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()
      await flushPromises()

      expect(document.body.textContent ?? '').toContain('obxk_plaintext_once')

      const copyButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Copy Key',
      )
      expect(copyButton).toBeTruthy()
      copyButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()
      expect(writeText).toHaveBeenCalledWith('obxk_plaintext_once')

      const closeButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Close',
      )
      expect(closeButton).toBeTruthy()
      closeButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()

      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
      const apiKeysButtonAgain = wrapper.findAll('button').find((button) => button.text() === 'API Keys')
      await apiKeysButtonAgain?.trigger('click')
      await flushPromises()
      await flushPromises()

      expect(document.body.textContent ?? '').not.toContain('obxk_plaintext_once')
      expect(document.body.textContent ?? '').toContain('ci-staging')

      const deleteButton = document.body.querySelector('.api-key-actions button')
      expect(deleteButton).toBeTruthy()
      deleteButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await confirmDestructiveAction()

      expect(document.body.textContent ?? '').not.toContain('ci-prod')

      const modalCloseButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Close',
      )
      modalCloseButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()

      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
      const logoutButton = wrapper.findAll('button').find((button) => button.text() === 'Logout')
      expect(logoutButton).toBeTruthy()
      await logoutButton?.trigger('click')
      await flushPromises()
      await flushPromises()
      await waitForRoute('/login', 40)

      expect(router.currentRoute.value.path).toBe('/login')
      expect(useAPIKeysStore().apiKeys).toEqual([])
    } finally {
      wrapper.unmount()
    }
  })

  it('redirects to login when api key list returns 401', async () => {
    let forceUnauthorized = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url === '/api/v1/console/api-keys' && method === 'GET') {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(defaultAPIKeysPayload())
      }
      throw new Error(`unexpected url: ${url}, method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/tokens')
    try {
      await waitForRoute('/tokens', 30)

      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
      const apiKeysButton = wrapper.findAll('button').find((button) => button.text() === 'API Keys')
      await apiKeysButton?.trigger('click')
      await flushPromises()
      await flushPromises()

      const closeButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Close',
      )
      closeButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()

      forceUnauthorized = true
      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
      const apiKeysButtonAgain = wrapper.findAll('button').find((button) => button.text() === 'API Keys')
      await apiKeysButtonAgain?.trigger('click')
      await flushPromises()
      await flushPromises()
      await waitForRoute('/login', 40)

      expect(router.currentRoute.value.path).toBe('/login')
    } finally {
      wrapper.unmount()
    }
  })
})
