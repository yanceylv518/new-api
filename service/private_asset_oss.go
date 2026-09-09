package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
)

const (
	privateAssetOSSConnectTimeout = 10 * time.Second
	privateAssetOSSRequestTimeout = 5 * time.Minute
	privateAssetOSSCleanupTimeout = 30 * time.Second
	privateAssetOSSPreviewTTL     = time.Hour
	privateAssetOSSUpstreamTTL    = 24 * time.Hour
)

type privateAssetOSSClient interface {
	PutObject(context.Context, *oss.PutObjectRequest, ...func(*oss.Options)) (*oss.PutObjectResult, error)
	DeleteObject(context.Context, *oss.DeleteObjectRequest, ...func(*oss.Options)) (*oss.DeleteObjectResult, error)
	Presign(context.Context, any, ...func(*oss.PresignOptions)) (*oss.PresignResult, error)
}

// PrivateAssetOSSStorage 封装私域素材需要的上传、删除和短期授权能力。
type PrivateAssetOSSStorage struct {
	client   privateAssetOSSClient
	bucket   string
	prefix   string
	location model.SeedanceAssetStorage
}

// NewPrivateAssetOSSStorage 使用当前系统设置创建 OSS 存储客户端。
func NewPrivateAssetOSSStorage(locations ...model.SeedanceAssetStorage) (*PrivateAssetOSSStorage, error) {
	current := system_setting.GetPrivateAssetOSSSettings()
	// 对象读写固定原位置；没有位置快照的已有对象仍受配置切换检查保护。
	var location model.SeedanceAssetStorage
	if len(locations) > 0 {
		location = locations[0]
	}
	if location.Bucket != "" {
		current.Region, current.Endpoint, current.Bucket = location.Region, location.Endpoint, location.Bucket
	}
	settings, err := system_setting.NormalizeAndValidatePrivateAssetOSSSettings(current)
	if err != nil {
		return nil, fmt.Errorf("private asset OSS is not configured: %w", err)
	}
	return newPrivateAssetOSSStorage(settings), nil
}

// newPrivateAssetOSSStorage 只接收已完成校验的配置并构造无共享可变状态的客户端。
func newPrivateAssetOSSStorage(settings system_setting.PrivateAssetOSSSettings) *PrivateAssetOSSStorage {
	config := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(settings.AccessKeyID, settings.AccessKeySecret)).
		WithRegion(settings.Region).
		WithConnectTimeout(privateAssetOSSConnectTimeout).
		WithReadWriteTimeout(privateAssetOSSRequestTimeout).
		WithRetryMaxAttempts(3)
	if settings.Endpoint != "" {
		config.WithEndpoint(settings.Endpoint)
	}
	return &PrivateAssetOSSStorage{
		client:   oss.NewClient(config),
		bucket:   settings.Bucket,
		prefix:   settings.Prefix,
		location: model.SeedanceAssetStorage{Region: settings.Region, Endpoint: settings.Endpoint, Bucket: settings.Bucket},
	}
}

// NewPrivateAssetOSSObjectKey 为用户素材生成不可预测且可按日期整理的对象键。
func (storage *PrivateAssetOSSStorage) NewPrivateAssetOSSObjectKey(userID int, extension string) (string, error) {
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("failed to generate OSS object key: %w", err)
	}
	token := hex.EncodeToString(randomBytes[:])
	return fmt.Sprintf("%susers/%d/%s/%s%s", storage.prefix, userID, time.Now().UTC().Format("2006/01/02"), token, extension), nil
}

// UploadPrivateAsset 将素材写入私有 Bucket，并禁止覆盖碰撞的对象键。
func (storage *PrivateAssetOSSStorage) UploadPrivateAsset(ctx context.Context, objectKey, contentType string, size int64, reader io.Reader) error {
	if err := validatePrivateAssetOSSObjectKey(objectKey); err != nil {
		return err
	}
	_, err := storage.client.PutObject(ctx, &oss.PutObjectRequest{
		Bucket:             oss.Ptr(storage.bucket),
		Key:                oss.Ptr(objectKey),
		Body:               reader,
		ContentLength:      oss.Ptr(size),
		ContentType:        oss.Ptr(contentType),
		ContentDisposition: oss.Ptr("inline"),
		CacheControl:       oss.Ptr("private, max-age=300"),
		ForbidOverwrite:    oss.Ptr("true"),
	})
	if err != nil {
		return fmt.Errorf("failed to upload private asset to OSS: %w", err)
	}
	return nil
}

// DeletePrivateAsset 删除用户已明确移除的 OSS 对象。
func (storage *PrivateAssetOSSStorage) DeletePrivateAsset(ctx context.Context, objectKey string) error {
	if strings.TrimSpace(objectKey) == "" {
		return nil
	}
	if err := validatePrivateAssetOSSObjectKey(objectKey); err != nil {
		return err
	}
	_, err := storage.client.DeleteObject(ctx, &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(storage.bucket),
		Key:    oss.Ptr(objectKey),
	})
	if err != nil {
		return fmt.Errorf("failed to delete private asset from OSS: %w", err)
	}
	return nil
}

// PrivateAssetPreviewURL 为浏览器预览生成一小时有效的签名地址。
func (storage *PrivateAssetOSSStorage) PrivateAssetPreviewURL(ctx context.Context, objectKey string) (string, error) {
	return storage.presignGet(ctx, objectKey, privateAssetOSSPreviewTTL)
}

// PrivateAssetUpstreamURL 为上游审核生成足够覆盖异步拉取过程的签名地址。
func (storage *PrivateAssetOSSStorage) PrivateAssetUpstreamURL(ctx context.Context, objectKey string) (string, error) {
	return storage.presignGet(ctx, objectKey, privateAssetOSSUpstreamTTL)
}

func (storage *PrivateAssetOSSStorage) presignGet(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	if err := validatePrivateAssetOSSObjectKey(objectKey); err != nil {
		return "", err
	}
	result, err := storage.client.Presign(ctx, &oss.GetObjectRequest{
		Bucket: oss.Ptr(storage.bucket),
		Key:    oss.Ptr(objectKey),
	}, oss.PresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("failed to sign private asset OSS URL: %w", err)
	}
	return result.URL, nil
}

func validatePrivateAssetOSSObjectKey(objectKey string) error {
	if objectKey == "" || len(objectKey) > 1023 || strings.HasPrefix(objectKey, "/") || strings.Contains(objectKey, `\`) {
		return fmt.Errorf("invalid OSS object key")
	}
	for _, segment := range strings.Split(objectKey, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("invalid OSS object key")
		}
	}
	return nil
}
