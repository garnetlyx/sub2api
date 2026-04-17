import { ref } from 'vue'
import { useAppStore } from '@/stores/app'
import copilotAPI, {
  type CopilotDeviceFlowStartResponse,
  type CopilotTokenInfo
} from '@/api/admin/copilot'

export function useCopilotOAuth() {
  const appStore = useAppStore()

  const authUrl = ref('')
  const sessionId = ref('')
  const loading = ref(false)
  const error = ref('')
  const userCode = ref('')
  const expiresIn = ref<number | null>(null)
  const interval = ref<number | null>(null)

  const resetState = () => {
    authUrl.value = ''
    sessionId.value = ''
    loading.value = false
    error.value = ''
    userCode.value = ''
    expiresIn.value = null
    interval.value = null
  }

  const applyDeviceFlow = (result: CopilotDeviceFlowStartResponse) => {
    authUrl.value = result.verification_uri_complete || result.verification_uri || ''
    sessionId.value = result.session_id
    userCode.value = result.user_code
    expiresIn.value = result.expires_in ?? null
    interval.value = result.interval ?? null
  }

  const startDeviceFlow = async (proxyId?: number | null): Promise<boolean> => {
    loading.value = true
    error.value = ''
    authUrl.value = ''
    sessionId.value = ''
    userCode.value = ''

    try {
      const result = await copilotAPI.startDeviceFlow({
        proxy_id: proxyId ?? undefined
      })
      applyDeviceFlow(result)
      return true
    } catch (err: any) {
      error.value = err.response?.data?.detail || err.message || 'Failed to start Copilot device code flow'
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  const pollDeviceFlow = async (
    currentSessionId: string,
    proxyId?: number | null
  ): Promise<CopilotTokenInfo | null> => {
    if (!currentSessionId.trim()) {
      error.value = 'Missing device session ID'
      return null
    }

    loading.value = true
    error.value = ''
    try {
      return await copilotAPI.pollDeviceFlow({
        session_id: currentSessionId.trim(),
        proxy_id: proxyId ?? undefined
      })
    } catch (err: any) {
      error.value = err.response?.data?.detail || err.message || 'Failed to complete Copilot device code flow'
      appStore.showError(error.value)
      return null
    } finally {
      loading.value = false
    }
  }

  const buildCredentials = (tokenInfo: CopilotTokenInfo): Record<string, unknown> => ({
    access_token: tokenInfo.access_token
  })

  const buildExtraInfo = (tokenInfo: CopilotTokenInfo): Record<string, unknown> | undefined => {
    const extra: Record<string, unknown> = {}
    if (tokenInfo.github_login) extra.github_login = tokenInfo.github_login
    if (tokenInfo.github_user_id) extra.github_user_id = tokenInfo.github_user_id
    if (tokenInfo.email) extra.email = tokenInfo.email
    if (tokenInfo.name) extra.name = tokenInfo.name
    if (Array.isArray(tokenInfo.available_models) && tokenInfo.available_models.length > 0) {
      extra.available_models = tokenInfo.available_models
    }
    return Object.keys(extra).length > 0 ? extra : undefined
  }

  return {
    authUrl,
    sessionId,
    loading,
    error,
    userCode,
    expiresIn,
    interval,
    resetState,
    startDeviceFlow,
    pollDeviceFlow,
    buildCredentials,
    buildExtraInfo
  }
}
