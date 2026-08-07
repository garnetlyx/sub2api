package admin

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetModelAvailability reports, for a given model, every account's ability to
// serve it together with a standardized reason when it cannot.
//
// GET /api/v1/admin/ops/model-availability/:model
//
// Path params:
//   - model: public model ID (e.g. gpt-5.6-sol, glm-5.2, claude--sonnet-4-5)
//
// The diagnostic is read-only and reuses the cached live model sources and the
// known-issue exclusion policy; it does not mutate scheduler or rate-limit
// state. It is the single endpoint to answer "why can't I use model X right
// now" across all account forms (OAuth pool, proxy provider, local runtime,
// bridge).
func (h *OpsHandler) GetModelAvailability(c *gin.Context) {
	if h.opsService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Ops service not available")
		return
	}
	if err := h.opsService.RequireMonitoringEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	model := strings.TrimSpace(c.Param("model"))
	if model == "" {
		response.BadRequest(c, "model is required")
		return
	}

	result, err := h.opsService.GetModelAvailability(c.Request.Context(), model)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
