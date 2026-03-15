import { computed, reactive, ref } from 'vue'

import type {
  StartupCommandBuildResult,
  WorkerDockerStartupConfig,
  WorkerStartupKind,
  WorkerSysStartupConfig,
  WorkerSysWhitelistMode,
} from '@/types/worker-startup-tool'

const defaultConsoleGRPCTarget = '127.0.0.1:50051'
const defaultHeartbeatIntervalSec = 5
const defaultHeartbeatJitterPct = 20
const defaultDockerBinaryPath = './onlyboxes-worker-docker'
const defaultSysBinaryPath = './onlyboxes-worker-sys'
const defaultTerminalOutputLimitBytes = 1024 * 1024
const defaultComputerUseOutputLimitBytes = 1024 * 1024
const defaultTerminalLeaseMinSec = 60
const defaultTerminalLeaseMaxSec = 1800
const defaultTerminalLeaseDefaultSec = 60
const defaultPythonExecDockerImage = 'python:slim'
const defaultTerminalExecDockerImage = 'coolfan1024/onlyboxes-default-worker:0.0.3'

type BuildState = {
  envEntries: Array<[string, string]>
  errors: string[]
  warnings: string[]
}

function emptyBuildState(): BuildState {
  return {
    envEntries: [],
    errors: [],
    warnings: [],
  }
}

function parsePositiveInt(
  value: number,
  fallbackValue: number,
): {
  value: number
  valid: boolean
} {
  const normalized = Number.isFinite(value) ? Math.floor(value) : Number.NaN
  if (normalized > 0) {
    return { value: normalized, valid: true }
  }
  return { value: fallbackValue, valid: false }
}

function parsePercentInt(
  value: number,
  fallbackValue: number,
): {
  value: number
  valid: boolean
} {
  const normalized = Number.isFinite(value) ? Math.floor(value) : Number.NaN
  if (normalized >= 0 && normalized <= 100) {
    return { value: normalized, valid: true }
  }
  return { value: fallbackValue, valid: false }
}

function defaultCallTimeoutSec(heartbeatSec: number): number {
  const hb = heartbeatSec > 0 ? heartbeatSec : defaultHeartbeatIntervalSec
  return Math.floor((hb * 5 + 1) / 2)
}

function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

function parseUniqueLineValues(input: string): string[] {
  if (input.trim() === '') {
    return []
  }

  const seen = new Set<string>()
  const result: string[] = []
  for (const line of input.split('\n')) {
    const trimmed = line.trim()
    if (trimmed === '' || seen.has(trimmed)) {
      continue
    }
    seen.add(trimmed)
    result.push(trimmed)
  }
  return result
}

function parseLabelsCSV(input: string): {
  value: string
  invalidCount: number
} {
  if (input.trim() === '') {
    return {
      value: '',
      invalidCount: 0,
    }
  }

  const entries: Array<[string, string]> = []
  const entryIndexByKey = new Map<string, number>()
  let invalidCount = 0

  for (const rawLine of input.split('\n')) {
    const line = rawLine.trim()
    if (line === '') {
      continue
    }

    const separatorIndex = line.indexOf('=')
    if (separatorIndex <= 0) {
      invalidCount += 1
      continue
    }

    const key = line.slice(0, separatorIndex).trim()
    const value = line.slice(separatorIndex + 1).trim()
    if (key === '') {
      invalidCount += 1
      continue
    }

    const existingIndex = entryIndexByKey.get(key)
    if (existingIndex === undefined) {
      entryIndexByKey.set(key, entries.length)
      entries.push([key, value])
      continue
    }

    entries[existingIndex] = [key, value]
  }

  return {
    value: entries.map(([key, value]) => `${key}=${value}`).join(','),
    invalidCount,
  }
}

function formatMultilineCommand(envEntries: Array<[string, string]>, binaryPath: string): string {
  const lines = envEntries.map(([key, value]) => `${key}=${shellQuote(value)} \\`)
  lines.push(shellQuote(binaryPath))
  return lines.join('\n')
}

