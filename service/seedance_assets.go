package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// SeedanceAssetClient 调用兼容火山方舟的 Seedance 素材 Action 接口。
type SeedanceAssetClient struct {
	ChannelID           int
	baseURL             string
	apiKey              string
	proxy               string
	KeyFingerprint      string
	AccountFingerprint  string
	syncAccount         string
	accountFingerprints []string
	sharedAccount       bool
	jsonAction          bool
	originREST          bool
}

// ForSeedanceModel 对未配置共享组的渠道区分模型及密钥。
func (client *SeedanceAssetClient) ForSeedanceModel(channel *model.Channel, modelName string) *SeedanceAssetClient {
	if client == nil {
		return nil
	}
	copy := *client
	copy.AccountFingerprint = model.SeedanceAssetModelScopeFingerprint(channel, modelName, client.KeyFingerprint)
	copy.syncAccount = copy.AccountFingerprint
	if !copy.sharedAccount {
		copy.accountFingerprints = []string{copy.AccountFingerprint}
	}
	return &copy
}

var seedanceAssetHTTPClient = &http.Client{Timeout: 30 * time.Second}

// 仅明确的资源不存在允许删除重试继续，路由 404 和鉴权错误不能视作删除成功。
var ErrSeedanceAssetNotFound = errors.New("seedance asset resource not found")

var errSeedanceAssetAccountMissing = errors.New("bound Seedance account no longer exists")

var ErrSeedanceAssetGroupPending = errors.New("the verified asset group is still processing")

var ErrSeedanceAssetServiceTemporary = errors.New("Asset service is temporarily unavailable. Please try again.")

var ErrSeedanceAssetChannelUnavailable = errors.New("no enabled Seedance channel available")

var errSeedanceAssetResponse = errors.New("invalid Seedance asset response")

const (
	seedanceAssetImageMaxDimension  = 6000
	seedanceAssetJPEGQuality        = 95
	seedanceAssetRequestTimeout     = 30 * time.Second
	seedanceAssetJSONImportTimeout  = 10 * time.Minute
	seedanceAssetURLDownloadTimeout = 10 * time.Minute
	seedanceAssetURLStoreTimeout    = 20 * time.Minute
	seedanceAssetResponseBodyLimit  = 1 << 20
	seedanceAssetErrorPreviewLimit  = 4096
	seedanceNativeGroupIDPrefix     = "group-"
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
	return common.PrivateAssetMultipartBodyLimit(false)
}

func SeedanceCompatMaxMultipartBodySize() int64 {
	return common.PrivateAssetMultipartBodyLimit(true)
}

// SeedanceAssetMaxUploadSize 返回管理员配置的各类素材文件大小上限。
func SeedanceAssetMaxUploadSize(assetType string) int64 {
	limits := common.GetManagementRateLimits().AssetLimits
	switch assetType {
	case "Image":
		return limits.ImageMiB << 20
	case "Video":
		return limits.VideoMiB << 20
	case "Audio":
		return limits.AudioMiB << 20
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
func isProgressiveJPEG(source io.Reader) (bool, error) {
	reader := bufio.NewReader(source)
	var signature [2]byte
	if _, err := io.ReadFull(reader, signature[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	if signature[0] != 0xff || signature[1] != 0xd8 {
		return false, nil
	}
	for offset := int64(2); ; {
		prefix, err := reader.ReadByte()
		if errors.Is(err, io.EOF) {
			return false, fmt.Errorf("truncated JPEG data")
		}
		if err != nil {
			return false, err
		}
		if prefix != 0xff {
			return false, fmt.Errorf("invalid JPEG marker at offset %d", offset)
		}
		offset++
		marker := byte(0xff)
		for marker == 0xff {
			marker, err = reader.ReadByte()
			if errors.Is(err, io.EOF) {
				return false, fmt.Errorf("truncated JPEG marker")
			}
			if err != nil {
				return false, err
			}
			offset++
		}
		if marker == 0xc2 {
			return true, nil
		}
		if marker == 0xda || marker == 0xd9 {
			return false, nil
		}
		if marker == 0x01 || marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) {
			continue
		}
		var lengthBytes [2]byte
		if _, err := io.ReadFull(reader, lengthBytes[:]); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return false, err
			}
			return false, fmt.Errorf("truncated JPEG segment length")
		}
		segmentLength := int(lengthBytes[0])<<8 | int(lengthBytes[1])
		if segmentLength < 2 {
			return false, fmt.Errorf("invalid JPEG segment length")
		}
		if _, err := io.CopyN(io.Discard, reader, int64(segmentLength-2)); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, fmt.Errorf("invalid JPEG segment length")
			}
			return false, err
		}
		offset += int64(segmentLength)
	}
}

