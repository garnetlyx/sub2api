import { apiClient } from '../client'

export interface CopilotAccessTokenRequest {
  access_token: string
  proxy_id?: number
}

export interface CopilotTokenInfo {
  github_login?: string
  github_user_id?: number
  email?: string
  name?: string
  access_token?: string
  available_models?: string[]
  [key: string]: unknown
}

export async function importAccessToken(
  payload: CopilotAccessTokenRequest
): Promise<CopilotTokenInfo> {
  const { data } = await apiClient.post<CopilotTokenInfo>('/admin/copilot/access-token', payload)
  return data
}

export default { importAccessToken }
