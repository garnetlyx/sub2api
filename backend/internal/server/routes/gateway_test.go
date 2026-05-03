package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newGatewayRoutesTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	RegisterGatewayRoutes(
		router,
		&handler.Handlers{
			Gateway:       &handler.GatewayHandler{},
			OpenAIGateway: &handler.OpenAIGatewayHandler{},
		},
		servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
			c.Next()
		}),
		nil,
		nil,
		nil,
		nil,
		&config.Config{},
	)

	return router
}

func TestShouldRouteMessagesToOpenAI_PrefersNativeAnthropic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"glm-5.1-zhipu"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	require.False(t, shouldRouteMessagesToOpenAI(c, &handler.Handlers{}))
}

func TestRequestTargetsOpenAIModel_UsesServiceSupport(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.1-zhipu"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	require.True(t, looksLikeOpenAIModel("gpt-5.4"))
	require.False(t, looksLikeOpenAIModel("glm-5.1-zhipu"))
	require.False(t, looksLikeOpenAIModel("claude-opus-4.7"))
}

func TestRequestTargetsOpenAIResponsesModel_FallsBackForNonOpenAIFamilyProxyNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"glm-5.1-zhipu"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	require.False(t, requestTargetsOpenAIResponsesModel(c, &handler.Handlers{}))

	c2rec := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(c2rec)
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	c2.Request.Header.Set("Content-Type", "application/json")
	require.True(t, requestTargetsOpenAIResponsesModel(c2, &handler.Handlers{}))

	c3rec := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(c3rec)
	c3.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"claude-opus-4.7"}`))
	c3.Request.Header.Set("Content-Type", "application/json")
	require.False(t, requestTargetsOpenAIResponsesModel(c3, &handler.Handlers{}))
}

func TestGetAPIKeyGroupID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	groupID := int64(42)
	c.Set("api_key", &service.APIKey{GroupID: &groupID})
	require.Equal(t, &groupID, getAPIKeyGroupID(c))
}

func TestGatewayRoutesOpenAIResponsesCompactPathIsRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, path := range []string{"/v1/responses/compact", "/responses/compact"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"gpt-5"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code, "path=%s should hit OpenAI responses handler", path)
	}
}

func TestGatewayRoutesOpenAIEmbeddingsPathIsRegistered(t *testing.T) {
	router := newGatewayRoutesTestRouter()

	for _, path := range []string{"/v1/embeddings", "/embeddings"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"embedding-3-zhipu","input":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code, "path=%s should hit OpenAI embeddings handler", path)
	}
}
