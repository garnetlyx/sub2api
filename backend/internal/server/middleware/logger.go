package middleware

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logger 请求日志中间件
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 开始时间
		startTime := time.Now()

		// 请求路径
		path := c.Request.URL.Path

		// 处理请求
		c.Next()

		// 跳过健康检查等高频探针路径的日志
		if path == "/health" || path == "/setup/status" {
			return
		}

		endTime := time.Now()
		latency := endTime.Sub(startTime)

		method := c.Request.Method
		statusCode := c.Writer.Status()
		ipSnapshot := ip.ObserveRequest(c)
		protocol := c.Request.Proto
		accountID, hasAccountID := c.Request.Context().Value(ctxkey.AccountID).(int64)
		platform, _ := c.Request.Context().Value(ctxkey.Platform).(string)
		model, _ := c.Request.Context().Value(ctxkey.Model).(string)
		userAgent := c.GetHeader("User-Agent")
		host := c.Request.Host

		fields := []zap.Field{
			zap.String("component", "http.access"),
			zap.Int("status_code", statusCode),
			zap.Int64("latency_ms", latency.Milliseconds()),
			zap.String("client_ip", ipSnapshot.TrustedClientIP),
			zap.String("trusted_client_ip", ipSnapshot.TrustedClientIP),
			zap.String("remote_addr", ipSnapshot.RemoteAddr),
			zap.String("remote_ip", ipSnapshot.RemoteIP),
			zap.String("header_client_ip", ipSnapshot.HeaderClientIP),
			zap.Bool("has_forwarded_headers", ipSnapshot.HasForwardedHeaders),
			zap.Bool("forwarded_ip_mismatch", ipSnapshot.ForwardedIPMismatch),
			zap.String("cf_connecting_ip", ipSnapshot.CFConnectingIP),
			zap.String("x_real_ip", ipSnapshot.XRealIP),
			zap.String("x_forwarded_for", ipSnapshot.XForwardedFor),
			zap.String("host", host),
			zap.String("user_agent", userAgent),
			zap.String("protocol", protocol),
			zap.String("method", method),
			zap.String("path", path),
		}
		if apiKey, ok := GetAPIKeyFromContext(c); ok && apiKey != nil {
			fields = append(fields,
				zap.Int64("api_key_id", apiKey.ID),
				zap.String("api_key_name", apiKey.Name),
				zap.Int64("user_id", apiKey.UserID),
			)
		} else if subject, ok := GetAuthSubjectFromContext(c); ok {
			fields = append(fields, zap.Int64("user_id", subject.UserID))
		}
		if role, ok := GetUserRoleFromContext(c); ok && role != "" {
			fields = append(fields, zap.String("user_role", role))
		}
		if group, ok := c.Request.Context().Value(ctxkey.Group).(*service.Group); ok && group != nil {
			fields = append(fields,
				zap.Int64("group_id", group.ID),
				zap.String("group_name", group.Name),
			)
		}
		if hasAccountID && accountID > 0 {
			fields = append(fields, zap.Int64("account_id", accountID))
		}
		if platform != "" {
			fields = append(fields, zap.String("platform", platform))
		}
		if model != "" {
			fields = append(fields, zap.String("model", model))
		}

		l := logger.FromContext(c.Request.Context()).With(fields...)
		l.Info("http request completed", zap.Time("completed_at", endTime))

		if len(c.Errors) > 0 {
			l.Warn("http request contains gin errors", zap.String("errors", c.Errors.String()))
		}
	}
}
