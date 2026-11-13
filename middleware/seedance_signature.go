package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	seedanceSignatureAlgorithm = "HMAC-SHA256"
	seedanceSignatureTimeForm  = "20060102T150405Z"
	seedanceSignatureMaxSkew   = 15 * time.Minute
)

var seedanceSignaturePartPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var seedanceSignedHeaderPattern = regexp.MustCompile(`^[a-z0-9-]{1,128}$`)

type seedanceSignatureAuthorization struct {
	accessKey string
	date      string
	region    string
	service   string
	headers   []string
	signature []byte
}

func parseSeedanceSignatureAuthorization(header string) (*seedanceSignatureAuthorization, error) {
	algorithm, rest, ok := strings.Cut(header, " ")
	if !ok || algorithm != seedanceSignatureAlgorithm {
		return nil, errors.New("invalid signature authorization")
	}
	fields := strings.Split(rest, ",")
	if len(fields) != 3 {
		return nil, errors.New("invalid signature authorization")
	}
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok || value == "" || values[key] != "" {
			return nil, errors.New("invalid signature authorization")
		}
		values[key] = value
	}
	credential := strings.Split(values["Credential"], "/")
	if len(credential) != 5 || !seedanceSignaturePartPattern.MatchString(credential[0]) ||
		len(credential[1]) != 8 || !seedanceSignaturePartPattern.MatchString(credential[2]) ||
		!seedanceSignaturePartPattern.MatchString(credential[3]) || credential[4] != "request" {
		return nil, errors.New("invalid signature credential")
	}
	signature, err := hex.DecodeString(values["Signature"])
	if err != nil || len(signature) != sha256.Size {
		return nil, errors.New("invalid signature")
	}
	headers := strings.Split(values["SignedHeaders"], ";")
	if len(headers) == 0 || len(headers) > 64 || !slices.Contains(headers, "host") || !slices.Contains(headers, "x-date") {
		return nil, errors.New("invalid signed headers")
	}
	seen := make(map[string]struct{}, len(headers))
	for _, name := range headers {
		if _, exists := seen[name]; exists || !seedanceSignedHeaderPattern.MatchString(name) || name == "authorization" {
			return nil, errors.New("invalid signed headers")
		}
		seen[name] = struct{}{}
	}
	return &seedanceSignatureAuthorization{
		accessKey: credential[0], date: credential[1], region: credential[2], service: credential[3], headers: headers, signature: signature,
	}, nil
}

// SeedanceCompatVideoPath 将客户兼容别名映射到插件原生入口。
func SeedanceCompatVideoPath(path string) (string, bool) {
	const video = "/doubao/api/v3/contents/generations/tasks"
	for _, alias := range []string{"/doubao/contents/generations/tasks", "/doubao/v1/contents/generations/tasks"} {
		if path == alias || strings.HasPrefix(path, alias+"/") {
			return video + strings.TrimPrefix(path, alias), true
		}
	}
	return path, false
}

// SeedanceCompatVideoRequestPath 保留别名转发前客户端实际使用的路径。
func SeedanceCompatVideoRequestPath(request *http.Request) (string, bool) {
	if request == nil || request.URL == nil {
		return "", false
	}
	path := request.URL.Path
	if request.RequestURI != "" {
		original, err := url.ParseRequestURI(request.RequestURI)
		if err != nil {
			return "", false
		}
		path = original.Path
	}
	_, ok := SeedanceCompatVideoPath(path)
	return path, ok
}

func seedanceSignaturePath(path string) bool {
	const video = "/doubao/api/v3/contents/generations/tasks"
	const assets = "/doubao/api/assets"
	_, alias := SeedanceCompatVideoPath(path)
	return alias || path == video || strings.HasPrefix(path, video+"/") ||
		path == assets || strings.HasPrefix(path, assets+"/") ||
		path == "/doubao/api/ark/assets" || path == "/doubao/v1/ark/asset" ||
		path == "/doubao/api/v3/asset" || path == "/doubao" || path == "/doubao/"
}

// seedanceSignatureURI 与火山 SDK 一样对已解码路径分段编码，保留斜杠。
func seedanceSignatureURI(path string) string {
	if path == "" {
		return "/"
	}
	const hexadecimal = "0123456789ABCDEF"
	var result strings.Builder
	for index := 0; index < len(path); index++ {
		value := path[index]
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' ||
			value == '-' || value == '_' || value == '.' || value == '~' || value == '/' {
			result.WriteByte(value)
		} else {
			result.WriteByte('%')
			result.WriteByte(hexadecimal[value>>4])
			result.WriteByte(hexadecimal[value&15])
		}
	}
	return result.String()
}

func seedanceSignatureHost(host string) string {
	name, port, err := net.SplitHostPort(host)
	if err == nil && (port == "80" || port == "443") {
		if strings.Contains(name, ":") {
			return "[" + name + "]"
		}
		return name
	}
	return host
}

