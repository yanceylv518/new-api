package requesttrace

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
)

type HTTPObservation struct {
	collector *Collector
	attempt   int
	exchange  int
}

const initialHTTPEventCapacity = 4

// FromRequest 允许后台结算使用未关联 HTTP 请求的上下文。
func FromRequest(req *http.Request) *Collector {
	if req == nil {
		return nil
	}
	return FromContext(req.Context())
}

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type observedClient struct {
	client    HTTPClient
	collector *Collector
	proxy     bool
}

func WrapHTTPClient(client HTTPClient, collector *Collector, proxy bool) HTTPClient {
	if collector == nil {
		return client
	}
	return &observedClient{client: client, collector: collector, proxy: proxy}
}

func (c *observedClient) Do(req *http.Request) (*http.Response, error) {
	request, observation := c.collector.HTTPRequest(req, c.proxy)
	resp, err := c.client.Do(request)
	observation.Response(resp, err)
	return resp, err
}

func (c *Collector) HTTPRequest(req *http.Request, proxy bool) (*http.Request, *HTTPObservation) {
	if c == nil {
		return req, nil
	}
	now := c.OffsetUS()
	c.mu.Lock()
	if c.closed || c.current < 0 || len(c.record.Data.Attempts[c.current].Exchanges) >= maxExchanges {
		if !c.closed {
			c.record.Data.Capture.Truncated = true
		}
		c.mu.Unlock()
		return req, nil
	}
	attempt := &c.record.Data.Attempts[c.current]
	observation := &HTTPObservation{collector: c, attempt: c.current, exchange: len(attempt.Exchanges)}
	attempt.Exchanges = append(attempt.Exchanges, Exchange{Index: len(attempt.Exchanges) + 1, Kind: "http", StartUS: now, Proxy: proxy, Events: make([]Event, 0, initialHTTPEventCapacity)})
	c.mu.Unlock()
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { observation.event("get_conn", false) },
		GotConn: func(info httptrace.GotConnInfo) {
			now := c.OffsetUS()
			reused := info.Reused
			observation.eventAt("got_conn", false, now, &reused)
		},
		DNSStart:          func(httptrace.DNSStartInfo) { observation.event("dns_start", false) },
		DNSDone:           func(info httptrace.DNSDoneInfo) { observation.event("dns_done", info.Err != nil) },
		ConnectStart:      func(string, string) { observation.event("connect_start", false) },
		ConnectDone:       func(_, _ string, err error) { observation.event("connect_done", err != nil) },
		TLSHandshakeStart: func() { observation.event("tls_start", false) },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { observation.event("tls_done", err != nil) },
		WroteRequest:      func(info httptrace.WroteRequestInfo) { observation.event("wrote_request", info.Err != nil) },
		GotFirstResponseByte: func() {
			observation.event("first_byte", false)
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), observation
}

func (o *HTTPObservation) update(update func(*Exchange)) {
	if o == nil {
		return
	}
	c := o.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		update(&c.record.Data.Attempts[o.attempt].Exchanges[o.exchange])
	}
}

func (o *HTTPObservation) event(name string, failed bool) {
	o.eventAt(name, failed, o.collector.OffsetUS(), nil)
}

func (o *HTTPObservation) eventAt(name string, failed bool, now int64, reused *bool) {
	c := o.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	exchange := &c.record.Data.Attempts[o.attempt].Exchanges[o.exchange]
	if reused != nil {
		exchange.Reused = reused
	}
	if name == "first_byte" && (exchange.FirstByteUS == nil || now < *exchange.FirstByteUS) {
		exchange.FirstByteUS = &now
	}
	if len(exchange.Events) < maxEvents && c.events < 512 {
		exchange.Events = append(exchange.Events, Event{Sequence: c.events + 1, Name: name, OffsetUS: now, Failed: failed})
		c.events++
	} else {
		c.record.Data.Capture.Truncated = true
	}
}

func (o *HTTPObservation) Response(resp *http.Response, err error) {
	if o == nil {
		return
	}
	now := o.collector.OffsetUS()
	c := o.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	exchange := &c.record.Data.Attempts[o.attempt].Exchanges[o.exchange]
	if err != nil || resp == nil {
		exchange.EndUS = &now
		exchange.EndReason = "request_error"
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.Canceled) {
			exchange.EndReason = "request_canceled"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			exchange.EndReason = "request_timeout"
		}
		return
	}
	exchange.HeadersUS, exchange.Status, exchange.Protocol = &now, resp.StatusCode, resp.Proto
	contentType, _, _ := strings.Cut(strings.ToLower(resp.Header.Get("Content-Type")), ";")
	contentType = strings.TrimSpace(contentType)
	exchange.EventStream = contentType == "text/event-stream" || contentType == "application/vnd.amazon.eventstream"
	if resp.Body == nil {
		exchange.EndUS = &now
		exchange.EndReason = "empty_body"
		return
	}
	resp.Body = &observedBody{ReadCloser: resp.Body, observation: o}
}

type observedBody struct {
	io.ReadCloser
	mu          sync.Mutex
	observation *HTTPObservation
	bytes       int64
	finished    bool
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var now int64
	if err != nil {
		now = b.observation.collector.OffsetUS()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytes += int64(n)
	if b.finished && n > 0 {
		b.observation.update(func(exchange *Exchange) { exchange.BytesRead = b.bytes })
	}
	if err != nil {
		reason := "read_error"
		var timeout interface{ Timeout() bool }
		if err == io.EOF {
			reason = "eof"
		} else if errors.Is(err, context.Canceled) {
			reason = "read_canceled"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			reason = "read_timeout"
		}
		b.finish(reason, now)
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	now := b.observation.collector.OffsetUS()
	b.mu.Lock()
	defer b.mu.Unlock()
	reason := "closed"
	if err != nil {
		reason = "close_error"
	}
	b.finish(reason, now)
	return err
}

func (b *observedBody) finish(reason string, now int64) {
	if b.finished {
		return
	}
	b.finished = true
	b.observation.update(func(exchange *Exchange) {
		exchange.EndUS, exchange.BytesRead, exchange.EndReason = &now, b.bytes, reason
	})
}
