package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/zhipu_web_search"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const maxZhipuSearchErrorJSONDepth = 16

var zhipuSearchJSONStringPattern = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)

func maskZhipuSearchCredentials(value string, keys []string) string {
	for _, key := range keys {
		if key == "" {
			continue
		}
		quoted := strconv.Quote(key)
		for _, candidate := range []string{key, url.QueryEscape(key), url.PathEscape(key), quoted[1 : len(quoted)-1]} {
			value = strings.ReplaceAll(value, candidate, "***")
		}
	}
	return value
}

// 保留上游 JSON 格式，并使用公共规则隐藏凭证。
func redactZhipuSearchErrorBody(body []byte, keys []string, depth int) ([]byte, error) {
	var raw json.RawMessage
	if err := common.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	var result bytes.Buffer
	last, cursor := 0, 0
	for cursor < len(body) {
		match := zhipuSearchJSONStringPattern.FindIndex(body[cursor:])
		if match == nil {
			break
		}
		start, end := cursor+match[0], cursor+match[1]
		segment := string(body[cursor:start])
		if maskZhipuSearchCredentials(segment, keys) != segment {
			return nil, errors.New("cannot safely redact upstream error")
		}
		var value string
		if err := common.Unmarshal(body[start:end], &value); err != nil {
			return nil, err
		}
		fieldTail := bytes.TrimLeft(body[end:], " \t\r\n")
		if len(fieldTail) > 0 && fieldTail[0] == ':' {
			probe := "{" + string(body[start:end]) + `:"field-value"}`
			if common.MaskCredentials(probe) != probe {
				fieldTail = bytes.TrimLeft(fieldTail[1:], " \t\r\n")
				fieldStart := len(body) - len(fieldTail)
				var fieldValue json.RawMessage
				if err := common.DecodeJson(bytes.NewReader(fieldTail), &fieldValue); err != nil {
					return nil, err
				}
				first := fieldValue[0]
				ambiguous := strings.EqualFold(value, "token") || strings.EqualFold(value, "key") || strings.EqualFold(value, "auth") || strings.EqualFold(value, "sig")
				if !ambiguous || first == '"' || first == '[' || first == '{' {
					fieldEnd := fieldStart + len(fieldValue)
					fragment := "{" + string(body[start:fieldEnd]) + "}"
					masked := common.MaskCredentials(fragment, keys...)
					var checked json.RawMessage
					if err := common.Unmarshal([]byte(masked), &checked); err != nil {
						return nil, err
					}
					if masked != fragment {
						result.Write(body[last:start])
						result.WriteString(masked[1 : len(masked)-1])
						last = fieldEnd
						cursor = fieldEnd
						continue
					}
				}
			}
		}
		masked := maskZhipuSearchCredentials(value, keys)
		trimmed := strings.TrimSpace(masked)
		var nested json.RawMessage
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '"') &&
			common.Unmarshal([]byte(masked), &nested) == nil {
			if depth >= maxZhipuSearchErrorJSONDepth {
				return nil, errors.New("upstream error nesting exceeds redaction limit")
			}
			cleaned, err := redactZhipuSearchErrorBody([]byte(masked), keys, depth+1)
			if err != nil {
				return nil, err
			}
			masked = string(cleaned)
		} else {
			masked = common.MaskCredentials(masked, keys...)
		}
		if masked != value {
			encoded, err := common.Marshal(masked)
			if err != nil {
				return nil, err
			}
			result.Write(body[last:start])
			result.Write(encoded)
			last = end
		}
		cursor = end
	}
	tail := string(body[cursor:])
	if maskZhipuSearchCredentials(tail, keys) != tail {
		return nil, errors.New("cannot safely redact upstream error")
	}
	if last == 0 {
		return body, nil
	}
	result.Write(body[last:])
	return result.Bytes(), nil
}

type zhipuSearchResponseWriter struct {
	context *gin.Context
}

func (w zhipuSearchResponseWriter) Write(p []byte) (int, error) {
	helper.ExtendWriteDeadline(w.context)
	return w.context.Writer.Write(p)
}