// normalizeSeedanceProgressiveJPEG 将上游实际无法处理的 progressive JPEG 规范化为 baseline JPEG。
func normalizeSeedanceProgressiveJPEG(ctx context.Context, path string, maxSize int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	progressive, err := isProgressiveJPEG(file)
	if err != nil {
		return 0, err
	}
	if !progressive {
		return info.Size(), nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	config, err := jpeg.DecodeConfig(file)
	if err != nil {
		return 0, fmt.Errorf("failed to read progressive JPEG dimensions: %w", err)
	}
	if config.Width > seedanceAssetImageMaxDimension || config.Height > seedanceAssetImageMaxDimension {
		return 0, fmt.Errorf("JPEG dimensions exceed the %d px limit", seedanceAssetImageMaxDimension)
	}
	release, err := acquireSeedanceImageDecode(ctx, config)
	if err != nil {
		return 0, err
	}
	defer release()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	decoded, err := jpeg.Decode(file)
	if err != nil {
		return 0, fmt.Errorf("failed to decode progressive JPEG: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	normalizedFile, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, err
	}
	encodeErr := jpeg.Encode(normalizedFile, decoded, &jpeg.Options{Quality: seedanceAssetJPEGQuality})
	info, statErr := normalizedFile.Stat()
	closeErr := normalizedFile.Close()
	if encodeErr != nil {
		return 0, fmt.Errorf("failed to encode baseline JPEG: %w", encodeErr)
	}
	if statErr != nil {
		return 0, statErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if info.Size() > maxSize {
		return 0, fmt.Errorf("normalized Image asset exceeds the %d MB upload limit", maxSize>>20)
	}
	return info.Size(), nil
}

// StoreSeedanceAssetUpload 校验并规范化用户文件，再持久写入 OSS。
// 本地文件只承担有界缓冲和图片转码，OSS 对象由用户手动删除素材时回收。
func StoreSeedanceAssetUpload(ctx context.Context, reader io.Reader, filename, contentType, declaredType string, size int64, userID int) (*SeedanceAssetUpload, error) {
	return storeSeedanceAssetUpload(ctx, reader, filename, contentType, declaredType, size, userID, 0)
}

// StoreSeedanceAssetURL 固化外部素材，避免账号同步依赖临时地址。
func StoreSeedanceAssetURL(ctx context.Context, sourceURL, assetType string, userID int) (*SeedanceAssetUpload, error) {
	if err := ValidateSeedanceAssetSourceURL(sourceURL); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, seedanceAssetURLStoreTimeout)
	defer cancel()
	downloadCtx, cancelDownload := context.WithTimeout(ctx, seedanceAssetURLDownloadTimeout)
	defer cancelDownload()
	request, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	client := *GetSSRFProtectedHTTPClient()
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("unable to fetch asset source")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asset source returned status %d", response.StatusCode)
	}
	size := max(response.ContentLength, 0)
	if size > SeedanceAssetMaxUploadSize(assetType) {
		return nil, fmt.Errorf("%s asset exceeds the %s upload limit", assetType, SeedanceAssetUploadMaxSizeLabel(assetType))
	}
	if err := model.CheckSeedanceAssetCapacity(ctx, userID, size); err != nil {
		return nil, err
	}
	return StoreSeedanceAssetUpload(ctx, response.Body, filepath.Base(parsed.Path), response.Header.Get("Content-Type"), assetType, size, userID)
}

// StoreSeedanceCompatImageUpload 使用兼容接口的独立图片上限。
func StoreSeedanceCompatImageUpload(ctx context.Context, reader io.Reader, filename, contentType string, size int64, userID int) (*SeedanceAssetUpload, error) {
	return storeSeedanceAssetUpload(ctx, reader, filename, contentType, "Image", size, userID, common.GetManagementRateLimits().AssetLimits.CompatImageMiB<<20)
}

func storeSeedanceAssetUpload(ctx context.Context, reader io.Reader, filename, contentType, declaredType string, size int64, userID int, imageLimit int64) (*SeedanceAssetUpload, error) {
	assetType, err := inferSeedanceAssetType(filename, contentType, declaredType)
	if err != nil {
		return nil, err
	}
	maxSize := SeedanceAssetMaxUploadSize(assetType)
	if assetType == "Image" && imageLimit > 0 {
		maxSize = imageLimit
	}
	if size < 0 || size > maxSize {
		return nil, fmt.Errorf("%s asset exceeds the %d MB upload limit", assetType, maxSize>>20)
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
	// 视频和音频不需要转码；multipart 文件可定位时直接流式上传，避免再次复制到临时文件。
	if assetType != "Image" {
		if seekable, ok := reader.(io.ReadSeeker); ok {
			return storeSeedanceAssetUploadStream(ctx, seekable, filename, assetType, userID, storage, extension)
		}
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
		return nil, fmt.Errorf("%s asset exceeds the %d MB upload limit", assetType, maxSize>>20)
	}
	if assetType == "Image" {
		written, err = normalizeSeedanceProgressiveJPEG(ctx, path, maxSize)
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
			if queueErr := queueSeedanceAssetObjectCleanup(objectKey, storage.location); queueErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to queue OSS cleanup: %w", queueErr))
			}
			return nil, errors.Join(err, fmt.Errorf("failed to clean up interrupted OSS upload: %w", cleanupErr))
		}
		return nil, err
	}
	upstreamURL, err := storage.PrivateAssetUpstreamURL(ctx, objectKey)
	if err != nil {
		cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
		defer cancel()
		if cleanupErr := storage.DeletePrivateAsset(cleanupCtx, objectKey); cleanupErr != nil {
			if queueErr := queueSeedanceAssetObjectCleanup(objectKey, storage.location); queueErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to queue OSS cleanup: %w", queueErr))
			}
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

	contentType, err := detectSeedanceAssetContentType(file, path)
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, contentType, nil
}

// storeSeedanceAssetUploadStream 复用可定位的上传流，只用真实文件大小构造 OSS Content-Length。
func storeSeedanceAssetUploadStream(ctx context.Context, reader io.ReadSeeker, filename, assetType string, userID int, storage *PrivateAssetOSSStorage, extension string) (*SeedanceAssetUpload, error) {
	actualSize, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to determine seedance asset size: %w", err)
	}
	if actualSize <= 0 {
		return nil, fmt.Errorf("seedance asset file is empty")
	}
	if actualSize > SeedanceAssetMaxUploadSize(assetType) {
		return nil, fmt.Errorf("%s asset exceeds the %s upload limit", assetType, SeedanceAssetUploadMaxSizeLabel(assetType))
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to rewind seedance asset upload: %w", err)
	}
	contentType, err := detectSeedanceAssetContentType(reader, filename)
	if err != nil {
		return nil, err
	}
	objectKey, err := storage.NewPrivateAssetOSSObjectKey(userID, extension)
	if err != nil {
		return nil, err
	}
	if err := storage.UploadPrivateAsset(ctx, objectKey, contentType, actualSize, io.LimitReader(reader, actualSize)); err != nil {
		cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
		defer cancel()
		if cleanupErr := storage.DeletePrivateAsset(cleanupCtx, objectKey); cleanupErr != nil {
			if queueErr := queueSeedanceAssetObjectCleanup(objectKey, storage.location); queueErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to queue OSS cleanup: %w", queueErr))
			}
			return nil, errors.Join(err, fmt.Errorf("failed to clean up interrupted OSS upload: %w", cleanupErr))
		}
		return nil, err
	}
	upstreamURL, err := storage.PrivateAssetUpstreamURL(ctx, objectKey)
	if err != nil {
		cleanupCtx, cancel := seedanceAssetCleanupContext(ctx)
		defer cancel()
		if cleanupErr := storage.DeletePrivateAsset(cleanupCtx, objectKey); cleanupErr != nil {
			if queueErr := queueSeedanceAssetObjectCleanup(objectKey, storage.location); queueErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to queue OSS cleanup: %w", queueErr))
			}
			return nil, errors.Join(err, fmt.Errorf("failed to clean up OSS object after signing failure: %w", cleanupErr))
		}
		return nil, err
	}
	return &SeedanceAssetUpload{
		ObjectKey: objectKey,
		URL:       upstreamURL,
		AssetType: assetType,
		Size:      actualSize,
		Storage:   storage.location,
		storage:   storage,
	}, nil
}

