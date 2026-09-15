package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 大媒体使用真实流式HTTP读写而非内存Recorder；分别检查完整传输、上游截断和下游取消。
func TestVideoMediaLargeTransferAndDisconnect(t *testing.T) {
	if os.Getenv("VIDEO_DEEP_AUDIT") != "1" {
		t.Skip("explicit large media audit only")
	}
	for _, mode := range []string{"complete", "upstream-truncated", "downstream-cancel"} {
		t.Run(mode, func(t *testing.T) {
			task := setupGenericTaskTest(t)
			allowPrivateTaskMediaTest(t)
			const size = 128 * 1024 * 1024
			upstreamEnded := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamEnded)
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Length", fmt.Sprint(size))
				chunk := make([]byte, 32768)
				for sent := 0; sent < size; sent += len(chunk) {
					if _, err := w.Write(chunk); err != nil {
						return
					}
					if sent >= 1024*1024 {
						if mode == "upstream-truncated" {
							return
						}
						if mode == "downstream-cancel" {
							w.(http.Flusher).Flush()
							<-r.Context().Done()
							return
						}
					}
				}
			}))
			defer upstream.Close()
			proxyErrors := make(chan error, 1)
			engine := gin.New()
			engine.GET("/video", func(c *gin.Context) {
				proxyErrors <- proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{URL: upstream.URL, Method: http.MethodGet, Credentialless: true})
			})
			proxy := httptest.NewServer(engine)
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/video", nil)
			require.NoError(t, err)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			if mode == "downstream-cancel" {
				_, err = io.CopyN(io.Discard, response.Body, 1024*1024)
				require.NoError(t, err)
				cancel()
				response.Body.Close()
				select {
				case <-upstreamEnded:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "upstream did not release after client cancellation")
				}
				select {
				case <-proxyErrors:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "proxy did not release after client cancellation")
				}
				return
			}
			count, readErr := io.Copy(io.Discard, response.Body)
			proxyErr := <-proxyErrors
			if mode == "complete" {
				require.NoError(t, readErr)
				require.NoError(t, proxyErr)
				assert.EqualValues(t, size, count)
			} else {
				assert.Error(t, readErr)
				// 响应头已写出后控制器只记录流错误，返回nil避免追加JSON；客户端必须观察到截断。
				assert.NoError(t, proxyErr)
				assert.Less(t, count, int64(size))
			}
			t.Logf("MEDIA mode=%s bytes=%d", mode, count)
		})
	}
}
