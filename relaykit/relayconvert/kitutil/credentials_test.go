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
