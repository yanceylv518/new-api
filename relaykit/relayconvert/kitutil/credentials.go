package kitutil

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const credentialFieldNames = `(?:api[ _-]?key|x[ _-]?api[ _-]?key|x[ _-]?goog[ _-]?api[ _-]?key|ocp[ _-]?apim[ _-]?subscription[ _-]?key|subscription[ _-]?key|access[ _-]?token|refresh[ _-]?token|id[ _-]?token|session[ _-]?(?:token|id)|auth[ _-]?token|client[ _-]?secret|secret[ _-]?access[ _-]?key|access[ _-]?key[ _-]?secret|private[ _-]?key|awsaccesskeyid|x[ _-]?amz[ _-]?(?:credential|security[ _-]?token|signature)|x[ _-]?tos[ _-]?(?:credential|security[ _-]?token|signature)|authorization|proxy[ _-]?authorization|cookie|set[ _-]?cookie|password|passwd|signature|credential|secret|token|auth|key|sig)`
const maxCredentialJSONDepth = 16
const credentialAuthScheme = `(?:Bearer|Basic)`

var (
	credentialURLPattern           = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*:(?://|\\/\\/)[^\s<>"']+`)
	credentialUserInfoPattern      = regexp.MustCompile(`(?i)(^[a-z][a-z0-9+.-]*://)[^/?#\s]*@`)
	credentialQueryPattern         = regexp.MustCompile(`(^|[&;])([^=&;]+)=([^&;]*)`)
	credentialNamePattern          = regexp.MustCompile(`(?i)^` + credentialFieldNames + `$`)
	credentialCookieHeaderPattern  = regexp.MustCompile(`(?i)(\b(?:cookie|set-cookie)[ \t]*:[ \t]*)[^\r\n]*`)
	credentialCookieContextPattern = regexp.MustCompile(`"(?:\\.|[^"\\])*"|` + credentialCookieHeaderPattern.String())
	credentialAssignmentPattern    = regexp.MustCompile(`(?i)\b` + credentialFieldNames + `\\?["']?\s*[:=]`)
	credentialValuePattern         = regexp.MustCompile(`(?i)^\s*(\\"(?:\\.|[^"\\])*?\\"|\\'(?:\\.|[^'\\])*?\\'|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|` + credentialAuthScheme + `\s+[^\s,"'&;}\]]+|[^\s,"'&;{}\[\]\\]+)`)
	credentialJSONEscapePattern    = regexp.MustCompile(`\\(?:u[[:xdigit:]]{4}(?:\\u[[:xdigit:]]{4})?|["\\/bfnrt])`)
	credentialAuthPrefix           = regexp.MustCompile(`(?i)^` + credentialAuthScheme + `\s`)
	credentialAuthPattern          = regexp.MustCompile(`(?i)(\b` + credentialAuthScheme + `\s+)[a-z0-9._~+/=-]+`)
	credentialPrivateKeyPattern    = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
)

// MaskCredentials 保留诊断地址，只隐藏凭证；已知密钥也覆盖没有字段名的上游回显。
func MaskCredentials(value string, knownKeys ...string) string {
	return maskCredentials(value, 0, true, knownKeys...)
}

// MaskDiagnosticText handles decoded messages and text/plain bodies. They may
// contain JSON fragments, but their own surrounding quotation marks are prose.
func MaskDiagnosticText(value string, knownKeys ...string) string {
	return maskCredentials(value, 0, false, knownKeys...)
}

func maskCredentials(value string, depth int, document bool, knownKeys ...string) string {
	value = maskJSONCredentialValues(value, depth, document, knownKeys...)
	return maskPlainCredentials(value, knownKeys...)
}

func maskPlainCredentials(value string, knownKeys ...string) string {
	for _, key := range knownKeys {
		if key == "" {
			continue
		}
		quoted := strconv.Quote(key)
		for _, encoded := range []string{key, url.QueryEscape(key), url.PathEscape(key), quoted[1 : len(quoted)-1]} {
			value = strings.ReplaceAll(value, encoded, "***")
		}
	}
	if strings.Contains(value, "-----BEGIN ") {
		value = credentialPrivateKeyPattern.ReplaceAllString(value, "***")
	}
	if strings.Contains(value, "://") || strings.Contains(value, `:\/\/`) {
		value = credentialURLPattern.ReplaceAllStringFunc(value, maskCredentialURL)
	}
	value = maskCookieHeaders(value)
	value = maskCredentialFields(value)
	// Both schemes start with ASCII B/b. The anchored check retains the
	// same case-folding and whitespace rules as the full expression.
	for rest := value; len(rest) > 0; {
		i := strings.IndexAny(rest, "Bb")
		if i < 0 {
			break
		}
		if credentialAuthPrefix.MatchString(rest[i:]) {
			return credentialAuthPattern.ReplaceAllString(value, "${1}***")
		}
		rest = rest[i+1:]
	}
	return value
}

func maskCredentialURL(rawURL string) string {
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
}

func maskCredentialFields(value string) string {
	var out strings.Builder
	last := 0
	for cursor := 0; cursor < len(value); {
		separator := strings.IndexAny(value[cursor:], ":=")
		if separator < 0 {
			break
		}
		separator += cursor
		label := credentialLabelSuffix(value[cursor : separator+1])
		cursor = separator + 1
		if !credentialAssignmentPattern.MatchString(label) {
			continue
		}
		match := credentialValuePattern.FindStringSubmatchIndex(value[cursor:])
		if match == nil {
			continue
		}
		secretStart, stop := cursor+match[2], cursor+match[3]
		quote := ""
		for _, prefix := range []string{`\"`, `\'`, `"`, `'`} {
			if strings.HasPrefix(value[secretStart:stop], prefix) {
				quote = prefix
				break
			}
		}
		out.WriteString(value[last:secretStart])
		out.WriteString(quote)
		out.WriteString("***")
		out.WriteString(quote)
		last = stop
		cursor = stop
	}
	if last == 0 {
		return value
	}
	out.WriteString(value[last:])
	return out.String()
}

// These separators cannot occur inside a credential label or its optional
// quote and whitespace. Each also preserves the regexp's word boundary.
func credentialLabelSuffix(text string) string {
	return text[strings.LastIndexAny(text, ".,;&/{}[]()")+1:]
}

// 按 JSON 值边界替换敏感字段，保留周围文字、字段顺序和非凭证值的原始精度。
func maskJSONCredentialValues(value string, depth int, document bool, knownKeys ...string) string {
	var result strings.Builder
	last, nesting := 0, 0
	first := len(value) - len(strings.TrimLeft(value, " \t\r\n"))
	for cursor := 0; cursor < len(value); {
		next := strings.IndexAny(value[cursor:], "{}[]\"")
		if next < 0 {
			break
		}
		start := cursor + next
		cursor = start + 1
		switch value[start] {
		case '{', '[':
			if nesting > 0 || start == first || startsJSONContainer(value[start:]) {
				nesting++
			}
			continue
		case '}', ']':
			if nesting > 0 {
				nesting--
			}
			continue
		}
		end := credentialJSONStringEnd(value, start)
		token := value[start:end]
		cursor = end
		// A prose quotation can end at the opening quote of an embedded JSON
		// key. In that case start scanning at the container itself.
		if nesting == 0 && start != first {
			if inner := strings.IndexAny(token[1:], "{["); inner >= 0 {
				open := start + 1 + inner
				if startsJSONContainer(value[open:]) {
					cursor = open
					continue
				}
			}
		}
		var decoded string
		if err := UnmarshalJsonStr(token, &decoded); err != nil {
			standalone := document && start == first && strings.TrimSpace(value[end:]) == ""
			if nesting == 0 && !standalone && !hasEscapedCredential(token[1:], depth, knownKeys...) {
				continue
			}
			result.WriteString(value[last:start])
			result.WriteString(`"***"`)
			last = end
			continue
		}
		var replacement string
		tail := strings.TrimLeft(value[end:], " \t\r\n")
		if credentialNamePattern.MatchString(decoded) && strings.HasPrefix(tail, ":") {
			tail = strings.TrimLeft(tail[1:], " \t\r\n")
			fieldStart := len(value) - len(tail)
			var raw json.RawMessage
			if err := DecodeJson(strings.NewReader(tail), &raw); err == nil && len(raw) > 0 {
				end = fieldStart + len(raw)
			} else {
				// 值被截断时无法确定结束边界，保留字段前的诊断内容并隐藏剩余敏感片段。
				end = len(value)
			}
			masked := `"***"`
			if strings.HasPrefix(tail, "[") {
				masked = `["***"]`
			}
			replacement = value[start:fieldStart] + masked
			cursor = end
		} else {
			// 深层嵌套字符串整体隐藏，避免反复解码造成无界递归。
			replacement = `"***"`
			if depth < maxCredentialJSONDepth {
				masked := maskCredentials(decoded, depth+1, false, knownKeys...)
				if masked == decoded {
					continue
				}
				if encoded, err := Marshal(masked); err == nil {
					replacement = string(encoded)
				}
			}
		}
		result.WriteString(value[last:start])
		result.WriteString(replacement)
		last = end
	}
	if last == 0 {
		return value
	}
	result.WriteString(value[last:])
	return result.String()
}

// Return the end of the quoted span, including its closing quote when present.
// The JSON decoder still validates escapes and incomplete tokens.
func credentialJSONStringEnd(value string, start int) int {
	for i := start + 1; i < len(value); {
		offset := strings.IndexAny(value[i:], "\"\\")
		if offset < 0 {
			break
		}
		i += offset
		if value[i] == '"' {
			return i + 1
		}
		i += 2 // An escaped quote cannot close the string.
	}
	return len(value)
}

func startsJSONContainer(text string) bool {
	next := strings.TrimLeft(text[1:], " \t\r\n")
	if next == "" {
		return false
	}
	if text[0] == '{' {
		return next[0] == '"'
	}
	return strings.IndexByte("\"[{0123456789-", next[0]) >= 0 ||
		strings.HasPrefix(next, "true") || strings.HasPrefix(next, "false") || strings.HasPrefix(next, "null")
}

// A broken quoted span in prose can still contain an escaped credential.
// Inspect a decoded copy, leaving the original diagnostic untouched when safe.
func hasEscapedCredential(text string, depth int, knownKeys ...string) bool {
	if !credentialJSONEscapePattern.MatchString(text) {
		return false
	}
	// 明文凭证会由外层按字段遮盖。只检查解码后才显现的凭证，
	// 避免因同一行包含正常路径和认证头而丢失整段诊断。
	text = maskPlainCredentials(text, knownKeys...)
	decoded := credentialJSONEscapePattern.ReplaceAllStringFunc(text, func(escape string) string {
		var s string
		if UnmarshalJsonStr("\""+escape+"\"", &s) == nil {
			return s
		}
		return escape
	})
	return decoded != text && (depth >= maxCredentialJSONDepth || maskCredentials(decoded, depth+1, false, knownKeys...) != decoded)
}

// JSON 字符串先解码再处理头行，避免转义换行和结束引号后的诊断内容被遮盖。
func maskCookieHeaders(value string) string {
	for rest := value; ; {
		colon := strings.IndexByte(rest, ':')
		if colon < 0 {
			return value
		}
		if credentialCookieHeaderPattern.MatchString(credentialLabelSuffix(rest[:colon+1])) {
			break
		}
		rest = rest[colon+1:]
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
