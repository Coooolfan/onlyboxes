import { describe, expect, it } from 'vitest'

import {
  buildWorkerDockerStartupCommand,
  buildWorkerSysStartupCommand,
  createDefaultWorkerDockerStartupConfig,
  createDefaultWorkerSysStartupConfig,
  useWorkerStartupTool,
} from '@/composables/useWorkerStartupTool'

describe('worker startup tool command builder', () => {
  it('builds multiline worker-docker command with required fields', () => {
    const config = createDefaultWorkerDockerStartupConfig()
    config.workerID = 'node-docker-1'
    config.workerSecret = 'secret-docker-1'

    const result = buildWorkerDockerStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain("WORKER_ID='node-docker-1' \\")
    expect(result.command).toContain("WORKER_SECRET='secret-docker-1' \\")
    expect(result.command).toContain('\n')
    expect(result.command).toContain("'./onlyboxes-worker-docker'")
  })

  it('serializes worker-sys whitelist and allowed paths as JSON strings', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.computerUseCommandWhitelistText = 'echo\ntime\necho'
    config.readImageAllowedPathsText = '/data/images\n/tmp/a.png\n/tmp/a.png'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain("WORKER_COMPUTER_USE_COMMAND_WHITELIST='[\"echo\",\"time\"]' \\")
    expect(result.command).toContain("WORKER_READ_IMAGE_ALLOWED_PATHS='[\"/data/images\",\"/tmp/a.png\"]' \\")
  })

  it('includes call timeout only in manual mode', () => {
    const config = createDefaultWorkerDockerStartupConfig()
    config.workerID = 'node-docker-1'
    config.workerSecret = 'secret-docker-1'

    const autoResult = buildWorkerDockerStartupCommand(config)
    expect(autoResult.command).not.toContain('WORKER_CALL_TIMEOUT_SEC=')

    config.callTimeoutMode = 'manual'
    config.callTimeoutSec = 9
    const manualResult = buildWorkerDockerStartupCommand(config)
    expect(manualResult.command).toContain("WORKER_CALL_TIMEOUT_SEC='9' \\")
  })

  it('raises terminal lease max when lower than min and clamps default', () => {
    const config = createDefaultWorkerDockerStartupConfig()
    config.workerID = 'node-docker-1'
    config.workerSecret = 'secret-docker-1'
    config.terminalLeaseMinSec = 120
    config.terminalLeaseMaxSec = 60
    config.terminalLeaseDefaultSec = 90

    const result = buildWorkerDockerStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain("WORKER_TERMINAL_LEASE_MIN_SEC='120' \\")
    expect(result.command).toContain("WORKER_TERMINAL_LEASE_MAX_SEC='120' \\")
    expect(result.command).toContain("WORKER_TERMINAL_LEASE_DEFAULT_SEC='120' \\")
  })

  it('returns validation errors when required credentials are missing', () => {
    const config = createDefaultWorkerSysStartupConfig()

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toContain('WORKER_ID is required.')
    expect(result.errors).toContain('WORKER_SECRET is required.')
  })

  it('adds warning in allow_all mode', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.computerUseCommandWhitelistMode = 'allow_all'
    config.computerUseCommandWhitelistText = 'echo\ntime'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.warnings).toContain(
      'WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE=allow_all approves all commands and disables whitelist checks.',
    )
    expect(result.command).not.toContain('WORKER_COMPUTER_USE_COMMAND_WHITELIST=')
  })
})

describe('useWorkerStartupTool with initial values', () => {
  it('fills worker-sys config when workerKind is worker-sys', () => {
    const { workerKind, workerSysConfig, workerDockerConfig } = useWorkerStartupTool({
      workerKind: 'worker-sys',
      workerID: 'test-id',
      workerSecret: 'test-secret',
    })

    expect(workerKind.value).toBe('worker-sys')
    expect(workerSysConfig.workerID).toBe('test-id')
    expect(workerSysConfig.workerSecret).toBe('test-secret')
    expect(workerDockerConfig.workerID).toBe('')
  })

  it('fills worker-docker config when workerKind is worker-docker', () => {
    const { workerKind, workerDockerConfig, workerSysConfig } = useWorkerStartupTool({
      workerKind: 'worker-docker',
      workerID: 'test-id',
    })

    expect(workerKind.value).toBe('worker-docker')
    expect(workerDockerConfig.workerID).toBe('test-id')
    expect(workerSysConfig.workerID).toBe('')
  })

  it('fills consoleGRPCTarget when provided', () => {
    const { workerDockerConfig } = useWorkerStartupTool({
      workerKind: 'worker-docker',
      workerID: 'test-id',
      workerSecret: 'test-secret',
      consoleGRPCTarget: 'example.com:50051',
    })

    expect(workerDockerConfig.consoleGRPCTarget).toBe('example.com:50051')
  })

  it('uses defaults when no initial values provided', () => {
    const { workerKind, workerDockerConfig } = useWorkerStartupTool()

    expect(workerKind.value).toBe('worker-docker')
    expect(workerDockerConfig.workerID).toBe('')
    expect(workerDockerConfig.workerSecret).toBe('')
  })
})
