package middleware

import (
	"github.com/QuantumNous/new-api/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func CORS() gin.HandlerFunc {
	config := cors.DefaultConfig()
	config.AllowAllOrigins = true
	config.AllowCredentials = true
	config.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	config.AllowHeaders = []string{"*"}
	defaultCORS := cors.New(config)
	config.AllowMethods = append(config.AllowMethods, "PATCH")
	config.AllowHeaders = append(config.AllowHeaders, "Authorization")
	config.ExposeHeaders = append(config.ExposeHeaders, "Retry-After", "Allow", "Location", "Content-Encoding")
	searchCORS := cors.New(config)
	return func(c *gin.Context) {
		if relayconstant.Path2RelayMode(c.Request.URL.Path) == relayconstant.RelayModeZhipuWebSearch {
			searchCORS(c)
			return
		}
		defaultCORS(c)
	}
}

func Version() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-New-Api-Version", common.Version)
		c.Next()
	}
}
