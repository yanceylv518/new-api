package kitutil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMaskCredentialsCookieHeaders(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "inline cookie list",
			input: "upstream rejected Cookie: first=one; sessionid=session-value",
			want:  "upstream rejected Cookie: ***",
		},
		{
			name:  "preserve following header and diagnostic URL",
			input: "https://upstream.example/v1/chat\r\nCookie: first=one; sessionid=session-value\r\nX-Debug: trace-value",
			want:  "https://upstream.example/v1/chat\r\nCookie: ***\r\nX-Debug: trace-value",
		},
		{
			name:  "case insensitive header with quoted cookie",
			input: "cOoKiE:\tfirst=\"one\"; sessionid=session-value\nX-Debug: trace-value",
			want:  "cOoKiE:\t***\nX-Debug: trace-value",
		},
		{
			name:  "set-cookie attributes and comma",
			input: "Set-Cookie: sessionid=session-value; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Secure; HttpOnly",
			want:  "Set-Cookie: ***",
		},
		{
			name:  "multiple cookie headers",
			input: "Cookie: first=one; sessionid=session-value\nset-cookie: sessionid=another-value; HttpOnly",
			want:  "Cookie: ***\nset-cookie: ***",
		},
		{
			name:  "empty cookie must not consume next line",
			input: "Cookie:\r\nX-Debug: trace-value",
			want:  "Cookie:***\r\nX-Debug: trace-value",
		},
		{
			name:  "JSON cookie retains surrounding fields",
			input: `{"Cookie":"first=one; sessionid=session-value","message":"upstream rejected"}`,
			want:  `{"Cookie":"***","message":"upstream rejected"}`,
		},
		{
			name:  "escaped JSON cookie retains surrounding fields",
			input: `{"message":"headers: {\"Cookie\":\"first=one; sessionid=session-value\"}"}`,
			want:  `{"message":"headers: {\"Cookie\":\"***\"}"}`,
		},
		{
			name:  "cookie header inside JSON retains diagnostics",
			input: `{"message":"Cookie: first=one; sessionid=session-value\r\nX-Debug: trace-value","code":"upstream_rejected"}`,
			want:  `{"message":"Cookie: ***\r\nX-Debug: trace-value","code":"upstream_rejected"}`,
		},
		{
			name:  "cookie header before JSON string end",
			input: `{"message":"Cookie: first=one; sessionid=session-value","code":"upstream_rejected"}`,
			want:  `{"message":"Cookie: ***","code":"upstream_rejected"}`,
		},
		{
			name:  "quoted cookie inside JSON message",
			input: `{"message":"Cookie: first=\"one\"; sessionid=session-value\nX-Debug: trace-value","code":"upstream_rejected"}`,
			want:  `{"message":"Cookie: ***\nX-Debug: trace-value","code":"upstream_rejected"}`,
		},
		{
			name:  "nested JSON cookie header",
			input: `{"message":"headers: {\"error\":\"Cookie: first=one; sessionid=session-value\",\"code\":\"upstream_rejected\"}"}`,
			want:  `{"message":"headers: {\"error\":\"Cookie: ***\",\"code\":\"upstream_rejected\"}"}`,
		},
		{
			name:  "multiple JSON cookie header messages",
			input: `{"first":"Cookie: sessionid=first-value","second":"Set-Cookie: sessionid=second-value; Secure","code":"upstream_rejected"}`,
			want:  `{"first":"Cookie: ***","second":"Set-Cookie: ***","code":"upstream_rejected"}`,
		},
		{
			name:  "session query names preserve diagnostic URL",
			input: "https://upstream.example/v1/chat?session_id=fixture-one&sessionid=fixture-two&Session-Id=fixture-three&model=diagnostic",
			want:  "https://upstream.example/v1/chat?session_id=***&sessionid=***&Session-Id=***&model=diagnostic",
		},
		{
			name:  "JSON escaped query separator",
			input: `{"message":"https://upstream.example/v1/chat?model=x\u0026session_id=fixture-session"}`,
			want:  `{"message":"https://upstream.example/v1/chat?model=x\u0026session_id=***"}`,
		},
		{
			name:  "JSON escaped query name",
			input: `{"url":"https://upstream.example/v1/chat?session\u005fid=fixture-session"}`,
			want:  `{"url":"https://upstream.example/v1/chat?session_id=***"}`,
		},
		{
			name:  "truncated cookie array",
			input: `upstream denied; headers: {"Cookie":["PHPSESSID=fixture-session"`,
			want:  `upstream denied; headers: {"Cookie":["***"]`,
		},
		{
			name:  "truncated sensitive object",
			input: `upstream denied; {"Authorization":{"value":"fixture-session"`,
			want:  `upstream denied; {"Authorization":"***"`,
		},
		{
			name:  "truncated cookie array nested in valid error",
			input: `{"message":"headers: {\"Cookie\":[\"PHPSESSID=fixture-session\"","code":"denied"}`,
			want:  `{"message":"headers: {\"Cookie\":[\"***\"]","code":"denied"}`,
		},
		{
			name:  "plain session credential fields",
			input: "sessionid=fixture-one session_id: fixture-two code=upstream_rejected",
			want:  "sessionid=*** session_id: *** code=upstream_rejected",
		},
		{
			name:  "cookie header array",
			input: `{"headers":{"Cookie":["sessionid=fixture-one","arbitrary=fixture-two"]},"code":"upstream_rejected"}`,
			want:  `{"headers":{"Cookie":["***"]},"code":"upstream_rejected"}`,
		},
		{
			name:  "set cookie array",
			input: `{"Set-Cookie":["arbitrary=fixture-one; HttpOnly","other=fixture-two; Secure"],"message":"retained"}`,
			want:  `{"Set-Cookie":["***"],"message":"retained"}`,
		},
		{
			name:  "escaped cookie array inside error message",
			input: `{"message":"headers: {\"Cookie\":[\"arbitrary=fixture-one\"]}","code":"upstream_rejected"}`,
			want:  `{"message":"headers: {\"Cookie\":[\"***\"]}","code":"upstream_rejected"}`,
		},
		{
			name:  "sensitive object value",
			input: `{"Authorization":{"type":"Bearer","value":"fixture-one"},"code":"retained"}`,
			want:  `{"Authorization":"***","code":"retained"}`,
		},
		{
			name:  "numeric session value",
			input: `{"session_id":123456789,"code":"retained"}`,
			want:  `{"session_id":"***","code":"retained"}`,
		},
		{
			name:  "unicode escaped sensitive field",
			input: `{"headers":{"\u0043ookie":["arbitrary=fixture-one"]},"code":"retained"}`,
			want:  `{"headers":{"\u0043ookie":["***"]},"code":"retained"}`,
		},
		{
			name:  "embedded JSON preserves surrounding error text and number precision",
			input: `status_code=502, {"Cookie":["arbitrary=fixture-one"],"amount":0.1234567890123456789} endpoint=https://upstream.example/v1/chat`,
			want:  `status_code=502, {"Cookie":["***"],"amount":0.1234567890123456789} endpoint=https://upstream.example/v1/chat`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MaskCredentials(test.input)
			require.Equal(t, test.want, got)
			require.Equal(t, got, MaskCredentials(got))
			if len(test.input) > 0 && test.input[0] == '{' {
				require.True(t, Valid([]byte(got)))
			}
		})
	}
}

func TestMaskCredentialsDecodedKnownKey(t *testing.T) {
	input := `{"message":"fixture-a\u0026b","code":"denied"}`
	require.Equal(t, `{"message":"***","code":"denied"}`, MaskCredentials(input, "fixture-a&b"))
}
