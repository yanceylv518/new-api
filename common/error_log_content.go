package common

import (
	"encoding/base64"
	"io"
	"regexp"
	"strings"
)

const errorLogBase64Placeholder = "[BASE64_REDACTED]"
const errorLogUnmarkedBase64MinLength = 256

var (
	errorLogDataURLPattern     = regexp.MustCompile(`(?i)(data:[^,\s"'<>]*;base64,)(?:[a-z0-9+/_=-]|\\[/rn])+`)
	errorLogBase64FieldPattern = regexp.MustCompile(`(?i)(\b(?:b64_json|bytes[_-]?base64[_-]?encoded|(?:image|audio|video|file)[_-]?base64|base64(?:[_-]?(?:image|audio|video|file|data))?)\\?["']?\s*[:=]\s*)(\\?["']?)((?:[a-z0-9+/_=-]|\\[/rn])+)(\\?["']?)`)
	errorLogBase64RunPattern   = regexp.MustCompile(`(?:[A-Za-z0-9+/_-]|\\[/rn])+={0,2}`)
	errorLogBase64Escapes      = strings.NewReplacer(`\/`, "/", `\r`, "", `\n`, "")
)

// SanitizeErrorLogContent 移除错误日志中的凭证和媒体编码，保留周围诊断文字。
func SanitizeErrorLogContent(content string, knownKeys ...string) string {
	content = MaskCredentials(content, knownKeys...)
	content = errorLogDataURLPattern.ReplaceAllString(content, "${1}"+errorLogBase64Placeholder)
	content = errorLogBase64FieldPattern.ReplaceAllString(content, "${1}${2}"+errorLogBase64Placeholder+"${4}")
	return errorLogBase64RunPattern.ReplaceAllStringFunc(content, func(encoded string) string {
		// 无字段或 data URL 标记时只识别长编码，避免把普通错误码和请求 ID 当成媒体。
		if len(encoded) < errorLogUnmarkedBase64MinLength {
			return encoded
		}
		payload := errorLogBase64Escapes.Replace(encoded)
		if len(payload) < errorLogUnmarkedBase64MinLength {
			return encoded
		}
		encoding := base64.StdEncoding.Strict()
		if strings.ContainsAny(payload, "-_") {
			encoding = base64.URLEncoding.Strict()
		}
		if !strings.Contains(payload, "=") {
			encoding = encoding.WithPadding(base64.NoPadding)
		}
		// 流式校验避免为大媒体再分配一份解码后的数据。
		if _, err := io.Copy(io.Discard, base64.NewDecoder(encoding, strings.NewReader(payload))); err != nil {
			return encoded
		}
		return errorLogBase64Placeholder
	})
}
