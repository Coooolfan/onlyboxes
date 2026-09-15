import { defaultConsoleRepoURL, defaultConsoleVersion } from '@/constants/console'
import { parseAPIError, request } from '@/services/http'
import type {
  AccountListItem,
  AccountListResponse,
  AccountProfile,
  ConsoleSessionPayload,
  RegisterAccountPayload,
} from '@/types/auth'

export class InvalidCredentialsError extends Error {
  constructor() {
    super('invalid credentials')
    this.name = 'InvalidCredentialsError'
  }
}

export function isInvalidCredentialsError(error: unknown): error is InvalidCredentialsError {
  return error instanceof InvalidCredentialsError
}

function parseAccountProfile(payload: unknown): AccountProfile {
  const value = payload as Partial<AccountProfile> | null
  const accountID = value?.account_id?.trim() ?? ''
  const username = value?.username?.trim() ?? ''
  const isAdmin = value?.is_admin === true
  if (!accountID || !username) {
    throw new Error('API returned invalid account profile.')
  }
  return {
    account_id: accountID,
    username,
    is_admin: isAdmin,
  }
}

function parseSessionPayload(payload: unknown): ConsoleSessionPayload {
  const value = payload as Partial<ConsoleSessionPayload> | null
  const consoleVersion =
    typeof value?.console_version === 'string' ? value.console_version.trim() : ''
  const consoleRepoURL =
    typeof value?.console_repo_url === 'string' ? value.console_repo_url.trim() : ''
  return {
    authenticated: value?.authenticated === true,
    account: parseAccountProfile(value?.account),
    registration_enabled: value?.registration_enabled === true,
    console_version: consoleVersion || defaultConsoleVersion,
    console_repo_url: consoleRepoURL || defaultConsoleRepoURL,
  }
}

function parseAccountListItem(payload: unknown): AccountListItem {
  const value = payload as Partial<AccountListItem> | null
  const profile = parseAccountProfile(value)
  const createdAt = typeof value?.created_at === 'string' ? value.created_at : ''
  const updatedAt = typeof value?.updated_at === 'string' ? value.updated_at : ''

  return {
    ...profile,
    created_at: createdAt,
    updated_at: updatedAt,
  }
}

export async function loginAPI(username: string, password: string): Promise<ConsoleSessionPayload> {
  const response = await fetch('/api/v1/auth/login', {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
    credentials: 'same-origin',
    body: JSON.stringify({ username, password }),
  })

  if (response.status === 401) {
    throw new InvalidCredentialsError()
  }
  if (!response.ok) {
    throw new Error(await parseAPIError(response))
  }

  return parseSessionPayload(await response.json())
}

export async function logoutAPI(): Promise<void> {
  await fetch('/api/v1/auth/logout', {
    method: 'POST',
    headers: {
      Accept: 'application/json',
    },
    credentials: 'same-origin',
  })
}

export async function probeSessionAPI(): Promise<ConsoleSessionPayload> {
  const response = await request('/api/v1/auth/session')
  if (!response.ok) {
    throw new Error(await parseAPIError(response))
  }
  return parseSessionPayload(await response.json())
}

export async function createAccountAPI(
  username: string,
  password: string,
): Promise<RegisterAccountPayload> {
  const response = await request('/api/v1/accounts', {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ username, password }),
  })
  if (!response.ok) {
    throw new Error(await parseAPIError(response))
  }

  const payload = (await response.json()) as Partial<RegisterAccountPayload> | null
  return {
    account: parseAccountProfile(payload?.account),
    created_at: typeof payload?.created_at === 'string' ? payload.created_at : '',
    updated_at: typeof payload?.updated_at === 'string' ? payload.updated_at : '',
  }
}

export async function changePasswordAPI(
  currentPassword: string,
  newPassword: string,
): Promise<void> {
  const response = await request('/api/v1/auth/password', {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      current_password: currentPassword,
      new_password: newPassword,
    }),
  })
  if (!response.ok) {
    throw new Error(await parseAPIError(response))
  }
}

export async function fetchAccountsAPI(
  page: number,
  pageSize: number,
  signal: AbortSignal,
): Promise<AccountListResponse> {
  const query = new URLSearchParams({
    page: String(page),
    page_size: String(pageSize),
  })
  const response = await request(`/api/v1/accounts?${query.toString()}`, { signal })
  if (!response.ok) {
    throw new Error(await parseAPIError(response))
  }
  const payload = (await response.json()) as Partial<AccountListResponse> | null
  const items = Array.isArray(payload?.items) ? payload.items.map(parseAccountListItem) : []
  return {
    items,
    total: typeof payload?.total === 'number' ? payload.total : items.length,
    page: typeof payload?.page === 'number' ? payload.page : page,
    page_size: typeof payload?.page_size === 'number' ? payload.page_size : pageSize,
  }
}

export async function deleteAccountAPI(accountID: string): Promise<void> {
  const response = await request(`/api/v1/accounts/${encodeURIComponent(accountID)}`, {
    method: 'DELETE',
  })
  if (response.status === 204) {
    return
  }
  if (!response.ok) {
    throw new Error(await parseAPIError(response))
  }
}
