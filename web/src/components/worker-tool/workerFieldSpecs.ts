export interface WorkerFieldSpec {
  /** Property name on the runtime config object. */
  key: string
  /** Environment variable or human label rendered above the control. */
  label: string
  hint: string
  kind: 'text' | 'number'
  placeholder?: string
  min?: number
  max?: number
  step?: number
  span?: boolean
}

const heartbeatFields: WorkerFieldSpec[] = [
  {
    key: 'heartbeatIntervalSec',
    label: 'WORKER_HEARTBEAT_INTERVAL_SEC',
    hint: 'Heartbeat interval in seconds; must be positive.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'heartbeatJitterPct',
    label: 'WORKER_HEARTBEAT_JITTER_PCT',
    hint: 'Random jitter percent applied to heartbeat scheduling.',
    kind: 'number',
    min: 0,
    max: 100,
  },
]

const terminalLeaseFields: WorkerFieldSpec[] = [
  {
    key: 'terminalLeaseMinSec',
    label: 'WORKER_TERMINAL_LEASE_MIN_SEC',
    hint: 'Minimum lease duration for terminal sessions.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalLeaseMaxSec',
    label: 'WORKER_TERMINAL_LEASE_MAX_SEC',
    hint: 'Maximum lease duration. Auto-raised to min if lower.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalLeaseDefaultSec',
    label: 'WORKER_TERMINAL_LEASE_DEFAULT_SEC',
    hint: 'Default lease; clamped into [min, max] range.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalOutputLimitBytes',
    label: 'WORKER_TERMINAL_OUTPUT_LIMIT_BYTES',
    hint: 'Per-stream stdout/stderr truncation limit in bytes.',
    kind: 'number',
    min: 1,
  },
]

export const dockerAdvancedFields: WorkerFieldSpec[] = [
  ...heartbeatFields,
  {
    key: 'pythonExecDockerImage',
    label: 'WORKER_PYTHON_EXEC_DOCKER_IMAGE',
    hint: 'Container image used for pythonExec capability.',
    kind: 'text',
  },
  {
    key: 'terminalExecDockerImage',
    label: 'WORKER_TERMINAL_EXEC_DOCKER_IMAGE',
    hint: 'Container image used for terminalExec sessions.',
    kind: 'text',
  },
  {
    key: 'pythonExecMemoryMib',
    label: 'WORKER_PYTHON_EXEC_MEMORY_MIB',
    hint: 'Memory limit for pythonExec containers in MiB.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalExecMemoryMib',
    label: 'WORKER_TERMINAL_EXEC_MEMORY_MIB',
    hint: 'Memory limit for terminalExec containers in MiB.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'pythonExecCpus',
    label: 'WORKER_PYTHON_EXEC_CPUS',
    hint: 'CPU limit for pythonExec containers.',
    kind: 'number',
    min: 0.1,
    step: 0.1,
  },
  {
    key: 'terminalExecCpus',
    label: 'WORKER_TERMINAL_EXEC_CPUS',
    hint: 'CPU limit for terminalExec containers.',
    kind: 'number',
    min: 0.1,
    step: 0.1,
  },
  {
    key: 'pythonExecMaxProcesses',
    label: 'WORKER_PYTHON_EXEC_MAX_PROCESSES',
    hint: 'Maximum process count inside pythonExec containers.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalExecMaxProcesses',
    label: 'WORKER_TERMINAL_EXEC_MAX_PROCESSES',
    hint: 'Maximum process count inside terminalExec containers.',
    kind: 'number',
    min: 1,
  },
  ...terminalLeaseFields,
  {
    key: 'terminalExportMaxBytes',
    label: 'WORKER_TERMINAL_EXPORT_MAX_BYTES',
    hint: 'Maximum export file size in bytes; 0 disables the limit.',
    kind: 'number',
    min: 0,
  },
]

export const boxliteAdvancedFields: WorkerFieldSpec[] = [
  ...heartbeatFields,
  {
    key: 'pythonExecBoxliteImage',
    label: 'WORKER_PYTHON_EXEC_BOXLITE_IMAGE',
    hint: 'Boxlite image used for one-shot pythonExec workloads.',
    kind: 'text',
  },
  {
    key: 'terminalExecBoxliteImage',
    label: 'WORKER_TERMINAL_EXEC_BOXLITE_IMAGE',
    hint: 'Boxlite image used for terminalExec and terminalResource sessions.',
    kind: 'text',
  },
  {
    key: 'pythonExecMemoryMib',
    label: 'WORKER_PYTHON_EXEC_MEMORY_MIB',
    hint: 'Python VM memory limit in MiB.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalExecMemoryMib',
    label: 'WORKER_TERMINAL_EXEC_MEMORY_MIB',
    hint: 'Terminal VM memory limit in MiB.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'pythonExecCpus',
    label: 'WORKER_PYTHON_EXEC_CPUS',
    hint: 'CPU count allocated to pythonExec boxes.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalExecCpus',
    label: 'WORKER_TERMINAL_EXEC_CPUS',
    hint: 'CPU count allocated to terminal boxes.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'pythonExecMaxProcesses',
    label: 'WORKER_PYTHON_EXEC_MAX_PROCESSES',
    hint: 'Maximum process count inside pythonExec boxes.',
    kind: 'number',
    min: 1,
  },
  {
    key: 'terminalExecMaxProcesses',
    label: 'WORKER_TERMINAL_EXEC_MAX_PROCESSES',
    hint: 'Maximum process count inside terminal boxes.',
    kind: 'number',
    min: 1,
  },
  ...terminalLeaseFields,
  {
    key: 'terminalExportMaxBytes',
    label: 'WORKER_TERMINAL_EXPORT_MAX_BYTES',
    hint: 'Maximum export payload size in bytes. Use 0 for unlimited.',
    kind: 'number',
    min: 0,
    span: true,
  },
]

export const sysAdvancedNumberFields: WorkerFieldSpec[] = [
  ...heartbeatFields,
  {
    key: 'computerUseOutputLimitBytes',
    label: 'WORKER_COMPUTER_USE_OUTPUT_LIMIT_BYTES',
    hint: 'Per-stream stdout/stderr truncation limit in bytes.',
    kind: 'number',
    min: 1,
  },
]