// detectSeedanceAssetContentType 从流头部探测 MIME 并在容器格式无法识别时回退到扩展名。
func detectSeedanceAssetContentType(reader io.ReadSeeker, filename string) (string, error) {
	header := make([]byte, 512)
	read, readErr := reader.Read(header)
	if _, seekErr := reader.Seek(0, io.SeekStart); seekErr != nil {
		return "", seekErr
	}
	if readErr != nil && readErr != io.EOF {
		return "", readErr
	}
	contentType := http.DetectContentType(header[:read])
	if contentType == "application/octet-stream" {
		extension := strings.ToLower(filepath.Ext(filename))
		if extension == ".heic" || extension == ".heif" {
			contentType = "image/heic"
		} else if extensionType := mime.TypeByExtension(extension); extensionType != "" {
			contentType = extensionType
		}
	}
	return contentType, nil
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
	var cleanupErr error
	if strings.TrimSpace(assetID) != "" {
		if client == nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("seedance asset client is nil"))
		} else if err := client.DeleteSeedanceAsset(cleanupCtx, assetID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("upstream asset cleanup failed: %w", err))
			if queueErr := queueSeedanceAssetUpstreamCleanup(model.SeedanceAssetCleanupKindUpstreamAsset, client, assetID); queueErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to queue upstream cleanup: %w", queueErr))
			}
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
			storage := model.SeedanceAssetStorage{}
			if len(upload) > 0 && upload[0] != nil {
				storage = upload[0].Storage
			}
			if queueErr := queueSeedanceAssetObjectCleanup(objectKey, storage); queueErr != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to queue OSS cleanup: %w", queueErr))
			}
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
	if err := client.DeleteSeedanceAssetGroup(cleanupCtx, groupID); err != nil {
		if queueErr := queueSeedanceAssetUpstreamCleanup(model.SeedanceAssetCleanupKindUpstreamGroup, client, groupID); queueErr != nil {
			return errors.Join(err, fmt.Errorf("failed to queue upstream group cleanup: %w", queueErr))
		}
		return err
	}
	return nil
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

// SeedanceAssetOSSUpstreamURL 为跨账号素材导入生成新的短期 OSS 读取地址。
func SeedanceAssetOSSUpstreamURL(ctx context.Context, objectKey string, location ...model.SeedanceAssetStorage) (string, error) {
	storage, err := privateAssetOSSStorageFactory(location...)
	if err != nil {
		return "", err
	}
	return storage.PrivateAssetUpstreamURL(ctx, objectKey)
}

