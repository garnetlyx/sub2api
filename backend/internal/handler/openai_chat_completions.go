package handler

import (
	"context"
	"errors"
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

// ChatCompletions handles OpenAI Chat Completions API requests.
// POST /v1/chat/completions
func (h *OpenAIGatewayHandler) ChatCompletions(c *gin.Context) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

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
		"handler.openai_gateway.chat_completions",
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
	reqStream := gjson.GetBytes(body, "stream").Bool()

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream, body)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)

	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription); err != nil {
		reqLog.Info("openai_chat_completions.billing_eligibility_check_failed", zap.Error(err))
		status, code, message := billingErrorDetails(err)
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHash(c, body)
	promptCacheKey := h.gatewayService.ExtractSessionID(c, body)

	currentAPIKey := apiKey
	currentSubscription := subscription
	currentChannelMapping := channelMapping
	compatibilityFallbackUsed := false
	var compatibilityFallbackSourceErr *service.UpstreamFailoverError
	compatibilityScopeKey := h.gatewayService.BuildOpenAICompatibilityScopeKey("chat_completions", reqModel, body)
	fs := NewFailoverState(h.maxAccountSwitches, false)
	loadCompatibilityExcludedPlatforms(c.Request.Context(), reqLog, h.gatewayService, currentAPIKey.GroupID, compatibilityScopeKey, fs, "openai_chat_completions.compatibility_exclusion_cache_hit")

	for {
		c.Set("openai_chat_completions_fallback_model", "")
		reqLog.Debug("openai_chat_completions.account_selecting",
			zap.Int("excluded_account_count", len(fs.FailedAccountIDs)),
			zap.Int("excluded_platform_count", len(fs.ExcludedPlatforms)),
		)
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
			c.Request.Context(),
			currentAPIKey.GroupID,
			"",
			sessionHash,
			reqModel,
			fs.FailedAccountIDs,
			fs.ExcludedPlatforms,
			service.OpenAIUpstreamTransportAny,
			"chat",
		)
		if err != nil {
			diagCtx, diagCancel := openAIAccountSelectionDiagnosticContext(c.Request.Context())
			diag := h.gatewayService.DiagnoseAccountSelection(diagCtx, currentAPIKey.GroupID, reqModel)
			diagCancel()
			_, copilotCompatExcluded := fs.ExcludedPlatforms["copilot"]
			reqLog.Warn("openai_chat_completions.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(fs.FailedAccountIDs)),
				zap.Int("excluded_platform_count", len(fs.ExcludedPlatforms)),
				zap.Int("total_candidates", diag.TotalCandidates),
				zap.Int("unschedulable", diag.UnschedulableCount),
				zap.Int("rate_limited", diag.RateLimitedCount),
				zap.Int("overloaded", diag.OverloadedCount),
				zap.Int("temp_unschedulable", diag.TempUnschedulableCount),
				zap.Int("model_filtered", diag.ModelFilteredCount),
				zap.Bool("copilot_compat_excluded", copilotCompatExcluded),
			)
			if len(fs.FailedAccountIDs) == 0 {
				defaultModel := ""
				if currentAPIKey.Group != nil {
					defaultModel = currentAPIKey.Group.DefaultMappedModel
				}
				if defaultModel != "" && defaultModel != reqModel {
					reqLog.Info("openai_chat_completions.fallback_to_default_model",
						zap.String("default_mapped_model", defaultModel),
					)
					selection, scheduleDecision, err = h.gatewayService.SelectAccountWithSchedulerForCapability(
						c.Request.Context(),
						currentAPIKey.GroupID,
						"",
						sessionHash,
						defaultModel,
						fs.FailedAccountIDs,
						fs.ExcludedPlatforms,
						service.OpenAIUpstreamTransportAny,
						"chat",
					)
					if err == nil && selection != nil {
						c.Set("openai_chat_completions_fallback_model", defaultModel)
					}
				}
				if err != nil {
					if compatibilityFallbackSourceErr != nil {
						h.handleFailoverExhausted(c, compatibilityFallbackSourceErr, streamStarted)
						return
					}
					h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "Service temporarily unavailable", streamStarted)
					return
				}
			} else {
				if fs.LastFailoverErr != nil {
					h.handleFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
				} else {
					h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", streamStarted)
			return
		}
		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_chat_completions.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		accountReleaseFunc, acquired := h.acquireResponsesAccountSlot(c, currentAPIKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !acquired {
			return
		}

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()

		defaultMappedModel := resolveOpenAIForwardDefaultMappedModel(currentAPIKey, c.GetString("openai_chat_completions_fallback_model"))
		forwardBody := body
		if currentChannelMapping.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, currentChannelMapping.MappedModel)
		}
		result, err := h.gatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, promptCacheKey, defaultMappedModel)

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		if err == nil && result != nil && result.FirstTokenMs != nil {
			service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
		}
		if err != nil {
			var bridgeErr *service.OpenAIBridgeFallbackError
			if errors.As(err, &bridgeErr) {
				reqLog.Warn("openai_chat_completions.bridge_fallback",
					zap.Int64("account_id", account.ID),
					zap.String("account_name", account.Name),
					zap.String("bridge_platform", bridgeErr.BridgePlatform),
					zap.String("bridge_endpoint", bridgeErr.Endpoint),
					zap.String("reason", bridgeErr.Message),
				)
				bridgeAccount, bridgeLookupErr := h.gatewayService.FindSchedulableBridgeAccount(c.Request.Context(), bridgeErr.BridgePlatform)
				if bridgeLookupErr == nil && bridgeAccount != nil {
					result, err = h.gatewayService.ForwardBridgePassthrough(c.Request.Context(), c, bridgeAccount, bridgeErr.Endpoint, forwardBody)
					if err == nil {
						account = bridgeAccount
					}
				}
			}
		}
		if err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				recordCompatibilityExclusion(c.Request.Context(), reqLog, h.gatewayService, currentAPIKey.GroupID, compatibilityScopeKey, account.Platform, failoverErr, "openai_chat_completions.compatibility_exclusion_cache_store_failed")
				h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
				fallbackAPIKey, fallbackChannelMapping, switched, fallbackErr := tryCompatibilityFallbackGroup(
					c.Request.Context(),
					reqLog,
					h.apiKeyService,
					h.billingCacheService,
					h.gatewayService,
					currentAPIKey,
					reqModel,
					failoverErr,
					compatibilityFallbackUsed,
				)
				if fallbackErr != nil {
					status, code, message := billingErrorDetails(fallbackErr)
					h.handleStreamingAwareError(c, status, code, message, streamStarted)
					return
				}
				if switched {
					currentAPIKey = fallbackAPIKey
					currentSubscription = nil
					currentChannelMapping = fallbackChannelMapping
					compatibilityFallbackUsed = true
					compatibilityFallbackSourceErr = failoverErr
					fs = NewFailoverState(h.maxAccountSwitches, false)
					loadCompatibilityExcludedPlatforms(c.Request.Context(), reqLog, h.gatewayService, currentAPIKey.GroupID, compatibilityScopeKey, fs, "openai_chat_completions.compatibility_exclusion_cache_hit")
					fs.SwitchCount = 1
					fs.LastFailoverErr = failoverErr
					h.gatewayService.RecordOpenAIAccountSwitch()
					reqLog.Warn("openai_chat_completions.compatibility_failover_retrying",
						zap.String("requested_model", reqModel),
						zap.String("canonical_model", service.CanonicalizePublicModel(reqModel)),
						zap.String("compatibility_category", failoverErr.CompatibilityCategory),
						zap.Any("fallback_group_id", currentAPIKey.GroupID),
					)
					continue
				}
				action := fs.HandleFailoverError(c.Request.Context(), h.gatewayService, account.ID, account.Platform, failoverErr)
				switch action {
				case FailoverContinue:
					h.gatewayService.RecordOpenAIAccountSwitch()
					reqLog.Warn("openai_chat_completions.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", fs.SwitchCount),
						zap.Int("max_switches", fs.MaxSwitches),
						zap.String("reason", failoverErr.Reason),
						zap.String("compatibility_category", failoverErr.CompatibilityCategory),
					)
					continue
				case FailoverExhausted:
					h.handleFailoverExhausted(c, fs.LastFailoverErr, streamStarted)
					return
				case FailoverCanceled:
					return
				default:
					return
				}
			}
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, false, nil)
			wroteFallback := h.ensureForwardErrorResponse(c, streamStarted)
			reqLog.Warn("openai_chat_completions.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Bool("fallback_error_response_written", wroteFallback),
				zap.Error(err),
			)
			return
		}
		if result != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, result.FirstTokenMs)
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResult(account.ID, true, nil)
		}

		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)

		h.submitUsageRecordTask(func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:             result,
				APIKey:             currentAPIKey,
				User:               currentAPIKey.User,
				Account:            account,
				Subscription:       currentSubscription,
				InboundEndpoint:    GetInboundEndpoint(c),
				UpstreamEndpoint:   GetUpstreamEndpoint(c, account.Platform),
				UserAgent:          userAgent,
				IPAddress:          clientIP,
				APIKeyService:      h.apiKeyService,
				ChannelUsageFields: currentChannelMapping.ToUsageFields(reqModel, result.UpstreamModel),
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.chat_completions"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", currentAPIKey.ID),
					zap.Any("group_id", currentAPIKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_chat_completions.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_chat_completions.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", fs.SwitchCount),
			zap.Bool("compatibility_fallback_used", compatibilityFallbackUsed),
		)
		return
	}
}
