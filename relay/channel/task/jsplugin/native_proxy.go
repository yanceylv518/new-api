package jsplugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/service"
)

// ExecuteNativeRequest 执行不创建任务的插件原生代理请求。
// 这类请求只用于只读或会话类厂商接口，不进入预扣费、任务轮询和结算链。
func (a *TaskAdaptor) ExecuteNativeRequest(
	ctx context.Context,
	requestContext pluginruntime.RouteRequestContext,
	modelName string,
	upstreamModel string,
	baseURL string,
	key string,
	channelType int,
	proxy string,
) (*channel.TaskActionResponse, error) {
	if a == nil || a.plugin == nil {
		return nil, fmt.Errorf("task plugin adaptor is unavailable")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("native proxy base URL is empty")
	}
	requestContext.RequestBody = jsonValue(requestContext.RequestBody)
	hookContext := requestContext.JSValue()
	hookContext["requestBody"] = requestContext.RequestBody
	hookContext["model"] = modelName
	hookContext["upstreamModel"] = upstreamModel
	hookContext["baseUrl"] = baseURL
	if err := a.applyUpstreamCredentials(hookContext, channelType, key, proxy); err != nil {
		return nil, err
	}
	value, err := a.plugin.Engine.Call(ctx, "buildNativeRequest", hookContext)
	if err != nil {
		return nil, err
	}
	var descriptor requestDescriptor
	if err = convert(value, &descriptor); err != nil {
		return nil, err
	}
	if descriptor.Credentialless || (descriptor.BodyType != "" && strings.ToLower(strings.TrimSpace(descriptor.BodyType)) != "json") || len(descriptor.Parts) > 0 {
		return nil, fmt.Errorf("native proxy request must be an authenticated JSON request")
	}
	if strings.TrimSpace(descriptor.URL) == "" {
		return nil, fmt.Errorf("native proxy request URL is empty")
	}
	if err = pluginruntime.ValidateRequestURL(descriptor.URL, baseURL, a.plugin.Meta.AllowedHosts); err != nil {
		return nil, err
	}

	var body io.Reader
	if descriptor.Body != nil {
		encoded, marshalErr := common.Marshal(descriptor.Body)
		if marshalErr != nil {
			return nil, fmt.Errorf("native proxy request body is invalid")
		}
		body = bytes.NewReader(encoded)
	}
	method := strings.ToUpper(strings.TrimSpace(descriptor.Method))
	if method == "" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, descriptor.URL, body)
	if err != nil {
		return nil, err
	}
	for name, headerValue := range descriptor.Headers {
		request.Header.Set(name, headerValue)
	}
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("native proxy HTTP client is unavailable")
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	started := time.Now()
	response, err := clientCopy.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxTaskPluginPersistedJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > maxTaskPluginPersistedJSONBytes {
		return nil, fmt.Errorf("native proxy response exceeds size limit")
	}
	requestID := response.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = response.Header.Get("Request-Id")
	}
	logger.LogDebug(
		ctx,
		"task_plugin subsystem=adaptor event=native_proxy_response plugin=%q method=%q status=%d elapsed_ms=%d",
		a.plugin.Meta.Key,
		method,
		response.StatusCode,
		time.Since(started).Milliseconds(),
	)
	return &channel.TaskActionResponse{StatusCode: response.StatusCode, Body: responseBody, RequestID: requestID}, nil
}
