package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
)

// SeedanceAssetClient 调用兼容火山方舟的 Seedance 素材 Action 接口。
type SeedanceAssetClient struct {
	baseURL        string
	apiKey         string
	path           string
	proxy          string
	KeyFingerprint string
}

var seedanceAssetHTTPClient = &http.Client{Timeout: 30 * time.Second}

// 仅明确的资源不存在允许删除重试继续，路由 404 和鉴权错误不能视作删除成功。
var ErrSeedanceAssetNotFound = errors.New("seedance asset resource not found")

const (
	seedanceAssetImageMaxSize      int64 = 30 << 20
	seedanceAssetVideoMaxSize      int64 = 50 << 20
	seedanceAssetAudioMaxSize      int64 = 15 << 20
	seedanceAssetMultipartOverhead       = 1 << 20
	seedanceAssetImageMaxDimension       = 6000
	seedanceAssetJPEGQuality             = 95
	seedanceAssetRequestTimeout          = 30 * time.Second
)

// privateAssetOSSStorageFactory 为素材流程提供统一的存储客户端创建入口。
var privateAssetOSSStorageFactory = NewPrivateAssetOSSStorage

// SeedanceAssetUpload 保存一次 OSS 上传的对象键、签名地址和文件信息。
type SeedanceAssetUpload struct {
	ObjectKey string
	URL       string
	AssetType string
	Size      int64
	Storage   model.SeedanceAssetStorage
	// 回滚复用上传时的客户端，配置切换不能把未入库对象的清理发送到另一 Bucket。
	storage *PrivateAssetOSSStorage
}

// SeedanceAssetMaxMultipartBodySize 返回上传请求体上限，额外空间用于 multipart 边界和表单字段。
func SeedanceAssetMaxMultipartBodySize() int64 {
	return seedanceAssetVideoMaxSize + seedanceAssetMultipartOverhead
}

// SeedanceAssetMaxUploadSize 返回各类素材对应的上游文件大小上限。
func SeedanceAssetMaxUploadSize(assetType string) int64 {
	switch assetType {
	case "Image":
		return seedanceAssetImageMaxSize
	case "Video":
		return seedanceAssetVideoMaxSize
	case "Audio":
		return seedanceAssetAudioMaxSize
	default:
		return 0
	}
}

// SeedanceAssetUploadMaxSizeLabel 返回给用户看的大小限制，保持限制与校验逻辑一致。
func SeedanceAssetUploadMaxSizeLabel(assetType string) string {
	maxSize := SeedanceAssetMaxUploadSize(assetType)
	if maxSize == 0 {
		return ""
	}
	return fmt.Sprintf("%d MB", maxSize>>20)
}

// inferSeedanceAssetType 根据浏览器 MIME 和扩展名推断上游要求的素材类型。
func inferSeedanceAssetType(filename, contentType, declaredType string) (string, error) {
	parsedContentType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		parsedContentType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	parsedContentType = strings.ToLower(parsedContentType)
	assetType := ""
	switch {
	case strings.HasPrefix(parsedContentType, "image/"):
		assetType = "Image"
	case strings.HasPrefix(parsedContentType, "video/"):
		assetType = "Video"
	case strings.HasPrefix(parsedContentType, "audio/"):
		assetType = "Audio"
	}
	if assetType == "" {
		extensionType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
		switch {
		case strings.HasPrefix(extensionType, "image/"):
			assetType = "Image"
		case strings.HasPrefix(extensionType, "video/"):
			assetType = "Video"
		case strings.HasPrefix(extensionType, "audio/"):
			assetType = "Audio"
		}
	}
	if declaredType != "" {
		if SeedanceAssetMaxUploadSize(declaredType) == 0 {
			return "", fmt.Errorf("unsupported seedance asset type")
		}
		if assetType != "" && assetType != declaredType {
			return "", fmt.Errorf("file type does not match asset type")
		}
		assetType = declaredType
	}
	if assetType == "" {
		return "", fmt.Errorf("unable to determine image, video, or audio file type")
	}
	return assetType, nil
}

