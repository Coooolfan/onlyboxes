import { computed, reactive, ref } from 'vue'

import type {
  StartupCommandBuildResult,
  WorkerBoxliteStartupConfig,
  WorkerDockerStartupConfig,
  WorkerStartupKind,
  WorkerSysStartupConfig,
  WorkerSysWhitelistMode,
} from '@/types/worker-startup-tool'

const defaultConsoleGRPCTarget = '127.0.0.1:50051'
const defaultHeartbeatIntervalSec = 5
const defaultHeartbeatJitterPct = 20
const defaultDockerBinaryPath = './onlyboxes-worker-docker'
const defaultBoxliteBinaryPath = './onlyboxes-worker-boxlite'
const defaultSysBinaryPath = './onlyboxes-worker-sys'
const defaultTerminalOutputLimitBytes = 1024 * 1024
const defaultComputerUseOutputLimitBytes = 1024 * 1024
const defaultTerminalLeaseMinSec = 60
const defaultTerminalLeaseMaxSec = 1800
const defaultTerminalLeaseDefaultSec = 60
const defaultPythonExecDockerImage = 'ghcr.io/astral-sh/uv:python3.12-bookworm-slim'
const defaultPythonExecBoxliteImage = 'ghcr.io/astral-sh/uv:python3.12-bookworm-slim'
const defaultTerminalExecDockerImage = 'coolfan1024/onlyboxes-runtime:default'
const defaultTerminalExecBoxliteImage = 'coolfan1024/onlyboxes-runtime:default'
const defaultDockerMemoryMib = 256
const defaultDockerCpus = 1
const defaultDockerMaxProcesses = 128
const defaultBoxliteMemoryMib = 256
const defaultBoxliteCpus = 1
const defaultBoxliteMaxProcesses = 128
const defaultTerminalExportMaxBytes = 0
const defaultTemporaryProbeNodeName = 'Temporary Probe'
const defaultTemporaryProbeInstallerTag = ''

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

