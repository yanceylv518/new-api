package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/gjson"
)

const (
	ZhipuSearchStd      = "search_std"
	ZhipuSearchPro      = "search_pro"
	ZhipuSearchProSogou = "search_pro_sogou"
	ZhipuSearchProQuark = "search_pro_quark"
)

func IsZhipuSearchEngine(engine string) bool {
	switch engine {
	case ZhipuSearchStd, ZhipuSearchPro, ZhipuSearchProSogou, ZhipuSearchProQuark:
		return true
	default:
		return false
	}
}

// ValidateZhipuSearchEngineField 校验请求结构及选路所需的搜索引擎。
func ValidateZhipuSearchEngineField(body []byte) error {
	_, err := ParseZhipuWebSearchRequest(body)
	return err
}

// ParseZhipuWebSearchRequest 读取选路和敏感词检查所需字段，保留请求原文。
func ParseZhipuWebSearchRequest(body []byte) (*ZhipuWebSearchRequest, error) {
	trimmed := bytes.TrimSpace(body)
	if !gjson.ValidBytes(body) || len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("search request must be a JSON object")
	}
	request := &ZhipuWebSearchRequest{RawBody: body}
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "search_engine":
			request.SearchEngine = strings.ToLower(strings.TrimSpace(value.String()))
		case "search_query":
			request.SearchQuery = value.String()
		}
		return true
	})
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return request, nil
}

type ZhipuWebSearchRequest struct {
	SearchEngine        string          `json:"search_engine"`
	SearchQuery         string          `json:"search_query"`
	SearchIntent        *bool           `json:"search_intent,omitempty"`
	Count               *int            `json:"count,omitempty"`
	SearchDomainFilter  *string         `json:"search_domain_filter,omitempty"`
	SearchRecencyFilter *string         `json:"search_recency_filter,omitempty"`
	ContentSize         *string         `json:"content_size,omitempty"`
	RequestID           *string         `json:"request_id,omitempty"`
	UserID              *string         `json:"user_id,omitempty"`
	RawBody             json.RawMessage `json:"-"`
}

func (r *ZhipuWebSearchRequest) Validate() error {
	if !IsZhipuSearchEngine(r.SearchEngine) {
		return errors.New("search_engine must be a supported Zhipu search engine")
	}
	return nil
}

func (r *ZhipuWebSearchRequest) GetTokenCountMeta() *types.TokenCountMeta {
	return &types.TokenCountMeta{CombineText: r.SearchQuery, TokenType: types.TokenTypeTokenizer}
}

func (r *ZhipuWebSearchRequest) IsStream(_ *http.Request) bool { return false }

func (r *ZhipuWebSearchRequest) SetModelName(name string) { r.SearchEngine = name }
