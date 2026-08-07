package service

import (
	"context"
	"sort"
	"strings"
	"time"
)

// BlockedByReason is a standardized enum explaining why an account cannot
// currently serve a requested model. Values are stable for tooling.
type BlockedByReason string

const (
	BlockedByNone              BlockedByReason = "" // account can serve the model now
	BlockedByRateLimit         BlockedByReason = "rate_limit"
	BlockedByOverloaded        BlockedByReason = "overloaded"
	BlockedByTempUnschedulable BlockedByReason = "temp_unschedulable"
	BlockedByNotSchedulable    BlockedByReason = "not_schedulable"
	BlockedByError             BlockedByReason = "error"
	BlockedByModelNotFound     BlockedByReason = "model_not_found"
	BlockedByNoCapability      BlockedByReason = "no_capability"
	BlockedByKnownIssue        BlockedByReason = "known_issue_excluded"
	BlockedByEmptyEndpoint     BlockedByReason = "empty_endpoint"
	// BlockedByUnknown covers platforms whose model-support check could not be
	// resolved at diagnostic time (e.g. lookup failed or platform unsupported).
	BlockedByUnknown BlockedByReason = "unknown"
)

// ModelAvailabilityAccount is the per-account view in GetModelAvailability.
type ModelAvailabilityAccount struct {
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	Platform    string `json:"platform"`
	Type        string `json:"type"`
	Kind        string `json:"kind"`

	// Schedulable state (independent of the requested model).
	State     string          `json:"state"`
	CanServe  bool            `json:"can_serve"`
	BlockedBy BlockedByReason `json:"blocked_by"`

	// Model-fit detail, populated when the live model source was consulted
	// regardless of schedulable state. For a rate_limited account this lets
	// operators distinguish "has the model but rate-limited" from "rate-limited
	// AND model absent".
	ModelInList   bool  `json:"model_in_list"`
	ModelListSize int   `json:"model_list_size"`
	Note          string `json:"note,omitempty"`

	// Scheduling timing fields (only set when relevant).
	RateLimitResetAt       *time.Time `json:"rate_limit_reset_at,omitempty"`
	OverloadUntil          *time.Time `json:"overload_until,omitempty"`
	TempUnschedulableUntil *time.Time `json:"temp_unschedulable_until,omitempty"`
}

// ModelAvailabilitySummary aggregates per-account outcomes.
type ModelAvailabilitySummary struct {
	Total              int `json:"total_accounts"`
	CanServe           int `json:"can_serve"`
	RateLimited        int `json:"rate_limited"`
	Overloaded         int `json:"overloaded"`
	TempUnschedulable  int `json:"temp_unschedulable"`
	NotSchedulable     int `json:"not_schedulable"`
	Error              int `json:"error"`
	ModelNotFound      int `json:"model_not_found"`
	NoCapability       int `json:"no_capability"`
	KnownIssueExcluded int `json:"known_issue_excluded"`
	EmptyEndpoint      int `json:"empty_endpoint"`
}

// ModelAvailabilityResult is the response shape of GetModelAvailability.
type ModelAvailabilityResult struct {
	Model      string                     `json:"model"`
	Available  bool                       `json:"available"`
	Summary    ModelAvailabilitySummary   `json:"summary"`
	Accounts   []ModelAvailabilityAccount `json:"accounts"`
	CollectedAt time.Time                 `json:"collected_at"`
}

// GetModelAvailability reports, for a given model, every account's ability to
// serve it and a standardized reason when it cannot. It is a read-only
// diagnostic that reuses existing gateway supports checks and the cached live
// model sources; it does not mutate scheduler or rate-limit state.
//
// The model-fit check runs even for rate-limited / overloaded accounts because
// the cached live model list is independent of the runtime scheduling state —
// this lets operators tell "rate-limited but has the model" (the model will
// come back when the window resets) from "rate-limited AND model absent".
func (s *OpsService) GetModelAvailability(ctx context.Context, requestedModel string) (*ModelAvailabilityResult, error) {
	if s == nil {
		return nil, nil
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return nil, errModelAvailabilityEmptyModel
	}

	accounts, err := s.listAllAccountsForOps(ctx, "")
	if err != nil {
		return nil, err
	}

	now := time.Now()
	out := &ModelAvailabilityResult{
		Model:       requestedModel,
		Accounts:    make([]ModelAvailabilityAccount, 0, len(accounts)),
		CollectedAt: now,
	}

	for i := range accounts {
		acc := &accounts[i]
		if acc.ID <= 0 {
			continue
		}
		item := s.classifyAccountForModel(ctx, acc, requestedModel, now)
		out.Accounts = append(out.Accounts, item)
		out.Summary.Total++
		switch item.BlockedBy {
		case BlockedByNone:
			out.Summary.CanServe++
		case BlockedByRateLimit:
			out.Summary.RateLimited++
		case BlockedByOverloaded:
			out.Summary.Overloaded++
		case BlockedByTempUnschedulable:
			out.Summary.TempUnschedulable++
		case BlockedByNotSchedulable:
			out.Summary.NotSchedulable++
		case BlockedByError:
			out.Summary.Error++
		case BlockedByModelNotFound:
			out.Summary.ModelNotFound++
		case BlockedByNoCapability:
			out.Summary.NoCapability++
		case BlockedByKnownIssue:
			out.Summary.KnownIssueExcluded++
		case BlockedByEmptyEndpoint:
			out.Summary.EmptyEndpoint++
		}
		if item.CanServe {
			out.Available = true
		}
	}

	sort.SliceStable(out.Accounts, func(i, j int) bool {
		if out.Accounts[i].BlockedBy != out.Accounts[j].BlockedBy {
			return out.Accounts[i].BlockedBy < out.Accounts[j].BlockedBy
		}
		return out.Accounts[i].AccountID < out.Accounts[j].AccountID
	})
	return out, nil
}