function parsePositiveNumber(
  value: number,
  fallbackValue: number,
): {
  value: number
  valid: boolean
} {
  const normalized = Number.isFinite(value) ? value : Number.NaN
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

function parseNonNegativeInt(
  value: number,
  fallbackValue: number,
): {
  value: number
  valid: boolean
} {
  const normalized = Number.isFinite(value) ? Math.floor(value) : Number.NaN
  if (normalized >= 0) {
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

const numericEnvKeySuffixes = [
  '_SEC',
  '_PCT',
  '_BYTES',
  '_MIB',
  '_CPUS',
  '_MAX_PROCESSES',
  '_MAX_INFLIGHT',
]

function tomlKey(envKey: string): string {
  return envKey.replace(/^WORKER_/, '').toLowerCase()
}

function tomlString(value: string): string {
  const escaped = value
    .replace(/\\/g, '\\\\')
    .replace(/"/g, '\\"')
    .replace(/\n/g, '\\n')
    .replace(/\r/g, '\\r')
    .replace(/\t/g, '\\t')
  return `"${escaped}"`
}

function tomlTableKey(value: string): string {
  return /^[A-Za-z0-9_-]+$/.test(value) ? value : tomlString(value)
}

function tomlValue(envKey: string, value: string): string {
  if (value === 'true' || value === 'false') {
    return value
  }
  if (
    numericEnvKeySuffixes.some((suffix) => envKey.endsWith(suffix)) &&
    /^\d+(\.\d+)?$/.test(value)
  ) {
    return value
  }
  if (value.startsWith('[')) {
    try {
      const parsed: unknown = JSON.parse(value)
      if (Array.isArray(parsed)) {
        return `[${parsed.map((item) => tomlString(String(item))).join(', ')}]`
      }
    } catch {
      // fall through to the string form
    }
  }
  return tomlString(value)
}

// formatConfigToml renders the same values as the shell command into the
// `config.toml` form read by the worker: the env key without the WORKER_
// prefix, lowercased, and WORKER_LABELS expanded into a [labels] table.
function formatConfigToml(envEntries: Array<[string, string]>): string {
  const lines: string[] = []
  let labelsCSV = ''

  for (const [key, value] of envEntries) {
    if (key === 'WORKER_LABELS') {
      labelsCSV = value
      continue
    }
    lines.push(`${tomlKey(key)} = ${tomlValue(key, value)}`)
  }

  if (labelsCSV) {
    lines.push('')
    lines.push('[labels]')
    for (const entry of labelsCSV.split(',')) {
      const separatorIndex = entry.indexOf('=')
      if (separatorIndex <= 0) {
        continue
      }
      const key = entry.slice(0, separatorIndex)
      const value = entry.slice(separatorIndex + 1)
      lines.push(`${tomlTableKey(key)} = ${tomlString(value)}`)
    }
  }

  return lines.length > 0 ? `${lines.join('\n')}\n` : ''
}

function buildResult(state: BuildState, binaryPath: string): StartupCommandBuildResult {
  return {
    command: formatMultilineCommand(state.envEntries, binaryPath),
    configToml: formatConfigToml(state.envEntries),
    errors: state.errors,
    warnings: state.warnings,
  }
}

function formatTemporaryProbeCommand(config: WorkerSysStartupConfig): StartupCommandBuildResult {
  const errors: string[] = []
  const workerID = config.workerID.trim()
  const workerSecret = config.workerSecret.trim()
  const consoleGRPCTarget = config.consoleGRPCTarget.trim()
  const installerOrigin = config.temporaryProbeInstallerOrigin.trim().replace(/\/$/, '')
  const tag = config.temporaryProbeTag.trim()

  if (!workerID) {
    errors.push('WORKER_ID is required.')
  }
  if (!workerSecret) {
    errors.push('WORKER_SECRET is required.')
  }
  if (!consoleGRPCTarget) {
    errors.push('WORKER_CONSOLE_GRPC_TARGET is required.')
  }
  if (!installerOrigin) {
    errors.push('Temporary Probe installer origin is required.')
  }

  const args = [
    '--node-id',
    shellQuote(workerID),
    '--worker-secret',
    shellQuote(workerSecret),
    '--grpc-target',
    shellQuote(consoleGRPCTarget),
    '--console-insecure',
    config.consoleInsecure ? 'true' : 'false',
  ]
  if (tag && tag !== defaultTemporaryProbeInstallerTag) {
    args.push('--tag', shellQuote(tag))
  }

  return {
    command: `curl -fsSL ${shellQuote(`${installerOrigin}/static/worker-startup.sh`)} | bash -s -- ${args.join(' ')}`,
    configToml: '',
    errors,
    warnings: [],
  }
}

function appendCommonEnv(
  state: BuildState,
  config: WorkerDockerStartupConfig | WorkerBoxliteStartupConfig | WorkerSysStartupConfig,
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
    pythonExecMemoryMib: defaultDockerMemoryMib,
    pythonExecCpus: defaultDockerCpus,
    pythonExecMaxProcesses: defaultDockerMaxProcesses,
    terminalExecDockerImage: defaultTerminalExecDockerImage,
    terminalExecMemoryMib: defaultDockerMemoryMib,
    terminalExecCpus: defaultDockerCpus,
    terminalExecMaxProcesses: defaultDockerMaxProcesses,
    terminalLeaseMinSec: defaultTerminalLeaseMinSec,
    terminalLeaseMaxSec: defaultTerminalLeaseMaxSec,
    terminalLeaseDefaultSec: defaultTerminalLeaseDefaultSec,
    terminalOutputLimitBytes: defaultTerminalOutputLimitBytes,
    terminalExportMaxBytes: defaultTerminalExportMaxBytes,
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
    startupPreset: 'custom',
    temporaryProbeInstallerOrigin: '',
    temporaryProbeTag: defaultTemporaryProbeInstallerTag,
    computerUseOutputLimitBytes: defaultComputerUseOutputLimitBytes,
    computerUseCommandWhitelistMode: 'exact',
    computerUseCommandWhitelistText: '',
    readImageAllowedPathsText: '',
  }
}

export function createDefaultWorkerBoxliteStartupConfig(): WorkerBoxliteStartupConfig {
  return {
    workerID: '',
    workerSecret: '',
    consoleGRPCTarget: defaultConsoleGRPCTarget,
    consoleInsecure: false,
    heartbeatIntervalSec: defaultHeartbeatIntervalSec,
    heartbeatJitterPct: defaultHeartbeatJitterPct,
    callTimeoutMode: 'auto',
    callTimeoutSec: defaultCallTimeoutSec(defaultHeartbeatIntervalSec),
    binaryPath: defaultBoxliteBinaryPath,
    nodeName: '',
    version: '',
    labelsText: '',
    boxliteHome: '',
    pythonExecBoxliteImage: defaultPythonExecBoxliteImage,
    pythonExecMemoryMib: defaultBoxliteMemoryMib,
    pythonExecCpus: defaultBoxliteCpus,
    pythonExecMaxProcesses: defaultBoxliteMaxProcesses,
    terminalExecBoxliteImage: defaultTerminalExecBoxliteImage,
    terminalExecMemoryMib: defaultBoxliteMemoryMib,
    terminalExecCpus: defaultBoxliteCpus,
    terminalExecMaxProcesses: defaultBoxliteMaxProcesses,
    terminalLeaseMinSec: defaultTerminalLeaseMinSec,
    terminalLeaseMaxSec: defaultTerminalLeaseMaxSec,
    terminalLeaseDefaultSec: defaultTerminalLeaseDefaultSec,
    terminalOutputLimitBytes: defaultTerminalOutputLimitBytes,
    terminalExportMaxBytes: defaultTerminalExportMaxBytes,
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

  const pythonExecMemoryMib = parsePositiveInt(config.pythonExecMemoryMib, defaultDockerMemoryMib)
  if (!pythonExecMemoryMib.valid) {
    state.errors.push('WORKER_PYTHON_EXEC_MEMORY_MIB must be a positive integer.')
  }

  const pythonExecCpus = parsePositiveNumber(config.pythonExecCpus, defaultDockerCpus)
  if (!pythonExecCpus.valid) {
    state.errors.push('WORKER_PYTHON_EXEC_CPUS must be a positive number.')
  }

  const pythonExecMaxProcesses = parsePositiveInt(
    config.pythonExecMaxProcesses,
    defaultDockerMaxProcesses,
  )
  if (!pythonExecMaxProcesses.valid) {
    state.errors.push('WORKER_PYTHON_EXEC_MAX_PROCESSES must be a positive integer.')
  }

  const terminalExecMemoryMib = parsePositiveInt(
    config.terminalExecMemoryMib,
    defaultDockerMemoryMib,
  )
  if (!terminalExecMemoryMib.valid) {
    state.errors.push('WORKER_TERMINAL_EXEC_MEMORY_MIB must be a positive integer.')
  }

  const terminalExecCpus = parsePositiveNumber(config.terminalExecCpus, defaultDockerCpus)
  if (!terminalExecCpus.valid) {
    state.errors.push('WORKER_TERMINAL_EXEC_CPUS must be a positive number.')
  }

  const terminalExecMaxProcesses = parsePositiveInt(
    config.terminalExecMaxProcesses,
    defaultDockerMaxProcesses,
  )
  if (!terminalExecMaxProcesses.valid) {
    state.errors.push('WORKER_TERMINAL_EXEC_MAX_PROCESSES must be a positive integer.')
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

  const terminalExportMaxBytes = parseNonNegativeInt(
    config.terminalExportMaxBytes,
    defaultTerminalExportMaxBytes,
  )
  if (!terminalExportMaxBytes.valid) {
    state.errors.push('WORKER_TERMINAL_EXPORT_MAX_BYTES must be a non-negative integer.')
  }

  state.envEntries.push(['WORKER_PYTHON_EXEC_DOCKER_IMAGE', pythonExecDockerImage])
  state.envEntries.push(['WORKER_PYTHON_EXEC_MEMORY_MIB', String(pythonExecMemoryMib.value)])
  state.envEntries.push(['WORKER_PYTHON_EXEC_CPUS', String(pythonExecCpus.value)])
  state.envEntries.push(['WORKER_PYTHON_EXEC_MAX_PROCESSES', String(pythonExecMaxProcesses.value)])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_DOCKER_IMAGE', terminalExecDockerImage])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_MEMORY_MIB', String(terminalExecMemoryMib.value)])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_CPUS', String(terminalExecCpus.value)])
  state.envEntries.push([
    'WORKER_TERMINAL_EXEC_MAX_PROCESSES',
    String(terminalExecMaxProcesses.value),
  ])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_MIN_SEC', String(terminalLeaseMinSec.value)])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_MAX_SEC', String(terminalLeaseMaxSec)])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_DEFAULT_SEC', String(terminalLeaseDefaultSec)])
  state.envEntries.push([
    'WORKER_TERMINAL_OUTPUT_LIMIT_BYTES',
    String(terminalOutputLimitBytes.value),
  ])
  state.envEntries.push(['WORKER_TERMINAL_EXPORT_MAX_BYTES', String(terminalExportMaxBytes.value)])

  return buildResult(state, config.binaryPath.trim())
}

export function buildWorkerBoxliteStartupCommand(
  config: WorkerBoxliteStartupConfig,
): StartupCommandBuildResult {
  const state = emptyBuildState()
  appendCommonEnv(state, config)

  const boxliteHome = config.boxliteHome.trim()
  const pythonExecBoxliteImage = config.pythonExecBoxliteImage.trim()
  const terminalExecBoxliteImage = config.terminalExecBoxliteImage.trim()
  if (!pythonExecBoxliteImage) {
    state.errors.push('WORKER_PYTHON_EXEC_BOXLITE_IMAGE is required.')
  }
  if (!terminalExecBoxliteImage) {
    state.errors.push('WORKER_TERMINAL_EXEC_BOXLITE_IMAGE is required.')
  }

  const pythonExecMemoryMib = parsePositiveInt(config.pythonExecMemoryMib, defaultBoxliteMemoryMib)
  if (!pythonExecMemoryMib.valid) {
    state.errors.push('WORKER_PYTHON_EXEC_MEMORY_MIB must be a positive integer.')
  }

  const pythonExecCpus = parsePositiveInt(config.pythonExecCpus, defaultBoxliteCpus)
  if (!pythonExecCpus.valid) {
    state.errors.push('WORKER_PYTHON_EXEC_CPUS must be a positive integer.')
  }

  const pythonExecMaxProcesses = parsePositiveInt(
    config.pythonExecMaxProcesses,
    defaultBoxliteMaxProcesses,
  )
  if (!pythonExecMaxProcesses.valid) {
    state.errors.push('WORKER_PYTHON_EXEC_MAX_PROCESSES must be a positive integer.')
  }

  const terminalExecMemoryMib = parsePositiveInt(
    config.terminalExecMemoryMib,
    defaultBoxliteMemoryMib,
  )
  if (!terminalExecMemoryMib.valid) {
    state.errors.push('WORKER_TERMINAL_EXEC_MEMORY_MIB must be a positive integer.')
  }

  const terminalExecCpus = parsePositiveInt(config.terminalExecCpus, defaultBoxliteCpus)
  if (!terminalExecCpus.valid) {
    state.errors.push('WORKER_TERMINAL_EXEC_CPUS must be a positive integer.')
  }

  const terminalExecMaxProcesses = parsePositiveInt(
    config.terminalExecMaxProcesses,
    defaultBoxliteMaxProcesses,
  )
  if (!terminalExecMaxProcesses.valid) {
    state.errors.push('WORKER_TERMINAL_EXEC_MAX_PROCESSES must be a positive integer.')
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

  const terminalExportMaxBytes = parseNonNegativeInt(
    config.terminalExportMaxBytes,
    defaultTerminalExportMaxBytes,
  )
  if (!terminalExportMaxBytes.valid) {
    state.errors.push('WORKER_TERMINAL_EXPORT_MAX_BYTES must be a non-negative integer.')
  }

  if (boxliteHome) {
    state.envEntries.push(['WORKER_BOXLITE_HOME', boxliteHome])
  }
  state.envEntries.push(['WORKER_PYTHON_EXEC_BOXLITE_IMAGE', pythonExecBoxliteImage])
  state.envEntries.push(['WORKER_PYTHON_EXEC_MEMORY_MIB', String(pythonExecMemoryMib.value)])
  state.envEntries.push(['WORKER_PYTHON_EXEC_CPUS', String(pythonExecCpus.value)])
  state.envEntries.push(['WORKER_PYTHON_EXEC_MAX_PROCESSES', String(pythonExecMaxProcesses.value)])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_BOXLITE_IMAGE', terminalExecBoxliteImage])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_MEMORY_MIB', String(terminalExecMemoryMib.value)])
  state.envEntries.push(['WORKER_TERMINAL_EXEC_CPUS', String(terminalExecCpus.value)])
  state.envEntries.push([
    'WORKER_TERMINAL_EXEC_MAX_PROCESSES',
    String(terminalExecMaxProcesses.value),
  ])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_MIN_SEC', String(terminalLeaseMinSec.value)])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_MAX_SEC', String(terminalLeaseMaxSec)])
  state.envEntries.push(['WORKER_TERMINAL_LEASE_DEFAULT_SEC', String(terminalLeaseDefaultSec)])
  state.envEntries.push([
    'WORKER_TERMINAL_OUTPUT_LIMIT_BYTES',
    String(terminalOutputLimitBytes.value),
  ])
  state.envEntries.push(['WORKER_TERMINAL_EXPORT_MAX_BYTES', String(terminalExportMaxBytes.value)])

  return buildResult(state, config.binaryPath.trim())
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
  if (config.startupPreset === 'temporary-probe') {
    return formatTemporaryProbeCommand(config)
  }

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
    state.envEntries.push([
      'WORKER_READ_IMAGE_ALLOWED_PATHS',
      JSON.stringify(readImageAllowedPaths),
    ])
  }

  return buildResult(state, config.binaryPath.trim())
}

