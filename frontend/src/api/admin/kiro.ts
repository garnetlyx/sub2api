import { apiClient } from '../client'

export interface KiroAuthUrlRequest {
  region?: string
  idp?: string
  proxy_id?: number
}

export interface KiroAuthUrlResponse {
  session_id: string
  auth_url: string
}

export interface KiroExchangeCodeRequest {
  session_id: string
  code: string
  state: string
  proxy_id?: number
  name?: string
  concurrency?: number
  priority?: number
  group_ids?: number[]
}

export interface KiroImportRefreshTokenRequest {
  refresh_token: string
  region?: string
  proxy_id?: number
}

export async function generateAuthUrl(
  payload: KiroAuthUrlRequest
): Promise<KiroAuthUrlResponse> {
  const { data } = await apiClient.post<KiroAuthUrlResponse>(
    '/admin/kiro/auth-url',
    payload
  )
  return data
}

export async function exchangeCode(
  payload: KiroExchangeCodeRequest
): Promise<Record<string, unknown>> {
  const { data } = await apiClient.post<Record<string, unknown>>(
    '/admin/kiro/exchange-code',
    payload
  )
  return data
}

export async function createFromOAuth(
  payload: KiroExchangeCodeRequest
): Promise<Record<string, unknown>> {
  const { data } = await apiClient.post<Record<string, unknown>>(
    '/admin/kiro/create-from-oauth',
    payload
  )
  return data
}

export async function importRefreshToken(
  payload: KiroImportRefreshTokenRequest
): Promise<Record<string, unknown>> {
  const { data } = await apiClient.post<Record<string, unknown>>(
    '/admin/kiro/import',
    payload
  )
  return data
}

export async function refreshAccountToken(
  accountId: number
): Promise<Record<string, unknown>> {
  const { data } = await apiClient.post<Record<string, unknown>>(
    `/admin/kiro/${accountId}/refresh`
  )
  return data
}

export default {
  generateAuthUrl,
  exchangeCode,
  createFromOAuth,
  importRefreshToken,
  refreshAccountToken
}
