package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

type originAssetResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
}

type originValidationSession struct {
	ID         string `json:"id"`
	BytedToken string `json:"byted_token"`
	H5Link     string `json:"h5_link"`
}

type originValidationGroup struct {
	GroupID   string `json:"group_id"`
	GroupType string `json:"group_type"`
	Bound     *bool  `json:"bound"`
}

type originAssetUpstreamError struct {
	upstream  *seedanceAssetUpstreamError
	temporary bool
}

func (err *originAssetUpstreamError) Error() string { return err.upstream.message }
func (err *originAssetUpstreamError) Unwrap() error { return err.upstream }
func (err *originAssetUpstreamError) Is(target error) bool {
	return err.temporary && target == ErrSeedanceAssetServiceTemporary
}

// OriginAssetPublicError 只返回已脱敏的渠道业务错误。
func OriginAssetPublicError(err error) (string, bool) {
	var upstream *originAssetUpstreamError
	if !errors.As(err, &upstream) {
		return "", false
	}
	return upstream.Error(), true
}

var (
	originAssetBrandPattern  = regexp.MustCompile(`(?i)originocean(?:\.[a-z]+)?`)
	originAssetKeyPattern    = regexp.MustCompile(`(?i)\b(?:bearer\s+|sk-)[^\s"',;<>]+`)
	originAssetSecretPattern = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|byted[_-]?token|token|authorization|credentials?|secret|password)["']?\s*[:=]\s*["']?[^\s"',;<>}]+`)
)

// 普通素材组由本站持久化，上传使用上游账号的当前素材库。
func createLocalSeedanceAssetGroup(groupType string, response any) error {
	if groupType != "" && !strings.EqualFold(groupType, "AIGC") {
		return errors.New("real-person groups require a validation session")
	}
	id, err := common.GenerateRandomCharsKey(32)
	if err != nil {
		return err
	}
	payload, err := common.Marshal(map[string]any{"Result": map[string]string{"Id": seedanceLocalGroupIDPrefix + id}})
	if err != nil {
		return err
	}
	return common.Unmarshal(payload, response)
}

// originAssetRequest 只向管理员配置的素材端点及其操作路径发送渠道凭证。
func (client *SeedanceAssetClient) originAssetRequest(ctx context.Context, action, method, suffix, contentType string, body io.Reader, contentLength int64, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := seedanceAssetRequestTimeout
	if action == "CreateAsset" {
		timeout = seedanceAssetJSONImportTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := strings.TrimRight(client.baseURL, "/") + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("asset service request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+client.apiKey)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	httpClient := seedanceAssetHTTPClient
	if strings.TrimSpace(client.proxy) != "" {
		httpClient, err = GetHttpClientWithProxy(client.proxy)
		if err != nil {
			return errors.New("failed to create asset service proxy")
		}
	}
	requestClient := *httpClient
	requestClient.Timeout = timeout
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := requestClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return &seedanceAssetTemporaryError{cause: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, seedanceAssetResponseBodyLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > seedanceAssetResponseBodyLimit {
		return fmt.Errorf("%w: asset service response is too large", errSeedanceAssetResponse)
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Error   json.RawMessage `json:"error"`
	}
	decodeErr := common.Unmarshal(raw, &envelope)
	hasError := len(envelope.Error) != 0 && strings.TrimSpace(string(envelope.Error)) != "null"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || hasError || envelope.Success != nil && !*envelope.Success {
		code, message := "", envelope.Message
		if hasError {
			var detail struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if common.Unmarshal(envelope.Error, &detail) == nil {
				code, message = detail.Code, detail.Message
			} else {
				_ = common.Unmarshal(envelope.Error, &message)
			}
		}
		if message == "" && len(envelope.Detail) != 0 {
			if err := common.Unmarshal(envelope.Detail, &message); err != nil {
				kind := common.GetJsonType(envelope.Detail)
				if kind == "object" || kind == "array" {
					message = string(envelope.Detail)
				}
			}
		}
		if message == "" {
			message = code
		}
		if message == "" && decodeErr != nil && !strings.HasPrefix(strings.TrimSpace(string(raw)), "<") {
			message = strings.TrimSpace(string(raw))
		}
		// 未完成认证时，服务把稳定错误码放在外层 message 的 JSON 中。
		if action == "GetVisualValidateResult" && resp.StatusCode == http.StatusBadRequest {
			if index := strings.IndexByte(message, '{'); index >= 0 {
				var nested struct {
					Code string `json:"errorCode"`
				}
				if common.Unmarshal([]byte(message[index:]), &nested) == nil && nested.Code == "C400999" {
					return ErrSeedanceAssetValidationPending
				}
			}
		}
		message = strings.TrimSpace(message)
		if message == "" {
			message = fmt.Sprintf("asset service returned status %d", resp.StatusCode)
		}
		if client.apiKey != "" {
			message = strings.ReplaceAll(message, client.apiKey, "[redacted]")
			code = strings.ReplaceAll(code, client.apiKey, "[redacted]")
		}
		message = originAssetBrandPattern.ReplaceAllString(message, "[redacted]")
		message = originAssetKeyPattern.ReplaceAllString(message, "[redacted]")
		message = originAssetSecretPattern.ReplaceAllString(message, "[redacted]")
		message = common.MaskSensitiveInfo(message)
		if len(message) > seedanceAssetErrorPreviewLimit {
			message = message[:seedanceAssetErrorPreviewLimit] + "..."
		}
		upstream := &seedanceAssetUpstreamError{action: action, code: strings.TrimSpace(code), httpStatus: resp.StatusCode, message: message}
		publicErr := &originAssetUpstreamError{upstream: upstream, temporary: isSeedanceAssetServiceTemporary(upstream)}
		if resp.StatusCode == http.StatusNotFound && (action == "GetAsset" || action == "DeleteAsset") &&
			(isSeedanceAssetDeleteNotFoundError(upstream) || strings.EqualFold(strings.TrimSpace(code), "not_found")) {
			return errors.Join(ErrSeedanceAssetNotFound, publicErr)
		}
		return publicErr
	}
	if result == nil && len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if decodeErr != nil {
		return fmt.Errorf("%w: asset service returned invalid JSON", errSeedanceAssetResponse)
	}
	if result != nil {
		if err := common.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("%w: invalid asset service response", errSeedanceAssetResponse)
		}
	}
	return nil
}

func originAssetFilename(sourceURL, assetType, contentType string) string {
	parsed, _ := url.Parse(sourceURL)
	name := ""
	if parsed != nil {
		name = path.Base(parsed.Path)
	}
	ext := strings.ToLower(path.Ext(name))
	if name == "." || name == "/" || len(ext) < 2 || len(ext) > 12 {
		ext = ""
	}
	if ext == "" {
		mediaType, _, _ := mime.ParseMediaType(contentType)
		switch mediaType {
		case "image/png":
			ext = ".png"
		case "image/webp":
			ext = ".webp"
		case "video/quicktime":
			ext = ".mov"
		case "audio/wav", "audio/x-wav":
			ext = ".wav"
		default:
			switch assetType {
			case "Image":
				ext = ".jpg"
			case "Video":
				ext = ".mp4"
			case "Audio":
				ext = ".mp3"
			}
		}
	}
	return "asset" + ext
}

// 上游只接受文件上传，因此先有界读取本站原件，再流式构造 multipart 请求。
func (client *SeedanceAssetClient) createOriginAsset(ctx context.Context, sourceURL, assetType, name string) (string, error) {
	if err := ValidateSeedanceAssetSourceURL(sourceURL); err != nil {
		return "", errors.New("private asset source is unavailable")
	}
	limit := SeedanceAssetMaxUploadSize(assetType)
	if limit <= 0 {
		return "", errors.New("unsupported asset type")
	}
	ctx, cancel := context.WithTimeout(ctx, seedanceAssetJSONImportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", errors.New("private asset source is unavailable")
	}
	sourceClient := *GetSSRFProtectedHTTPClient()
	sourceClient.Timeout = 0
	resp, err := sourceClient.Do(req)
	if err != nil {
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			return "", fmt.Errorf("private asset source request failed: %w", requestErr.Err)
		}
		return "", fmt.Errorf("private asset source request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("private asset source returned status %d", resp.StatusCode)
		serverError := resp.StatusCode >= 500 && resp.StatusCode <= 599 && resp.StatusCode != http.StatusNotImplemented && resp.StatusCode != http.StatusHTTPVersionNotSupported
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || serverError {
			return "", errors.Join(ErrSeedanceAssetServiceTemporary, statusErr)
		}
		return "", statusErr
	}
	if resp.ContentLength > limit {
		return "", errors.New("private asset exceeds the configured size limit")
	}
	file, err := os.CreateTemp("", "seedance-origin-asset-*")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	size, err := io.Copy(file, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if size == 0 || size > limit {
		return "", errors.New("private asset is empty or exceeds the configured size limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	filename := originAssetFilename(sourceURL, assetType, resp.Header.Get("Content-Type"))
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	header.Set("Content-Type", mediaType)
	if _, err := writer.CreatePart(header); err != nil {
		return "", err
	}
	prefix := append([]byte(nil), framing.Bytes()...)
	framing.Reset()
	if strings.TrimSpace(name) != "" {
		if err := writer.WriteField("name", name); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	suffix := append([]byte(nil), framing.Bytes()...)
	body := io.MultiReader(bytes.NewReader(prefix), file, bytes.NewReader(suffix))
	var result originAssetResponse
	if err := client.originAssetRequest(ctx, "CreateAsset", http.MethodPost, "", writer.FormDataContentType(), body, int64(len(prefix))+size+int64(len(suffix)), &result); err != nil {
		return "", err
	}
	id := strings.TrimSpace(result.ID)
	if _, err := originAssetPathID(id); err != nil {
		return "", fmt.Errorf("%w: asset service returned an invalid asset ID", errSeedanceAssetResponse)
	}
	return id, nil
}

func (client *SeedanceAssetClient) getOriginAsset(ctx context.Context, assetID string) (SeedanceAssetDetails, error) {
	shortID, err := originAssetPathID(assetID)
	if err != nil {
		return SeedanceAssetDetails{}, err
	}
	var result originAssetResponse
	if err := client.originAssetRequest(ctx, "GetAsset", http.MethodGet, "/"+url.PathEscape(shortID), "", nil, 0, &result); err != nil {
		return SeedanceAssetDetails{}, err
	}
	returnedID, err := originAssetPathID(result.ID)
	if err != nil || returnedID != shortID || strings.TrimSpace(result.Status) == "" {
		return SeedanceAssetDetails{}, fmt.Errorf("%w: asset service returned invalid asset details", errSeedanceAssetResponse)
	}
	kind := ""
	switch strings.ToLower(strings.TrimSpace(result.Kind)) {
	case "image":
		kind = "Image"
	case "video":
		kind = "Video"
	case "audio":
		kind = "Audio"
	}
	status := strings.TrimSpace(result.Status)
	if strings.EqualFold(status, "ready") {
		status = "Active"
	}
	return SeedanceAssetDetails{ID: assetID, Status: status, AssetType: kind}, nil
}

func originAssetPathID(assetID string) (string, error) {
	id := strings.TrimSpace(assetID)
	if strings.HasPrefix(id, "asset://") {
		parsed, err := url.Parse(id)
		if err != nil || !strings.EqualFold(parsed.Host, "originocean") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return "", errors.New("invalid asset service ID")
		}
		id = strings.TrimPrefix(parsed.Path, "/")
	}
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\?#") ||
		strings.IndexFunc(id, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", errors.New("invalid asset service ID")
	}
	return id, nil
}

func (client *SeedanceAssetClient) deleteOriginAsset(ctx context.Context, assetID string) error {
	shortID, err := originAssetPathID(assetID)
	if err != nil {
		return err
	}
	err = client.originAssetRequest(ctx, "DeleteAsset", http.MethodDelete, "/"+url.PathEscape(shortID), "", nil, 0, nil)
	if isSeedanceAssetDeleteNotFoundError(err) {
		return nil
	}
	return err
}

func (client *SeedanceAssetClient) createOriginValidationSession(ctx context.Context) (originValidationSession, error) {
	var result originValidationSession
	body := strings.NewReader("{}")
	err := client.originAssetRequest(ctx, "CreateVisualValidateSession", http.MethodPost, "/liveness/sessions", "application/json", body, 2, &result)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(result.BytedToken) == "" || strings.TrimSpace(result.H5Link) == "" {
		return result, fmt.Errorf("%w: incomplete validation session", errSeedanceAssetResponse)
	}
	link, err := url.Parse(result.H5Link)
	if err != nil || link.Scheme != "https" || link.Host == "" || link.User != nil {
		return result, fmt.Errorf("%w: invalid validation link", errSeedanceAssetResponse)
	}
	return result, nil
}

func (client *SeedanceAssetClient) getOriginValidationResult(ctx context.Context, token string) (string, error) {
	body, err := common.Marshal(map[string]string{"byted_token": token})
	if err != nil {
		return "", err
	}
	var result originValidationGroup
	if err := client.originAssetRequest(ctx, "GetVisualValidateResult", http.MethodPost, "/liveness/asset-groups/query", "application/json", bytes.NewReader(body), int64(len(body)), &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.GroupID) == "" {
		return "", ErrSeedanceAssetValidationPending
	}
	if !strings.EqualFold(result.GroupType, "LivenessFace") {
		return "", fmt.Errorf("%w: invalid validation group", errSeedanceAssetResponse)
	}
	var bound originValidationGroup
	if err := client.originAssetRequest(ctx, "BindVisualValidateResult", http.MethodPost, "/liveness/asset-groups/bind", "application/json", bytes.NewReader(body), int64(len(body)), &bound); err != nil {
		return "", err
	}
	if bound.Bound == nil || !*bound.Bound || bound.GroupID != result.GroupID {
		return "", fmt.Errorf("%w: validation group was not bound", errSeedanceAssetResponse)
	}
	return result.GroupID, nil
}
