package kitutil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var longPathDiagnostic = `The 24" display profile at C:\temp\wide.icc could not be loaded. ` +
	strings.Repeat("validation message for the provided field. ", 192)

func BenchmarkMaskCredentialsLongPathDiagnostic(b *testing.B) {
	const key = "upstream-placeholder"
	if got := MaskCredentials(longPathDiagnostic, key); got != longPathDiagnostic {
		b.Fatal("diagnostic text changed")
	}
	b.SetBytes(int64(len(longPathDiagnostic)))
	b.ReportAllocs()
	for b.Loop() {
		_ = MaskCredentials(longPathDiagnostic, key)
	}
}

func TestMaskCredentialsLongPathDiagnostic(t *testing.T) {
	const knownKey = "upstream-placeholder"
	for _, tc := range []struct {
		name, detail, want string
	}{
		{"no credential", "", ""},
		{"known value", "server rejected upstream-placeholder; retry later", "server rejected ***; retry later"},
		{"field", "X-API-Key = fixture-field; retry later", "X-API-Key = ***; retry later"},
		{"adjacent assignments", "key=x-api key=fixture-field; retry later", "key=*** key=***; retry later"},
		{"ordinary field boundaries", "monkey=capuchin; result_key_name=display; api_key: 'sample value'", "monkey=capuchin; result_key_name=display; api_key: '***'"},
		{"authorization scheme", "proxy reported bAsIc fixture-auth; retry later", "proxy reported bAsIc ***; retry later"},
		{"cookie", "cOoKiE: sid=fixture-cookie; theme=light\r\nretry later", "cOoKiE: ***\r\nretry later"},
		{"URL query", "https://provider.example/search?access_token=fixture-query&trace=kept", "https://provider.example/search?access_token=***&trace=kept"},
		{"escaped URL query", `https:\/\/provider.example\/search?access%5ftoken=fixture-query&trace=kept`, `https:\/\/provider.example\/search?access%5ftoken=***&trace=kept`},
		{"private key block", "-----BEGIN PRIVATE KEY-----\nfixture\n-----END PRIVATE KEY----- retry later", "*** retry later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise both a late credential and a long diagnostic after the credential.
			for _, pair := range [][2]string{
				{longPathDiagnostic + tc.detail, longPathDiagnostic + tc.want},
				{tc.detail + "\n" + longPathDiagnostic, tc.want + "\n" + longPathDiagnostic},
			} {
				require.Equal(t, pair[1], MaskCredentials(pair[0], knownKey))
				require.Equal(t, pair[1], MaskDiagnosticText(pair[0], knownKey))
			}
		})
	}
}

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

func TestMaskCredentialsPreservesQuotesInDiagnostics(t *testing.T) {
	for _, text := range []string{
		`The 24" display is unavailable; retry tomorrow.`,
		`{"error":{"message":"The 24\" display is unavailable; retry tomorrow."}}`,
		`The 24" display has no profile at C:\Profiles\wide.icc. Retry after installing it.`,
		`{"error":{"message":"Could not read \"C:\\Profiles\\wide.icc\". Retry after installing it."}}`,
		`Unexpected delimiter: " at column 18. Expected a number.`,
		`Expected { before delimiter " at column 18.`,
		`"C:\Profiles\wide.icc" was not found.`,
	} {
		require.Equal(t, text, MaskCredentials(text))
	}
	withCredential := `The 24" display at C:\temp\wide.icc is unavailable. Authorization: Bearer fixture-token`
	require.Equal(t, `The 24" display at C:\temp\wide.icc is unavailable. Authorization: ***`, MaskCredentials(withCredential))
	encodedCredential := `API returned "invalid\q: \u0066ixture-secret`
	require.Equal(t, `API returned "***"`, MaskCredentials(encodedCredential, "fixture-secret"))
}
