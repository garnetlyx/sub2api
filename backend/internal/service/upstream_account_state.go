package service

import (
	"net/http"
	"strings"
)

// UpstreamFailoverReasonAccountState marks upstream errors that report an
// account-level condition — a lapsed subscription, exhausted quota, or
// billing problem — rather than a problem with the request itself. The error
// is deterministic per account: the only useful recovery is switching
// accounts, and the failing account should sink in the scheduler until its
// state changes upstream (renewal, top-up). Detection is protocol-shape only
// (error code/message vocabulary), never provider identity.
const UpstreamFailoverReasonAccountState = "account_state"

// accountStateNeedles are lowercase substrings identifying account-level
// conditions in the upstream error code or message. Request-shape errors
// (parameters, context length, tool schema, model names) never use this
// vocabulary, so a match is a safe failover signal: worst case the next
// account returns the same error and the client sees it after switch
// exhaustion.
var accountStateNeedles = []string{
	"subscription",
	"insufficient_quota",
	"insufficient quota",
	"quota",
	"arrears",
	"billing",
	"payment",
	"balance",
}

// isUpstreamAccountStateError reports whether a 4xx upstream response
// describes an account-level condition. Statuses 401/403/429/5xx already
// trigger failover by status alone; this covers providers that report
// account state as 400/402 (e.g. lapsed subscription plans).
func isUpstreamAccountStateError(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusPaymentRequired:
	default:
		return false
	}
	code := strings.ToLower(strings.TrimSpace(extractUpstreamErrorCode(upstreamBody)))
	msg := strings.ToLower(strings.TrimSpace(upstreamMsg))
	if msg == "" {
		msg = strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(upstreamBody)))
	}
	if code == "" && msg == "" {
		return false
	}
	for _, needle := range accountStateNeedles {
		if strings.Contains(code, needle) || strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// accountStateFailoverReason returns the failover reason for an upstream
// error response, or "" when it is not an account-state error.
func accountStateFailoverReason(statusCode int, upstreamMsg string, upstreamBody []byte) string {
	if isUpstreamAccountStateError(statusCode, upstreamMsg, upstreamBody) {
		return UpstreamFailoverReasonAccountState
	}
	return ""
}
