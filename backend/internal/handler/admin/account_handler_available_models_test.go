package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type availableModelsAdminService struct {
	*stubAdminService
	account service.Account
}

func (s *availableModelsAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if s.account.ID == id {
		acc := s.account
		return &acc, nil
	}
	return s.stubAdminService.GetAccount(context.Background(), id)
}

type availableModelsHTTPUpstreamStub struct {
	body  string
	calls atomic.Int64
}

func (s *availableModelsHTTPUpstreamStub) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

func (s *availableModelsHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

func setupAvailableModelsRouter(adminSvc service.AdminService, services ...any) *gin.Engine {
	var gatewaySvc *service.GatewayService
	var openaiGatewaySvc *service.OpenAIGatewayService
	for _, svc := range services {
		switch typed := svc.(type) {
		case *service.GatewayService:
			gatewaySvc = typed
		case *service.OpenAIGatewayService:
			openaiGatewaySvc = typed
		}
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, gatewaySvc, openaiGatewaySvc)
	router.GET("/api/v1/admin/accounts/:id/models", handler.GetAvailableModels)
	return router
}

func TestAccountHandlerGetAvailableModels_WithoutLiveResolverDoesNotUseModelMapping(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       42,
			Name:     "openai-oauth",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-5": "gpt-5.1",
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "Live model resolver unavailable")
}

func TestAccountHandlerGetAvailableModels_OpenAIAPIKeyUsesLiveResolver(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       43,
			Name:     "openai-apikey",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "https://openai-compatible.test/v1",
			},
		},
	}
	upstream := &availableModelsHTTPUpstreamStub{body: `{"data":[{"id":"gpt-5-5"},{"id":"minimax-m2-7"}]}`}
	cfg := &config.Config{}
	gatewaySvc := service.NewGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	openaiSvc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, gatewaySvc)
	router := setupAvailableModelsRouter(svc, gatewaySvc, openaiSvc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/43/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{"gpt-5.5", "minimax-m2.7"}, []string{resp.Data[0].ID, resp.Data[1].ID})
	require.Equal(t, int64(1), upstream.calls.Load())
}

func TestAccountHandlerGetAvailableModels_CopilotRequiresLiveResolver(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       44,
			Name:     "copilot",
			Platform: service.PlatformCopilot,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/44/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "Live model resolver unavailable")
}

func TestAccountHandlerGetAvailableModels_KiroRequiresLiveResolver(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       45,
			Name:     "kiro",
			Platform: service.PlatformKiro,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/45/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "Live model resolver unavailable")
}
