package common

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeErrorLogContentWrappedBase64(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("synthetic-media", 32) + "x"))
	lines := make([]string, 0)
	for start := 0; start < len(payload); start += 76 {
		lines = append(lines, payload[start:min(start+76, len(payload))])
	}
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"single line data URL", "data:image/png;base64," + payload + " code=denied", "data:image/png;base64,[BASE64_REDACTED] code=denied"},
		{"escaped newlines", `{"b64_json":"` + strings.Join(lines, `\n`) + `","code":"denied"}`, `{"b64_json":"[BASE64_REDACTED]","code":"denied"}`},
		{"short diagnostic ID", "request_id=review-request code=denied", "request_id=review-request code=denied"},
		{"header after media", "data:image/png;base64," + strings.Join(lines, "\n") + "\nX-Debug: trace-value", "data:image/png;base64,[BASE64_REDACTED]\nX-Debug: trace-value"},
		{"short media field", `{"b64_json":"TQ==","code":"denied"}`, `{"b64_json":"[BASE64_REDACTED]","code":"denied"}`},
		{"short data URL", "data:image/png;base64,TQ== code=denied", "data:image/png;base64,[BASE64_REDACTED] code=denied"},
		{"multiple media fields", `{"image_base64":"TQ==","audio_base64":"TWE=","code":"denied"}`, `{"image_base64":"[BASE64_REDACTED]","audio_base64":"[BASE64_REDACTED]","code":"denied"}`},
		{"decode error diagnostic", "base64: illegal base64 data at input byte 17", "base64: illegal base64 data at input byte 17"},
		{"indented wrapped media", `{"b64_json":"` + strings.Join(lines, "\n  ") + `","code":"denied"}`, `{"b64_json":"[BASE64_REDACTED]","code":"denied"}`},
		{"escaped indented media", `{"b64_json":"` + strings.Join(lines, `\n\t`) + `","code":"denied"}`, `{"b64_json":"[BASE64_REDACTED]","code":"denied"}`},
		{"unmarked media", "媒体拒绝 " + payload + " endpoint=http://origin.example.invalid/v1/images", "媒体拒绝 [BASE64_REDACTED] endpoint=http://origin.example.invalid/v1/images"},
	}
	for _, newline := range []struct{ name, value string }{{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"}} {
		wrapped := strings.Join(lines, newline.value)
		tests = append(tests,
			struct{ name, input, want string }{"data URL " + newline.name, "data:image/png;base64," + wrapped + " code=denied", "data:image/png;base64,[BASE64_REDACTED] code=denied"},
			struct{ name, input, want string }{"JSON field " + newline.name, `{"b64_json":"` + wrapped + `","code":"denied"}`, `{"b64_json":"[BASE64_REDACTED]","code":"denied"}`},
			struct{ name, input, want string }{"unquoted field " + newline.name, "audio_base64=" + wrapped + " code=denied", "audio_base64=[BASE64_REDACTED] code=denied"},
		)
	}
	urlPayload := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff, 0xff}, 64))
	urlWrapped := urlPayload[:76] + "\n" + urlPayload[76:152] + "\n" + urlPayload[152:]
	tests = append(tests,
		struct{ name, input, want string }{"URL safe encoding", "data:video/mp4;base64," + urlWrapped + " code=denied", "data:video/mp4;base64,[BASE64_REDACTED] code=denied"},
		struct{ name, input, want string }{"diagnostic after unpadded media", "data:video/mp4;base64," + urlWrapped + "\nRetry failed", "data:video/mp4;base64,[BASE64_REDACTED]\nRetry failed"},
		struct{ name, input, want string }{"escaped slash encoding", `{"b64_json":"` + strings.ReplaceAll(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb, 0xff, 0xff}, 64)), "/", `\/`) + `","code":"denied"}`, `{"b64_json":"[BASE64_REDACTED]","code":"denied"}`},
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SanitizeErrorLogContent(test.input)
			require.Equal(t, test.want, got)
			require.Equal(t, got, SanitizeErrorLogContent(got))
		})
	}
}
