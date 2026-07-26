import { describe, expect, it } from 'vitest'

import {
  buildWorkerBoxliteStartupCommand,
  buildWorkerDockerStartupCommand,
  buildWorkerSysStartupCommand,
  createDefaultWorkerBoxliteStartupConfig,
  createDefaultWorkerDockerStartupConfig,
  createDefaultWorkerSysStartupConfig,
  useWorkerStartupTool,
} from '@/composables/useWorkerStartupTool'

describe('worker startup tool command builder', () => {
  it('builds multiline worker-docker command with required fields', () => {
    const config = createDefaultWorkerDockerStartupConfig()
    config.workerID = 'node-docker-1'
    config.workerSecret = 'secret-docker-1'
    config.pythonExecMemoryMib = 512
    config.pythonExecCpus = 2
    config.pythonExecMaxProcesses = 256
    config.terminalExecMemoryMib = 1024
    config.terminalExecCpus = 3
    config.terminalExecMaxProcesses = 512
    config.terminalExportMaxBytes = 2048

    const result = buildWorkerDockerStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain("WORKER_ID='node-docker-1' \\")
    expect(result.command).toContain("WORKER_SECRET='secret-docker-1' \\")
    expect(result.command).toContain("WORKER_PYTHON_EXEC_MEMORY_MIB='512' \\")
    expect(result.command).toContain("WORKER_PYTHON_EXEC_CPUS='2' \\")
    expect(result.command).toContain("WORKER_PYTHON_EXEC_MAX_PROCESSES='256' \\")
    expect(result.command).toContain("WORKER_TERMINAL_EXEC_MEMORY_MIB='1024' \\")
    expect(result.command).toContain("WORKER_TERMINAL_EXEC_CPUS='3' \\")
    expect(result.command).toContain("WORKER_TERMINAL_EXEC_MAX_PROCESSES='512' \\")
    expect(result.command).toContain("WORKER_TERMINAL_EXPORT_MAX_BYTES='2048' \\")
    expect(result.command).toContain('\n')
    expect(result.command).toContain("'./onlyboxes-worker-docker'")
  })

  it('renders a config.toml with typed values and a labels table', () => {
    const config = createDefaultWorkerDockerStartupConfig()
    config.workerID = 'node-docker-1'
    config.workerSecret = 'secret-docker-1'
    config.consoleInsecure = true
    config.nodeName = 'edge-1'
    config.labelsText = 'region=cn\nowner=team-a'
    config.pythonExecMemoryMib = 512

    const result = buildWorkerDockerStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.configToml).toContain('id = "node-docker-1"')
    expect(result.configToml).toContain('secret = "secret-docker-1"')
    expect(result.configToml).toContain('console_grpc_target = "127.0.0.1:50051"')
    expect(result.configToml).toContain('console_insecure = true')
    expect(result.configToml).toContain('heartbeat_interval_sec = 5')
    expect(result.configToml).toContain('python_exec_memory_mib = 512')
    expect(result.configToml).toContain('node_name = "edge-1"')
    expect(result.configToml).toContain('[labels]')
    expect(result.configToml).toContain('region = "cn"')
    expect(result.configToml).toContain('owner = "team-a"')
    expect(result.configToml).not.toContain('labels = ')
  })

  it('renders worker-sys list values as config.toml arrays', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.computerUseCommandWhitelistText = 'echo\ntime'
    config.readImageAllowedPathsText = '/data/images'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.configToml).toContain('computer_use_command_whitelist_mode = "exact"')
    expect(result.configToml).toContain('computer_use_command_whitelist = ["echo", "time"]')
    expect(result.configToml).toContain('read_image_allowed_paths = ["/data/images"]')
  })

  it('does not render a config.toml for the Temporary Probe installer command', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.startupPreset = 'temporary-probe'
    config.temporaryProbeInstallerOrigin = 'https://console.example.test'
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.configToml).toBe('')
  })

  it('serializes worker-sys whitelist and allowed paths as JSON strings', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.computerUseCommandWhitelistText = 'echo\ntime\necho'
    config.readImageAllowedPathsText = '/data/images\n/tmp/a.png\n/tmp/a.png'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain('WORKER_COMPUTER_USE_COMMAND_WHITELIST=\'["echo","time"]\' \\')
    expect(result.command).toContain(
      'WORKER_READ_IMAGE_ALLOWED_PATHS=\'["/data/images","/tmp/a.png"]\' \\',
    )
  })

  it('builds Temporary Probe installer command with only dynamic CLI args', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.startupPreset = 'temporary-probe'
    config.temporaryProbeInstallerOrigin = 'https://console.example.test'
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.consoleGRPCTarget = 'console.example.test:50051'
    config.consoleInsecure = true
    config.nodeName = 'Temporary Probe'
    config.computerUseCommandWhitelistMode = 'allow_all'
    config.readImageAllowedPathsText = '/'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toBe(
      "curl -fsSL 'https://console.example.test/static/worker-startup.sh' | bash -s -- --node-id 'node-sys-1' --worker-secret 'secret-sys-1' --grpc-target 'console.example.test:50051' --console-insecure true",
    )
    expect(result.command).not.toContain('WORKER_NODE_NAME')
    expect(result.command).not.toContain('WORKER_CONSOLE_INSECURE')
    expect(result.command).not.toContain('WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE')
    expect(result.command).not.toContain('WORKER_READ_IMAGE_ALLOWED_PATHS')
  })

  it('passes --console-insecure false when Temporary Probe disables insecure mode', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.startupPreset = 'temporary-probe'
    config.temporaryProbeInstallerOrigin = 'https://console.example.test'
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.consoleGRPCTarget = 'console.example.test:50051'
    config.consoleInsecure = false

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain(' --console-insecure false')
    expect(result.command).not.toContain(' --console-insecure true')
  })

  it('adds an optional tag arg to Temporary Probe installer command when overridden', () => {
    const config = createDefaultWorkerSysStartupConfig()
    config.startupPreset = 'temporary-probe'
    config.temporaryProbeInstallerOrigin = 'https://console.example.test'
    config.workerID = 'node-sys-1'
    config.workerSecret = 'secret-sys-1'
    config.consoleGRPCTarget = 'console.example.test:50051'
    config.temporaryProbeTag = 'v0.5.1'

    const result = buildWorkerSysStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain(" --tag 'v0.5.1'")
  })

  it('builds worker-boxlite command with runtime-specific env vars', () => {
    const config = createDefaultWorkerBoxliteStartupConfig()
    config.workerID = 'node-boxlite-1'
    config.workerSecret = 'secret-boxlite-1'
    config.boxliteHome = '/var/lib/onlyboxes/boxlite'
    config.pythonExecMemoryMib = 512
    config.terminalExportMaxBytes = 0

    const result = buildWorkerBoxliteStartupCommand(config)

    expect(result.errors).toEqual([])
    expect(result.command).toContain("WORKER_ID='node-boxlite-1' \\")
    expect(result.command).toContain("WORKER_BOXLITE_HOME='/var/lib/onlyboxes/boxlite' \\")
    expect(result.command).toContain("WORKER_PYTHON_EXEC_MEMORY_MIB='512' \\")
    expect(result.command).toContain("WORKER_TERMINAL_EXPORT_MAX_BYTES='0' \\")
    expect(result.command).toContain("'./onlyboxes-worker-boxlite'")
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

  it('fills worker-boxlite config when workerKind is worker-boxlite', () => {
    const { workerKind, workerBoxliteConfig, workerDockerConfig, workerSysConfig } =
      useWorkerStartupTool({
        workerKind: 'worker-boxlite',
        workerID: 'test-id',
        workerSecret: 'test-secret',
      })

    expect(workerKind.value).toBe('worker-boxlite')
    expect(workerBoxliteConfig.workerID).toBe('test-id')
    expect(workerBoxliteConfig.workerSecret).toBe('test-secret')
    expect(workerDockerConfig.workerID).toBe('')
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
