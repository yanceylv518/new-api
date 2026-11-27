package common

import (
	"errors"
	"slices"
	"strings"
)

// DoubaoCredentials 将视频 API Key 与同一账号的素材签名凭证作为一套渠道密钥。
type DoubaoCredentials struct {
	APIKey    string
	AccessKey string
	SecretKey string
	Region    string
}

func ParseDoubaoCredentials(raw string) (DoubaoCredentials, error) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 1 && len(parts) != 3 && len(parts) != 4 {
		return DoubaoCredentials{}, errors.New("Doubao credentials must be APIKey or APIKey|AccessKey|SecretKey[|Region]")
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" || len(parts[i]) > 4096 || strings.ContainsAny(parts[i], "\r\n\x00") {
			return DoubaoCredentials{}, errors.New("Doubao credentials contain an empty or invalid field")
		}
		for _, value := range parts[i] {
			if value < 33 || value > 126 {
				return DoubaoCredentials{}, errors.New("Doubao credentials must use printable ASCII without spaces")
			}
		}
	}
	credentials := DoubaoCredentials{APIKey: parts[0], Region: "cn-beijing"}
	if len(parts) >= 3 {
		credentials.AccessKey, credentials.SecretKey = parts[1], parts[2]
		for _, value := range credentials.AccessKey {
			if !(value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-' || value == '_') {
				return DoubaoCredentials{}, errors.New("Doubao AccessKey has invalid characters")
			}
		}
	}
	if len(parts) == 4 {
		credentials.Region = parts[3]
		if len(credentials.Region) > 64 {
			return DoubaoCredentials{}, errors.New("Doubao region is invalid")
		}
		for _, value := range credentials.Region {
			if !(value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '-') {
				return DoubaoCredentials{}, errors.New("Doubao region is invalid")
			}
		}
	}
	return credentials, nil
}

// MaskDoubaoCredentials 也处理上游只回显组合凭证中某一项的错误。
func MaskDoubaoCredentials(message, raw string) string {
	if raw == "" {
		return message
	}
	message = strings.ReplaceAll(message, raw, "[redacted]")
	parts := strings.Split(raw, "|")
	if len(parts) == 3 || len(parts) == 4 {
		secrets := parts[:3]
		slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
		for _, part := range secrets {
			if secret := strings.TrimSpace(part); secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
	}
	return message
}
