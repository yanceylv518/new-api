package zhipu_web_search

import (
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

type Adaptor struct {
	openai.Adaptor
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.RelayMode != relayconstant.RelayModeZhipuWebSearch {
		return "", errors.New("Zhipu Web Search channels only support web search requests")
	}
	baseURL := info.ChannelBaseUrl
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeZhipuWebSearch)
	}
	return strings.TrimRight(baseURL, "/") + "/api/paas/v4/web_search", nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)
	req.Set("Authorization", "Bearer "+info.ApiKey)
	return nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (any, error) {
	return channel.DoApiRequest(a, c, info, body)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer resp.Body.Close()
	body, err := ReadResponseBody(resp, info)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeReadResponseBodyFailed)
	}
	var raw json.RawMessage
	if err := common.Unmarshal(body, &raw); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	return &dto.Usage{}, nil
}

func (a *Adaptor) GetModelList() []string {
	return []string{dto.ZhipuSearchStd, dto.ZhipuSearchPro, dto.ZhipuSearchProSogou, dto.ZhipuSearchProQuark}
}

func (a *Adaptor) GetChannelName() string {
	return "zhipu_web_search"
}

var ErrResponseBodyTooLarge = errors.New("upstream response body exceeds configured size limit")
var ErrUnsupportedContentEncoding = errors.New("unsupported upstream response content encoding")

// ReadResponseBody 按渠道上限读取响应并约束 zstd 解码窗口；调用方负责关闭响应体，0 表示不额外限制。
func ReadResponseBody(resp *http.Response, info *relaycommon.RelayInfo) ([]byte, error) {
	var maxBytes int64
	if info.ChannelType == constant.ChannelTypeZhipuWebSearch {
		if err := info.ChannelSetting.ValidateResponseBodyLimit(); err != nil {
			return nil, err
		}
		maxBytes = info.ChannelSetting.MaxResponseBodyMiB << 20
	}
	var reader io.Reader = resp.Body
	encoding := strings.Join(resp.Header.Values("Content-Encoding"), ",")
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusResetContent || resp.StatusCode == http.StatusNotModified {
		encoding = ""
	}
	decoded := false
	if encoding != "" {
		encodings := strings.Split(strings.ToLower(encoding), ",")
		var zstdLayers int64
		for _, coding := range encodings {
			if strings.TrimSpace(coding) == "zstd" {
				zstdLayers++
			}
		}
		for i := len(encodings) - 1; i >= 0; i-- {
			switch strings.TrimSpace(encodings[i]) {
			case "identity":
			case "gzip":
				gzipReader, err := gzip.NewReader(reader)
				if err != nil {
					return nil, err
				}
				defer gzipReader.Close()
				reader = gzipReader
				decoded = true
			case "deflate":
				deflateReader, err := zlib.NewReader(reader)
				if err != nil {
					return nil, err
				}
				defer deflateReader.Close()
				reader = deflateReader
				decoded = true
			case "br":
				reader = brotli.NewReader(reader)
				decoded = true
			case "zstd":
				var options []zstd.DOption
				if maxBytes > 0 {
					// 嵌套解码器同时持有窗口，共用本次响应的窗口预算。
					window := maxBytes / zstdLayers
					if window < zstd.MinWindowSize {
						return nil, ErrResponseBodyTooLarge
					}
					options = []zstd.DOption{
						zstd.WithDecoderMaxMemory(uint64(window)),
						zstd.WithDecoderConcurrency(1),
						zstd.WithDecodeBuffersBelow(0),
					}
				}
				zstdReader, err := zstd.NewReader(reader, options...)
				if err != nil {
					return nil, err
				}
				defer zstdReader.Close()
				reader = zstdReader
				decoded = true
			default:
				return nil, fmt.Errorf("%w: %s", ErrUnsupportedContentEncoding, encoding)
			}
		}
	}
	if decoded {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
	}
	if maxBytes == 0 {
		return io.ReadAll(reader)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w: %d MiB", ErrResponseBodyTooLarge, info.ChannelSetting.MaxResponseBodyMiB)
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
		return nil, fmt.Errorf("%w: %w", ErrResponseBodyTooLarge, err)
	}
	if err != nil {
		return body, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%w: %d MiB", ErrResponseBodyTooLarge, info.ChannelSetting.MaxResponseBodyMiB)
	}
	return body, nil
}
