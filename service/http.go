package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/gin-gonic/gin"
)

func CloseResponseBodyGracefully(httpResponse *http.Response) {
	if httpResponse == nil || httpResponse.Body == nil {
		return
	}
	err := httpResponse.Body.Close()
	if err != nil {
		common.SysError("failed to close response body: " + err.Error())
	}
}

// SetUpstreamRequestID bounds the log value to its varchar(128) column without
// cutting a UTF-8 character or modifying the upstream response header.
func SetUpstreamRequestID(c *gin.Context, value string) {
	if c == nil {
		return
	}
	if len(value) > 128 {
		characters := 0
		for index := range value {
			if characters == 128 {
				value = strings.Clone(value[:index])
				break
			}
			characters++
		}
	}
	c.Set(common.UpstreamRequestIdKey, value)
}

// ShouldCopyUpstreamHeader 不复制 Content-Length 和上游的 X-Oneapi-Request-Id。
// 记录上游请求 ID 时，X-Oneapi-Request-Id 优先于 X-Request-Id。
func ShouldCopyUpstreamHeader(c *gin.Context, k string, v []string) bool {
	if strings.EqualFold(k, "Content-Length") {
		return false
	}
	if strings.EqualFold(k, common.RequestIdKey) {
		if c != nil && len(v) > 0 && v[0] != "" {
			SetUpstreamRequestID(c, v[0])
		}
		return false
	}
	if strings.EqualFold(k, "X-Request-Id") && c != nil && c.GetString(common.UpstreamRequestIdKey) == "" && len(v) > 0 && v[0] != "" {
		SetUpstreamRequestID(c, v[0])
	}
	return true
}

func IOCopyBytesGracefully(c *gin.Context, src *http.Response, data []byte) {
	if c.Writer == nil {
		return
	}

	body := io.NopCloser(bytes.NewBuffer(data))

	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	if src != nil {
		for k, v := range src.Header {
			if !ShouldCopyUpstreamHeader(c, k, v) {
				continue
			}
			c.Writer.Header().Set(k, v[0])
		}
	}

	// set Content-Length header manually BEFORE calling WriteHeader
	c.Writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

	// Write header with status code (this sends the headers)
	if src != nil {
		c.Writer.WriteHeader(src.StatusCode)
	} else {
		c.Writer.WriteHeader(http.StatusOK)
	}

	_, err := io.Copy(c.Writer, body)
	if err != nil {
		logger.LogError(c, fmt.Sprintf("failed to copy response body: %s", err.Error()))
	}
	c.Writer.Flush()
}
