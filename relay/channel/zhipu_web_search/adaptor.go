package zhipu_web_search

import (
	"encoding/json"
	"errors"
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
	"github.com/gin-gonic/gin"
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
	body, err := io.ReadAll(resp.Body)
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
