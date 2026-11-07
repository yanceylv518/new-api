package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

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

func ValidateZhipuSearchEngineField(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if !gjson.ValidBytes(body) || len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("search request must be a JSON object")
	}
	found, invalid := false, false
	gjson.GetBytes(body, "@keys").ForEach(func(_, key gjson.Result) bool {
		name := key.String()
		if !strings.EqualFold(name, "search_engine") {
			return true
		}
		if found || name != "search_engine" {
			invalid = true
			return false
		}
		found = true
		return true
	})
	if !found || invalid {
		return errors.New("search_engine must be provided exactly once with the correct spelling")
	}
	return nil
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
	if strings.TrimSpace(r.SearchQuery) == "" || utf8.RuneCountInString(r.SearchQuery) > 70 {
		return errors.New("search_query must contain between 1 and 70 characters")
	}
	if r.Count != nil {
		if *r.Count < 1 || *r.Count > 50 {
			return errors.New("count must be between 1 and 50")
		}
	}
	if r.SearchRecencyFilter != nil {
		switch *r.SearchRecencyFilter {
		case "oneDay", "oneWeek", "oneMonth", "oneYear", "noLimit":
		default:
			return errors.New("invalid search_recency_filter")
		}
	}
	if r.ContentSize != nil && *r.ContentSize != "medium" && *r.ContentSize != "high" {
		return errors.New("content_size must be medium or high")
	}
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{{"request_id", r.RequestID, 64}, {"user_id", r.UserID, 128}} {
		if field.value == nil {
			continue
		}
		length := utf8.RuneCountInString(*field.value)
		if length < 6 || length > field.max {
			return fmt.Errorf("%s must contain between 6 and %d characters", field.name, field.max)
		}
	}
	return nil
}

func (r *ZhipuWebSearchRequest) GetTokenCountMeta() *types.TokenCountMeta {
	return &types.TokenCountMeta{CombineText: r.SearchQuery, TokenType: types.TokenTypeTokenizer}
}

func (r *ZhipuWebSearchRequest) IsStream(_ *http.Request) bool { return false }

func (r *ZhipuWebSearchRequest) SetModelName(name string) { r.SearchEngine = name }
