package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type CopilotOAuthHandler struct {
	copilotOAuthService *service.CopilotOAuthService
	adminService        service.AdminService
}

func NewCopilotOAuthHandler(copilotOAuthService *service.CopilotOAuthService, adminService service.AdminService) *CopilotOAuthHandler {
	return &CopilotOAuthHandler{
		copilotOAuthService: copilotOAuthService,
		adminService:        adminService,
	}
}

type CopilotImportAccessTokenRequest struct {
	AccessToken string `json:"access_token" binding:"required"`
	ProxyID     *int64 `json:"proxy_id"`
}

func (h *CopilotOAuthHandler) ImportAccessToken(c *gin.Context) {
	var req CopilotImportAccessTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.copilotOAuthService.ImportAccessToken(c.Request.Context(), req.AccessToken, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, result)
}

func (h *CopilotOAuthHandler) RefreshAccountToken(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}

	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account.Platform != service.PlatformCopilot || !account.IsOAuth() {
		response.BadRequest(c, "Account is not a Copilot OAuth account")
		return
	}

	result, err := h.copilotOAuthService.ImportAccessToken(c.Request.Context(), account.GetCredential("access_token"), account.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	credentials := h.copilotOAuthService.BuildAccountCredentials(result)
	extra := h.copilotOAuthService.BuildAccountExtra(result)
	for k, v := range account.Extra {
		if _, exists := extra[k]; !exists {
			extra[k] = v
		}
	}

	updatedAccount, err := h.adminService.UpdateAccount(c.Request.Context(), accountID, &service.UpdateAccountInput{
		Credentials: credentials,
		Extra:       extra,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.AccountFromService(updatedAccount))
}

func (h *CopilotOAuthHandler) CreateAccountFromAccessToken(c *gin.Context) {
	var req struct {
		AccessToken string  `json:"access_token" binding:"required"`
		ProxyID     *int64  `json:"proxy_id"`
		Name        string  `json:"name"`
		Concurrency int     `json:"concurrency"`
		Priority    int     `json:"priority"`
		GroupIDs    []int64 `json:"group_ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.copilotOAuthService.ImportAccessToken(c.Request.Context(), req.AccessToken, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "GitHub Copilot"
		if login := strings.TrimSpace(result.GitHubLogin); login != "" {
			name = "GitHub Copilot (" + login + ")"
		}
	}

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:        name,
		Platform:    service.PlatformCopilot,
		Type:        service.AccountTypeOAuth,
		Credentials: h.copilotOAuthService.BuildAccountCredentials(result),
		Extra:       h.copilotOAuthService.BuildAccountExtra(result),
		ProxyID:     req.ProxyID,
		Concurrency: req.Concurrency,
		Priority:    req.Priority,
		GroupIDs:    req.GroupIDs,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.AccountFromService(account))
}
