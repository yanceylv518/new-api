package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/zhipu_web_search"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
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

func ZhipuWebSearchHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	info.InitChannelMeta(c)
	if info.ChannelType != constant.ChannelTypeZhipuWebSearch && info.ChannelType != constant.ChannelTypeNewAPI {
		return types.NewError(errors.New("channel does not support Zhipu Web Search API"), types.ErrorCodeInvalidRequest)
	}
	request, ok := info.Request.(*dto.ZhipuWebSearchRequest)
	if !ok || request == nil {
		return types.NewErrorWithStatusCode(errors.New("invalid Zhipu search request"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if err := helper.ModelMappedHelper(c, info, request); err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}
	if info.UpstreamModelName != info.OriginModelName {
		return types.NewErrorWithStatusCode(errors.New("search_engine cannot be changed by channel model mapping"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	jsonData := request.RawBody
	effective := *request
	if len(info.ParamOverride) > 0 {
		var err error
		jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
		if err != nil {
			return newAPIErrorFromParamOverride(err)
		}
		if err := dto.ValidateZhipuSearchEngineField(jsonData); err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		effective = dto.ZhipuWebSearchRequest{}
		if err := common.Unmarshal(jsonData, &effective); err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if effective.SearchEngine != info.OriginModelName {
			return types.NewErrorWithStatusCode(errors.New("search_engine cannot be changed by channel parameters"), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if err := effective.Validate(); err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
	}

	if effective.SearchIntent == nil {
		var err error
		jsonData, err = sjson.SetBytes(jsonData, "search_intent", false)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
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
	stopOnDisconnect := context.AfterFunc(c.Request.Context(), func() { _ = response.Body.Close() })
	defer stopOnDisconnect()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		keys := info.RedactionKeys()
		if response.Request != nil {
			for _, cookie := range response.Request.Cookies() {
				keys = append(keys, cookie.Value)
				info.UpstreamCredentialValues = append(info.UpstreamCredentialValues, cookie.Value)
			}
		}
		errorBody, readErr := zhipu_web_search.ReadResponseBody(response, info)
		if readErr != nil {
			apiErr := types.NewErrorWithStatusCode(fmt.Errorf("read upstream error response: %w", readErr), types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			apiErr.SetLogError(fmt.Errorf("read upstream error response: %w, partial body: %s", readErr, errorBody))
			return apiErr
		}
		copyResponse := *response
		copyResponse.Body = io.NopCloser(bytes.NewReader(errorBody))
		apiErr := service.RelayErrorHandler(c.Request.Context(), &copyResponse, false, keys...)
		if safeBody, err := redactZhipuSearchErrorBody(errorBody, keys, 0); err == nil {
			c.Data(response.StatusCode, response.Header.Get("Content-Type"), safeBody)
		}
		return apiErr
	}

	// 上游已接受搜索，客户端断开也不能重试或退回已产生的费用。
	defer service.PostTextConsumeQuota(c, info, &dto.Usage{}, []string{fmt.Sprintf("搜索引擎 %s 调用 1 次", effective.SearchEngine)})
	responseBody, err := zhipu_web_search.ReadResponseBody(response, info)
	if err != nil {
		return types.NewErrorWithStatusCode(fmt.Errorf("read upstream search response: %w", err), types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	var raw json.RawMessage
	if err := common.Unmarshal(responseBody, &raw); err != nil {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid upstream search response: %w", err), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		c.Writer.Header().Set("Content-Type", contentType)
	}
	c.Writer.WriteHeader(response.StatusCode)
	if _, err := io.Copy(zhipuSearchResponseWriter{context: c}, bytes.NewReader(responseBody)); err != nil {
		return types.NewError(err, types.ErrorCodeDoRequestFailed, types.ErrOptionWithSkipRetry())
	}
	return nil
}
