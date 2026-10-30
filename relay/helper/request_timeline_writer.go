package helper

import (
	"bufio"
	"context"
	"net"
	"net/http"

	"github.com/QuantumNous/new-api/pkg/requesttimeline"
	"github.com/gin-gonic/gin"
)

// This writer observes existing writes; it never buffers, reorders, or parses
// payloads. The relay's existing write mutex still owns write serialization.
type timelineResponseWriter struct {
	gin.ResponseWriter
	trace      *requesttimeline.Trace
	ctx        context.Context
	pending    bool
	writeError error
}

func ObserveRequestTimelineWriter(c *gin.Context, trace *requesttimeline.Trace) func() {
	original := c.Writer
	c.Writer = &timelineResponseWriter{ResponseWriter: original, trace: trace, ctx: c.Request.Context()}
	return func() { c.Writer = original }
}

func (w *timelineResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.pending = true
	}
	if err != nil {
		w.writeError = err
		w.trace.PayloadWritten(err)
	}
	return n, err
}
func (w *timelineResponseWriter) WriteString(data string) (int, error) {
	n, err := w.ResponseWriter.WriteString(data)
	if n > 0 {
		w.pending = true
	}
	if err != nil {
		w.writeError = err
		w.trace.PayloadWritten(err)
	}
	return n, err
}
func (w *timelineResponseWriter) WriteHeartbeat(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		w.trace.PayloadWritten(err)
	}
	return n, err
}
func (w *timelineResponseWriter) Flush() { _ = w.FlushError() }
func (w *timelineResponseWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if w.pending {
		observed := err
		if observed == nil {
			observed = w.writeError
		}
		if observed == nil {
			observed = w.ctx.Err()
		}
		w.trace.PayloadWritten(observed)
		w.pending = false
	}
	return err
}
func (w *timelineResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *timelineResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.Hijack()
}
