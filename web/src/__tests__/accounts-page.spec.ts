import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises } from '@vue/test-utils'

import router from '../router'
import {
  adminSessionPayload,
  defaultAccountsPayload,
  jsonResponse,
  mountApp,
  noContentResponse,
  unauthorizedResponse,
  waitForRoute,
} from './testkit'

describe('Accounts Page', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('loads accounts and deletes a member account', async () => {
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true),
    )

    let accountItems = defaultAccountsPayload().items
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/console/accounts?')) {
        return jsonResponse({
          items: accountItems,
          total: accountItems.length,
          page: 1,
          page_size: 20,
        })
      }
      if (url === '/api/v1/console/accounts/acc-member' && method === 'DELETE') {
        accountItems = accountItems.filter((item) => item.account_id !== 'acc-member')
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url} method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')
    try {
      expect(wrapper.find('.account-panel').exists()).toBe(true)
      expect(wrapper.text()).toContain('member-test')

      const accountDeleteBtn = wrapper.find('.account-panel .account-delete-btn')
      expect(accountDeleteBtn.exists()).toBe(true)
      await accountDeleteBtn.trigger('click')
      await flushPromises()
      await flushPromises()

      const deleteCall = fetchMock.mock.calls.find(
        ([url, init]) =>
          String(url) === '/api/v1/console/accounts/acc-member' &&
          String((init as RequestInit | undefined)?.method).toUpperCase() === 'DELETE',
      )
      expect(deleteCall).toBeTruthy()
      expect(wrapper.text()).not.toContain('member-test')
    } finally {
      wrapper.unmount()
    }
  })

  it('shows create-account panel when registration is enabled and submits form', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/console/accounts?')) {
        return jsonResponse(defaultAccountsPayload())
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

    const wrapper = await mountApp('/accounts')
    try {
      const openCreateAccountBtn = wrapper
        .findAll('button')
        .find((button) => button.text() === 'Create Account')
      expect(openCreateAccountBtn).toBeTruthy()
      await openCreateAccountBtn?.trigger('click')
      await flushPromises()

      const createAccountModal = document.body.querySelector('.account-modal')
      expect(createAccountModal).toBeTruthy()

      const createAccountNameInput = document.body.querySelector<HTMLInputElement>(
        '.account-modal input[type="text"]',
      )
      const createAccountPasswordInput = document.body.querySelector<HTMLInputElement>(
        '.account-modal input[type="password"]',
      )
      const createAccountForm = document.body.querySelector<HTMLFormElement>(
        '.account-modal form.account-form',
      )
      expect(createAccountNameInput).toBeTruthy()
      expect(createAccountPasswordInput).toBeTruthy()
      expect(createAccountForm).toBeTruthy()
      createAccountNameInput!.value = 'member-new'
      createAccountNameInput!.dispatchEvent(new Event('input', { bubbles: true }))
      createAccountPasswordInput!.value = 'member-pass'
      createAccountPasswordInput!.dispatchEvent(new Event('input', { bubbles: true }))
      createAccountForm!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
      await flushPromises()

      expect(document.body.textContent ?? '').toContain('Created account member-new')
      expect(createAccountNameInput?.value ?? '').toBe('')
      expect(createAccountPasswordInput?.value ?? '').toBe('')
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
      if (url.startsWith('/api/v1/console/accounts?')) {
        return jsonResponse(defaultAccountsPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')

    expect(wrapper.find('#create-account-username').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('Create Account')

    wrapper.unmount()
  })

  it('returns to login when refresh receives 401', async () => {
    let forceUnauthorized = false
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/console/accounts?')) {
        return forceUnauthorized ? unauthorizedResponse() : jsonResponse(defaultAccountsPayload())
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')
    try {
      forceUnauthorized = true
      const refreshBtn = wrapper.findAll('button').find((button) => button.text() === 'Refresh')
      expect(refreshBtn).toBeTruthy()
      await refreshBtn?.trigger('click')
      await flushPromises()
      await flushPromises()
      await waitForRoute('/login')

      expect(router.currentRoute.value.path).toBe('/login')
    } finally {
      wrapper.unmount()
    }
  })

  it('logs out from accounts page and returns to login', async () => {
    let authenticated = true
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return authenticated ? jsonResponse(adminSessionPayload) : unauthorizedResponse()
      }
      if (url.startsWith('/api/v1/console/accounts?')) {
        return authenticated ? jsonResponse(defaultAccountsPayload()) : unauthorizedResponse()
      }
      if (url === '/api/v1/console/logout') {
        authenticated = false
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')
    try {
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
    } finally {
      wrapper.unmount()
    }
  })

  it('changes password from accounts page modal', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      const method = String(init?.method ?? 'GET').toUpperCase()
      if (url === '/api/v1/console/session') {
        return jsonResponse(adminSessionPayload)
      }
      if (url.startsWith('/api/v1/console/accounts?')) {
        return jsonResponse(defaultAccountsPayload())
      }
      if (url === '/api/v1/console/password' && method === 'POST') {
        return noContentResponse()
      }
      throw new Error(`unexpected url: ${url}, method=${method}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountApp('/accounts')
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
})
