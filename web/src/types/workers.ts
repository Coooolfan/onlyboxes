export type WorkerStatus = 'all' | 'online' | 'offline'
export type WorkerType = 'normal' | 'worker-sys'

export interface CapabilityDeclaration {
  name: string
}

export interface WorkerItem {
  node_id: string
  node_name: string
  executor_kind: string
  capabilities: CapabilityDeclaration[]
  labels: Record<string, string>
  version: string
  status: Exclude<WorkerStatus, 'all'>
  registered_at: string
  last_seen_at: string
}

export interface WorkerListResponse {
  items: WorkerItem[]
  total: number
  page: number
  page_size: number
}

export interface WorkerStatsResponse {
  total: number
  online: number
  offline: number
  stale: number
  stale_after_sec: number
  generated_at: string
}

export interface CapabilityInflightItem {
  name: string
  inflight: number
  max_inflight: number
}

export interface WorkerInflightItem {
  node_id: string
  active_session_count: number
  capabilities: CapabilityInflightItem[]
}

export interface WorkerInflightResponse {
  workers: WorkerInflightItem[]
  generated_at: string
}

export interface WorkerStartupCommandResponse {
  node_id: string
  type: WorkerType
  worker_secret: string
}

export interface TrustedTokenItem {
  id: string
  name: string
  token_masked: string
  created_at: string
  updated_at: string
}

export interface TrustedTokenListResponse {
  items: TrustedTokenItem[]
  total: number
}

export interface TrustedTokenCreateResponse {
  id: string
  name: string
  token: string
  token_masked: string
  generated: boolean
  created_at: string
  updated_at: string
}

export interface TrustedTokenCreateInput {
  name: string
}

export interface APIKeyItem {
  id: string
  name: string
  key_masked: string
  created_at: string
  updated_at: string
}

export interface APIKeyListResponse {
  items: APIKeyItem[]
  total: number
}

export interface APIKeyCreateResponse {
  id: string
  name: string
  key: string
  key_masked: string
  created_at: string
  updated_at: string
}

export interface APIKeyCreateInput {
  name: string
}
