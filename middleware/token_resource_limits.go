package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"

	"github.com/gin-gonic/gin"
)

// TokenResourceLimits runs after TokenAuth and rate limiting, but before any
// request parser, plugin preparation, channel selection or billing. Request
// decompression and BodyStorageCleanup remain owned by the existing router.
func TokenResourceLimits() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}
		settings, err := setting.GetTokenResourceLimitSettings()
		if err != nil {
			common.SysError("invalid token resource limit configuration: " + err.Error())
			abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "Resource policy is temporarily unavailable.", "resource_policy_unavailable")
			return
		}
		if !settings.Enabled {
			c.Next()
			return
		}
		policy, err := model.GetTokenResourcePolicy(c.Request.Context(), common.GetContextKeyInt(c, constant.ContextKeyTokenId))
		if err != nil {
			common.SysError("failed to load token resource policy: " + err.Error())
			abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "Resource policy is temporarily unavailable.", "resource_policy_unavailable")
			return
		}
		limits := policy.Resolve(model.TokenResourceLimits{
			MaxRequestBodyBytes:   settings.DefaultMaxRequestBodyBytes,
			MaxConcurrentRequests: settings.DefaultMaxConcurrentRequests,
		})
		if limits.MaxConcurrentRequests > 0 {
			redisClient := common.RDB
			if !common.RedisEnabled {
				redisClient = nil
			}
			lease, err := service.AcquireTokenConcurrency(c.Request.Context(), redisClient, common.GetContextKeyInt(c, constant.ContextKeyTokenId), limits.MaxConcurrentRequests)
			if errors.Is(err, service.ErrTokenConcurrencyExceeded) {
				abortWithOpenAiMessage(c, http.StatusTooManyRequests, "Too many concurrent requests.", "concurrency_limit_exceeded")
				return
			}
			if err != nil {
				common.SysError("failed to acquire token concurrency lease: " + err.Error())
				abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "Resource policy is temporarily unavailable.", "resource_policy_unavailable")
				return
			}
			defer func() {
				if cause := context.Cause(lease.Context()); cause != nil && !errors.Is(cause, context.Canceled) {
					common.SysError("token concurrency request stopped: " + cause.Error())
				}
				if err := lease.Close(); err != nil {
					common.SysError("failed to release token concurrency lease: " + err.Error())
				}
			}()
			c.Request = c.Request.WithContext(lease.Context())
			if c.Request.Context().Err() != nil {
				abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "Request was canceled.", "request_canceled")
				return
			}
		}
		if limits.MaxRequestBodyBytes == 0 || c.Request.Body == nil {
			c.Next()
			return
		}

		// Bound the decoded reader before BodyStorage allocates or writes data.
		// Content-Length can describe compressed bytes or be unknown, so it is
		// only a storage hint, never the basis for accepting/rejecting a body.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limits.MaxRequestBodyBytes)
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			if common.IsRequestBodyTooLargeError(err) {
				abortWithOpenAiMessage(c, http.StatusRequestEntityTooLarge, "Request body is too large.", "request_body_too_large")
			} else {
				abortWithOpenAiMessage(c, http.StatusBadRequest, "Unable to read request body.", "invalid_request_body")
			}
			return
		}
		// Also enforce the limit if an earlier middleware cached the body.
		if storage.Size() > limits.MaxRequestBodyBytes {
			abortWithOpenAiMessage(c, http.StatusRequestEntityTooLarge, "Request body is too large.", "request_body_too_large")
			return
		}
		// GetBodyStorage positions the cache at zero and closes the original
		// input. Replay the same storage for downstream JSON/multipart parsers.
		c.Request.Body = io.NopCloser(storage)
		c.Next()
	}
}
