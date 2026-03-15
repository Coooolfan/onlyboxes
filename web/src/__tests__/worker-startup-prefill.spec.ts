import { describe, expect, it } from 'vitest'

import { consumePrefill, setPrefill } from '@/composables/useWorkerStartupPrefill'

describe('useWorkerStartupPrefill', () => {
  it('returns null when no data has been set', () => {
    expect(consumePrefill()).toBeNull()
  })

  it('returns data after setPrefill', () => {
    setPrefill({
      workerKind: 'worker-docker',
      workerID: 'node-1',
      workerSecret: 'secret-1',
    })
    const result = consumePrefill()
    expect(result).toEqual({
      workerKind: 'worker-docker',
      workerID: 'node-1',
      workerSecret: 'secret-1',
    })
  })

  it('returns null on second consume (one-time consumption)', () => {
    setPrefill({
      workerKind: 'worker-sys',
      workerID: 'node-2',
      workerSecret: 'secret-2',
    })
    consumePrefill()
    expect(consumePrefill()).toBeNull()
  })

  it('overwrites previous data on consecutive setPrefill calls', () => {
    setPrefill({
      workerKind: 'worker-docker',
      workerID: 'first',
      workerSecret: 'first-secret',
    })
    setPrefill({
      workerKind: 'worker-sys',
      workerID: 'second',
      workerSecret: 'second-secret',
      consoleGRPCTarget: 'example.com:50051',
    })
    const result = consumePrefill()
    expect(result).toEqual({
      workerKind: 'worker-sys',
      workerID: 'second',
      workerSecret: 'second-secret',
      consoleGRPCTarget: 'example.com:50051',
    })
  })
})
