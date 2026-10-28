package common

import (
	"encoding/base64"
	"io"
	"regexp"
	"strings"
)

const errorLogBase64Placeholder = "[BASE64_REDACTED]"
const errorLogUnmarkedBase64MinLength = 256
const errorLogBase64FieldNames = `(?:b64_json|bytes[_-]?base64[_-]?encoded|(?:image|audio|video|file)[_-]?base64|base64(?:[_-]?(?:image|audio|video|file|data))?)`
const errorLogBase64Chunk = `(?:[a-z0-9+/_-]|\\[/rnt])+`

var (
	errorLogDataURLPattern         = regexp.MustCompile(`(?i)(data:[^,\s"'<>]*;base64,)(?:[a-z0-9+/_=-]|\\[/rnt])+`)
	errorLogBase64FieldPattern     = regexp.MustCompile(`(?i)(\b` + errorLogBase64FieldNames + `\\?["']?\s*[:=]\s*)(\\?["']?)((?:[a-z0-9+/_=-]|\\[/rnt])+)(\\?["']?)`)
	errorLogMultilineBase64Pattern = regexp.MustCompile(`(?i)(data:[^,\s"'<>]*;base64,|\b` + errorLogBase64FieldNames + `\\?["']?\s*[:=]\s*\\?["']?)(` + errorLogBase64Chunk + `(?:(?:\r\n|\r|\n)[ \t]*` + errorLogBase64Chunk + `)+={0,2})(\\?["']|[ \t\r\n<>,;}\]]|$)`)
	errorLogBase64RunPattern       = regexp.MustCompile(`(?:[A-Za-z0-9+/_-]|\\[/rnt])+={0,2}`)
	errorLogBase64Escapes          = strings.NewReplacer(`\/`, "/", `\r`, "", `\n`, "", `\t`, "", "\r", "", "\n", "", "\t", "", " ", "")
)

// SanitizeErrorLogContent 移除错误日志中的凭证和媒体编码，保留周围诊断文字。
func SanitizeErrorLogContent(content string, knownKeys ...string) string {
	content = MaskCredentials(content, knownKeys...)
	content = errorLogMultilineBase64Pattern.ReplaceAllStringFunc(content, func(match string) string {
		parts := errorLogMultilineBase64Pattern.FindStringSubmatch(match)
		payload, suffix := parts[2], ""
		if !isErrorLogBase64(payload) {
			// 下一行可能是普通诊断文字，只回退一行，保持校验成本与输入长度成正比。
			end := strings.LastIndexAny(payload, "\r\n")
			if end > 0 && payload[end] == '\n' && payload[end-1] == '\r' {
				end--
			}
			payload, suffix = payload[:end], payload[end:]
			if !isErrorLogBase64(payload) {
				return match
			}
		}
		return parts[1] + errorLogBase64Placeholder + suffix + parts[3]
	})
	content = errorLogDataURLPattern.ReplaceAllString(content, "${1}"+errorLogBase64Placeholder)
	content = errorLogBase64FieldPattern.ReplaceAllStringFunc(content, func(match string) string {
		parts := errorLogBase64FieldPattern.FindStringSubmatch(match)
		field := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(parts[1]), ":="))
		// 裸 base64 标签也会出现在解码错误文字中，先校验以免吞掉原始诊断。
		if strings.EqualFold(field, "base64") && parts[2] == "" && !isErrorLogBase64(parts[3]) {
			return match
		}
		return parts[1] + parts[2] + errorLogBase64Placeholder + parts[4]
	})
	return errorLogBase64RunPattern.ReplaceAllStringFunc(content, func(encoded string) string {
		// 无字段或 data URL 标记时只识别长编码，避免把普通错误码和请求 ID 当成媒体。
		if len(encoded) < errorLogUnmarkedBase64MinLength {
			return encoded
		}
		payload := errorLogBase64Escapes.Replace(encoded)
		if len(payload) < errorLogUnmarkedBase64MinLength {
			return encoded
		}
		if !isErrorLogBase64(payload) {
			return encoded
		}
		return errorLogBase64Placeholder
	})
}

func isErrorLogBase64(payload string) bool {
	payload = errorLogBase64Escapes.Replace(payload)
	encoding := base64.StdEncoding.Strict()
	if strings.ContainsAny(payload, "-_") {
		encoding = base64.URLEncoding.Strict()
	}
	if !strings.Contains(payload, "=") {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	// 流式校验避免为大媒体再分配一份解码后的数据。
	_, err := io.Copy(io.Discard, base64.NewDecoder(encoding, strings.NewReader(payload)))
	return err == nil
}