// classifyAccountForModel derives the per-account model-availability row.
// State assessment (rate_limit/overload/etc.) is separated from model-fit
// assessment so that a rate_limited account still reports whether the model
// is present in its live catalog.
func (s *OpsService) classifyAccountForModel(ctx context.Context, acc *Account, requestedModel string, now time.Time) ModelAvailabilityAccount {
	item := ModelAvailabilityAccount{
		AccountID:   acc.ID,
		AccountName: acc.Name,
		Platform:    acc.Platform,
		Type:        acc.Type,
		Kind:        acc.AccountKind(),
	}

	// 1. Scheduling state — derived the same way as ops_account_availability.go.
	state, schedulingBlockedBy := assessSchedulingState(acc, now)
	item.State = state
	switch schedulingBlockedBy {
	case BlockedByRateLimit:
		item.BlockedBy = BlockedByRateLimit
		item.RateLimitResetAt = acc.RateLimitResetAt
	case BlockedByOverloaded:
		item.BlockedBy = BlockedByOverloaded
		item.OverloadUntil = acc.OverloadUntil
	case BlockedByTempUnschedulable:
		item.BlockedBy = BlockedByTempUnschedulable
		item.TempUnschedulableUntil = acc.TempUnschedulableUntil
	case BlockedByNotSchedulable:
		item.BlockedBy = BlockedByNotSchedulable
	case BlockedByError:
		item.BlockedBy = BlockedByError
	default:
		item.BlockedBy = BlockedByNone
	}

	// 2. Model-fit — always run, regardless of scheduling state, using the
	//    cached live model source. This is the cheap path; cache miss will
	//    trigger a single upstream lookup which is acceptable for an admin
	//    diagnostic endpoint.
	modelFit, modelNote := s.assessModelFit(ctx, acc, requestedModel)
	item.ModelInList = modelFit.inList
	item.ModelListSize = modelFit.listSize
	if modelNote != "" {
		item.Note = modelNote
	}

	// 3. Combine: a scheduling-blocked account is blocked by its scheduling
	//    reason even if it has the model. A schedulable account is blocked by
	//    whatever the model-fit check failed on (if anything).
	if schedulingBlockedBy == BlockedByNone {
		item.BlockedBy = modelFit.blockedBy
		item.CanServe = modelFit.supported
	} else {
		// Scheduling-blocked: can_serve reflects model-fit alone (would it work
		// if scheduling unblocked?). This is the "has model but rate-limited"
		// distinction.
		item.CanServe = false
	}
	return item
}

// assessSchedulingState mirrors the state derivation in
// ops_account_availability.go (line ~53-68). Kept inline rather than shared to
// keep the diagnostic self-contained and avoid coupling to dashboard-specific
// status normalization (which suppresses rate_limit when status=error).
func assessSchedulingState(acc *Account, now time.Time) (string, BlockedByReason) {
	if acc.Status == StatusError {
		return "error", BlockedByError
	}
	if acc.TempUnschedulableUntil != nil && now.Before(*acc.TempUnschedulableUntil) {
		return "temp_unschedulable", BlockedByTempUnschedulable
	}
	if acc.RateLimitResetAt != nil && now.Before(*acc.RateLimitResetAt) {
		return "rate_limited", BlockedByRateLimit
	}
	if acc.OverloadUntil != nil && now.Before(*acc.OverloadUntil) {
		return "overloaded", BlockedByOverloaded
	}
	if acc.Status != StatusActive || !acc.Schedulable {
		return "inactive", BlockedByNotSchedulable
	}
	return "active", BlockedByNone
}

// modelFitResult captures model-fit assessment.
type modelFitResult struct {
	supported  bool
	inList     bool
	listSize   int
	blockedBy  BlockedByReason
}

