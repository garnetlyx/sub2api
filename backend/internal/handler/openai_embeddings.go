package handler

import (
	"context"
	"net/http"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Embeddings handles OpenAI Embeddings API requests.
// POST /v1/embeddings
func (h *OpenAIGatewayHandler) Embeddings(c *gin.Context) {
	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.embeddings",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}
	if !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	reqLog = reqLog.With(zap.String("model", reqModel))

	setOpsRequestContext(c, reqModel, false, body)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))

	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, new(bool), reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription); err != nil {
		reqLog.Info("openai_embeddings.billing_eligibility_check_failed", zap.Error(err))
		status, code, message := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	account, err := h.gatewayService.FindSchedulableEmbeddingAccount(c.Request.Context(), reqModel)
	if err != nil || account == nil {
		reqLog.Warn("openai_embeddings.account_select_failed", zap.Error(err))
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Service temporarily unavailable")
		return
	}

	selection := &service.AccountSelectionResult{
		Account: account,
		WaitPlan: &service.AccountWaitPlan{
			AccountID:      account.ID,
			MaxConcurrency: account.Concurrency,
			Timeout:        30 * time.Second,
			MaxWaiting:     100,
		},
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, false, new(bool), reqLog)
	if !acquired {
		return
	}

	service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
	forwardStart := time.Now()

	forwardBody := body
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
	}
	result, err := h.gatewayService.ForwardBridgePassthrough(c.Request.Context(), c, account, "/v1/embeddings", forwardBody)

	forwardDurationMs := time.Since(forwardStart).Milliseconds()
	if accountReleaseFunc != nil {
		accountReleaseFunc()
	}
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, forwardDurationMs)
	if err != nil {
		h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
		wroteFallback := h.ensureForwardErrorResponse(c, false)
		reqLog.Warn("openai_embeddings.forward_failed",
			zap.Int64("account_id", account.ID),
			zap.Bool("fallback_error_response_written", wroteFallback),
			zap.Error(err),
		)
		return
	}
	h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)

	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	h.submitUsageRecordTask(func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			InboundEndpoint:    GetInboundEndpoint(c),
			UpstreamEndpoint:   GetUpstreamEndpoint(c, account.Platform),
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			APIKeyService:      h.apiKeyService,
			ChannelUsageFields: channelMapping.ToUsageFields(reqModel, result.UpstreamModel),
		}); err != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.embeddings"),
				zap.Int64("user_id", subject.UserID),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.String("model", reqModel),
				zap.Int64("account_id", account.ID),
			).Error("openai_embeddings.record_usage_failed", zap.Error(err))
		}
	})
}
