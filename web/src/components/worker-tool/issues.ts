export interface WorkerIssue {
  level: 'error' | 'warning'
  message: string
}

export function toWorkerIssues(errors: string[], warnings: string[]): WorkerIssue[] {
  return [
    ...errors.map((message) => ({ level: 'error' as const, message })),
    ...warnings.map((message) => ({ level: 'warning' as const, message })),
  ]
}