func sanitizeZhipuSearchRedirect(response *http.Response, keys []string) error {
	locations := response.Header.Values("Location")
	if len(locations) == 0 {
		return nil
	}
	unsafeRedirect := errors.New("upstream search redirect target is not allowed")
	if response.Request == nil || response.Request.URL == nil {
		return unsafeRedirect
	}
	base := response.Request.URL
	basePort := base.Port()
	if basePort == "" {
		basePort = "80"
		if strings.EqualFold(base.Scheme, "https") {
			basePort = "443"
		}
	}
	safeLocations := make([]string, 0, len(locations))
	for _, location := range locations {
		if location == "" {
			safeLocations = append(safeLocations, "")
			continue
		}
		if strings.TrimSpace(location) != location || strings.ContainsAny(location, "\\\r\n\t") {
			return unsafeRedirect
		}
		reference, err := url.Parse(location)
		if err != nil || reference.Opaque != "" || reference.User != nil {
			return unsafeRedirect
		}
		target := base.ResolveReference(reference)
		port := target.Port()
		if port == "" {
			port = "80"
			if strings.EqualFold(target.Scheme, "https") {
				port = "443"
			}
		}
		if !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Hostname(), base.Hostname()) || port != basePort || target.User != nil {
			return unsafeRedirect
		}
		if strings.HasPrefix(target.Path, "//") || strings.ContainsAny(target.Path, "\\\r\n\t") {
			return unsafeRedirect
		}
		// 先按完整 URL 脱敏，再去掉上游源，避免客户端把网关凭证带到上游或其他源。
		safeTarget, err := url.Parse(common.MaskCredentials(target.String(), keys...))
		if err != nil {
			return unsafeRedirect
		}
		value := safeTarget.RequestURI()
		if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\\\r\n\t") {
			return unsafeRedirect
		}
		if safeTarget.Fragment != "" {
			value += "#" + safeTarget.EscapedFragment()
		}
		safeLocations = append(safeLocations, value)
	}
	response.Header["Location"] = safeLocations
	return nil
}

func copyZhipuSearchResponseHeaders(c *gin.Context, response *http.Response, keys []string) {
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		c.Writer.Header().Set("Content-Type", common.MaskCredentials(contentType, keys...))
	} else {
		c.Writer.Header()["Content-Type"] = nil
	}
	for _, name := range []string{"Retry-After", "Allow", "Location", "Content-Encoding"} {
		for _, value := range response.Header.Values(name) {
			c.Writer.Header().Add(name, common.MaskCredentials(value, keys...))
		}
	}
	if len(response.Header.Values("Content-Length")) == 0 {
		c.Writer.Header()["Content-Length"] = nil
	}
}

func ZhipuWebSearchHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	info.InitChannelMeta(c)
	if info.ChannelType != constant.ChannelTypeZhipuWebSearch && info.ChannelType != constant.ChannelTypeNewAPI {
		return types.NewError(errors.New("channel does not support Zhipu Web Search API"), types.ErrorCodeInvalidRequest)
	}
	request, ok := info.Request.(*dto.ZhipuWebSearchRequest)
	if !ok || request == nil {
		return types.NewErrorWithStatusCode(errors.New("invalid Zhipu search request"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	invalidRequest := common.GetContextKeyBool(c, constant.ContextKeyZhipuSearchInvalidRequest)
	jsonData := request.RawBody
	effective := *request
	if !invalidRequest && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
		if err := helper.ModelMappedHelper(c, info, request); err != nil {
			return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
		}
		if info.UpstreamModelName != info.OriginModelName {
			return types.NewErrorWithStatusCode(errors.New("search_engine cannot be changed by channel model mapping"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if len(info.ParamOverride) > 0 {
			var err error
			jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
			if err != nil {
				return newAPIErrorFromParamOverride(err)
			}
			parsed, err := dto.ParseZhipuWebSearchRequest(jsonData)
			if err != nil {
				return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
			effective = *parsed
			if effective.SearchEngine != info.OriginModelName {
				return types.NewErrorWithStatusCode(errors.New("search_engine cannot be changed by channel parameters"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
			}
		}

		before, quota, err := helper.ZhipuSearchQuota(info.PriceData)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		info.PriceData.DiscountAmounts = hosttypes.NewDiscountAmounts(before, quota)
		if quota > 0 {
			info.ForcePreConsume = true
			if info.Billing == nil {
				if billingErr := service.PreConsumeBilling(c, quota, info); billingErr != nil {
					return billingErr
				}
				info.PriceData.FreeModel = false
			} else if err := info.Billing.Reserve(quota); err != nil {
				return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
			}
		}
		info.ReservedDiscountAmounts = info.PriceData.DiscountAmounts
	}

	body, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
	if err != nil {
		return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
	}
	defer closer.Close()
	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)
	result, err := adaptor.DoRequest(c, info, body)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	response, ok := result.(*http.Response)
	if !ok || response == nil {
		return types.NewOpenAIError(errors.New("invalid upstream response"), types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}
	defer response.Body.Close()
	keys := info.RedactionKeys()
	if response.Request != nil {
		for _, cookie := range response.Request.Cookies() {
			keys = append(keys, cookie.Value)
			info.UpstreamCredentialValues = append(info.UpstreamCredentialValues, cookie.Value)
		}
	}
	if err := sanitizeZhipuSearchRedirect(response, keys); err != nil {
		response.Header.Del("Location")
		switch response.StatusCode {
		case http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusFound,
			http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			return types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		}
	}
	successStatus := response.StatusCode >= 200 && response.StatusCode < 300
	if c.Request.Method == http.MethodHead {
		copyZhipuSearchResponseHeaders(c, response, keys)
		c.Status(response.StatusCode)
		c.Writer.WriteHeaderNow()
		if !successStatus {
			common.SetContextKey(c, constant.ContextKeyZhipuSearchBusinessError, true)
			return types.NewErrorWithStatusCode(fmt.Errorf("upstream status %d", response.StatusCode), types.ErrorCodeBadResponseStatusCode, response.StatusCode, types.ErrOptionWithSkipRetry())
		}
		return nil
	}
	responseBody, readErr := zhipu_web_search.ReadResponseBody(response, info)
	if readErr != nil {
		if requestErr := c.Request.Context().Err(); requestErr != nil && errors.Is(readErr, requestErr) {
			return types.NewRequestCancelledError(requestErr)
		}
		apiErr := types.NewErrorWithStatusCode(fmt.Errorf("read upstream search response: %w", readErr), types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		apiErr.SetLogError(fmt.Errorf("read upstream search response: %w, partial body: %s", readErr, common.SanitizeErrorLogContent(string(responseBody), keys...)))
		return apiErr
	}
	var responseObject struct{}
	structuredError := false
	if successStatus {
		if err := common.Unmarshal(responseBody, &responseObject); err != nil {
			// 格式损坏的错误正文也可能回显渠道凭证，不能绕过脱敏。
			responseBody = []byte(common.MaskCredentials(string(responseBody), keys...))
		} else {
			structuredError = len(dto.ExtractJSONError(responseBody)) > 0
		}
	}
	if !successStatus || structuredError {
		common.SetContextKey(c, constant.ContextKeyZhipuSearchBusinessError, true)
		copyResponse := *response
		copyResponse.Body = io.NopCloser(bytes.NewReader(responseBody))
		apiErr := service.RelayErrorHandler(c.Request.Context(), &copyResponse, false, keys...)
		safeBody, redactErr := redactZhipuSearchErrorBody(responseBody, keys, 0)
		if redactErr != nil {
			var raw json.RawMessage
			if common.Unmarshal(responseBody, &raw) != nil && utf8.Valid(responseBody) {
				safeBody = []byte(common.MaskCredentials(string(responseBody), keys...))
				redactErr = nil
			}
		}
		if redactErr != nil {
			logger.LogWarn(c, "处理智谱上游错误响应失败: "+common.MaskCredentials(redactErr.Error(), keys...))
		}
		if redactErr == nil {
			copyZhipuSearchResponseHeaders(c, response, keys)
			c.Status(response.StatusCode)
			if _, writeErr := c.Writer.Write(safeBody); writeErr != nil {
				logger.LogWarn(c, "转发智谱上游错误失败: "+common.MaskCredentials(writeErr.Error(), keys...))
			}
		}
		return apiErr
	}
	if c.Request.Method == http.MethodOptions {
		copyZhipuSearchResponseHeaders(c, response, keys)
		c.Status(response.StatusCode)
		if _, writeErr := c.Writer.Write(responseBody); writeErr != nil {
			logger.LogWarn(c, "转发智谱上游响应失败: "+common.MaskCredentials(writeErr.Error(), keys...))
		}
		return nil
	}
	if invalidRequest {
		// 无法确定请求的引擎价格，不能按临时选路的引擎结算。
		return types.NewErrorWithStatusCode(errors.New("upstream accepted a search request without a supported engine"), types.ErrorCodeModelPriceError, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	// 完整读取且未识别到业务错误后才结算；下游写入失败不退还已完成搜索的费用。
	defer service.PostTextConsumeQuota(c, info, &dto.Usage{}, []string{fmt.Sprintf("搜索引擎 %s 调用 1 次", effective.SearchEngine)})
	copyZhipuSearchResponseHeaders(c, response, keys)
	c.Writer.WriteHeader(response.StatusCode)
	if _, err := io.Copy(zhipuSearchResponseWriter{context: c}, bytes.NewReader(responseBody)); err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
	}
	return nil
}
