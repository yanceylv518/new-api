package router

import (
	"net"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

// seedanceCompatAssetReferenceDispatcher 将素材引用格式限制在兼容入口处理。
func seedanceCompatAssetReferenceDispatcher() gin.HandlerFunc {
	engine := gin.New()
	setSeedanceCompatAssetRoutes(engine)
	return func(c *gin.Context) {
		const prefix = "/doubao/api/assets/asset://"
		if !strings.HasPrefix(c.Request.URL.Path, prefix) {
			return
		}
		resource := strings.TrimPrefix(c.Request.URL.Path, prefix)
		id, suffix, hasSuffix := strings.Cut(resource, "/")
		if !strings.HasPrefix(id, "asset-") || id == "asset-" || len(id) > 128 ||
			(hasSuffix && suffix != "download" && suffix != "public-url" && suffix != "optimize") {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "asset resource not found"})
			return
		}
		request := c.Request.Clone(c.Request.Context())
		request.URL.Path = "/doubao/api/assets/" + resource
		request.URL.RawPath = ""
		// 内层路由使用外层已校验的客户端 IP，不能重新信任转发头。
		request.RemoteAddr = net.JoinHostPort(c.ClientIP(), "0")
		request.Header.Del("X-Forwarded-For")
		request.Header.Del("X-Real-IP")
		engine.ServeHTTP(c.Writer, request)
		c.Abort()
	}
}

func setSeedanceCompatAssetRoutes(router *gin.Engine) {
	router.GET("/doubao/api/assets/public/:id", middleware.RouteTag("relay"), middleware.DownloadRateLimit(), middleware.DisableCache(), controller.DownloadSeedanceCompatPublicAsset)
	router.GET("/doubao/api/a/:id", middleware.RouteTag("relay"), middleware.DownloadRateLimit(), middleware.DisableCache(), controller.DownloadSeedanceCompatPublicAsset)
	router.GET("/doubao/api/assets/liveness/callback", middleware.RouteTag("relay"), middleware.CriticalRateLimit("seedance-validation-callback"), middleware.DisableCache(), controller.SeedanceCompatValidationCallback)
	assets := router.Group("/doubao/api/assets")
	assets.Use(middleware.RouteTag("relay"), func(c *gin.Context) {
		scheme, key, ok := strings.Cut(c.GetHeader("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(strings.TrimSpace(key), "sk-") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Bearer API key is required"})
			return
		}
		c.Request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
		c.Request.Header.Del("Sec-WebSocket-Protocol")
	}, middleware.TokenAuth(), middleware.SeedanceAssetRateLimit(), middleware.DisableCache())
	assets.GET("/groups", controller.ListSeedanceCompatGroups)
	assets.POST("/groups", controller.CreateSeedanceCompatGroup)
	assets.PUT("/groups/:id", controller.UpdateSeedanceCompatGroup)
	assets.DELETE("/groups/:id", controller.DeleteSeedanceCompatGroup)
	assets.GET("/list", controller.ListSeedanceCompatAssets)
	assets.GET("/summary", controller.SeedanceCompatAssetSummary)
	assets.GET("/client-list", controller.SeedanceCompatAssetClientList)
	assets.GET("/tree", controller.SeedanceCompatAssetTree)
	assets.POST("/liveness/session", controller.CreateSeedanceCompatValidationSession)
	assets.GET("/liveness/history", controller.ListSeedanceCompatValidationHistory)
	assets.POST("", controller.CreateSeedanceCompatAsset)
	assets.POST("/upload", middleware.UploadRateLimit(), controller.UploadSeedanceCompatAsset)
	assets.GET("/:id", controller.GetSeedanceCompatAsset)
	assets.PUT("/:id", controller.UpdateSeedanceCompatAsset)
	assets.GET("/:id/download", controller.DownloadSeedanceCompatAsset)
	assets.GET("/:id/public-url", controller.SeedanceCompatAssetPublicURL)
	assets.POST("/:id/optimize", controller.OptimizeSeedanceCompatAsset)
	assets.DELETE("/:id", controller.DeleteSeedanceCompatAsset)
}
