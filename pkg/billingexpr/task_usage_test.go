package billingexpr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskUsageExpressionUsesFactsAndTaskQuotaConversion(t *testing.T) {
	expression := `tier("1080p", u("seconds") * (u("resolution") == "1080p" ? 0.4 : 0.2))`
	cost, trace, err := RunExprWithRequest(expression, TokenParams{}, RequestInput{Usage: map[string]any{"seconds": 10.0, "resolution": "1080p"}})
	require.NoError(t, err)
	assert.Equal(t, 4.0, cost)
	assert.Equal(t, "1080p", trace.MatchedTier)
	result, err := ComputeTieredQuotaWithRequest(&BillingSnapshot{ExprString: expression, ExprHash: ExprHashString(expression), GroupRatio: 2, QuotaPerUnit: 500000, ExprVersion: 1, TaskUsageBilling: true}, TokenParams{}, RequestInput{Usage: map[string]any{"seconds": 10.0, "resolution": "1080p"}})
	require.NoError(t, err)
	assert.Equal(t, 4_000_000, result.ActualQuotaAfterGroup)
}

func TestTaskUsageSnapshotsWithoutHashUseTheirOwnExpression(t *testing.T) {
	InvalidateCache()
	t.Cleanup(InvalidateCache)
	for _, tc := range []struct {
		expression string
		quota      int
	}{
		{`tier("video", u("tokens") * 5 / 1000000)`, 1000},
		{`tier("video", u("tokens") * 2 / 1000000)`, 400},
		{`tier("video", u("tokens") * 3 / 1000000)`, 600},
	} {
		snapshot := &BillingSnapshot{ExprString: tc.expression, GroupRatio: 1, QuotaPerUnit: 500000, TaskUsageBilling: true}
		result, err := ComputeTieredQuotaWithRequest(snapshot, TokenParams{}, RequestInput{Usage: map[string]any{"tokens": 400.0}})
		require.NoError(t, err, tc.expression)
		assert.Equal(t, tc.quota, result.ActualQuotaAfterGroup, tc.expression)
	}
	_, err := ComputeTieredQuotaWithRequest(&BillingSnapshot{ExprString: `tier("video", fixed(0.003))`, TaskUsageBilling: true}, TokenParams{}, RequestInput{})
	require.ErrorContains(t, err, "fixed pricing is not supported")
	_, _, err = RunExprByHashWithRequest(`tier(`, "", TokenParams{}, RequestInput{})
	require.Error(t, err)
	assert.True(t, UsedVarsByHash(`p + 1`, "")["p"])
	assert.True(t, UsedVarsByHash(`c + 1`, "")["c"])
	assert.False(t, UsedVarsByHash(`c + 1`, "")["p"])
}
