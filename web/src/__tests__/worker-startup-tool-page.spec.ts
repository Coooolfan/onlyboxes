import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import App from '../App.vue'
import router from '../router'
import { setPrefill } from '../composables/useWorkerStartupPrefill'
import { jsonResponse, memberSessionPayload } from './testkit'

async function settleUI(rounds = 4) {
  for (let i = 0; i < rounds; i += 1) {
    await flushPromises()
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 0)
    })
  }
}

async function mountRoute(path: string) {
  const pinia = createPinia()
  setActivePinia(pinia)
  const wrapper = mount(App, {
    global: {
      plugins: [pinia, router],
    },
  })

  await router.push(path)
  await settleUI()
  return wrapper
}

describe('Worker Startup Tool Page', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it('allows non-admin users to access the route without loading workers/accounts/tokens APIs', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      if (
        url.startsWith('/api/v1/workers') ||
        url.startsWith('/api/v1/console/accounts') ||
        url.startsWith('/api/v1/console/tokens')
      ) {
        throw new Error(`unexpected dashboard api call: ${url}`)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountRoute('/tools/worker-startup')

    expect(router.currentRoute.value.path).toBe('/tools/worker-startup')
    expect(wrapper.text()).toContain('Worker Startup Tool')
    expect(wrapper.get('[data-testid="copy-startup-command"]').attributes('disabled')).toBeDefined()

    const requestedURLs = fetchMock.mock.calls.map(([input]) => String(input))
    expect(requestedURLs.some((url) => url.startsWith('/api/v1/workers'))).toBe(false)
    expect(requestedURLs.some((url) => url.startsWith('/api/v1/console/accounts'))).toBe(false)
    expect(requestedURLs.some((url) => url.startsWith('/api/v1/console/tokens'))).toBe(false)

    wrapper.unmount()
  })

  it('updates command preview after switching worker type', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountRoute('/tools/worker-startup')

    expect(wrapper.get('[data-testid="startup-command-preview"]').text()).toContain(
      './onlyboxes-worker-docker',
    )

    await wrapper.get('[data-testid="worker-kind-sys-btn"]').trigger('click')
    await settleUI()

    expect(wrapper.get('[data-testid="startup-command-preview"]').text()).toContain(
      './onlyboxes-worker-sys',
    )

    wrapper.unmount()
  })

  it('updates command preview after switching to worker-boxlite', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountRoute('/tools/worker-startup')

    await wrapper.get('[data-testid="worker-kind-boxlite-btn"]').trigger('click')
    await settleUI()

    const previewText = wrapper.get('[data-testid="startup-command-preview"]').text()
    expect(previewText).toContain('./onlyboxes-worker-boxlite')
    expect(previewText).toContain('WORKER_PYTHON_EXEC_BOXLITE_IMAGE')

    wrapper.unmount()
  })

  it('shows prefilled credential hints and blocks route leave when opened from goToStartupTool', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)
    const confirmMock = vi.fn(() => false)
    vi.stubGlobal('confirm', confirmMock)

    setPrefill({
      workerKind: 'worker-docker',
      workerID: 'node-2',
      workerSecret: 'secret-2',
      consoleGRPCTarget: '127.0.0.1:50051',
    })

    const wrapper = await mountRoute('/tools/worker-startup')

    expect(wrapper.get('[data-testid="prefilled-credentials-notice"]').text()).toContain(
      'WORKER_ID and WORKER_SECRET are already filled in',
    )
    expect(wrapper.get('[data-testid="worker-id-prefilled-hint"]').text()).toContain(
      'Already Filled',
    )
    expect(wrapper.get('[data-testid="worker-secret-prefilled-hint"]').text()).toContain(
      'Already Filled',
    )

    await router.push('/tokens')
    await settleUI()

    expect(confirmMock).toHaveBeenCalledWith(
      'WORKER_ID and WORKER_SECRET are already filled in. This is the last time the key will be visible. Please confirm you have saved them before leaving this page.',
    )
    expect(router.currentRoute.value.path).toBe('/tools/worker-startup')

    wrapper.unmount()
  })

  it('applies Temporary Probe preset without clearing prefilled credentials', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    setPrefill({
      workerKind: 'worker-sys',
      workerID: 'prefilled-id',
      workerSecret: 'prefilled-secret',
      consoleGRPCTarget: 'old.example.test:50051',
    })

    const wrapper = await mountRoute('/tools/worker-startup')

    const temporaryProbeButton = wrapper
      .findAll('button')
      .find((node) => node.text().trim() === 'Temporary Probe')
    if (!temporaryProbeButton) {
      throw new Error('Temporary Probe preset button not found')
    }

    await temporaryProbeButton.trigger('click')
    await settleUI()

    const expectedGRPCTarget = `${window.location.hostname}:50051`
    const previewText = wrapper.get('[data-testid="startup-command-preview"]').text()
    expect(previewText).toContain('/static/worker-startup.sh')
    expect(previewText).toContain("bash -s -- --worker-id 'prefilled-id'")
    expect(previewText).toContain("--worker-secret 'prefilled-secret'")
    expect(previewText).toContain(`--grpc-target '${expectedGRPCTarget}'`)
    expect(previewText).not.toContain('WORKER_NODE_NAME=')
    expect(previewText).not.toContain('WORKER_CONSOLE_INSECURE=')
    expect(previewText).not.toContain('WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE=')
    expect(previewText).not.toContain('WORKER_READ_IMAGE_ALLOWED_PATHS=')

    wrapper.unmount()
  })

  it('reflects advanced sys inputs into command preview', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountRoute('/tools/worker-startup')

    await wrapper.get('[data-testid="worker-kind-sys-btn"]').trigger('click')
    await settleUI()

    const details = wrapper.get('[data-testid="sys-advanced-section"]')
    ;(details.element as HTMLDetailsElement).open = true
    await settleUI()

    await wrapper.get('[data-testid="sys-whitelist-textarea"]').setValue('echo\ntime')
    await wrapper.get('[data-testid="sys-paths-textarea"]').setValue('/tmp/a.png')
    await settleUI()

    const previewText = wrapper.get('[data-testid="startup-command-preview"]').text()
    expect(previewText).toContain('WORKER_COMPUTER_USE_COMMAND_WHITELIST=\'["echo","time"]\' \\')
    expect(previewText).toContain('WORKER_READ_IMAGE_ALLOWED_PATHS=\'["/tmp/a.png"]\' \\')

    wrapper.unmount()
  })

  it('disables whitelist textarea in allow_all mode', async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url === '/api/v1/console/session') {
        return jsonResponse(memberSessionPayload)
      }
      throw new Error(`unexpected url: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock as unknown as typeof fetch)

    const wrapper = await mountRoute('/tools/worker-startup')

    await wrapper.get('[data-testid="worker-kind-sys-btn"]').trigger('click')
    await settleUI()

    const details = wrapper.get('[data-testid="sys-advanced-section"]')
    ;(details.element as HTMLDetailsElement).open = true
    await settleUI()

    const whitelist = wrapper.get('[data-testid="sys-whitelist-textarea"]')
    expect(whitelist.attributes('disabled')).toBeUndefined()

    const allowAllButton = wrapper
      .findAll('button')
      .find((node) => node.text().trim() === 'allow_all')
    if (!allowAllButton) {
      throw new Error('allow_all mode button not found')
    }
    await allowAllButton.trigger('click')
    await settleUI()

    expect(
      wrapper.get('[data-testid="sys-whitelist-textarea"]').attributes('disabled'),
    ).toBeDefined()
    expect(wrapper.text()).toContain('Disabled because allow_all mode ignores whitelist entries.')

    wrapper.unmount()
  })
})