func seedanceSignatureMAC(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func validSeedanceSignatureDate(request *http.Request, authorization *seedanceSignatureAuthorization, now time.Time) bool {
	dates := request.Header.Values("X-Date")
	if len(dates) != 1 || len(dates[0]) != len(seedanceSignatureTimeForm) {
		return false
	}
	date, err := time.Parse(seedanceSignatureTimeForm, dates[0])
	return err == nil && date.Format("20060102") == authorization.date && now.Sub(date) <= seedanceSignatureMaxSkew && date.Sub(now) <= seedanceSignatureMaxSkew
}

func verifySeedanceSignature(request *http.Request, authorization *seedanceSignatureAuthorization, secret string, bodyHash []byte, now time.Time) error {
	if !validSeedanceSignatureDate(request, authorization, now) {
		return errors.New("signature date is outside the allowed window")
	}
	if hashes := request.Header.Values("X-Content-Sha256"); len(hashes) > 0 {
		if len(hashes) != 1 {
			return errors.New("invalid content hash")
		}
		provided, err := hex.DecodeString(strings.TrimSpace(hashes[0]))
		if err != nil || !hmac.Equal(provided, bodyHash) {
			return errors.New("invalid content hash")
		}
	}
	original, err := url.ParseRequestURI(request.RequestURI)
	if err != nil {
		return errors.New("invalid signed request URI")
	}
	query, err := url.ParseQuery(original.RawQuery)
	if err != nil {
		return errors.New("invalid signed query")
	}
	var canonicalHeaders strings.Builder
	for _, name := range authorization.headers {
		value := ""
		if name == "host" {
			value = seedanceSignatureHost(request.Host)
		} else {
			values := request.Header.Values(name)
			if len(values) != 1 {
				return errors.New("signed header is missing or duplicated")
			}
			value = values[0]
		}
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(value))
		canonicalHeaders.WriteByte('\n')
	}
	digest := hex.EncodeToString(bodyHash)
	canonical := strings.Join([]string{
		request.Method, seedanceSignatureURI(original.Path), strings.ReplaceAll(query.Encode(), "+", "%20"),
		canonicalHeaders.String(), strings.Join(authorization.headers, ";"), digest,
	}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonical))
	scope := strings.Join([]string{authorization.date, authorization.region, authorization.service, "request"}, "/")
	toSign := strings.Join([]string{seedanceSignatureAlgorithm, request.Header.Get("X-Date"), scope, hex.EncodeToString(canonicalHash[:])}, "\n")
	key := seedanceSignatureMAC([]byte(secret), authorization.date)
	key = seedanceSignatureMAC(key, authorization.region)
	key = seedanceSignatureMAC(key, authorization.service)
	key = seedanceSignatureMAC(key, "request")
	if !hmac.Equal(seedanceSignatureMAC(key, toSign), authorization.signature) {
		return errors.New("invalid signature")
	}
	return nil
}

// SeedanceSignatureAuth 在解压和路径改写之前验证客户签名，然后复用现有令牌鉴权。
func SeedanceSignatureAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !seedanceSignaturePath(c.Request.URL.Path) || !strings.HasPrefix(header, seedanceSignatureAlgorithm) {
			c.Next()
			return
		}
		invalid := func() {
			logger.LogWarn(c, "seedance signature authentication rejected")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid signature credentials"})
		}
		if len(c.Request.Header.Values("Authorization")) != 1 {
			invalid()
			return
		}
		authorization, err := parseSeedanceSignatureAuthorization(header)
		if err != nil || !validSeedanceSignatureDate(c.Request, authorization, time.Now()) {
			invalid()
			return
		}
		credential, err := model.GetTokenSignatureCredentialByAccessKey(c.Request.Context(), authorization.accessKey)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			invalid()
			return
		}
		if err != nil {
			logger.LogError(c, "seedance signature credential lookup failed")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "signature authentication is temporarily unavailable"})
			return
		}
		secret, err := credential.OpenSecret()
		if err != nil {
			logger.LogError(c, "seedance signature credential decryption failed")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "signature authentication is temporarily unavailable"})
			return
		}
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			if common.IsRequestBodyTooLargeError(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
			} else {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			}
			return
		}
		defer storage.Close()
		reader, err := storage.NewReader()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		hasher := sha256.New()
		_, hashErr := io.Copy(hasher, reader)
		_ = reader.Close()
		if hashErr != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		if err := verifySeedanceSignature(c.Request, authorization, secret, hasher.Sum(nil), time.Now()); err != nil {
			invalid()
			return
		}
		token, err := model.GetTokenById(credential.TokenID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			invalid()
			return
		}
		if err != nil || token == nil {
			logger.LogError(c, "seedance signature token lookup failed")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "signature authentication is temporarily unavailable"})
			return
		}
		replay, err := storage.NewReader()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		defer replay.Close()
		c.Request.Body = replay
		c.Request.GetBody = storage.NewReader
		switch c.GetHeader("Content-Encoding") {
		case "gzip", "br", "zstd":
			// 后续中间件应缓存解压后的正文，而非已经验签的压缩字节。
			c.Set(common.KeyBodyStorage, nil)
			c.Set(common.KeyRequestBody, nil)
		}
		c.Request.Header.Del("Sec-WebSocket-Protocol")
		c.Request.Header.Set("Authorization", "Bearer sk-"+token.Key)
		defer c.Request.Header.Set("Authorization", header)
		c.Next()
	}
}
