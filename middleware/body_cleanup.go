package middleware

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// BodyStorageCleanup 请求体存储清理中间件
// 在请求处理完成后自动清理磁盘/内存缓存
func BodyStorageCleanup() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if trace := common.GetRequestTrace(c); trace != nil {
				if trace.ClientObserver != nil {
					trace.ClientObserver.Finish()
					trace.Failed = trace.Failed || trace.ClientObserver.HasError()
				}
				detailed := (trace.Config.Level == "all" && trace.SampledSuccess) || trace.Failed || c.Writer.Status() >= 400
				for _, attempt := range trace.Attempts {
					if attempt.Diagnostic || attempt.Failed || attempt.Upstream.Status >= 400 || attempt.Upstream.ErrorType != "" {
						detailed = true
					}
					if attempt.Observer != nil {
						attempt.Observer.Finish()
						detailed = detailed || attempt.Observer.HasError()
					}
				}
				if value, exists := c.Get(common.KeyBodyStorage); exists {
					if storage, ok := value.(common.BodyStorage); ok {
						trace.CaptureRequest(storage, detailed)
					}
				}
			}
			// 请求结束后清理存储
			common.CleanupBodyStorage(c)

			// 清理文件缓存（URL 下载的文件等）
			service.CleanupFileSources(c)
		}()
		c.Next()
	}
}