// isProgressiveJPEG 按 JPEG 段结构查找 SOF2，避免把扫描数据中的字节误判为标记。
func isProgressiveJPEG(data []byte) (bool, error) {
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return false, nil
	}
	for offset := 2; offset < len(data); {
		if data[offset] != 0xff {
			return false, fmt.Errorf("invalid JPEG marker at offset %d", offset)
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			return false, fmt.Errorf("truncated JPEG marker")
		}
		marker := data[offset]
		offset++
		if marker == 0xc2 {
			return true, nil
		}
		if marker == 0xda || marker == 0xd9 {
			return false, nil
		}
		if marker == 0x01 || marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		if offset+2 > len(data) {
			return false, fmt.Errorf("truncated JPEG segment length")
		}
		segmentLength := int(data[offset])<<8 | int(data[offset+1])
		if segmentLength < 2 || offset+segmentLength > len(data) {
			return false, fmt.Errorf("invalid JPEG segment length")
		}
		offset += segmentLength
	}
	return false, fmt.Errorf("truncated JPEG data")
}

// normalizeSeedanceProgressiveJPEG 将上游实际无法处理的 progressive JPEG 规范化为 baseline JPEG。
func normalizeSeedanceProgressiveJPEG(path string, maxSize int64) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	progressive, err := isProgressiveJPEG(data)
	if err != nil {
		return 0, err
	}
	if !progressive {
		return int64(len(data)), nil
	}

	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("failed to read progressive JPEG dimensions: %w", err)
	}
	if config.Width > seedanceAssetImageMaxDimension || config.Height > seedanceAssetImageMaxDimension {
		return 0, fmt.Errorf("JPEG dimensions exceed the %d px limit", seedanceAssetImageMaxDimension)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("failed to decode progressive JPEG: %w", err)
	}
	var normalized bytes.Buffer
	if err := jpeg.Encode(&normalized, decoded, &jpeg.Options{Quality: seedanceAssetJPEGQuality}); err != nil {
		return 0, fmt.Errorf("failed to encode baseline JPEG: %w", err)
	}
	if int64(normalized.Len()) > maxSize {
		return 0, fmt.Errorf("normalized Image asset exceeds the %s upload limit", SeedanceAssetUploadMaxSizeLabel("Image"))
	}

	normalizedFile, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	written, writeErr := normalizedFile.Write(normalized.Bytes())
	closeErr := normalizedFile.Close()
	if writeErr != nil {
		return 0, writeErr
	}
	if written != normalized.Len() {
		return 0, io.ErrShortWrite
	}
	if closeErr != nil {
		return 0, closeErr
	}
	return int64(written), nil
}

// StoreSeedanceAssetUpload 校验并规范化用户文件，再持久写入 OSS。
// 本地文件只承担有界缓冲和图片转码，OSS 对象由用户手动删除素材时回收。
func StoreSeedanceAssetUpload(ctx context.Context, reader io.Reader, filename, contentType, declaredType string, size int64, userID int) (*SeedanceAssetUpload, error) {
	assetType, err := inferSeedanceAssetType(filename, contentType, declaredType)
	if err != nil {
		return nil, err
	}
	maxSize := SeedanceAssetMaxUploadSize(assetType)
	if size < 0 || size > maxSize {
		return nil, fmt.Errorf("%s asset exceeds the %s upload limit", assetType, SeedanceAssetUploadMaxSizeLabel(assetType))
	}
	storage, err := privateAssetOSSStorageFactory()
	if err != nil {
		return nil, err
	}

	extension := strings.ToLower(filepath.Ext(filepath.Base(filename)))
	if len(extension) > 12 || strings.ContainsAny(extension, `/\\:*?"<>|`) {
		extension = ""
	}
	if extension == "" {
		extension = ".bin"
	}
	file, err := os.CreateTemp("", "new-api-seedance-asset-*"+extension)
	if err != nil {
		return nil, fmt.Errorf("failed to create seedance asset temporary file: %w", err)
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()

	written, copyErr := io.Copy(file, io.LimitReader(reader, maxSize+1))
	closeErr := file.Close()
	if copyErr != nil {
		return nil, fmt.Errorf("failed to save seedance asset upload: %w", copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("failed to close seedance asset upload: %w", closeErr)
	}
	if written == 0 {
		return nil, fmt.Errorf("seedance asset file is empty")
	}
	if written > maxSize {
		return nil, fmt.Errorf("%s asset exceeds the %s upload limit", assetType, SeedanceAssetUploadMaxSizeLabel(assetType))
	}
	if assetType == "Image" {
		written, err = normalizeSeedanceProgressiveJPEG(path, maxSize)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize Seedance image: %w", err)
		}
	}

	storedFile, detectedContentType, err := openSeedanceAssetUploadPath(path)
	if err != nil {
		return nil, err
	}
	defer storedFile.Close()
	objectKey, err := storage.NewPrivateAssetOSSObjectKey(userID, extension)
	if err != nil {
		return nil, err
	}
	if err := storage.UploadPrivateAsset(ctx, objectKey, detectedContentType, written, storedFile); err != nil {
		cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
		defer cancel()
		if cleanupErr := storage.DeletePrivateAsset(cleanupCtx, objectKey); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("failed to clean up interrupted OSS upload: %w", cleanupErr))
		}
		return nil, err
	}
	upstreamURL, err := storage.PrivateAssetUpstreamURL(ctx, objectKey)
	if err != nil {
		cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
		defer cancel()
		if cleanupErr := storage.DeletePrivateAsset(cleanupCtx, objectKey); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("failed to clean up OSS object after signing failure: %w", cleanupErr))
		}
		return nil, err
	}
	return &SeedanceAssetUpload{
		ObjectKey: objectKey,
		URL:       upstreamURL,
		AssetType: assetType,
		Size:      written,
		Storage:   storage.location,
		storage:   storage,
	}, nil
}

