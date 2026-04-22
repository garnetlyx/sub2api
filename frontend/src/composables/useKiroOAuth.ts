import { ref } from 'vue'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'

export interface KiroTokenInfo {
  access_token?: string
  refresh_token?: string
  profile_arn?: string
  region?: string
  idp?: string
  expires_in?: number
  available_models?: string[]
  [key: string]: unknown
}

export function useKiroOAuth() {
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

  const generateAuthUrl = async (
    region: string,
    idp: string = 'Google',
    proxyId?: number | null
  ): Promise<boolean> => {
    loading.value = true
    authUrl.value = ''
    sessionId.value = ''
    error.value = ''

    try {
      const payload: Record<string, unknown> = {
        region: region || 'us-east-1',
        idp: idp || 'Google'
      }
      if (proxyId) payload.proxy_id = proxyId

      const response = await adminAPI.kiro.generateAuthUrl(payload as any)
      authUrl.value = response.auth_url
      sessionId.value = response.session_id
      return true
    } catch (err: any) {
      error.value = err.response?.data?.detail || err.message || 'Failed to generate Kiro auth URL'
      appStore.showError(error.value)
      return false
    } finally {
      loading.value = false
    }
  }

  const createFromOAuth = async (params: {
    code: string
    state: string
    name?: string
    concurrency?: number
    priority?: number
    groupIds?: number[]
    proxyId?: number | null
  }): Promise<KiroTokenInfo | null> => {
    const code = params.code?.trim()
    if (!code || !sessionId.value || !params.state?.trim()) {
      error.value = 'Missing required OAuth parameters'
      return null
    }

    loading.value = true
    error.value = ''

    try {
      const payload: Record<string, unknown> = {
        session_id: sessionId.value,
        code,
        state: params.state.trim(),
        name: params.name || '',
        concurrency: params.concurrency || 3,
        priority: params.priority || 50
      }
      if (params.groupIds?.length) payload.group_ids = params.groupIds
      if (params.proxyId) payload.proxy_id = params.proxyId

      const result = await adminAPI.kiro.createFromOAuth(payload as any)
      return result as KiroTokenInfo
    } catch (err: any) {
      error.value = err.response?.data?.detail || err.message || 'Failed to create Kiro account'
      appStore.showError(error.value)
      return null
    } finally {
      loading.value = false
    }
  }

  return {
    authUrl,
    sessionId,
    loading,
    error,
    resetState,
    generateAuthUrl,
    createFromOAuth
  }
}
