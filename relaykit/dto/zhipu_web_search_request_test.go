package dto

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZhipuWebSearchOfficialFields(t *testing.T) {
	for _, field := range []string{"search_engine", "search_query", "search_intent", "count", "search_domain_filter", "search_recency_filter", "content_size", "request_id", "user_id"} {
		t.Run(field, func(t *testing.T) {
			body := []byte(`{"search_engine":"search_std","search_query":"test","search_intent":false,"count":10,"search_domain_filter":"www.example.com","search_recency_filter":"noLimit","content_size":"medium","request_id":"request-1","user_id":"user-001"}`)
			require.NoError(t, ValidateZhipuSearchEngineField(body))
			variant := strings.Replace(string(body), `"`+field+`":`, `"`+strings.ToUpper(field)+`":`, 1)
			assert.Error(t, ValidateZhipuSearchEngineField([]byte(variant)))
			duplicate := string(body[:len(body)-1]) + fmt.Sprintf(`,"%s":null}`, field)
			assert.Error(t, ValidateZhipuSearchEngineField([]byte(duplicate)))
		})
	}
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"search_engine":"search_std","search_query":"test","count":51,"COUNT":1}`, false},
		{`{"search_engine":"search_std","search_query":"test","search_intent":null}`, false},
		{`{"search_engine":"search_std","search_query":"test","count":null}`, false},
		{`{"search_engine":"search_pro_sogou","search_query":"test","count":11}`, false},
		{`{"search_engine":"search_pro_sogou","search_query":"test","count":20}`, true},
		{`{"search_engine":"search_pro_quark","search_query":"test","count":10}`, false},
		{`{"search_engine":"search_pro_quark","search_query":"test","search_domain_filter":"www.example.com"}`, false},
		{`{"search_engine":"search_pro_quark","search_query":"test","search_recency_filter":"oneWeek","content_size":"high"}`, true},
	} {
		t.Run(tc.body, func(t *testing.T) {
			err := ValidateZhipuSearchEngineField([]byte(tc.body))
			if err == nil {
				var request ZhipuWebSearchRequest
				err = kitutil.Unmarshal([]byte(tc.body), &request)
				if err == nil {
					err = request.Validate()
				}
			}
			assert.Equal(t, tc.valid, err == nil)
		})
	}
}