func openSeedanceAssetUploadPath(path string) (*os.File, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}

	header := make([]byte, 512)
	read, readErr := file.Read(header)
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		_ = file.Close()
		return nil, "", seekErr
	}
	if readErr != nil && readErr != io.EOF {
		_ = file.Close()
		return nil, "", readErr
	}
	contentType := http.DetectContentType(header[:read])
	if contentType == "application/octet-stream" {
		if extensionType := mime.TypeByExtension(filepath.Ext(path)); extensionType != "" {
			contentType = extensionType
		}
	}
	return file, contentType, nil
}

// RemoveSeedanceAssetObject 删除 OSS 对象；非 OSS 素材无需执行存储清理。
func RemoveSeedanceAssetObject(ctx context.Context, objectKey string, location ...model.SeedanceAssetStorage) error {
	if strings.TrimSpace(objectKey) == "" {
		return nil
	}
	storage, err := privateAssetOSSStorageFactory(location...)
	if err != nil {
		return err
	}
	return storage.DeletePrivateAsset(ctx, objectKey)
}

// seedanceAssetCleanupContext 为外部请求失败后的补偿操作提供独立且有界的生命周期。
func seedanceAssetCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), privateAssetOSSCleanupTimeout)
}

// RollbackSeedanceAsset 同时回收上游素材和本地 OSS 对象，供本地落库失败时使用。
func RollbackSeedanceAsset(ctx context.Context, client *SeedanceAssetClient, assetID, objectKey string, upload ...*SeedanceAssetUpload) error {
	cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
	defer cancel()
	if client == nil {
		return errors.New("seedance asset client is nil")
	}

	var cleanupErr error
	if strings.TrimSpace(assetID) != "" {
		if err := client.DeleteSeedanceAsset(cleanupCtx, assetID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("upstream asset cleanup failed: %w", err))
		}
	}
	if strings.TrimSpace(objectKey) != "" {
		var err error
		if len(upload) > 0 && upload[0] != nil && upload[0].storage != nil {
			err = upload[0].storage.DeletePrivateAsset(cleanupCtx, objectKey)
		} else {
			err = RemoveSeedanceAssetObject(cleanupCtx, objectKey)
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("OSS asset cleanup failed: %w", err))
		}
	}
	return cleanupErr
}

// RollbackSeedanceAssetGroup 回收上游素材组，供本地授权映射创建失败时使用。
func RollbackSeedanceAssetGroup(ctx context.Context, client *SeedanceAssetClient, groupID string) error {
	cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
	defer cancel()
	if client == nil {
		return errors.New("seedance asset client is nil")
	}
	if strings.TrimSpace(groupID) == "" {
		return nil
	}
	return client.DeleteSeedanceAssetGroup(cleanupCtx, groupID)
}

