package router

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

// setSeedanceAssetValidationActionRoute 将真人认证 Action 放入 Doubao 原生接口命名空间。
func setSeedanceAssetValidationActionRoute(router *gin.Engine) {
	router.POST("/doubao/v2/assets", middleware.RouteTag("relay"), middleware.TokenAuth(), middleware.SeedanceAssetRateLimit(), middleware.DisableCache(), controller.SeedanceAssetV2Action)
}

// setSeedanceAssetAPIRoutes 将用户级素材管理暴露给 API Key；不进入视频分发、
// 预扣和任务创建链路，所有资源归属继续由共用控制器按认证用户检查。
func setSeedanceAssetAPIRoutes(router *gin.Engine) {
	// 认证完成后由上游浏览器跳转到能力地址；state 校验后才会读取本地会话。
	router.GET("/v1/seedance/validation-sessions/:id/callback", middleware.RouteTag("relay"), middleware.DisableCache(), controller.SeedanceAssetValidationCallback)
	assets := router.Group("/v1/seedance")
	assets.Use(middleware.RouteTag("relay"), func(c *gin.Context) {
		// 程序接口只从 Authorization 接收凭证，不回退浏览器会话或 URL 参数。
		scheme, key, ok := strings.Cut(c.GetHeader("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(strings.TrimSpace(key), "sk-") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "message": "Bearer API key is required"})
			return
		}
		c.Request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
		// 此处没有 WebSocket 升级能力，防止通用认证用该头覆盖已校验的 Bearer。
		c.Request.Header.Del("Sec-WebSocket-Protocol")
	}, middleware.TokenAuth(), middleware.SeedanceAssetRateLimit(), middleware.DisableCache())
	assets.GET("/asset-groups", controller.ListSeedanceAssetGroups)
	assets.GET("/asset-groups/:id", controller.GetSeedanceAssetGroup)
	assets.POST("/asset-groups", controller.CreateSeedanceAssetGroup)
	assets.PUT("/asset-groups/:id", controller.UpdateSeedanceAssetGroup)
	assets.DELETE("/asset-groups/:id", controller.DeleteSeedanceAssetGroup)
	assets.POST("/validation-sessions", controller.CreateSeedanceAssetValidationSession)
	assets.GET("/validation-sessions/:id", controller.GetSeedanceAssetValidationSession)
	assets.GET("/assets", controller.ListSeedanceAssets)
	assets.GET("/assets/:id", controller.GetSeedanceAsset)
	assets.POST("/assets", controller.CreateSeedanceAsset)
	assets.POST("/assets/upload", middleware.UploadRateLimit(), controller.UploadSeedanceAsset)
	assets.POST("/assets/batch-delete", controller.DeleteSeedanceAssetBatch)
	assets.POST("/assets/:id/refresh", controller.RefreshSeedanceAsset)
	assets.DELETE("/assets/:id", controller.DeleteSeedanceAsset)
}
