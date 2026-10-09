package model

import (
	"database/sql/driver"
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 保护契约:PostgreSQL 走 simple protocol(PrepareStmt 关闭)时,driver.Valuer
// 返回 []byte 会被 pgx 按 bytea 十六进制字面量编码,写入 json 列触发
// SQLSTATE 22P02。所有 json 列的 Value() 必须返回 string(或 nil)。
func TestJSONColumnValuersReturnString(t *testing.T) {
	testCases := []struct {
		name   string
		valuer driver.Valuer
		want   string
	}{
		{
			name:   "ChannelInfo",
			valuer: ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
			want:   `{"is_multi_key":true,"multi_key_size":2,"multi_key_status_list":null,"multi_key_polling_index":0,"multi_key_mode":""}`,
		},
		{
			name:   "Properties",
			valuer: Properties{Input: "hello"},
			want:   `{"input":"hello"}`,
		},
		{
			name:   "TaskPrivateData",
			valuer: TaskPrivateData{TokenId: 7},
			want:   `{"token_id":7}`,
		},
		{
			name:   "JSONValue",
			valuer: JSONValue(`[{"k":"v"}]`),
			want:   `[{"k":"v"}]`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			value, err := testCase.valuer.Value()
			require.NoError(t, err)
			str, ok := value.(string)
			require.True(t, ok, "Value() must return string, got %T", value)
			assert.JSONEq(t, testCase.want, str)
		})
	}
}

func TestTaskPrivateDataEncryptsCredentialsAndRecoveryEvidence(t *testing.T) {
	value := TaskPrivateData{Key: "fixture-upstream-key", SubmissionPending: true, SubmissionResponse: json.RawMessage(`{"body":{"id":"fixture-id"}}`), SubmissionContext: json.RawMessage(`{"image":"private-fixture-url"}`)}
	stored, err := value.Value()
	require.NoError(t, err)
	text := stored.(string)
	assert.NotContains(t, text, "fixture-upstream-key")
	assert.NotContains(t, text, "private-fixture-url")
	var loaded TaskPrivateData
	require.NoError(t, loaded.Scan(stored))
	assert.Equal(t, value.Key, loaded.Key)
	assert.JSONEq(t, string(value.SubmissionResponse), string(loaded.SubmissionResponse))
	assert.JSONEq(t, string(value.SubmissionContext), string(loaded.SubmissionContext))
	old := common.CryptoSecret
	common.CryptoSecret = "different-fixture-secret"
	t.Cleanup(func() { common.CryptoSecret = old })
	assert.Error(t, loaded.Scan(stored))
}

// 空值仍返回 nil,保持列的 NULL 语义。
func TestJSONColumnValuersZeroValueIsNil(t *testing.T) {
	for name, valuer := range map[string]driver.Valuer{
		"Properties":      Properties{},
		"TaskPrivateData": TaskPrivateData{},
		"JSONValue":       JSONValue(nil),
	} {
		t.Run(name, func(t *testing.T) {
			value, err := valuer.Value()
			require.NoError(t, err)
			assert.Nil(t, value)
		})
	}
}

// 保护契约:json 列的 Scan 必须同时接受 []byte 与 string——不同驱动/协议
// 模式返回类型不同,静默丢弃 string 会把已有数据清零。
func TestJSONColumnScannersAcceptStringAndBytes(t *testing.T) {
	toInput := func(kind string, payload string) any {
		if kind == "bytes" {
			return []byte(payload)
		}
		return payload
	}

	for _, kind := range []string{"bytes", "string"} {
		t.Run(kind, func(t *testing.T) {
			var info ChannelInfo
			require.NoError(t, info.Scan(toInput(kind, `{"is_multi_key":true,"multi_key_size":2}`)))
			assert.True(t, info.IsMultiKey)
			assert.Equal(t, 2, info.MultiKeySize)

			var props Properties
			require.NoError(t, props.Scan(toInput(kind, `{"input":"hello"}`)))
			assert.Equal(t, "hello", props.Input)

			var private TaskPrivateData
			require.NoError(t, private.Scan(toInput(kind, `{"key":"k"}`)))
			assert.Equal(t, "k", private.Key)
		})
	}
}