// RollbackSeedanceAssetGroupName 恢复远端素材组名称，避免本地更新失败后两端不一致。
func RollbackSeedanceAssetGroupName(ctx context.Context, client *SeedanceAssetClient, groupID, name string) error {
	cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
	defer cancel()
	if client == nil {
		return errors.New("seedance asset client is nil")
	}
	return client.UpdateSeedanceAssetGroup(cleanupCtx, groupID, name, &struct{}{})
}

// SeedanceAssetOSSPreviewURL 为 OSS 素材生成新的短期预览地址。
func SeedanceAssetOSSPreviewURL(ctx context.Context, objectKey string, location ...model.SeedanceAssetStorage) (string, error) {
	storage, err := privateAssetOSSStorageFactory(location...)
	if err != nil {
		return "", err
	}
	return storage.PrivateAssetPreviewURL(ctx, objectKey)
}

// NewSeedanceAssetClient 使用渠道账号调用素材 Action，不依赖视频生成的 Go 适配器。
func NewSeedanceAssetClient(channel *model.Channel, binding ...string) (*SeedanceAssetClient, error) {
	if channel == nil || channel.Status != common.ChannelStatusEnabled {
		return nil, fmt.Errorf("Seedance channel is unavailable")
	}
	if !isSeedanceAssetChannel(channel) {
		return nil, fmt.Errorf("channel does not support Seedance assets")
	}
	var key string
	if len(binding) == 0 {
		selected, _, err := channel.GetNextEnabledKey()
		if err != nil {
			return nil, err
		}
		key = selected
	} else if binding[0] == "" && !channel.ChannelInfo.IsMultiKey {
		key = channel.Key
	} else {
		// 以密钥摘要绑定账号，不依赖可重新排序的密钥下标，也不把密钥写入素材表。
		for index, candidate := range channel.GetKeys() {
			if fmt.Sprintf("%x", sha256.Sum256([]byte(candidate))) != binding[0] {
				continue
			}
			if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; exists && status != common.ChannelStatusEnabled {
				return nil, fmt.Errorf("bound Seedance account is disabled")
			}
			key = candidate
			break
		}
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("bound Seedance account is unavailable")
	}
	path := os.Getenv("SEEDANCE_ASSET_API_PATH")
	if path == "" {
		path = "/seedance"
	}
	baseURL := strings.TrimRight(channel.GetBaseURL(), "/")
	assetPath := "/" + strings.Trim(path, "/")
	if strings.HasSuffix(baseURL, assetPath) {
		baseURL = strings.TrimSuffix(baseURL, assetPath)
	}
	return &SeedanceAssetClient{
		baseURL:        baseURL,
		apiKey:         key,
		path:           assetPath,
		proxy:          channel.GetSetting().Proxy,
		KeyFingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(key))),
	}, nil
}