function appendCommonEnv(
  state: BuildState,
  config: WorkerDockerStartupConfig | WorkerSysStartupConfig,
): {
  heartbeatSec: number
} {
  const workerID = config.workerID.trim()
  const workerSecret = config.workerSecret.trim()
  const consoleGRPCTarget = config.consoleGRPCTarget.trim()
  const nodeName = config.nodeName.trim()
  const version = config.version.trim()
  const labels = parseLabelsCSV(config.labelsText)

  if (!workerID) {
    state.errors.push('WORKER_ID is required.')
  }
  if (!workerSecret) {
    state.errors.push('WORKER_SECRET is required.')
  }
  if (!consoleGRPCTarget) {
    state.errors.push('WORKER_CONSOLE_GRPC_TARGET is required.')
  }
  if (!config.binaryPath.trim()) {
    state.errors.push('Binary path is required.')
  }
  if (labels.invalidCount > 0) {
    state.warnings.push(
      `Ignored ${labels.invalidCount} invalid WORKER_LABELS line(s). Expected "key=value".`,
    )
  }

  const heartbeatSec = parsePositiveInt(config.heartbeatIntervalSec, defaultHeartbeatIntervalSec)
  if (!heartbeatSec.valid) {
    state.errors.push('WORKER_HEARTBEAT_INTERVAL_SEC must be a positive integer.')
  }

  const heartbeatJitter = parsePercentInt(config.heartbeatJitterPct, defaultHeartbeatJitterPct)
  if (!heartbeatJitter.valid) {
    state.errors.push('WORKER_HEARTBEAT_JITTER_PCT must be an integer in [0, 100].')
  }

  const callTimeoutAuto = defaultCallTimeoutSec(heartbeatSec.value)
  const callTimeoutManual = parsePositiveInt(config.callTimeoutSec, callTimeoutAuto)
  if (config.callTimeoutMode === 'manual') {
    if (!callTimeoutManual.valid) {
      state.errors.push('WORKER_CALL_TIMEOUT_SEC must be a positive integer in manual mode.')
    } else if (callTimeoutManual.value < heartbeatSec.value * 2) {
      state.warnings.push(
        'WORKER_CALL_TIMEOUT_SEC is lower than 2 * WORKER_HEARTBEAT_INTERVAL_SEC; timeout may be too aggressive.',
      )
    }
  }

  state.envEntries.push(['WORKER_CONSOLE_GRPC_TARGET', consoleGRPCTarget])
  if (config.consoleInsecure) {
    state.envEntries.push(['WORKER_CONSOLE_INSECURE', 'true'])
  }
  state.envEntries.push(['WORKER_ID', workerID])
  state.envEntries.push(['WORKER_SECRET', workerSecret])
  state.envEntries.push(['WORKER_HEARTBEAT_INTERVAL_SEC', String(heartbeatSec.value)])
  state.envEntries.push(['WORKER_HEARTBEAT_JITTER_PCT', String(heartbeatJitter.value)])
  if (config.callTimeoutMode === 'manual') {
    state.envEntries.push(['WORKER_CALL_TIMEOUT_SEC', String(callTimeoutManual.value)])
  }
  if (nodeName) {
    state.envEntries.push(['WORKER_NODE_NAME', nodeName])
  }
  if (version) {
    state.envEntries.push(['WORKER_VERSION', version])
  }
  if (labels.value) {
    state.envEntries.push(['WORKER_LABELS', labels.value])
  }

  return {
    heartbeatSec: heartbeatSec.value,
  }
}

export function createDefaultWorkerDockerStartupConfig(): WorkerDockerStartupConfig {
  return {
    workerID: '',
    workerSecret: '',
    consoleGRPCTarget: defaultConsoleGRPCTarget,
    consoleInsecure: false,
    heartbeatIntervalSec: defaultHeartbeatIntervalSec,
    heartbeatJitterPct: defaultHeartbeatJitterPct,
    callTimeoutMode: 'auto',
    callTimeoutSec: defaultCallTimeoutSec(defaultHeartbeatIntervalSec),
    binaryPath: defaultDockerBinaryPath,
    nodeName: '',
    version: '',
    labelsText: '',
    pythonExecDockerImage: defaultPythonExecDockerImage,
    terminalExecDockerImage: defaultTerminalExecDockerImage,
    terminalLeaseMinSec: defaultTerminalLeaseMinSec,
    terminalLeaseMaxSec: defaultTerminalLeaseMaxSec,
    terminalLeaseDefaultSec: defaultTerminalLeaseDefaultSec,
    terminalOutputLimitBytes: defaultTerminalOutputLimitBytes,
  }
}

