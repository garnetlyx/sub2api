import { ref } from 'vue'
import { useAppStore } from '@/stores/app'
import copilotAPI, { type CopilotTokenInfo } from '@/api/admin/copilot'

export function useCopilotOAuth() {
  const appStore = useAppStore()

  const authUrl = ref('')
  const sessionId = ref('')
  const loading = ref(false)
  const error = ref('')

  const resetState = () => {
    authUrl.value = ''
    sessionId.value = ''
    loading.value = false
    error.value = ''
  }

  const importAccessToken = async (
    accessToken: string,
    proxyId?: number | null
  ): Promise<CopilotTokenInfo | null> => {
    if (!accessToken.trim()) {
      error.value = 'Missing GitHub access token'
      return null
    }

    loading.value = true
    error.value = ''
    try {
      return await copilotAPI.importAccessToken({
        access_token: accessToken.trim(),
        proxy_id: proxyId ?? undefined
      })
    } catch (err: any) {
      error.value = err.response?.data?.detail || err.message || 'Failed to import GitHub access token'
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
    resetState,
    importAccessToken,
    buildCredentials,
    buildExtraInfo
  }
}
