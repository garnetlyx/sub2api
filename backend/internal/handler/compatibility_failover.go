package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

type channelMappingResolver interface {
	ResolveChannelMappingAndRestrict(ctx context.Context, groupID *int64, model string) (service.ChannelMappingResult, bool)
}

func tryCompatibilityFallbackGroup(
	ctx context.Context,
	reqLog *zap.Logger,
	apiKeyService *service.APIKeyService,
	billingCacheService *service.BillingCacheService,
	resolver channelMappingResolver,
	currentAPIKey *service.APIKey,
	requestedModel string,
	failoverErr *service.UpstreamFailoverError,
	alreadyUsed bool,
) (*service.APIKey, service.ChannelMappingResult, bool, error) {
	if alreadyUsed || currentAPIKey == nil || currentAPIKey.Group == nil || failoverErr == nil || !failoverErr.IsCompatibilityMismatch() {
		return currentAPIKey, service.ChannelMappingResult{MappedModel: requestedModel}, false, nil
	}

	fallbackGroupID := currentAPIKey.Group.FallbackGroupID
	if fallbackGroupID == nil || *fallbackGroupID <= 0 {
		return currentAPIKey, service.ChannelMappingResult{MappedModel: requestedModel}, false, nil
	}
	if currentAPIKey.GroupID != nil && *currentAPIKey.GroupID == *fallbackGroupID {
		return currentAPIKey, service.ChannelMappingResult{MappedModel: requestedModel}, false, nil
	}
	if apiKeyService == nil {
		return currentAPIKey, service.ChannelMappingResult{MappedModel: requestedModel}, false, nil
	}

	fallbackGroup, err := apiKeyService.GetGroupByID(ctx, *fallbackGroupID)
	if err != nil || fallbackGroup == nil || !fallbackGroup.IsActive() {
		if reqLog != nil {
			reqLog.Warn("gateway.compatibility_fallback_group_unavailable",
				zap.Any("fallback_group_id", fallbackGroupID),
				zap.Error(err),
			)
		}
		return currentAPIKey, service.ChannelMappingResult{MappedModel: requestedModel}, false, nil
	}

	fallbackAPIKey := cloneAPIKeyWithGroup(currentAPIKey, fallbackGroup)
	if billingCacheService != nil {
		if err := billingCacheService.CheckBillingEligibility(ctx, fallbackAPIKey.User, fallbackAPIKey, fallbackGroup, nil); err != nil {
			return nil, service.ChannelMappingResult{}, false, err
		}
	}

	channelMapping := service.ChannelMappingResult{MappedModel: requestedModel}
	if resolver != nil {
		channelMapping, _ = resolver.ResolveChannelMappingAndRestrict(ctx, fallbackAPIKey.GroupID, requestedModel)
	}

	if reqLog != nil {
		reqLog.Warn("gateway.compatibility_fallback_group_switch",
			zap.String("requested_model", requestedModel),
			zap.String("canonical_model", service.CanonicalizePublicModel(requestedModel)),
			zap.Any("from_group_id", currentAPIKey.GroupID),
			zap.Int64("to_group_id", fallbackGroup.ID),
			zap.String("reason", failoverErr.Reason),
			zap.String("compatibility_category", failoverErr.CompatibilityCategory),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.String("upstream_message", service.ExtractUpstreamErrorMessage(failoverErr.ResponseBody)),
		)
	}

	return fallbackAPIKey, channelMapping, true, nil
}
