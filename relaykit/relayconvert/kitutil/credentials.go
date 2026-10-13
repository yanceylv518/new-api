package kitutil

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const credentialFieldNames = `(?:api[ _-]?key|x[ _-]?api[ _-]?key|x[ _-]?goog[ _-]?api[ _-]?key|ocp[ _-]?apim[ _-]?subscription[ _-]?key|subscription[ _-]?key|access[ _-]?token|refresh[ _-]?token|id[ _-]?token|session[ _-]?token|auth[ _-]?token|client[ _-]?secret|secret[ _-]?access[ _-]?key|access[ _-]?key[ _-]?secret|private[ _-]?key|awsaccesskeyid|x[ _-]?amz[ _-]?(?:credential|security[ _-]?token|signature)|x[ _-]?tos[ _-]?(?:credential|security[ _-]?token|signature)|authorization|proxy[ _-]?authorization|cookie|set[ _-]?cookie|password|passwd|signature|credential|secret|token|auth|key|sig)`

var (
	credentialURLPattern           = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*:(?://|\\/\\/)[^\s<>"']+`)
	credentialUserInfoPattern      = regexp.MustCompile(`(?i)(^[a-z][a-z0-9+.-]*://)[^/?#\s]*@`)
	credentialQueryPattern         = regexp.MustCompile(`(^|[&;])([^=&;]+)=([^&;]*)`)
	credentialNamePattern          = regexp.MustCompile(`(?i)^` + credentialFieldNames + `$`)
	credentialCookieHeaderPattern  = regexp.MustCompile(`(?i)(\b(?:cookie|set-cookie)[ \t]*:[ \t]*)[^\r\n]*`)
	credentialCookieContextPattern = regexp.MustCompile(`"(?:\\.|[^"\\])*"|` + credentialCookieHeaderPattern.String())
	credentialValuePattern         = regexp.MustCompile(`(?i)(\b` + credentialFieldNames + `\\?["']?\s*[:=]\s*)(\\"(?:\\.|[^"\\])*?\\"|\\'(?:\\.|[^'\\])*?\\'|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|(?:Bearer|Basic)\s+[^\s,"'&;}\]]+|[^\s,"'&;}\]\\]+)`)
	credentialAuthPattern          = regexp.MustCompile(`(?i)(\b(?:Bearer|Basic)\s+)[a-z0-9._~+/=-]+`)
	credentialPrivateKeyPattern    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
)

// MaskCredentials 保留诊断地址，只隐藏凭证；已知密钥也覆盖没有字段名的上游回显。
func MaskCredentials(value string, knownKeys ...string) string {
	for _, key := range knownKeys {
		if key == "" {
			continue
		}
		quoted := strconv.Quote(key)
		for _, encoded := range []string{key, url.QueryEscape(key), url.PathEscape(key), quoted[1 : len(quoted)-1]} {
			value = strings.ReplaceAll(value, encoded, "***")
		}
	}
	value = credentialPrivateKeyPattern.ReplaceAllString(value, "***")
	value = credentialURLPattern.ReplaceAllStringFunc(value, func(rawURL string) string {
		escaped := strings.Contains(rawURL, `\/`)
		if escaped {
			rawURL = strings.ReplaceAll(rawURL, `\/`, `/`)
		}
		rawURL = credentialUserInfoPattern.ReplaceAllString(rawURL, "${1}***@")
		head, query, hasQuery := strings.Cut(rawURL, "?")
		if hasQuery {
			query, fragment, hasFragment := strings.Cut(query, "#")
			// 单独解析参数名，非法端口或参数值转义不能让密钥脱敏失效。
			query = credentialQueryPattern.ReplaceAllStringFunc(query, func(parameter string) string {
				parts := credentialQueryPattern.FindStringSubmatch(parameter)
				name, err := url.QueryUnescape(parts[2])
				if err != nil || !credentialNamePattern.MatchString(name) {
					return parameter
				}
				return parts[1] + parts[2] + "=***"
			})
			rawURL = head + "?" + query
			if hasFragment {
				rawURL += "#" + fragment
			}
		}
		if escaped {
			rawURL = strings.ReplaceAll(rawURL, `/`, `\/`)
		}
		return rawURL
	})
	value = maskCookieHeaders(value)
	// 错误可能混合 HTTP 头、JSON 和普通文本，按凭证字段匹配而不改写整个消息。
	value = credentialValuePattern.ReplaceAllStringFunc(value, func(field string) string {
		parts := credentialValuePattern.FindStringSubmatch(field)
		secret := parts[2]
		for _, quote := range []string{`\"`, `\'`, `"`, `'`} {
			if strings.HasPrefix(secret, quote) {
				return parts[1] + quote + "***" + quote
			}
		}
		return parts[1] + "***"
	})
	return credentialAuthPattern.ReplaceAllString(value, "${1}***")
}

// JSON 字符串先解码再处理头行，避免转义换行和结束引号后的诊断内容被遮盖。
func maskCookieHeaders(value string) string {
	if !credentialCookieHeaderPattern.MatchString(value) {
		return value
	}
	return credentialCookieContextPattern.ReplaceAllStringFunc(value, func(part string) string {
		if part[0] != '"' {
			return credentialCookieHeaderPattern.ReplaceAllString(part, "${1}***")
		}
		if !credentialCookieHeaderPattern.MatchString(part) {
			return part
		}
		var decoded string
		if err := UnmarshalJsonStr(part, &decoded); err != nil {
			return `"` + credentialCookieHeaderPattern.ReplaceAllString(part[1:len(part)-1], "${1}***") + `"`
		}
		masked := maskCookieHeaders(decoded)
		if masked == decoded {
			return part
		}
		encoded, err := Marshal(masked)
		if err != nil {
			return `"***"`
		}
		return string(encoded)
	})
}
