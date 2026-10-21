package middleware

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/requesttrace"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

func RequestTraceHandler(next http.Handler, enabled func() bool, submit func(*requesttrace.Record)) http.Handler {
	node := common.GetNodeIdentity().Name
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled() || !requesttrace.IsRelayPath(r.URL.Path) && jsplugin.IsReservedRoutePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		collector := requesttrace.New(common.NewRequestId(), node, r.Method, start)
		collector.SetSamplingPolicy(system_setting.GetRequestTraceSamplingPolicy())
		r = r.WithContext(requesttrace.WithCollector(r.Context(), collector))
		defer func() { submit(collector.Freeze(0, r.Context().Err())) }()
		next.ServeHTTP(w, r)
	})
}

func RequestTraceContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		collector := requesttrace.FromContext(c.Request.Context())
		if collector == nil {
			c.Next()
			return
		}
		c.Writer = &requestTraceResponseWriter{ResponseWriter: c.Writer, collector: collector, context: c.Request.Context()}
		defer func() {
			route := c.FullPath()
			if c.GetString(RouteTagKey) == "relay" || requesttrace.IsRelayPath(route) {
				collector.Enable(route, requestTraceKind(route))
			}
			collector.Status(c.Writer.Status())
			collector.FinishResponse(c.Writer.Size(), c.Request.Context().Err())
		}()
		c.Next()
	}
}

func requestTraceKind(route string) string {
	switch {
	case strings.Contains(route, "realtime"):
		return "realtime"
	case strings.Contains(route, "/responses"):
		return "responses"
	case strings.Contains(route, "embedding"):
		return "embedding"
	case strings.Contains(route, "/images/") || strings.HasSuffix(route, "/edits"):
		return "image"
	case strings.Contains(route, "/audio/"):
		return "audio"
	case strings.Contains(route, "/rerank"):
		return "rerank"
	case strings.Contains(route, "/chat/") || strings.HasSuffix(route, "/completions") || strings.HasSuffix(route, "/messages"):
		return "chat"
	case strings.Contains(route, "/submit/") || strings.Contains(route, "/tasks"):
		return "task"
	case strings.Contains(route, "/mj/") || strings.Contains(route, "/task/"):
		return "task"
	case strings.Contains(route, "/models"):
		return "models"
	default:
		return "relay"
	}
}

type requestTraceResponseWriter struct {
	gin.ResponseWriter
	collector *requesttrace.Collector
	context   context.Context
}

func (w *requestTraceResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.collector.Write(n, err, w.context)
	return n, err
}

func (w *requestTraceResponseWriter) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	w.collector.Write(n, err, w.context)
	return n, err
}

func (w *requestTraceResponseWriter) WriteHeartbeat(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		w.collector.Write(0, err, w.context)
	}
	return n, err
}

func (w *requestTraceResponseWriter) Flush() { _ = w.FlushError() }

func (w *requestTraceResponseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.collector.Write(0, err, w.context)
	}
	return err
}

func (w *requestTraceResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := w.ResponseWriter.Hijack()
	if err == nil {
		w.collector.Hijack()
	}
	return conn, buffer, err
}

func (w *requestTraceResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