// NewSeedanceAssetClient 使用渠道账号调用素材 Action，不依赖视频生成的 Go 适配器。
func NewSeedanceAssetClient(channel *model.Channel, binding ...string) (*SeedanceAssetClient, error) {
	if channel == nil || channel.Status != common.ChannelStatusEnabled {
		return nil, fmt.Errorf("Seedance channel is unavailable")
	}
	if !isSeedanceAssetChannel(channel) {
		return nil, fmt.Errorf("channel does not support Seedance assets")
	}
	settings := channel.GetSetting()
	if err := settings.ValidateSeedanceAssetURL(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(settings.SeedanceAssetURL) == "" {
		return nil, errors.New("configure the channel's complete Seedance asset service URL before using private assets")
	}
	var key string
	if len(binding) == 0 {
		selected, _, err := channel.GetNextEnabledKey()
		if err != nil {
			return nil, err
		}
		key = selected
	} else if strings.TrimSpace(binding[0]) == "" {
		// 旧映射没有账号摘要时回退到当前可用账号，避免删除任务永久卡在无法绑定账号。
		if !channel.ChannelInfo.IsMultiKey {
			key = channel.Key
		} else {
			selected, _, err := channel.GetNextEnabledKey()
			if err != nil {
				return nil, err
			}
			key = selected
		}
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
	baseURL := strings.TrimSpace(settings.SeedanceAssetURL)
	keyFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	account := SeedanceAssetAccountFingerprint(channel, keyFingerprint)
	return &SeedanceAssetClient{
		ChannelID:           channel.Id,
		baseURL:             baseURL,
		apiKey:              key,
		proxy:               channel.GetSetting().Proxy,
		KeyFingerprint:      keyFingerprint,
		AccountFingerprint:  account,
		syncAccount:         account,
		accountFingerprints: model.SeedanceAssetAccountFingerprints(channel, keyFingerprint),
		sharedAccount:       settings.SeedanceAssetShareGroup != "",
		jsonAction:          settings.TaskPluginKey == "anyllm",
		originREST:          settings.TaskPluginKey == "originocean",
	}, nil
}

// NewSeedanceAssetClientForSelectedKey 保持素材管理请求与本次模型请求使用同一把渠道密钥。
func NewSeedanceAssetClientForSelectedKey(channel *model.Channel, key string) (*SeedanceAssetClient, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("selected Seedance channel key is unavailable")
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	return NewSeedanceAssetClient(channel, fingerprint)
}

// ResolveSeedanceAssetClient 使用仍启用且指向同一上游账号的渠道恢复旧素材绑定。
func ResolveSeedanceAssetClient(ctx context.Context, channelID int, keyFingerprint, accountFingerprint string) (*SeedanceAssetClient, error) {
	return resolveSeedanceAssetClient(ctx, channelID, keyFingerprint, accountFingerprint, true)
}

func resolveSeedanceAssetClient(ctx context.Context, channelID int, keyFingerprint, accountFingerprint string, allowSharedKey bool) (*SeedanceAssetClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if model.DB == nil {
		return nil, errors.New("database is not initialized")
	}

	expectedAccountFingerprint := strings.TrimSpace(accountFingerprint)
	var originalChannel model.Channel
	originalChannelErr := model.DB.WithContext(ctx).First(&originalChannel, "id = ?", channelID).Error
	if originalChannelErr != nil && !errors.Is(originalChannelErr, gorm.ErrRecordNotFound) {
		return nil, originalChannelErr
	}
	var unavailableErr error
	if originalChannelErr == nil {
		if expectedAccountFingerprint == "" {
			expectedAccountFingerprint = SeedanceAssetAccountFingerprint(&originalChannel, keyFingerprint)
		}
		client, err := NewSeedanceAssetClient(&originalChannel, keyFingerprint)
		if err == nil &&
			(expectedAccountFingerprint == "" || client.matchesAccountFingerprint(expectedAccountFingerprint) ||
				model.SeedanceAssetModelScopeMatches(&originalChannel, keyFingerprint, expectedAccountFingerprint)) {
			return client, nil
		}
		if seedanceAssetChannelHasBinding(&originalChannel, keyFingerprint, expectedAccountFingerprint) ||
			(originalChannel.GetSetting().SeedanceAssetURL == "" && seedanceAssetChannelHasBinding(&originalChannel, keyFingerprint, "")) {
			unavailableErr = err
		}
	}
	if expectedAccountFingerprint == "" {
		if unavailableErr != nil {
			return nil, unavailableErr
		}
		return nil, errSeedanceAssetAccountMissing
	}

	const batchSize = 100
	lastID := 0
	for {
		var channels []*model.Channel
		if err := model.DB.WithContext(ctx).
			Where("id > ? AND type IN ?", lastID,
				[]int{constant.ChannelTypeDoubaoVideo, constant.ChannelTypeTaskPlugin}).
			Order("id asc").Limit(batchSize).Find(&channels).Error; err != nil {
			return nil, err
		}
		for _, channel := range channels {
			lastID = channel.Id
			if allowSharedKey && model.SeedanceAssetSharedAccountMatches(channel, expectedAccountFingerprint) {
				client, err := NewSeedanceAssetClient(channel)
				if err == nil && client.matchesAccountFingerprint(expectedAccountFingerprint) {
					return client, nil
				}
				if err != nil {
					unavailableErr = err
				}
				continue
			}
			if !seedanceAssetChannelHasBinding(channel, keyFingerprint, expectedAccountFingerprint) {
				continue
			}
			client, err := NewSeedanceAssetClient(channel, keyFingerprint)
			if err == nil && client.matchesAccountFingerprint(expectedAccountFingerprint) {
				return client, nil
			}
			if err != nil {
				unavailableErr = err
			}
		}
		if len(channels) < batchSize {
			break
		}
	}
	if unavailableErr != nil {
		return nil, unavailableErr
	}
	return nil, errSeedanceAssetAccountMissing
}

// 停用账号仍保留绑定，不能与已经移除的渠道或密钥混为一谈。
func seedanceAssetChannelHasBinding(channel *model.Channel, keyFingerprint, accountFingerprint string) bool {
	if !isSeedanceAssetChannel(channel) {
		return false
	}
	for _, key := range channel.GetKeys() {
		fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		if (keyFingerprint == "" || fingerprint == keyFingerprint) &&
			(accountFingerprint == "" || model.SeedanceAssetAccountFingerprintMatches(channel, fingerprint, accountFingerprint) ||
				model.SeedanceAssetModelScopeMatches(channel, fingerprint, accountFingerprint)) {
			return true
		}
	}
	return false
}

// SeedanceAssetAccountFingerprint 生成不包含密钥明文、可跨同账号渠道复用的命名空间。
func SeedanceAssetAccountFingerprint(channel *model.Channel, keyFingerprint string) string {
	return model.SeedanceAssetAccountFingerprint(channel, keyFingerprint)
}

func (client *SeedanceAssetClient) matchesAccountFingerprint(fingerprint string) bool {
	return fingerprint != "" && (client.AccountFingerprint == fingerprint || slices.Contains(client.accountFingerprints, fingerprint))
}

func seedanceAssetAccountFingerprint(baseURL, assetPath, keyFingerprint string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(baseURL+"\x00"+assetPath+"\x00"+keyFingerprint)))
}