export function createDefaultWorkerSysStartupConfig(): WorkerSysStartupConfig {
  return {
    workerID: '',
    workerSecret: '',
    consoleGRPCTarget: defaultConsoleGRPCTarget,
    consoleInsecure: false,
    heartbeatIntervalSec: defaultHeartbeatIntervalSec,
    heartbeatJitterPct: defaultHeartbeatJitterPct,
    callTimeoutMode: 'auto',
    callTimeoutSec: defaultCallTimeoutSec(defaultHeartbeatIntervalSec),
    binaryPath: defaultSysBinaryPath,
    nodeName: '',
    version: '',
    labelsText: '',
    computerUseOutputLimitBytes: defaultComputerUseOutputLimitBytes,
    computerUseCommandWhitelistMode: 'exact',
    computerUseCommandWhitelistText: '',
    readImageAllowedPathsText: '',
  }
}

export function buildWorkerDockerStartupCommand(
  config: WorkerDockerStartupConfig,
): StartupCommandBuildResult {
  const state = emptyBuildState()
  appendCommonEnv(state, config)

  const pythonExecDockerImage = config.pythonExecDockerImage.trim()
  const terminalExecDockerImage = config.terminalExecDockerImage.trim()
  if (!pythonExecDockerImage) {
    state.errors.push('WORKER_PYTHON_EXEC_DOCKER_IMAGE is required.')
  }
  if (!terminalExecDockerImage) {
    state.errors.push('WORKER_TERMINAL_EXEC_DOCKER_IMAGE is required.')
  }

  const terminalLeaseMinSec = parsePositiveInt(
    config.terminalLeaseMinSec,
    defaultTerminalLeaseMinSec,
  )
  if (!terminalLeaseMinSec.valid) {
    state.errors.push('WORKER_TERMINAL_LEASE_MIN_SEC must be a positive integer.')
  }

  const terminalLeaseMaxRaw = parsePositiveInt(
    config.terminalLeaseMaxSec,
    defaultTerminalLeaseMaxSec,
  )
  if (!terminalLeaseMaxRaw.valid) {
    state.errors.push('WORKER_TERMINAL_LEASE_MAX_SEC must be a positive integer.')
  }

  const terminalLeaseMaxSec = Math.max(terminalLeaseMinSec.value, terminalLeaseMaxRaw.value)
  if (terminalLeaseMaxRaw.value < terminalLeaseMinSec.value) {
    state.warnings.push(
      'WORKER_TERMINAL_LEASE_MAX_SEC was lower than WORKER_TERMINAL_LEASE_MIN_SEC and was raised automatically.',
    )
  }

  const terminalLeaseDefaultRaw = parsePositiveInt(
    config.terminalLeaseDefaultSec,
    defaultTerminalLeaseDefaultSec,
  )
  if (!terminalLeaseDefaultRaw.valid) {
    state.errors.push('WORKER_TERMINAL_LEASE_DEFAULT_SEC must be a positive integer.')
  }
  const terminalLeaseDefaultSec = Math.max(
    terminalLeaseMinSec.value,
    Math.min(terminalLeaseMaxSec, terminalLeaseDefaultRaw.value),
  )

  const terminalOutputLimitBytes = parsePositiveInt(
    config.terminalOutputLimitBytes,
    defaultTerminalOutputLimitBytes,
  )
  if (!terminalOutputLimitBytes.valid) {
    state.errors.push('WORKER_TERMINAL_OUTPUT_LIMIT_BYTES must be a positive integer.')
  }

  state.envEntries.push(['WORKER_PYTHON_EXEC_DOCKER_IMAGE', pythonExecDockerImage])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_DOCKER_IMAGE', terminalExecDockerImage])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_MIN_SEC', String(terminalLeaseMinSec.value)])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_MAX_SEC', String(terminalLeaseMaxSec)])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_DEFAULT_SEC', String(terminalLeaseDefaultSec)])
  state.envEntries.push([
    'WORKER_TERMINAL_OUTPUT_LIMIT_BYTES',
    String(terminalOutputLimitBytes.value),
  ])

  return {
    command: formatMultilineCommand(state.envEntries, config.binaryPath.trim()),
    errors: state.errors,
    warnings: state.warnings,
  }
}

