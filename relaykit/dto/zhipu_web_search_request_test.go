package dto

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZhipuWebSearchRequestParsing(t *testing.T) {
	for _, tc := range []struct {
		name, body, engine, query string
		valid                     bool
	}{
		{"uppercase engine with whitespace", `{"search_engine":" SEARCH_STD ","search_query":"test"}`, "search_std", "test", true},
		{"optional values", `{"search_engine":"search_pro_quark","search_query":true,"count":"5","search_intent":null,"search_domain_filter":[]}`, "search_pro_quark", "true", true},
		{"numeric query", `{"search_engine":"search_std","search_query":123}`, "search_std", "123", true},
		{"long query", `{"search_engine":"search_std","search_query":"` + strings.Repeat("x", 71) + `"}`, "search_std", strings.Repeat("x", 71), true},
		{"last matching fields", `{"search_engine":"search_std","search_engine":"search_pro","search_query":"first","search_query":"last","SEARCH_QUERY":"ignored"}`, "search_pro", "last", true},
		{"valid final values", `{"search_engine":"unknown","search_engine":"search_std","search_query":"","search_query":"test"}`, "search_std", "test", true},
		{"escaped matching engine", `{"search_engine":"search_std","search_\u0065ngine":"search_pro","search_query":"test"}`, "search_pro", "test", true},
		{"missing query", `{"search_engine":"search_std"}`, "search_std", "", true},
		{"missing engine", `{"search_query":"test"}`, "", "", false},
		{"unknown final engine", `{"search_engine":"search_std","search_engine":"unknown","search_query":"test"}`, "", "", false},
		{"upper case engine only", `{"SEARCH_ENGINE":"search_std","search_query":"test"}`, "", "", false},
		{"invalid JSON", `{"search_engine":`, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := ParseZhipuWebSearchRequest([]byte(tc.body))
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.engine, request.SearchEngine)
			assert.Equal(t, tc.query, request.SearchQuery)
			assert.Equal(t, tc.body, string(request.RawBody))
		})
	}
}