func (client *SeedanceAssetClient) call(ctx context.Context, action string, request any, response any) (callErr error) {
	defer func() {
		if callErr == nil {
			return
		}
		if client.jsonAction {
			common.SysError("private asset action " + action + " failed: " + strings.ReplaceAll(callErr.Error(), client.apiKey, "[redacted]"))
		}
		if errors.Is(callErr, context.Canceled) {
			return
		}
		if isSeedanceAssetServiceTemporary(callErr) {
			callErr = &seedanceAssetTemporaryError{cause: callErr}
			return
		}
		if !client.jsonAction {
			return
		}
		if errors.Is(callErr, errSeedanceAssetResponse) {
			callErr = fmt.Errorf("%w: Asset service returned an invalid response for %s", errSeedanceAssetResponse, action)
			return
		}
		var upstream *seedanceAssetUpstreamError
		if errors.As(callErr, &upstream) {
			clean := *upstream
			clean.message = "Asset service rejected " + action
			if errors.Is(callErr, ErrSeedanceAssetNotFound) {
				callErr = errors.Join(ErrSeedanceAssetNotFound, &clean)
			} else {
				callErr = &clean
			}
			return
		}
		callErr = errors.New("Asset service request failed for " + action)
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	requestTimeout := seedanceAssetRequestTimeout
	if client.jsonAction && action == "CreateAsset" {
		// JSON Action 素材协议的导入同步等待处理，客户端期限按协议保留十分钟。
		requestTimeout = seedanceAssetJSONImportTimeout
	}
	// 请求级截止时间覆盖直连、代理、响应头及响应体读取，不依赖通用 Relay 超时。
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	requestURL := client.baseURL
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return err
	}
	body, err := common.Marshal(request)
	if err != nil {
		return err
	}
	if client.jsonAction {
		var fields map[string]json.RawMessage
		if err := common.Unmarshal(body, &fields); err != nil {
			return err
		}
		if fields == nil {
			return errors.New("asset Action request must be a JSON object")
		}
		fields["Action"], err = common.Marshal(action)
		if err != nil {
			return err
		}
		body, err = common.Marshal(fields)
		if err != nil {
			return err
		}
	} else {
		query := parsedURL.Query()
		query.Set("Action", action)
		query.Set("Version", "2024-01-01")
		parsedURL.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsedURL.String(), bytes.NewReader(body))
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
	requestClient.Timeout = requestTimeout
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
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, seedanceAssetResponseBodyLimit+1))
	if readErr != nil {
		return readErr
	}
	if len(raw) > seedanceAssetResponseBodyLimit {
		return fmt.Errorf("seedance asset API response body exceeds %d bytes", seedanceAssetResponseBodyLimit)
	}
	var envelope seedanceAssetResponseEnvelope
	decodeErr := common.Unmarshal(raw, &envelope)
	if decodeErr == nil {
		if err := envelope.businessError(); err != nil {
			var upstream *seedanceAssetUpstreamError
			if !errors.As(err, &upstream) {
				return errors.New(strings.ReplaceAll(err.Error(), client.apiKey, "[redacted]"))
			}
			clean := *upstream
			clean.action = action
			clean.httpStatus = resp.StatusCode
			clean.message = strings.ReplaceAll(clean.message, client.apiKey, "[redacted]")
			// 兼容旧代理把稳定错误码嵌入消息、但留空 code 的返回。
			if action == "GetVisualValidateResult" && clean.code == "" {
				for word := range strings.FieldsSeq(clean.message) {
					candidate := strings.Trim(word, "()[]{}:;,\"")
					code, _, _ := strings.Cut(candidate, ".")
					if strings.EqualFold(code, "NotFound") {
						clean.code = candidate
						break
					}
				}
			}
			if errors.Is(err, ErrSeedanceAssetNotFound) {
				return errors.Join(ErrSeedanceAssetNotFound, &clean)
			}
			return &clean
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return seedanceAssetHTTPError(resp.StatusCode, raw, client.apiKey)
	}
	if decodeErr != nil {
		return errors.Join(errSeedanceAssetResponse, seedanceAssetInvalidJSONError(resp.StatusCode, resp.Header.Get("Content-Type"), raw, client.apiKey, decodeErr))
	}
	if err := common.Unmarshal(raw, response); err != nil {
		return fmt.Errorf("%w: %w", errSeedanceAssetResponse, err)
	}
	return nil
}

type seedanceAssetResponseEnvelope struct {
	Code             string              `json:"code"`
	Message          string              `json:"message"`
	Success          *bool               `json:"success"`
	Error            *seedanceAssetError `json:"error"`
	ResponseMetadata struct {
		Error *seedanceAssetError `json:"Error"`
	} `json:"ResponseMetadata"`
}

type seedanceAssetResponse struct {
	seedanceAssetResponseEnvelope
	Result struct {
		ID         string          `json:"Id"`
		AssetID    string          `json:"AssetId"`
		Status     string          `json:"Status"`
		URL        string          `json:"URL"`
		AssetType  json.RawMessage `json:"AssetType"`
		BytedToken string          `json:"BytedToken"`
		H5Link     string          `json:"H5Link"`
		GroupID    string          `json:"GroupId"`
		GroupType  string          `json:"GroupType"`
	} `json:"Result"`
	Data *seedanceAssetResponseData `json:"data"`
}

// seedanceAssetError 统一接收官方及兼容代理常见的嵌套错误结构。
type seedanceAssetError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

type seedanceAssetContent struct {
	URL      string `json:"url"`
	ImageURL string `json:"image_url"`
	VideoURL string `json:"video_url"`
	AudioURL string `json:"audio_url"`
}

// seedanceAssetResponseData 兼容素材接口和兼容代理返回的多层 data 结构。
type seedanceAssetResponseData struct {
	ID        string                     `json:"id"`
	AssetID   string                     `json:"assetId"`
	AssetIDV1 string                     `json:"asset_id"`
	Status    string                     `json:"status"`
	URL       string                     `json:"url"`
	ResultURL string                     `json:"result_url"`
	AssetType json.RawMessage            `json:"asset_type"`
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

// seedanceAssetBusinessError 提取不同 HTTP 状态下的业务失败，保留上游可操作的错误信息。
func (response seedanceAssetResponseEnvelope) businessError() error {
	if response.Error != nil {
		return newSeedanceAssetBusinessError(response.Error.Code, response.Error.Message)
	}
	if response.ResponseMetadata.Error != nil {
		return newSeedanceAssetBusinessError(response.ResponseMetadata.Error.Code, response.ResponseMetadata.Error.Message)
	}
	switch strings.ToLower(strings.TrimSpace(response.Code)) {
	case "", "0", "ok", "success":
		if response.Success == nil || *response.Success {
			return nil
		}
	}
	return newSeedanceAssetBusinessError(response.Code, response.Message)
}

type seedanceAssetUpstreamError struct {
	code       string
	message    string
	action     string
	httpStatus int
}

func (err *seedanceAssetUpstreamError) Error() string {
	return "seedance asset API request failed: " + err.message
}

type seedanceAssetTemporaryError struct{ cause error }

func (err *seedanceAssetTemporaryError) Error() string {
	return ErrSeedanceAssetServiceTemporary.Error()
}
func (err *seedanceAssetTemporaryError) Unwrap() error { return err.cause }
func (err *seedanceAssetTemporaryError) Is(target error) bool {
	return target == ErrSeedanceAssetServiceTemporary
}

func isSeedanceAssetServiceTemporary(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var mysqlError *mysqldriver.MySQLError
	if errors.As(err, &mysqlError) && (mysqlError.Number == 1205 || mysqlError.Number == 1213 || mysqlError.Number == 2006 || mysqlError.Number == 2013) {
		return true
	}
	var databaseError interface{ SQLState() string }
	if errors.As(err, &databaseError) {
		state := databaseError.SQLState()
		if state == "40001" || state == "40P01" || strings.HasPrefix(state, "08") {
			return true
		}
	}
	if errors.Is(err, ErrSeedanceAssetServiceTemporary) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var connectionError *net.OpError
	if errors.As(err, &connectionError) {
		var dnsError *net.DNSError
		if errors.As(err, &dnsError) && dnsError.IsNotFound {
			return false
		}
		return true
	}
	var upstream *seedanceAssetUpstreamError
	if !errors.As(err, &upstream) {
		return false
	}
	if upstream.httpStatus == http.StatusUnauthorized || upstream.httpStatus == http.StatusForbidden {
		return false
	}
	code, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(upstream.code)), ".")
	switch code {
	case "notfound", "assetnotfound", "assetgroupnotfound", "resourcenotfound", "not_found", "invalidparameter", "invalidargument", "invalidrequest", "invalid_request_error", "validationerror", "unauthorized", "forbidden", "accessdenied", "permissiondenied", "permission_error", "authenticationerror", "invalid_api_key", "unsupportedaction", "unsupported_action", "unsupportedoperation", "unsupported_operation", "notimplemented":
		return false
	case "internalservicetimeout", "requesttimeout", "serviceunavailable", "temporarilyunavailable", "toomanyrequests", "throttling", "throttlingexception", "serveroverloaded", "600502":
		return true
	}
	switch upstream.httpStatus {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// newSeedanceAssetBusinessError 保留业务错误码，避免调用方依赖可变的显示文案。
func newSeedanceAssetBusinessError(code, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = strings.TrimSpace(code)
	}
	if message == "" {
		message = "upstream rejected operation"
	}
	err := &seedanceAssetUpstreamError{code: strings.TrimSpace(code), message: message}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "assetnotfound", "assetgroupnotfound", "resourcenotfound", "resourcenotfound.asset", "resourcenotfound.assetgroup", "not_found":
		return errors.Join(ErrSeedanceAssetNotFound, err)
	}
	return err
}