function normalizeWhitelistMode(mode: string): WorkerSysWhitelistMode {
  if (mode === 'exact' || mode === 'prefix' || mode === 'allow_all') {
    return mode
  }
  return 'exact'
}

export function buildWorkerSysStartupCommand(
  config: WorkerSysStartupConfig,
): StartupCommandBuildResult {
  const state = emptyBuildState()
  appendCommonEnv(state, config)

  const outputLimit = parsePositiveInt(
    config.computerUseOutputLimitBytes,
    defaultComputerUseOutputLimitBytes,
  )
  if (!outputLimit.valid) {
    state.errors.push('WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES must be a positive integer.')
  }

  const whitelistMode = normalizeWhitelistMode(config.computerUseCommandWhitelistMode)
  const whitelistEntries = parseUniqueLineValues(config.computerUseCommandWhitelistText)
  const readImageAllowedPaths = parseUniqueLineValues(config.readImageAllowedPathsText)

  if (whitelistMode === 'allow_all') {
    state.warnings.push(
      'WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE=allow_all approves all commands and disables whitelist checks.',
    )
  }
  if (whitelistMode !== 'allow_all' && whitelistEntries.length === 0) {
    state.warnings.push(
      'WORKER_COMPUTER_USE_COMMAND_WHITELIST is empty; exact/prefix mode will block all commands.',
    )
  }
  if (readImageAllowedPaths.length === 0) {
    state.warnings.push(
      'WORKER_READ_IMAGE_ALLOWED_PATHS is empty; readImage access will be denied by default.',
    )
  }

  state.envEntries.push(['WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES', String(outputLimit.value)])
  state.envEntries.push(['WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE', whitelistMode])
  if (whitelistMode !== 'allow_all' && whitelistEntries.length > 0) {
    state.envEntries.push([
      'WORKER_COMPUTER_USE_COMMAND_WHITELIST',
      JSON.stringify(whitelistEntries),
    ])
  }
  if (readImageAllowedPaths.length > 0) {
    state.envEntries.push(['WORKER_READ_IMAGE_ALLOWED_PATHS', JSON.stringify(readImageAllowedPaths)])
  }

  return {
    command: formatMultilineCommand(state.envEntries, config.binaryPath.trim()),
    errors: state.errors,
    warnings: state.warnings,
  }
}

export interface WorkerStartupToolInitialValues {
  workerKind?: WorkerStartupKind
  workerID?: string
  workerSecret?: string
  consoleGRPCTarget?: string
}

export function useWorkerStartupTool(initial?: WorkerStartupToolInitialValues) {
  const workerKind = ref<WorkerStartupKind>(initial?.workerKind ?? 'worker-docker')
  const workerDockerConfig = reactive<WorkerDockerStartupConfig>(createDefaultWorkerDockerStartupConfig())
  const workerSysConfig = reactive<WorkerSysStartupConfig>(createDefaultWorkerSysStartupConfig())

  if (initial) {
    const target = workerKind.value === 'worker-docker' ? workerDockerConfig : workerSysConfig
    if (initial.workerID !== undefined) {
      target.workerID = initial.workerID
    }
    if (initial.workerSecret !== undefined) {
      target.workerSecret = initial.workerSecret
    }
    if (initial.consoleGRPCTarget !== undefined) {
      target.consoleGRPCTarget = initial.consoleGRPCTarget
    }
  }

  const workerDockerResult = computed(() => buildWorkerDockerStartupCommand(workerDockerConfig))
  const workerSysResult = computed(() => buildWorkerSysStartupCommand(workerSysConfig))

  const currentBuildResult = computed<StartupCommandBuildResult>(() => {
    return workerKind.value === 'worker-docker' ? workerDockerResult.value : workerSysResult.value
  })

  const commandText = computed(() => currentBuildResult.value.command)
  const errorMessages = computed(() => currentBuildResult.value.errors)
  const warningMessages = computed(() => currentBuildResult.value.warnings)
  const canCopyCommand = computed(
    () => errorMessages.value.length === 0 && commandText.value.trim().length > 0,
  )

  function selectWorkerKind(kind: WorkerStartupKind): void {
    workerKind.value = kind
  }

  return {
    workerKind,
    workerDockerConfig,
    workerSysConfig,
    commandText,
    errorMessages,
    warningMessages,
    canCopyCommand,
    selectWorkerKind,
  }
}