func (client *SeedanceAssetClient) call(ctx context.Context, action string, request any, response any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// 请求级截止时间覆盖直连、代理、响应头及响应体读取，不依赖通用 Relay 超时。
	ctx, cancel := context.WithTimeout(ctx, seedanceAssetRequestTimeout)
	defer cancel()
	requestURL := fmt.Sprintf("%s%s?Action=%s&Version=2024-01-01", client.baseURL, client.path, action)
	body, err := common.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+client.apiKey)
	req.Header.Set("Content-Type", "application/json")
	httpClient := seedanceAssetHTTPClient
	if strings.TrimSpace(client.proxy) != "" {
		httpClient, err = GetHttpClientWithProxy(client.proxy)
		if err != nil {
			return fmt.Errorf("failed to create Seedance asset proxy client: %w", err)
		}
	}
	// Action 请求禁止重定向，防止携带账号凭证跨目标发送或被导向私网。
	requestClient := *httpClient
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := requestClient.Do(req)
	if err != nil {
		// URL 可能包含管理员配置，不在错误响应中返回其完整内容。
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			return fmt.Errorf("seedance asset request failed: %w", requestErr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	decodeErr := common.DecodeJson(io.LimitReader(resp.Body, 1<<20), &raw)
	var envelope seedanceAssetResponse
	if decodeErr == nil {
		decodeErr = common.Unmarshal(raw, &envelope)
		if decodeErr == nil {
			if err := envelope.businessError(); err != nil {
				// 上游可能回显凭证，保持资源不存在语义但过滤实际账号密钥。
				message := strings.ReplaceAll(err.Error(), client.apiKey, "[redacted]")
				if errors.Is(err, ErrSeedanceAssetNotFound) {
					return fmt.Errorf("%w: %s", ErrSeedanceAssetNotFound, message)
				}
				return errors.New(message)
			}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("seedance asset API returned status %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return decodeErr
	}
	return common.Unmarshal(raw, response)
}

type seedanceAssetResponse struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	Success          *bool  `json:"success"`
	ResponseMetadata struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"ResponseMetadata"`
	Result struct {
		ID     string `json:"Id"`
		Status string `json:"Status"`
		URL    string `json:"URL"`
	} `json:"Result"`
	Data *seedanceAssetResponseData `json:"data"`
}

type seedanceAssetContent struct {
	URL      string `json:"url"`
	ImageURL string `json:"image_url"`
	VideoURL string `json:"video_url"`
	AudioURL string `json:"audio_url"`
}

// seedanceAssetResponseData 兼容素材接口和兼容代理返回的多层 data 结构。
type seedanceAssetResponseData struct {
	Status    string                     `json:"status"`
	URL       string                     `json:"url"`
	ResultURL string                     `json:"result_url"`
	Content   seedanceAssetContent       `json:"content"`
	Data      *seedanceAssetResponseData `json:"data"`
}

// seedanceAssetResponseStatus 从兼容响应中提取统一的审核状态。
func (response seedanceAssetResponse) status() string {
	if status := strings.TrimSpace(response.Result.Status); status != "" {
		return status
	}
	for data := response.Data; data != nil; data = data.Data {
		if status := strings.TrimSpace(data.Status); status != "" {
			return status
		}
	}
	return ""
}

// seedanceAssetResponsePreviewURL 从素材 URL 或任务 content 中提取可预览地址。
func (response seedanceAssetResponse) previewURL() string {
	if previewURL := strings.TrimSpace(response.Result.URL); previewURL != "" {
		return previewURL
	}
	for data := response.Data; data != nil; data = data.Data {
		candidates := []string{
			data.URL,
			data.ResultURL,
			data.Content.URL,
			data.Content.ImageURL,
			data.Content.VideoURL,
			data.Content.AudioURL,
		}
		for _, candidate := range candidates {
			if previewURL := strings.TrimSpace(candidate); previewURL != "" {
				return previewURL
			}
		}
	}
	return ""
}

// seedanceAssetBusinessError 提取 HTTP 200 响应中的业务失败，保留上游可操作的错误信息。
func (response seedanceAssetResponse) businessError() error {
	if response.ResponseMetadata.Error != nil {
		response.Code, response.Message = response.ResponseMetadata.Error.Code, response.ResponseMetadata.Error.Message
	}
	switch strings.ToLower(strings.TrimSpace(response.Code)) {
	case "", "0", "ok", "success":
		if response.Success == nil || *response.Success {
			return nil
		}
	}
	message := strings.TrimSpace(response.Message)
	if message == "" {
		message = strings.TrimSpace(response.Code)
	}
	if message == "" {
		message = "upstream rejected operation"
	}
	err := fmt.Errorf("seedance asset API request failed: %s", message)
	switch strings.ToLower(strings.TrimSpace(response.Code)) {
	case "assetnotfound", "assetgroupnotfound", "resourcenotfound", "resourcenotfound.asset", "resourcenotfound.assetgroup", "not_found":
		return errors.Join(ErrSeedanceAssetNotFound, err)
	}
	return err
}

// ValidateSeedanceAssetSourceURL 在用户输入边界验证地址；OSS 签名地址由受信任存储配置生成。
func ValidateSeedanceAssetSourceURL(sourceURL string) error {
	parsed, err := url.Parse(sourceURL)
	if err != nil || len(sourceURL) > 2048 || parsed == nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("invalid asset source URL")
	}
	return ValidateSSRFProtectedFetchURL(sourceURL)
}

// CreateSeedanceAsset 将公网资源提交给上游托管。
func (client *SeedanceAssetClient) CreateSeedanceAsset(ctx context.Context, groupID, sourceURL, assetType, name string) (string, error) {
	var response seedanceAssetResponse
	err := client.call(ctx, "CreateAsset", map[string]string{"GroupId": groupID, "URL": sourceURL, "AssetType": assetType, "Name": name}, &response)
	if err == nil {
		err = response.businessError()
	}
	if err == nil && response.Result.ID == "" {
		return "", fmt.Errorf("seedance asset API returned an empty asset ID")
	}
	return response.Result.ID, err
}

// CreateSeedanceAssetGroup 创建上游素材组并返回组 ID。
func (client *SeedanceAssetClient) CreateSeedanceAssetGroup(ctx context.Context, name string, response any) error {
	return client.call(ctx, "CreateAssetGroup", map[string]string{"Name": name}, response)
}

// GetSeedanceAsset 查询上游素材审核状态及审核通过后的临时预览地址。
func (client *SeedanceAssetClient) GetSeedanceAsset(ctx context.Context, assetID string) (string, string, error) {
	var response seedanceAssetResponse
	err := client.call(ctx, "GetAsset", map[string]string{"Id": assetID}, &response)
	if err != nil {
		return "", "", err
	}
	if err := response.businessError(); err != nil {
		return "", "", err
	}
	// 查询错误或空响应不是审核结论，禁止用空状态覆盖已保存的状态。
	status := response.status()
	if status == "" {
		return "", "", fmt.Errorf("seedance asset API returned no asset status")
	}
	return status, response.previewURL(), nil
}

// DeleteSeedanceAsset 删除上游素材。
func (client *SeedanceAssetClient) DeleteSeedanceAsset(ctx context.Context, assetID string) error {
	err := client.call(ctx, "DeleteAsset", map[string]string{"Id": assetID}, &struct{}{})
	if errors.Is(err, ErrSeedanceAssetNotFound) {
		return nil
	}
	return err
}

// DeleteSeedanceAssetGroup 删除上游素材组。
func (client *SeedanceAssetClient) DeleteSeedanceAssetGroup(ctx context.Context, groupID string) error {
	err := client.call(ctx, "DeleteAssetGroup", map[string]string{"Id": groupID}, &struct{}{})
	if errors.Is(err, ErrSeedanceAssetNotFound) {
		return nil
	}
	return err
}

// UpdateSeedanceAssetGroup 更新上游素材组名称。
func (client *SeedanceAssetClient) UpdateSeedanceAssetGroup(ctx context.Context, groupID, name string, response any) error {
	return client.call(ctx, "UpdateAssetGroup", map[string]string{"Id": groupID, "Name": name}, response)
}

// isSeedanceAssetChannel 同时识别旧渠道类型和目标架构的显式 Doubao 插件渠道。
func isSeedanceAssetChannel(channel *model.Channel) bool {
	switch channel.Type {
	case constant.ChannelTypeDoubaoVideo:
		return true
	case constant.ChannelTypeTaskPlugin:
		return channel.GetSetting().TaskPluginKey == "doubao"
	default:
		return false
	}
}

// FindSeedanceAssetChannel 按主键游标扫描启用渠道，避免前 100 条无效配置遮蔽可用账号。
func FindSeedanceAssetChannel() (*model.Channel, error) {
	const batchSize = 100
	lastID := 0
	for {
		var channels []*model.Channel
		err := model.DB.Where("id > ? AND status = ? AND type IN ?", lastID, common.ChannelStatusEnabled,
			[]int{constant.ChannelTypeDoubaoVideo, constant.ChannelTypeTaskPlugin}).
			Order("id asc").Limit(batchSize).Find(&channels).Error
		if err != nil {
			return nil, err
		}
		for _, channel := range channels {
			lastID = channel.Id
			if !isSeedanceAssetChannel(channel) || channel.GetBaseURL() == "" {
				continue
			}
			// 只检查可用密钥，不提前推进轮询索引；实际账号选择由客户端完成。
			for index, key := range channel.GetKeys() {
				if strings.TrimSpace(key) == "" {
					continue
				}
				if channel.ChannelInfo.IsMultiKey {
					if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; exists && status != common.ChannelStatusEnabled {
						continue
					}
				}
				return channel, nil
			}
		}
		if len(channels) < batchSize {
			return nil, fmt.Errorf("no enabled Seedance channel available")
		}
	}
}
