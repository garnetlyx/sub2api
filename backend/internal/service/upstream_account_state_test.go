package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsUpstreamAccountStateError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		message    string
		body       string
		want       bool
	}{
		{
			name:       "lapsed subscription by code and message",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"code":"InvalidSubscription","message":"Your account does not have a valid CodingPlan subscription, or your subscription has expired."}}`,
			want:       true,
		},
		{
			name:       "quota exhausted",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota."}}`,
			want:       true,
		},
		{
			name:       "payment required",
			statusCode: http.StatusPaymentRequired,
			message:    "insufficient balance",
			want:       true,
		},
		{
			name:       "request parameter error is not account state",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"code":"invalid_parameter","message":"invalid temperature: only 1 is allowed for this model"}}`,
			want:       false,
		},
		{
			name:       "context limit is not account state",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`,
			want:       false,
		},
		{
			name:       "status-coded rate limit remains outside body classifier",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"message":"quota exceeded"}}`,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isUpstreamAccountStateError(tt.statusCode, tt.message, []byte(tt.body)))
		})
	}
}

func TestBuildOpenAIUpstreamFailoverError_AccountStateDisablesSameAccountRetry(t *testing.T) {
	account := &Account{
		Type: AccountTypeOAuth,
		Extra: map[string]any{
			"pool_mode": true,
		},
	}
	body := []byte(`{"error":{"code":"InvalidSubscription","message":"subscription has expired"}}`)

	failoverErr := buildOpenAIUpstreamFailoverError(account, http.StatusBadRequest, "subscription has expired", body)

	require.NotNil(t, failoverErr)
	require.Equal(t, UpstreamFailoverReasonAccountState, failoverErr.Reason)
	require.False(t, failoverErr.IsCompatibilityMismatch())
	require.False(t, failoverErr.RetryableOnSameAccount)
}
