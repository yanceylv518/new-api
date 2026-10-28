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

// ShouldCopyUpstreamHeader 不复制 Content-Length 和上游的 X-Oneapi-Request-Id。
// 公共发送入口已按配置选取请求 ID；其他响应路径沿用默认响应头的优先级。
func ShouldCopyUpstreamHeader(c *gin.Context, k string, v []string) bool {
	if strings.EqualFold(k, "Content-Length") {
		return false
	}
	if strings.EqualFold(k, common.RequestIdKey) {
		if c != nil && !c.GetBool(common.UpstreamRequestIDCapturedContextKey) && len(v) > 0 && v[0] != "" {
			c.Set(common.UpstreamRequestIdKey, v[0])
		}
		return false
	}
	if strings.EqualFold(k, "X-Request-Id") && c != nil && !c.GetBool(common.UpstreamRequestIDCapturedContextKey) && c.GetString(common.UpstreamRequestIdKey) == "" && len(v) > 0 && v[0] != "" {
		c.Set(common.UpstreamRequestIdKey, v[0])
	}
	return true
}

func IOCopyBytesGracefully(c *gin.Context, src *http.Response, data []byte) {
	if c.Writer == nil {
		return
	}

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

	var err error
	if writer, ok := c.Writer.(interface{ WriteTraceResponse([]byte) (int, error) }); ok {
		_, err = writer.WriteTraceResponse(data)
	} else {
		body := io.NopCloser(bytes.NewBuffer(data))
		_, err = io.Copy(c.Writer, body)
	}
	if err != nil {
		logger.LogError(c, fmt.Sprintf("failed to copy response body: %s", err.Error()))
	}
	c.Writer.Flush()
}
