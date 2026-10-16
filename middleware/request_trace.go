package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type traceResponseWriter struct {
	gin.ResponseWriter
	observer *common.TraceBodyObserver
}

func (w *traceResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.observer.Observe(data[:n], w.Header().Get("Content-Type"))
	}
	return n, err
}

func (w *traceResponseWriter) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	if n > 0 {
		w.observer.Observe([]byte(data[:n]), w.Header().Get("Content-Type"))
	}
	return n, err
}

func RequestTrace() gin.HandlerFunc {
	return func(c *gin.Context) {
		config := common.GetRequestTraceConfig()
		path := c.Request.URL.Path
		traceable := c.Request.Method == http.MethodPost && (strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/completions") || path == "/v1/messages" || strings.HasPrefix(path, "/v1/responses") || strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent"))
		if config.Level == "off" || !traceable {
			c.Next()
			return
		}
		trace := &common.RequestTraceCollector{Config: config, Started: time.Now(), Stage: "auth", Client: common.RequestTraceClient{
			UserAgent: common.TraceMetadataString(c.Request.UserAgent(), 256), ContentType: common.TraceMetadataString(c.GetHeader("Content-Type"), 128), AnthropicVersion: common.TraceMetadataString(c.GetHeader("anthropic-version"), 64),
		}}
		if c.Request.ContentLength >= 0 {
			size := c.Request.ContentLength
			trace.Client.WireBytes = &size
		}
		c.Set(common.RequestTraceContextKey, trace)
		observer := common.NewTraceBodyObserver(trace.Started)
		trace.ClientObserver = observer
		original := c.Writer
		c.Writer = &traceResponseWriter{ResponseWriter: original, observer: observer}
		defer func() {
			c.Writer = original
			// Gin 归池前不清空 Keys，收尾完成后解除对事件缓存的引用。
			c.Set(common.RequestTraceContextKey, nil)
		}()
		c.Next()
		observer.Finish()
		response, clientMetadata, clientReason := observer.Snapshot()
		failed := trace.Failed || response.Error || c.Writer.Status() >= 400 || clientMetadata.ErrorType != ""
		userId := c.GetInt("id")
		if userId == 0 && (!failed || !config.AnonymousErrors) {
			return
		}
		if !failed && !common.LogConsumeEnabled {
			return
		}
		trace.Lock()
		errorLogged := trace.ErrorLogged
		trace.Unlock()
		if failed && len(trace.Attempts) == 0 && !errorLogged {
			if userId > 0 && (!config.PreChannelErrors || (trace.Stage == "pre_consume" && c.Writer.Status() == http.StatusForbidden)) {
				return
			}
			other := model.NewLogOther()
			other.SetPublic("request_path", c.Request.URL.Path)
			other.SetPublic("status_code", c.Writer.Status())
			other.SetPublic("error_type", clientMetadata.ErrorType)
			other.SetPublic("error_code", clientMetadata.ErrorCode)
			model.RecordErrorLog(c, userId, 0, c.GetString("original_model"), c.GetString("token_name"), "Request failed before upstream dispatch", c.GetInt("token_id"), int(time.Since(trace.Started).Seconds()), strings.Contains(response.ContentType, "text/event-stream"), c.GetString("group"), other)
		}
		attempts := trace.Attempts
		if len(attempts) == 0 {
			attempts = []*common.RequestTraceAttempt{{Attempt: -1, Started: trace.Started}}
		}
		rows := make([]model.RequestTrace, 0, len(attempts))
		for index, attempt := range attempts {
			upstream := attempt.Upstream
			attemptResponse := common.RequestTraceResponse{}
			var reason *string
			if attempt.Observer != nil {
				attempt.Observer.Finish()
				var metadata common.RequestTraceUpstream
				attemptResponse, metadata, reason = attempt.Observer.Snapshot()
				upstream.ResponseID, upstream.Model, upstream.Object, upstream.Created = metadata.ResponseID, metadata.Model, metadata.Object, metadata.Created
				upstream.Fingerprint, upstream.ServiceTier, upstream.ErrorType, upstream.ErrorCode = metadata.Fingerprint, metadata.ServiceTier, metadata.ErrorType, metadata.ErrorCode
			}
			last := index == len(attempts)-1
			attemptFailed := attempt.Failed || attemptResponse.Error || upstream.Status >= 400 || upstream.ErrorType != "" || (last && failed)
			status, responseBytes := upstream.Status, int64(0)
			if attempt.Observer != nil {
				responseBytes = attempt.Observer.Size()
			}
			stage := ""
			if attemptFailed {
				stage = "upstream"
			}
			if last {
				status = c.Writer.Status()
				responseBytes = int64(max(0, c.Writer.Size()))
				attemptResponse = response
				if clientReason != nil {
					reason = clientReason
				}
				if reason == nil && strings.Contains(response.ContentType, "text/event-stream") {
					missing := ""
					reason = &missing
				}
				if failed {
					stage = trace.Stage
					if status == http.StatusTooManyRequests {
						stage = "rate_limit"
					}
				}
			}
			data := common.RequestTraceData{Trace: common.RequestTraceSummary{Milliseconds: time.Since(trace.Started).Milliseconds(), RequestBytes: trace.RequestBytes, ResponseBytes: responseBytes, Status: status, FinishReason: reason, Stage: stage}}
			if last {
				data.Trace.ClientErrorType = clientMetadata.ErrorType
			}
			if config.Level == "all" || attemptFailed {
				data.AdminInfo = &common.RequestTraceAdmin{ChannelID: attempt.ChannelID, Client: trace.Client, Shape: trace.Shape, Upstream: upstream, Response: attemptResponse, ShapeIncomplete: trace.ShapeIncomplete}
			}
			encoded, err := common.Marshal(data)
			if err != nil {
				logger.LogError(c, "failed to encode request trace: "+err.Error())
				continue
			}
			rows = append(rows, model.RequestTrace{RequestId: c.GetString(common.RequestIdKey), Attempt: attempt.Attempt, UserId: userId, CreatedAt: common.GetTimestamp(), Data: string(encoded)})
		}
		// 客户端断开后仍保存已采集的摘要，但数据库写入受独立超时约束。
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
		defer cancel()
		if err := model.RecordRequestTraces(ctx, rows); err != nil {
			logger.LogError(c, "failed to record request trace: "+err.Error())
		}
	}
}
