package model

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const tokenSignatureSecretPurpose = "token-signature:v1:"

type TokenSignatureCredential struct {
	TokenID          int    `json:"token_id" gorm:"primaryKey;autoIncrement:false"`
	AccessKey        string `json:"access_key" gorm:"size:64;not null;uniqueIndex"`
	SecretCiphertext string `json:"-" gorm:"size:256;not null"`
	CreatedAt        int64  `json:"created_at" gorm:"bigint;not null"`
}

// IssueTokenSignatureCredential 在锁定令牌后原子替换签名密钥，旧 AK 随事务失效。
func IssueTokenSignatureCredential(ctx context.Context, userID, tokenID int) (*TokenSignatureCredential, string, error) {
	if userID <= 0 || tokenID <= 0 {
		return nil, "", gorm.ErrRecordNotFound
	}
	var randomAccess [16]byte
	var randomSecret [32]byte
	if _, err := rand.Read(randomAccess[:]); err != nil {
		return nil, "", fmt.Errorf("generate access key: %w", err)
	}
	if _, err := rand.Read(randomSecret[:]); err != nil {
		return nil, "", fmt.Errorf("generate secret key: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(randomSecret[:])
	sealed, err := common.SealTaskSecret(tokenSignatureSecretPurpose + secret)
	if err != nil {
		return nil, "", fmt.Errorf("encrypt signature key: %w", err)
	}
	credential := &TokenSignatureCredential{
		TokenID: tokenID, AccessKey: "AK" + hex.EncodeToString(randomAccess[:]),
		SecretCiphertext: sealed, CreatedAt: common.GetTimestamp(),
	}
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
			return err
		}
		if err := tx.Where("token_id = ?", tokenID).Delete(&TokenSignatureCredential{}).Error; err != nil {
			return err
		}
		return tx.Create(credential).Error
	})
	if err != nil {
		return nil, "", err
	}
	return credential, secret, nil
}

func GetTokenSignatureCredential(ctx context.Context, tokenID int) (*TokenSignatureCredential, error) {
	var credential TokenSignatureCredential
	if err := DB.WithContext(ctx).Where("token_id = ?", tokenID).First(&credential).Error; err != nil {
		return nil, err
	}
	return &credential, nil
}

func GetTokenSignatureCredentialByAccessKey(ctx context.Context, accessKey string) (*TokenSignatureCredential, error) {
	var credential TokenSignatureCredential
	if err := DB.WithContext(ctx).Where("access_key = ?", accessKey).First(&credential).Error; err != nil {
		return nil, err
	}
	// MySQL 默认排序规则可能忽略大小写，凭据标识必须按字节精确匹配。
	if credential.AccessKey != accessKey {
		return nil, gorm.ErrRecordNotFound
	}
	return &credential, nil
}

func (credential *TokenSignatureCredential) OpenSecret() (string, error) {
	if credential == nil || credential.SecretCiphertext == "" {
		return "", errors.New("invalid encrypted signature credential")
	}
	plain, err := common.OpenTaskSecret(credential.SecretCiphertext)
	if err != nil || plain == credential.SecretCiphertext || !strings.HasPrefix(plain, tokenSignatureSecretPurpose) {
		return "", errors.New("invalid encrypted signature credential")
	}
	secret := strings.TrimPrefix(plain, tokenSignatureSecretPurpose)
	if secret == "" {
		return "", errors.New("invalid encrypted signature credential")
	}
	return secret, nil
}

func RevokeTokenSignatureCredential(ctx context.Context, userID, tokenID int) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
			return err
		}
		return tx.Where("token_id = ?", tokenID).Delete(&TokenSignatureCredential{}).Error
	})
}
