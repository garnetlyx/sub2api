import { apiClient } from '../client'

export interface CopilotDeviceFlowStartRequest {
  proxy_id?: number
}

export interface CopilotDeviceFlowStartResponse {
  session_id: string
  user_code: string
  verification_uri: string
  verification_uri_complete?: string
  expires_in?: number
  interval?: number
}

export interface CopilotDeviceFlowPollRequest {
  session_id: string
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

export async function startDeviceFlow(
  payload: CopilotDeviceFlowStartRequest = {}
): Promise<CopilotDeviceFlowStartResponse> {
  const { data } = await apiClient.post<CopilotDeviceFlowStartResponse>(
    '/admin/copilot/device-code/start',
    payload
  )
  return data
}

export async function pollDeviceFlow(
  payload: CopilotDeviceFlowPollRequest
): Promise<CopilotTokenInfo> {
  const { data } = await apiClient.post<CopilotTokenInfo>('/admin/copilot/device-code/poll', payload)
  return data
}

export default { startDeviceFlow, pollDeviceFlow }
