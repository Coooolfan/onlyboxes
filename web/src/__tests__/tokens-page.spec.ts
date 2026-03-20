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

      const nameInput = document.body.querySelector<HTMLInputElement>('.token-modal input')
      expect(nameInput).toBeTruthy()
      nameInput!.value = 'ci-staging'
      nameInput!.dispatchEvent(new window.Event('input', { bubbles: true }))
      nameInput!.dispatchEvent(new window.Event('change', { bubbles: true }))
      await flushPromises()

      const createTokenBtn = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Create Token',
      )
      expect(createTokenBtn).toBeTruthy()
      expect((createTokenBtn as HTMLButtonElement | undefined)?.disabled).toBe(false)
      createTokenBtn?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()
      await flushPromises()

      expect(document.body.textContent ?? '').toContain('obx_plaintext_once')
      expect(document.body.textContent ?? '').toContain('claude code')
      expect(document.body.textContent ?? '').toContain('http header')
      expect(document.body.textContent ?? '').toContain('mcp json')

      const httpHeaderTab = Array.from(document.body.querySelectorAll('button')).find((button) =>
        (button.textContent ?? '').includes('http header'),
      )
      expect(httpHeaderTab).toBeTruthy()
      httpHeaderTab?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()
      const httpHeaderValue = document.body.querySelector('.token-usage-value')?.textContent ?? ''
      expect(httpHeaderValue).toContain('Authorization: Bearer obx_plaintext_once')
      expect(httpHeaderValue).not.toContain('X-Onlyboxes-Token')

      const mcpJSONTab = Array.from(document.body.querySelectorAll('button')).find((button) =>
        (button.textContent ?? '').includes('mcp json'),
      )
      expect(mcpJSONTab).toBeTruthy()
      mcpJSONTab?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()
      const mcpJSONValue = document.body.querySelector('.token-usage-value')?.textContent ?? ''
      expect(mcpJSONValue).toContain('"Authorization": "Bearer obx_plaintext_once"')
      expect(mcpJSONValue).not.toContain('X-Onlyboxes-Token')

      const claudeCodeTab = Array.from(document.body.querySelectorAll('button')).find((button) =>
        (button.textContent ?? '').includes('claude code'),
      )
      expect(claudeCodeTab).toBeTruthy()
      claudeCodeTab?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()

      const claudeCopyButton = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Copy',
      )
      expect(claudeCopyButton).toBeTruthy()
      claudeCopyButton?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await flushPromises()
      expect(writeText).toHaveBeenCalledTimes(1)
      const copiedClaudeCommand = String(writeText.mock.calls[0]?.[0] ?? '')
      expect(copiedClaudeCommand).toContain('claude mcp add --transport http onlyboxes')
      expect(copiedClaudeCommand).toContain('Authorization: Bearer obx_plaintext_once')
      expect(copiedClaudeCommand).not.toContain('X-Onlyboxes-Token')

      const doneBtn = Array.from(document.body.querySelectorAll('button')).find(
        (button) => (button.textContent ?? '').trim() === 'Done',
      )
      expect(doneBtn).toBeTruthy()
      doneBtn?.dispatchEvent(new MouseEvent('click', { bubbles: true }))
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
      await wrapper.get('header.h-16 .relative > button').trigger('click')
      await flushPromises()
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

  it('changes password from tokens page modal', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (url === '/api/v1/console/tokens') {
        return jsonResponse(defaultTokensPayload())
      }
      if (url === '/api/v1/console/password' && method === 'POST') {
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
      currentInput!.value = 'member-pass'
      currentInput!.dispatchEvent(new Event('input', { bubbles: true }))
      newInput!.value = 'member-pass-next'
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
        current_password: 'member-pass',
        new_password: 'member-pass-next',
      })
    } finally {
      wrapper.unmount()
    }
  })
})
