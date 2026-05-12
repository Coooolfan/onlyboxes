export type WorkerStartupKind = 'worker-docker' | 'worker-boxlite' | 'worker-sys'

export type WorkerCallTimeoutMode = 'auto' | 'manual'

export type WorkerSysWhitelistMode = 'exact' | 'prefix' | 'allow_all'

export type WorkerSysStartupPreset = 'custom' | 'temporary-probe'

export interface WorkerStartupBaseConfig {
  workerID: string
  workerSecret: string
  consoleGRPCTarget: string
  consoleInsecure: boolean
  heartbeatIntervalSec: number
  heartbeatJitterPct: number
  callTimeoutMode: WorkerCallTimeoutMode
  callTimeoutSec: number
  binaryPath: string
  nodeName: string
  version: string
  labelsText: string
}

export interface WorkerDockerStartupConfig extends WorkerStartupBaseConfig {
  pythonExecDockerImage: string
  pythonExecMemoryMib: number
  pythonExecCpus: number
  pythonExecMaxProcesses: number
  terminalExecDockerImage: string
  terminalExecMemoryMib: number
  terminalExecCpus: number
  terminalExecMaxProcesses: number
  terminalLeaseMinSec: number
  terminalLeaseMaxSec: number
  terminalLeaseDefaultSec: number
  terminalOutputLimitBytes: number
  terminalExportMaxBytes: number
}

export interface WorkerBoxliteStartupConfig extends WorkerStartupBaseConfig {
  boxliteHome: string
  pythonExecBoxliteImage: string
  pythonExecMemoryMib: number
  pythonExecCpus: number
  pythonExecMaxProcesses: number
  terminalExecBoxliteImage: string
  terminalExecMemoryMib: number
  terminalExecCpus: number
  terminalExecMaxProcesses: number
  terminalLeaseMinSec: number
  terminalLeaseMaxSec: number
  terminalLeaseDefaultSec: number
  terminalOutputLimitBytes: number
  terminalExportMaxBytes: number
}

export interface WorkerSysStartupConfig extends WorkerStartupBaseConfig {
  startupPreset: WorkerSysStartupPreset
  temporaryProbeInstallerOrigin: string
  temporaryProbeTag: string
  computerUseOutputLimitBytes: number
  computerUseCommandWhitelistMode: WorkerSysWhitelistMode
  computerUseCommandWhitelistText: string
  readImageAllowedPathsText: string
}

export interface StartupCommandBuildResult {
  command: string
  errors: string[]
  warnings: string[]
}