// assessModelFit dispatches to the appropriate gateway's supports check,
// returning a fine-grained reason. It reuses the cached live model source and
// the known-issue exclusion policy — no new upstream contracts.
func (s *OpsService) assessModelFit(ctx context.Context, acc *Account, requestedModel string) (modelFitResult, string) {
	// Bridge accounts are protocol translators only; they have no real catalog.
	if isInternalLiteLLMBridgeOnlyAccount(acc) {
		return modelFitResult{blockedBy: BlockedByEmptyEndpoint}, "internal litellm bridge (no own catalog)"
	}

	switch {
	case acc.UsesOpenAIGateway():
		return s.assessOpenAIGatewayModelFit(ctx, acc, requestedModel), ""
	case acc.Platform == PlatformAnthropic, acc.IsGemini(), acc.Platform == PlatformAntigravity:
		return s.assessGatewayModelFit(ctx, acc, requestedModel), ""
	default:
		// Unknown platform: report as unknown rather than guessing.
		return modelFitResult{blockedBy: BlockedByUnknown}, "unsupported platform for model-fit diagnostic"
	}
}

// assessOpenAIGatewayModelFit mirrors the failure branches of
// OpenAIGatewayService.supportsOpenAIGatewayRequestedModel (openai_gateway_service.go:2277)
// but returns a reason instead of a bare bool. It does NOT call that method
// directly because we want the reason here, and the hot path should not be
// refactored to return tuples just to serve this diagnostic.
func (s *OpsService) assessOpenAIGatewayModelFit(ctx context.Context, acc *Account, requestedModel string) modelFitResult {
	if s.openAIGatewayService == nil {
		return modelFitResult{blockedBy: BlockedByUnknown}
	}
	// Static gating (capability, known-issue) — same order as the hot path.
	if !acc.SupportsCapability("") {
		return modelFitResult{blockedBy: BlockedByNoCapability}
	}
	policy := LoadKnownIssueModelExclusionPolicy(ctx, s.openAIGatewayService.settingRepoOrNil())
	if _, excluded := policy.Excludes(requestedModel, acc, "openai-compatible", ""); excluded {
		return modelFitResult{blockedBy: BlockedByKnownIssue}
	}
	source, err := s.openAIGatewayService.cachedLiveModelSourceForAccount(ctx, *acc)
	if err != nil {
		return modelFitResult{blockedBy: BlockedByUnknown}
	}
	if strings.TrimSpace(source.Endpoint) == "" {
		// setup-token accounts are intentionally endpoint-less.
		if acc.Type == AccountTypeSetupToken {
			return modelFitResult{supported: true}
		}
		return modelFitResult{blockedBy: BlockedByEmptyEndpoint}
	}
	if len(source.Models) == 0 {
		return modelFitResult{blockedBy: BlockedByEmptyEndpoint}
	}
	inList := modelListContainsRequestedModel(source.Models, requestedModel)
	if !inList {
		return modelFitResult{inList: false, listSize: len(source.Models), blockedBy: BlockedByModelNotFound}
	}
	return modelFitResult{supported: true, inList: true, listSize: len(source.Models)}
}

// assessGatewayModelFit mirrors GatewayService.isModelSupportedByLiveSource
// for the generic gateway path (Anthropic / Gemini / Antigravity).
func (s *OpsService) assessGatewayModelFit(ctx context.Context, acc *Account, requestedModel string) modelFitResult {
	if s.gatewayService == nil {
		return modelFitResult{blockedBy: BlockedByUnknown}
	}
	policy := LoadKnownIssueModelExclusionPolicy(ctx, s.gatewayService.settingService.SettingRepoOrNil())
	if _, excluded := policy.Excludes(requestedModel, acc, acc.Platform, ""); excluded {
		return modelFitResult{blockedBy: BlockedByKnownIssue}
	}
	source, err := s.gatewayService.cachedLiveModelSourceForAccountWithLoader(ctx, *acc, s.gatewayService.liveModelSourceForAccount)
	if err != nil {
		// Fall back to static capability check (matches isModelSupportedByAccountWithContext).
		if acc.IsModelSupported(requestedModel) {
			return modelFitResult{supported: true}
		}
		return modelFitResult{blockedBy: BlockedByModelNotFound}
	}
	if strings.TrimSpace(source.Endpoint) == "" {
		// No live source for this account kind; fall back to static capability.
		if acc.IsModelSupported(requestedModel) {
			return modelFitResult{supported: true}
		}
		return modelFitResult{blockedBy: BlockedByModelNotFound}
	}
	if len(source.Models) == 0 {
		return modelFitResult{blockedBy: BlockedByEmptyEndpoint}
	}
	inList := modelListContainsRequestedModel(source.Models, requestedModel)
	if !inList {
		return modelFitResult{inList: false, listSize: len(source.Models), blockedBy: BlockedByModelNotFound}
	}
	return modelFitResult{supported: true, inList: true, listSize: len(source.Models)}
}

// errModelAvailabilityEmptyModel is returned when no model is supplied.
var errModelAvailabilityEmptyModel = &modelAvailabilityError{message: "model is required"}

type modelAvailabilityError struct{ message string }

func (e *modelAvailabilityError) Error() string { return e.message }