// seedanceAssetHTTPError 为未识别的非 2xx 响应保留有限正文，并过滤渠道凭证。
func seedanceAssetHTTPError(status int, body []byte, apiKey string) error {
	message := strings.TrimSpace(string(body))
	message = strings.ReplaceAll(message, apiKey, "[redacted]")
	if len(message) > seedanceAssetErrorPreviewLimit {
		message = message[:seedanceAssetErrorPreviewLimit] + "..."
	}
	if message == "" {
		return &seedanceAssetUpstreamError{httpStatus: status, message: fmt.Sprintf("seedance asset API returned status %d", status)}
	}
	return &seedanceAssetUpstreamError{httpStatus: status, message: fmt.Sprintf("seedance asset API returned status %d: %s", status, message)}
}

func seedanceAssetInvalidJSONError(status int, contentType string, body []byte, apiKey string, decodeErr error) error {
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		return fmt.Errorf("seedance asset API returned invalid JSON (status %d, content-type %s): the configured asset service URL returned an HTML page; check the complete asset endpoint", status, contentType)
	}
	message := strings.TrimSpace(string(body))
	message = strings.ReplaceAll(message, apiKey, "[redacted]")
	if len(message) > seedanceAssetErrorPreviewLimit {
		message = message[:seedanceAssetErrorPreviewLimit] + "..."
	}
	if message == "" {
		message = "<empty response>"
	}
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		contentType = "unknown content type"
	}
	return fmt.Errorf(
		"seedance asset API returned invalid JSON (status %d, content-type %s): %s: %w",
		status,
		contentType,
		message,
		decodeErr,
	)
}

// ValidateSeedanceAssetSourceURL 在用户输入边界验证地址；OSS 签名地址由受信任存储配置生成。
func ValidateSeedanceAssetSourceURL(sourceURL string) error {
	parsed, err := url.Parse(sourceURL)
	if err != nil || len(sourceURL) > 2048 || parsed == nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("invalid asset source URL")
	}
	return ValidateSSRFProtectedFetchURL(sourceURL)
}

