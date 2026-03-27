import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import App from '../App.vue'
import router from '../router'

export const adminSessionPayload = {
  authenticated: true,
  account: {
    account_id: 'acc-admin',
    username: 'admin-test',
    is_admin: true,
  },
  registration_enabled: true,
  console_version: 'v0.1.0',
  console_repo_url: 'https://github.com/Coooolfan/onlyboxes',
}

export const memberSessionPayload = {
  authenticated: true,
  account: {
    account_id: 'acc-member',
    username: 'member-test',
    is_admin: false,
  },
  registration_enabled: false,
  console_version: 'v0.1.0',
  console_repo_url: 'https://github.com/Coooolfan/onlyboxes',
}

export const statsPayload = {
  total: 150,
  online: 120,
  offline: 30,
  stale: 10,
  stale_after_sec: 45,
  generated_at: '2026-02-16T10:00:10Z',
}

export const workersPayload = {
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

export const inflightPayload = {
  workers: [
    {
      node_id: 'node-1',
      capabilities: [{ name: 'echo', inflight: 1, max_inflight: 4 }],
    },
  ],
  generated_at: '2026-02-16T10:00:10Z',
}

export function defaultTokensPayload() {
  return {
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
}

export function defaultAPIKeysPayload() {
  return {
    items: [
      {
        id: 'apik-1',
        name: 'ci-prod',
        key_masked: 'obxk****9fa1',
        created_at: '2026-02-16T10:00:00Z',
        updated_at: '2026-02-16T10:00:00Z',
      },
    ],
    total: 1,
  }
}

export function defaultAccountsPayload() {
  return {
    items: [
      {
        account_id: 'acc-admin',
        username: 'admin-test',
        is_admin: true,
        created_at: '2026-02-16T10:00:00Z',
        updated_at: '2026-02-16T10:00:00Z',
      },
      {
        account_id: 'acc-member',
        username: 'member-test',
        is_admin: false,
        created_at: '2026-02-16T11:00:00Z',
        updated_at: '2026-02-16T11:00:00Z',
      },
    ],
    total: 2,
    page: 1,
    page_size: 20,
  }
}

export function jsonResponse(payload: unknown) {
  return {
    ok: true,
    status: 200,
    statusText: 'OK',
    json: async () => payload,
  }
}

export function noContentResponse() {
  return {
    ok: true,
    status: 204,
    statusText: 'No Content',
    json: async () => ({}),
  }
}

export function unauthorizedResponse() {
  return {
    ok: false,
    status: 401,
    statusText: 'Unauthorized',
    json: async () => ({ error: 'authentication required' }),
  }
}

export async function mountApp(path: string) {
  const pinia = createPinia()
  setActivePinia(pinia)
  const wrapper = mount(App, {
    global: {
      plugins: [pinia, router],
    },
  })

  await router.push('/login')
  await settleUI()
  await router.push(path)
  await settleUI()

  return wrapper
}

export async function waitForRoute(path: string, maxAttempts = 30) {
  for (let i = 0; i < maxAttempts; i += 1) {
    if (router.currentRoute.value.path === path) {
      return
    }
    await flushPromises()
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 0)
    })
  }
}

async function settleUI(rounds = 4) {
  for (let i = 0; i < rounds; i += 1) {
    await flushPromises()
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 0)
    })
  }
}
