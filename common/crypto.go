package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const taskSealedPrefix = "task-sealed:v1:"

// SealTaskSecret 使用独立用途密钥加密任务凭证；各节点必须配置相同且持久的 CRYPTO_SECRET。
func SealTaskSecret(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if CryptoSecret == "" {
		return "", errors.New("task encryption secret is unavailable")
	}
	key := sha256.Sum256([]byte("new-api/task-secret/v1:" + CryptoSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(value), []byte(taskSealedPrefix))
	return taskSealedPrefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// OpenTaskSecret 保留既有明文记录的读取，后续写入自动加密；验证失败不能回退成明文密钥。
func OpenTaskSecret(value string) (string, error) {
	if !strings.HasPrefix(value, taskSealedPrefix) {
		return value, nil
	}
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, taskSealedPrefix))
	if err != nil {
		return "", errors.New("invalid encrypted task credential")
	}
	key := sha256.Sum256([]byte("new-api/task-secret/v1:" + CryptoSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(data) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("invalid encrypted task credential")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(taskSealedPrefix))
	if err != nil {
		return "", errors.New("task credential decryption failed; verify CRYPTO_SECRET")
	}
	return string(plain), nil
}

func GenerateHMACWithKey(key []byte, data string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func GenerateHMAC(data string) string {
	h := hmac.New(sha256.New, []byte(CryptoSecret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func Password2Hash(password string) (string, error) {
	passwordBytes := []byte(password)
	hashedPassword, err := bcrypt.GenerateFromPassword(passwordBytes, bcrypt.DefaultCost)
	return string(hashedPassword), err
}

func ValidatePasswordAndHash(password string, hash string) bool {
	if strings.HasPrefix(hash, "$argon2id$") {
		return validateArgon2AccountPassword(password, hash)
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