export function getCurrentSiteTemporaryProbeValues(): { origin: string; grpcTarget: string } {
  if (typeof window === 'undefined') {
    return { origin: '', grpcTarget: ':50051' }
  }
  return {
    origin: window.location.origin,
    grpcTarget: `${window.location.hostname}:50051`,
  }
}

export function applyTemporaryProbePreset(config: WorkerSysStartupConfig): void {
  const { origin, grpcTarget } = getCurrentSiteTemporaryProbeValues()
  config.startupPreset = 'temporary-probe'
  config.consoleGRPCTarget = grpcTarget
  config.consoleInsecure = true
  config.nodeName = defaultTemporaryProbeNodeName
  config.computerUseCommandWhitelistMode = 'allow_all'
  config.computerUseCommandWhitelistText = ''
  config.readImageAllowedPathsText = '/'
  config.temporaryProbeInstallerOrigin = origin
  if (!config.temporaryProbeTag.trim()) {
    config.temporaryProbeTag = defaultTemporaryProbeInstallerTag
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
  const workerDockerConfig = reactive<WorkerDockerStartupConfig>(
    createDefaultWorkerDockerStartupConfig(),
  )
  const workerBoxliteConfig = reactive<WorkerBoxliteStartupConfig>(
    createDefaultWorkerBoxliteStartupConfig(),
  )
  const workerSysConfig = reactive<WorkerSysStartupConfig>(createDefaultWorkerSysStartupConfig())

  if (initial) {
    const target =
      workerKind.value === 'worker-docker'
        ? workerDockerConfig
        : workerKind.value === 'worker-boxlite'
          ? workerBoxliteConfig
          : workerSysConfig
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
  const workerBoxliteResult = computed(() => buildWorkerBoxliteStartupCommand(workerBoxliteConfig))
  const workerSysResult = computed(() => buildWorkerSysStartupCommand(workerSysConfig))

  const currentBuildResult = computed<StartupCommandBuildResult>(() => {
    if (workerKind.value === 'worker-docker') {
      return workerDockerResult.value
    }
    if (workerKind.value === 'worker-boxlite') {
      return workerBoxliteResult.value
    }
    return workerSysResult.value
  })

  const commandText = computed(() => currentBuildResult.value.command)
  const configTomlText = computed(() => currentBuildResult.value.configToml)
  const errorMessages = computed(() => currentBuildResult.value.errors)
  const warningMessages = computed(() => currentBuildResult.value.warnings)
  const canCopyCommand = computed(
    () => errorMessages.value.length === 0 && commandText.value.trim().length > 0,
  )
  const canDownloadConfigFile = computed(
    () => errorMessages.value.length === 0 && configTomlText.value.trim().length > 0,
  )

  function selectWorkerKind(kind: WorkerStartupKind): void {
    workerKind.value = kind
  }

  function selectTemporaryProbePreset(): void {
    workerKind.value = 'worker-sys'
    applyTemporaryProbePreset(workerSysConfig)
  }

  return {
    workerKind,
    workerDockerConfig,
    workerBoxliteConfig,
    workerSysConfig,
    commandText,
    configTomlText,
    errorMessages,
    warningMessages,
    canCopyCommand,
    canDownloadConfigFile,
    selectWorkerKind,
    selectTemporaryProbePreset,
  }
}
