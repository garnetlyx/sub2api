package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type KiroOAuthHandler struct {
	kiroOAuthService *service.KiroOAuthService
	adminService     service.AdminService
}

func NewKiroOAuthHandler(kiroOAuthService *service.KiroOAuthService, adminService service.AdminService) *KiroOAuthHandler {
	return &KiroOAuthHandler{
		kiroOAuthService: kiroOAuthService,
		adminService:     adminService,
	}
}

func (h *KiroOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req struct {
		Region  string `json:"region"`
		Idp     string `json:"idp"`
		ProxyID *int64 `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		req = struct {
			Region  string `json:"region"`
			Idp     string `json:"idp"`
			ProxyID *int64 `json:"proxy_id"`
		}{}
	}

	idp := kiro.SocialGoogle
	if strings.EqualFold(req.Idp, "github") {
		idp = kiro.SocialGithub
	}

	result, err := h.kiroOAuthService.GenerateAuthURL(c.Request.Context(), req.Region, idp, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, result)
}

func (h *KiroOAuthHandler) ExchangeCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id" binding:"required"`
		Code      string `json:"code" binding:"required"`
		State     string `json:"state" binding:"required"`
		ProxyID   *int64 `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.kiroOAuthService.ExchangeCode(c.Request.Context(), req.SessionID, req.Code, req.State, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, result)
}

func (h *KiroOAuthHandler) CreateFromOAuth(c *gin.Context) {
	var req struct {
		SessionID   string  `json:"session_id" binding:"required"`
		Code        string  `json:"code" binding:"required"`
		State       string  `json:"state" binding:"required"`
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

	result, err := h.kiroOAuthService.ExchangeCode(c.Request.Context(), req.SessionID, req.Code, req.State, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Kiro"
		idp := strings.TrimSpace(result.Idp)
		if idp != "" {
			name = "Kiro (" + idp + ")"
		}
	}

	existing, err := h.kiroOAuthService.FindByProfileArn(c.Request.Context(), result.ProfileArn)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	if existing != nil {
		extra := h.kiroOAuthService.BuildAccountExtra(result)
		for k, v := range existing.Extra {
			if _, exists := extra[k]; !exists {
				extra[k] = v
			}
		}
		updatedAccount, updateErr := h.adminService.UpdateAccount(c.Request.Context(), existing.ID, &service.UpdateAccountInput{
			Name:        name,
			Credentials: h.kiroOAuthService.BuildAccountCredentials(result),
			Extra:       extra,
		})
		if updateErr != nil {
			response.ErrorFrom(c, updateErr)
			return
		}
		response.Success(c, dto.AccountFromService(updatedAccount))
		return
	}

	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:        name,
		Platform:    service.PlatformKiro,
		Type:        service.AccountTypeOAuth,
		Credentials: h.kiroOAuthService.BuildAccountCredentials(result),
		Extra:       h.kiroOAuthService.BuildAccountExtra(result),
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

func (h *KiroOAuthHandler) RefreshAccountToken(c *gin.Context) {
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
	if account.Platform != service.PlatformKiro || !account.IsOAuth() {
		response.BadRequest(c, "Account is not a Kiro OAuth account")
		return
	}

	result, err := h.kiroOAuthService.RefreshByRefreshToken(c.Request.Context(), account, account.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	credentials := h.kiroOAuthService.BuildAccountCredentials(result)
	extra := h.kiroOAuthService.BuildAccountExtra(result)
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

func (h *KiroOAuthHandler) ImportRefreshToken(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
		Region       string `json:"region"`
		ProxyID      *int64 `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.kiroOAuthService.ImportRefreshToken(c.Request.Context(), req.RefreshToken, req.Region, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, result)
}
