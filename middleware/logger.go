package middleware

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(redactTaskArtifactAccessQuery())
	server.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		var requestID string
		if param.Keys != nil {
			requestID, _ = param.Keys[common.RequestIdKey].(string)
		}
		tag, _ := param.Keys[RouteTagKey].(string)
		if tag == "" {
			tag = "web"
		}
		path := param.Path
		pathOnly, _, _ := strings.Cut(path, "?")
		// 回调和公开下载查询包含凭证，只脱敏日志，不影响后续处理。
		if strings.HasPrefix(path, "/api/oauth/") || strings.HasPrefix(path, "/oauth/") ||
			strings.HasPrefix(path, "/doubao/api/assets/public/") || strings.HasPrefix(path, "/doubao/api/a/") ||
			strings.HasPrefix(path, "/doubao/api/assets/liveness/callback") ||
			(strings.HasPrefix(pathOnly, "/v1/seedance/validation-sessions/") && strings.HasSuffix(pathOnly, "/callback")) ||
			(strings.HasPrefix(pathOnly, "/v1/tasks/") && strings.HasSuffix(pathOnly, "/callback")) {
			path = pathOnly
		}
		return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
			param.TimeStamp.Format("2006/01/02 - 15:04:05"),
			tag,
			requestID,
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			path,
		)
	}))
}