// DetectSeedanceAssetURLType 为省略类型的兼容请求确定上游必填的媒体类型。
func DetectSeedanceAssetURLType(ctx context.Context, sourceURL string) (string, error) {
	if err := ValidateSeedanceAssetSourceURL(sourceURL); err != nil {
		return "", err
	}
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(filepath.Ext(parsed.Path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".tif", ".tiff", ".gif", ".heic", ".heif", ".bmp":
		return "Image", nil
	case ".mp4", ".mov":
		return "Video", nil
	case ".wav", ".mp3":
		return "Audio", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Range", "bytes=0-511")
	response, err := GetSSRFProtectedHTTPClient().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return "", fmt.Errorf("asset source returned status %d", response.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 512))
	if err != nil {
		return "", err
	}
	for _, contentType := range []string{http.DetectContentType(content), response.Header.Get("Content-Type")} {
		mediaType, _, _ := mime.ParseMediaType(contentType)
		switch {
		case strings.HasPrefix(mediaType, "image/"):
			return "Image", nil
		case strings.HasPrefix(mediaType, "video/"):
			return "Video", nil
		case strings.HasPrefix(mediaType, "audio/"):
			return "Audio", nil
		}
	}
	return "", errors.New("asset media type could not be determined")
}

// CreateSeedanceAsset 将公网资源提交给上游托管。
func (client *SeedanceAssetClient) CreateSeedanceAsset(ctx context.Context, groupID, sourceURL, assetType, name string) (string, error) {
	if client.originREST {
		return client.createOriginAsset(ctx, sourceURL, assetType, name)
	}
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

func (client *SeedanceAssetClient) getSeedanceHumanGroupStatus(ctx context.Context, groupID string) (string, error) {
	var response struct {
		Result struct {
			ID        string          `json:"Id"`
			GroupType string          `json:"GroupType"`
			Status    json.RawMessage `json:"Status"`
		} `json:"Result"`
	}
	if err := client.call(ctx, "GetAssetGroup", map[string]string{"Id": groupID}, &response); err != nil {
		return "", err
	}
	if response.Result.ID != groupID || !strings.EqualFold(response.Result.GroupType, "LivenessFace") {
		return "", fmt.Errorf("%w: asset service returned an invalid verified group", errSeedanceAssetResponse)
	}
	// 原生组由认证结果确认，组详情不含审核状态；包装组仍必须显式返回状态。
	if len(response.Result.Status) == 0 && strings.HasPrefix(groupID, seedanceNativeGroupIDPrefix) {
		return "Active", nil
	}
	var status string
	if len(response.Result.Status) == 0 || common.Unmarshal(response.Result.Status, &status) != nil {
		return "", fmt.Errorf("%w: asset service returned no valid group status", errSeedanceAssetResponse)
	}
	status = strings.TrimSpace(status)
	if status == "" || len(status) > 32 {
		return "", fmt.Errorf("%w: asset service returned no valid group status", errSeedanceAssetResponse)
	}
	return status, nil
}

func seedanceHumanGroupStatusError(status string) error {
	switch strings.ToLower(status) {
	case "active":
		return nil
	case "failed", "rejected", "disabled", "deleted":
		return ErrSeedanceAssetValidationFailed
	default:
		return ErrSeedanceAssetGroupPending
	}
}

// EnsureSeedanceAssetGroupReady 在接收原件前核实真人组，防止旧的 Active 记录绕过上游就绪检查。
func EnsureSeedanceAssetGroupReady(ctx context.Context, client *SeedanceAssetClient, group *model.SeedanceAssetGroup) error {
	if !client.jsonAction || !strings.EqualFold(group.GroupType, "LivenessFace") {
		return nil
	}
	status, err := client.getSeedanceHumanGroupStatus(ctx, group.GroupID)
	if err != nil {
		return err
	}
	if strings.EqualFold(status, "Active") {
		status = "Active"
	}
	if status != group.Status {
		result := model.DB.WithContext(ctx).Model(&model.SeedanceAssetGroup{}).
			Where("id = ? AND user_id = ? AND status <> ?", group.ID, group.UserID, "Deleting").Update("status", status)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			// MySQL 的同值更新可返回零行；并发检查已写入相同状态时仍视为成功。
			var current model.SeedanceAssetGroup
			readErr := model.DB.WithContext(ctx).Select("status").Where("id = ? AND user_id = ?", group.ID, group.UserID).First(&current).Error
			if errors.Is(readErr, gorm.ErrRecordNotFound) || current.Status == "Deleting" {
				return model.ErrSeedanceAssetUnavailable
			}
			if readErr != nil {
				return readErr
			}
			group.Status = current.Status
			return seedanceHumanGroupStatusError(current.Status)
		}
		group.Status = status
	}
	return seedanceHumanGroupStatusError(status)
}

// CreateSeedanceAssetGroup 创建上游素材组并返回组 ID；GroupType 由上游决定是否支持。
func (client *SeedanceAssetClient) CreateSeedanceAssetGroup(ctx context.Context, name, groupType string, response any) error {
	if client.originREST {
		return createOriginLocalAssetGroup(groupType, response)
	}
	request := map[string]string{"Name": name}
	// GroupType 不做本地枚举校验，避免把上游新增或渠道自定义类型拦截在网关内。
	if groupType != "" {
		request["GroupType"] = groupType
	}
	return client.call(ctx, "CreateAssetGroup", request, response)
}

type SeedanceAssetDetails struct {
	ID         string
	Status     string
	PreviewURL string
	AssetType  string
}

// 上游可选类型的格式异常不改变素材状态。
func seedanceAssetOptionalType(assetType json.RawMessage) string {
	var kind string
	if common.Unmarshal(assetType, &kind) != nil {
		return ""
	}
	kind = strings.TrimSpace(kind)
	if kind != "Image" && kind != "Video" && kind != "Audio" {
		return ""
	}
	return kind
}

// UpdateValues 只保存上游实际返回的素材详情。
func (details SeedanceAssetDetails) UpdateValues(primary bool) map[string]any {
	values := map[string]any{"status": details.Status}
	if details.PreviewURL != "" {
		values["preview_url"] = details.PreviewURL
	}
	if primary && details.AssetType != "" {
		values["asset_type"] = details.AssetType
	}
	return values
}

// GetSeedanceAssetDetails 查询素材状态、预览及类型。
func (client *SeedanceAssetClient) GetSeedanceAssetDetails(ctx context.Context, assetID string) (SeedanceAssetDetails, error) {
	if client.originREST {
		return client.getOriginAsset(ctx, assetID)
	}
	var response seedanceAssetResponse
	err := client.call(ctx, "GetAsset", map[string]string{"Id": assetID}, &response)
	if err != nil {
		return SeedanceAssetDetails{}, err
	}
	if err := response.businessError(); err != nil {
		return SeedanceAssetDetails{}, err
	}
	// 查询错误或空响应不是审核结论，禁止用空状态覆盖已保存的状态。
	status := response.status()
	if status == "" {
		return SeedanceAssetDetails{}, fmt.Errorf("seedance asset API returned no asset status")
	}
	details := SeedanceAssetDetails{ID: strings.TrimSpace(response.Result.ID), Status: status, PreviewURL: response.previewURL(), AssetType: seedanceAssetOptionalType(response.Result.AssetType)}
	if details.ID == "" {
		details.ID = strings.TrimSpace(response.Result.AssetID)
	}
	statusFound := strings.TrimSpace(response.Result.Status) != ""
	for data := response.Data; data != nil; data = data.Data {
		if !statusFound && strings.TrimSpace(data.Status) != "" {
			statusFound = true
			if details.ID == "" {
				for _, id := range []string{data.ID, data.AssetID, data.AssetIDV1} {
					if details.ID = strings.TrimSpace(id); details.ID != "" {
						break
					}
				}
			}
		}
		if details.AssetType == "" {
			details.AssetType = seedanceAssetOptionalType(data.AssetType)
		}
	}
	return details, nil
}

// GetCheckedSeedanceAssetDetails 拒绝与请求素材 ID 不一致的审核结果。
func (client *SeedanceAssetClient) GetCheckedSeedanceAssetDetails(ctx context.Context, assetID string) (SeedanceAssetDetails, error) {
	details, err := client.GetSeedanceAssetDetails(ctx, assetID)
	if err != nil {
		return SeedanceAssetDetails{}, err
	}
	if details.ID != "" && details.ID != assetID {
		return SeedanceAssetDetails{}, fmt.Errorf("%w: asset service returned a different asset", errSeedanceAssetResponse)
	}
	return details, nil
}

// GetSeedanceAsset 保留既有调用方的状态和预览接口。
func (client *SeedanceAssetClient) GetSeedanceAsset(ctx context.Context, assetID string) (string, string, error) {
	details, err := client.GetSeedanceAssetDetails(ctx, assetID)
	return details.Status, details.PreviewURL, err
}

// DeleteSeedanceAsset 删除上游素材。
func (client *SeedanceAssetClient) DeleteSeedanceAsset(ctx context.Context, assetID string) error {
	if client.originREST {
		return client.deleteOriginAsset(ctx, assetID)
	}
	err := client.call(ctx, "DeleteAsset", map[string]string{"Id": assetID}, &struct{}{})
	if isSeedanceAssetDeleteNotFoundError(err) {
		return nil
	}
	return err
}

// DeleteSeedanceAssetGroup 删除上游素材组。
func (client *SeedanceAssetClient) DeleteSeedanceAssetGroup(ctx context.Context, groupID string) error {
	if client.originREST {
		return nil
	}
	err := client.call(ctx, "DeleteAssetGroup", map[string]string{"Id": groupID}, &struct{}{})
	if isSeedanceAssetDeleteNotFoundError(err) {
		return nil
	}
	return err
}

// isSeedanceAssetDeleteNotFoundError 仅为删除操作识别上游的幂等“不存在”。
// 部分代理返回 NotFound.<request-id> 或只返回“resource does not exist”，不能依赖固定错误码；
// 查询接口仍保留原始错误，避免把认证会话或其他读取失败误判为已完成。
func isSeedanceAssetDeleteNotFoundError(err error) bool {
	if err == nil || errors.Is(err, ErrSeedanceAssetNotFound) {
		return err != nil
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"notfound.",
		"assetnotfound",
		"assetgroupnotfound",
		"resourcenotfound",
		"resource does not exist",
		"resource not found",
		"asset group not found",
		"asset not found",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// UpdateSeedanceAssetGroup 更新上游素材组名称。
func (client *SeedanceAssetClient) UpdateSeedanceAssetGroup(ctx context.Context, groupID, name string, response any) error {
	if client.jsonAction {
		return errors.New("upstream does not support renaming asset groups")
	}
	return client.call(ctx, "UpdateAssetGroup", map[string]string{"Id": groupID, "Name": name}, response)
}

// SupportsGroupRename 区分远端改名与仅保存站内名称，避免调用上游未提供的 Action。
func (client *SeedanceAssetClient) SupportsGroupRename() bool {
	return !client.jsonAction && !client.originREST
}

// isSeedanceAssetChannel 识别支持站内 Seedance 素材映射的渠道。
func isSeedanceAssetChannel(channel *model.Channel) bool {
	switch channel.Type {
	case constant.ChannelTypeDoubaoVideo:
		return true
	case constant.ChannelTypeTaskPlugin:
		key := channel.GetSetting().TaskPluginKey
		return key == "doubao" || key == "anyllm" || key == "originocean"
	default:
		return false
	}
}

// SupportsSeedanceAssets 判断渠道是否实现 Seedance 素材 Action 接口。
func SupportsSeedanceAssets(channel *model.Channel) bool {
	return isSeedanceAssetChannel(channel)
}

// FindSeedanceAssetChannels 按优先级降序返回可用渠道；同级按 ID 排序，并用组合游标跨页。
func FindSeedanceAssetChannels() ([]*model.Channel, error) {
	const batchSize = 100
	lastID := 0
	var lastPriority int64
	result := make([]*model.Channel, 0)
	for {
		var channels []*model.Channel
		query := model.DB.Where("status = ? AND type IN ?", common.ChannelStatusEnabled,
			[]int{constant.ChannelTypeDoubaoVideo, constant.ChannelTypeTaskPlugin})
		if lastID > 0 {
			query = query.Where("(COALESCE(priority, 0) < ? OR (COALESCE(priority, 0) = ? AND id > ?))", lastPriority, lastPriority, lastID)
		}
		err := query.Order("COALESCE(priority, 0) DESC").Order("id ASC").Limit(batchSize).Find(&channels).Error
		if err != nil {
			return nil, err
		}
		for _, channel := range channels {
			lastID = channel.Id
			lastPriority = channel.GetPriority()
			if !isSeedanceAssetChannel(channel) || channel.GetBaseURL() == "" || strings.TrimSpace(channel.GetSetting().SeedanceAssetURL) == "" {
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
				result = append(result, channel)
				break
			}
		}
		if len(channels) < batchSize {
			if len(result) == 0 {
				return nil, ErrSeedanceAssetChannelUnavailable
			}
			return result, nil
		}
	}
}

// FindSeedanceAssetChannel 优先选择最高优先级的可用渠道，找到后立即返回，不加载全部候选。
func FindSeedanceAssetChannel() (*model.Channel, error) {
	const batchSize = 100
	lastID := 0
	var lastPriority int64
	for {
		var channels []*model.Channel
		query := model.DB.Where("status = ? AND type IN ?", common.ChannelStatusEnabled,
			[]int{constant.ChannelTypeDoubaoVideo, constant.ChannelTypeTaskPlugin})
		if lastID > 0 {
			query = query.Where("(COALESCE(priority, 0) < ? OR (COALESCE(priority, 0) = ? AND id > ?))", lastPriority, lastPriority, lastID)
		}
		err := query.Order("COALESCE(priority, 0) DESC").Order("id ASC").Limit(batchSize).Find(&channels).Error
		if err != nil {
			return nil, err
		}
		for _, channel := range channels {
			lastID = channel.Id
			lastPriority = channel.GetPriority()
			if !isSeedanceAssetChannel(channel) || channel.GetBaseURL() == "" || strings.TrimSpace(channel.GetSetting().SeedanceAssetURL) == "" {
				continue
			}
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
