package service

import "strconv"

func OpenAITokenCacheKey(account *Account) string {
	return "openai:account:" + strconv.FormatInt(account.ID, 10)
}

// OpenAIUserTokenCacheKey generates a refresh lock key scoped by chatgpt_user_id
// so that accounts sharing the same underlying OpenAI user identity are serialized
// during token refresh. This prevents concurrent refresh calls from triggering
// OpenAI's refresh-token-reuse detection and subsequent token family revocation.
// Falls back to per-account key when chatgpt_user_id is unavailable.
func OpenAIUserTokenCacheKey(account *Account) string {
	uid := account.GetChatGPTUserID()
	if uid != "" {
		return "openai:user:" + uid
	}
	return OpenAITokenCacheKey(account)
}

// OpenAIWorkspaceTokenCacheKey generates a refresh lock key scoped by
// chatgpt_account_id (team workspace). Serializes refresh calls across
// distinct seat-holders of the same team workspace, complementing the
// per-user lock. ChatGPT Team/Business mandates a minimum of 2 seats, so
// multiple chatgpt_user_id values can share one workspace — without this
// lock, their refreshes race and OpenAI's seat-harvesting detection trips.
// Falls back to per-account key when chatgpt_account_id is unavailable.
func OpenAIWorkspaceTokenCacheKey(account *Account) string {
	ws := account.GetChatGPTAccountID()
	if ws != "" {
		return "openai:ws:" + ws
	}
	return OpenAITokenCacheKey(account)
}

func ClaudeTokenCacheKey(account *Account) string {
	return "claude:account:" + strconv.FormatInt(account.ID, 10)
}
