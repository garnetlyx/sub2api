package service

// resolveOpenAIForwardModel determines the upstream model for OpenAI-compatible
// forwarding. Passthrough is the default: group-level defaults are only used
// when the caller explicitly retried routing with that fallback model.
func resolveOpenAIForwardModel(account *Account, requestedModel, defaultMappedModel string) string {
	if account == nil {
		return requestedModel
	}

	if defaultMappedModel != "" && defaultMappedModel == requestedModel {
		return defaultMappedModel
	}
	mappedModel, _ := account.ResolveUpstreamModel(requestedModel)
	return mappedModel
}
