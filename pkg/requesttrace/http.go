package requesttrace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
)

type HTTPObservation struct {
	collector *Collector
	attempt   int
	exchange  int
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
	observation := c.StartUpstream("http", proxy)
	if observation == nil {
		return req, nil
	}
	trace := &httptrace.ClientTrace{GotFirstResponseByte: observation.FirstByte}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), observation
}

func (c *Collector) StartUpstream(kind string, proxy bool) *HTTPObservation {
	if c == nil {
		return nil
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if c.current < 0 {
		if len(c.record.Data.Attempts) >= maxAttempts {
			c.record.Data.Capture.Truncated = true
			return nil
		}
		if c.record.AttemptCount == 0 {
			c.record.AttemptCount = 1
		}
		var channelID int64
		if c.record.FinalChannelID != nil {
			channelID = *c.record.FinalChannelID
		}
		c.current = len(c.record.Data.Attempts)
		c.record.Data.Attempts = append(c.record.Data.Attempts, Attempt{
			Index: c.record.AttemptCount, ChannelID: channelID, StartUS: now, Exchanges: []Exchange{},
		})
	}
	if len(c.record.Data.Attempts[c.current].Exchanges) >= maxExchanges {
		c.record.Data.Capture.Truncated = true
		return nil
	}
	attempt := &c.record.Data.Attempts[c.current]
	observation := &HTTPObservation{collector: c, attempt: c.current, exchange: len(attempt.Exchanges)}
	attempt.Exchanges = append(attempt.Exchanges, Exchange{Index: len(attempt.Exchanges) + 1, Kind: kind, StartUS: now, Proxy: proxy, Events: []Event{}})
	c.forwarding.Store(true)
	return observation
}

func (o *HTTPObservation) FirstByte() {
	if o == nil {
		return
	}
	c := o.collector
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		exchange := &c.record.Data.Attempts[o.attempt].Exchanges[o.exchange]
		// 自动重定向可能多次触发回调，保留最终响应的首字节。
		exchange.FirstByteUS = &now
	}
}

func (o *HTTPObservation) Response(resp *http.Response, err error) {
	if o == nil {
		return
	}
	c := o.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	exchange := &c.record.Data.Attempts[o.attempt].Exchanges[o.exchange]
	if err != nil || resp == nil {
		now := c.OffsetUS()
		exchange.EndUS = &now
		c.record.Data.Attempts[o.attempt].EndUS = &now
		exchange.EndReason = "request_error"
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.Canceled) {
			exchange.EndReason = "request_canceled"
		} else if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			exchange.EndReason = "request_timeout"
		}
		return
	}
	exchange.Status, exchange.Protocol = resp.StatusCode, resp.Proto
	contentType, _, _ := strings.Cut(strings.ToLower(resp.Header.Get("Content-Type")), ";")
	contentType = strings.TrimSpace(contentType)
	exchange.EventStream = contentType == "text/event-stream" || contentType == "application/vnd.amazon.eventstream"
}
