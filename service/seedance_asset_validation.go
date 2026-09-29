/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

const (
	seedanceAssetValidationSessionTTL       = 30 * time.Minute
	seedanceAssetValidationLease            = 30 * time.Second
	seedanceValidationCipherVersion         = "v1"
	seedanceAssetValidationProjectNameLimit = 191
	seedanceAssetValidationDescriptionLimit = 1024
	seedanceAssetValidationTagsLimit        = 512
)

var (
	ErrSeedanceAssetValidationPending     = errors.New("Seedance validation is still pending")
	ErrSeedanceAssetValidationExpired     = errors.New("Seedance validation session expired")
	ErrSeedanceAssetValidationFailed      = errors.New("Seedance validation failed")
	ErrSeedanceAssetValidationProject     = errors.New("ProjectName does not match the validation session")
	ErrSeedanceAssetValidationUnsupported = errors.New("Seedance validation is unsupported by this channel")
)

type SeedanceAssetValidationStart struct {
	Session    *model.SeedanceAssetValidationSession
	BytedToken string
	LaunchURL  string
}

type SeedanceAssetValidationGroupDetails struct {
	Name        string
	Description string
	Tags        string
}

type SeedanceAssetValidationSessionView struct {
	ID           uint   `json:"id"`
	Status       string `json:"status"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Tags         string `json:"tags,omitempty"`
	LaunchURL    string `json:"launch_url,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
	GroupID      string `json:"group_id,omitempty"`
	LocalGroupID uint   `json:"local_group_id,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

func BuildSeedanceAssetValidationSessionView(session *model.SeedanceAssetValidationSession) SeedanceAssetValidationSessionView {
	if session == nil {
		return SeedanceAssetValidationSessionView{}
	}
	return SeedanceAssetValidationSessionView{
		ID:           session.ID,
		Status:       session.Status,
		Name:         session.Name,
		Description:  session.Description,
		Tags:         session.Tags,
		LaunchURL:    session.LaunchURL,
		ExpiresAt:    session.ExpiresAt,
		GroupID:      session.UpstreamGroupID,
		LocalGroupID: session.LocalGroupID,
		LastError:    session.LastError,
		CreatedAt:    session.CreatedAt.Unix(),
		UpdatedAt:    session.UpdatedAt.Unix(),
	}
}

// ValidateSeedanceAssetValidationCallbackURL 校验官方要求的跳转地址，不对其发起网络请求。
func ValidateSeedanceAssetValidationCallbackURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return errors.New("validation callback URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("validation callback URL is invalid")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return errors.New("validation callback URL must use http or https")
	}
	return nil
}

func newSeedanceAssetValidationState() (string, string, error) {
	state, err := common.GenerateRandomCharsKey(48)
	if err != nil {
		return "", "", err
	}
	stateHash, err := seedanceAssetValidationHMAC("seedance-validation-state-v1:" + state)
	if err != nil {
		return "", "", err
	}
	return state, stateHash, nil
}

func seedanceAssetValidationSecret() (string, error) {
	secret := os.Getenv("CRYPTO_SECRET")
	if secret == "" {
		secret = os.Getenv("SESSION_SECRET")
	}
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("CRYPTO_SECRET or SESSION_SECRET is required for validation sessions")
	}
	return secret, nil
}

func seedanceAssetValidationHMAC(value string) (string, error) {
	secret, err := seedanceAssetValidationSecret()
	if err != nil {
		return "", err
	}
	return common.GenerateHMACWithKey([]byte(secret), value), nil
}

func seedanceAssetValidationTokenHash(token string) (string, error) {
	return seedanceAssetValidationHMAC("seedance-validation-token-v1:" + token)
}

func seedanceAssetValidationCipherKey() ([]byte, error) {
	secret, err := seedanceAssetValidationSecret()
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte("seedance-validation-token-v1:" + secret))
	return key[:], nil
}

func encryptSeedanceAssetValidationToken(token string) (string, error) {
	key, err := seedanceAssetValidationCipherKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(token), nil)
	return seedanceValidationCipherVersion + "." +
		base64.RawURLEncoding.EncodeToString(nonce) + "." +
		base64.RawURLEncoding.EncodeToString(sealed), nil
}

func decryptSeedanceAssetValidationToken(ciphertext string) (string, error) {
	parts := strings.Split(ciphertext, ".")
	if len(parts) != 3 || parts[0] != seedanceValidationCipherVersion {
		return "", errors.New("validation session token ciphertext is invalid")
	}
	key, err := seedanceAssetValidationCipherKey()
	if err != nil {
		return "", err
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return "", errors.New("validation session token nonce is invalid")
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return "", errors.New("validation session token ciphertext is invalid")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(nonce) != gcm.NonceSize() {
		return "", errors.New("validation session token nonce size is invalid")
	}
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", errors.New("validation session token cannot be decrypted")
	}
	return string(plaintext), nil
}

func seedanceAssetValidationBaseURL() (string, error) {
	base := strings.TrimSpace(system_setting.TaskPublicAddress)
	if base == "" {
		base = strings.TrimSpace(system_setting.ServerAddress)
	}
	if err := ValidateSeedanceAssetValidationCallbackURL(base); err != nil {
		return "", fmt.Errorf("configure ServerAddress or TaskPublicAddress, or provide callback_url: %w", err)
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("configured public address must not contain a query or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func buildSeedanceAssetValidationCallbackURL(sessionID uint, state string) (string, error) {
	base, err := seedanceAssetValidationBaseURL()
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + fmt.Sprintf("/v1/seedance/validation-sessions/%d/callback", sessionID)
	parsed.RawQuery = url.Values{"state": []string{state}}.Encode()
	return parsed.String(), nil
}

// StartSeedanceAssetValidationSession creates the upstream session and binds it to one channel account.
func StartSeedanceAssetValidationSession(ctx context.Context, userID int, channel *model.Channel, details SeedanceAssetValidationGroupDetails, projectName, callbackURL string) (*SeedanceAssetValidationStart, error) {
	return StartSeedanceAssetValidationSessionWithFallback(ctx, userID, []*model.Channel{channel}, details, projectName, callbackURL)
}

// StartSeedanceAssetValidationSessionWithFallback retries only explicit unsupported-action responses.
// Authentication, callback, network and upstream business errors remain terminal for the selected request.
func StartSeedanceAssetValidationSessionWithFallback(ctx context.Context, userID int, channels []*model.Channel, details SeedanceAssetValidationGroupDetails, projectName, callbackURL string) (*SeedanceAssetValidationStart, error) {
	if model.DB == nil || userID <= 0 || len(channels) == 0 {
		return nil, errors.New("no enabled Seedance validation channel available")
	}
	if _, err := seedanceAssetValidationCipherKey(); err != nil {
		return nil, err
	}
	details.Name = strings.TrimSpace(details.Name)
	details.Description = strings.TrimSpace(details.Description)
	details.Tags = strings.TrimSpace(details.Tags)
	if utf8.RuneCountInString(details.Name) > 64 {
		return nil, errors.New("validation group name is too long")
	}
	if utf8.RuneCountInString(details.Description) > seedanceAssetValidationDescriptionLimit {
		return nil, errors.New("validation group description is too long")
	}
	if utf8.RuneCountInString(details.Tags) > seedanceAssetValidationTagsLimit {
		return nil, errors.New("validation group tags are too long")
	}
	projectName = strings.TrimSpace(projectName)
	if len(projectName) > seedanceAssetValidationProjectNameLimit {
		return nil, errors.New("validation project name is too long")
	}
	if callbackURL != "" {
		if err := ValidateSeedanceAssetValidationCallbackURL(callbackURL); err != nil {
			return nil, err
		}
	}
	state, stateHash, err := newSeedanceAssetValidationState()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	startIndex := -1
	var channel *model.Channel
	var client *SeedanceAssetClient
	var lastErr error
	for index, candidate := range channels {
		if candidate == nil {
			continue
		}
		candidateClient, clientErr := NewSeedanceAssetClient(candidate)
		if clientErr != nil {
			lastErr = clientErr
			continue
		}
		channel = candidate
		client = candidateClient
		startIndex = index
		break
	}
	if channel == nil || client == nil {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, errors.New("no enabled Seedance validation channel available")
	}
	session := &model.SeedanceAssetValidationSession{
		UserID:             userID,
		ChannelID:          channel.Id,
		KeyFingerprint:     client.KeyFingerprint,
		AccountFingerprint: client.AccountFingerprint,
		Name:               details.Name,
		Description:        details.Description,
		Tags:               details.Tags,
		ProjectName:        projectName,
		CallbackStateHash:  stateHash,
		Status:             model.SeedanceAssetValidationStatusCreating,
		ExpiresAt:          now.Add(seedanceAssetValidationSessionTTL).Unix(),
	}
	if err := model.DB.WithContext(ctx).Create(session).Error; err != nil {
		return nil, err
	}
	if session.Name == "" {
		session.Name = fmt.Sprintf("真人认证素材组-%d", session.ID)
		if err := model.DB.Model(session).Update("name", session.Name).Error; err != nil {
			return nil, err
		}
	}
	if callbackURL == "" {
		callbackURL, err = buildSeedanceAssetValidationCallbackURL(session.ID, state)
		if err != nil {
			_ = failSeedanceAssetValidationSession(session.ID, userID, err.Error())
			return nil, err
		}
	}
	var upstream struct {
		BytedToken string
		H5Link     string
	}
	lastErr = nil
	for index := startIndex; index < len(channels); index++ {
		candidate := channels[index]
		if candidate == nil {
			continue
		}
		client, err = NewSeedanceAssetClient(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		if index > 0 {
			channel = candidate
			result := model.DB.WithContext(ctx).Model(session).
				Where("id = ? AND user_id = ? AND status = ?", session.ID, userID, model.SeedanceAssetValidationStatusCreating).
				Updates(map[string]any{
					"channel_id":          channel.Id,
					"key_fingerprint":     client.KeyFingerprint,
					"account_fingerprint": client.AccountFingerprint,
				})
			if result.Error != nil {
				_ = failSeedanceAssetValidationSession(session.ID, userID, result.Error.Error())
				return nil, result.Error
			}
		}
		upstream, err = client.CreateSeedanceAssetValidationSession(ctx, callbackURL, projectName)
		if err == nil {
			break
		}
		lastErr = err
		if !errors.Is(err, ErrSeedanceAssetValidationUnsupported) {
			_ = failSeedanceAssetValidationSession(session.ID, userID, err.Error())
			return nil, err
		}
	}
	if lastErr != nil && strings.TrimSpace(upstream.BytedToken) == "" {
		_ = failSeedanceAssetValidationSession(session.ID, userID, lastErr.Error())
		return nil, lastErr
	}
	ciphertext, err := encryptSeedanceAssetValidationToken(upstream.BytedToken)
	if err != nil {
		_ = failSeedanceAssetValidationSession(session.ID, userID, err.Error())
		return nil, err
	}
	tokenHash, err := seedanceAssetValidationTokenHash(upstream.BytedToken)
	if err != nil {
		_ = failSeedanceAssetValidationSession(session.ID, userID, err.Error())
		return nil, err
	}
	result := model.DB.WithContext(ctx).Model(session).
		Where("id = ? AND user_id = ? AND status = ?", session.ID, userID, model.SeedanceAssetValidationStatusCreating).
		Updates(map[string]any{
			"byted_token_hash":       tokenHash,
			"byted_token_ciphertext": ciphertext,
			"launch_url":             upstream.H5Link,
			"status":                 model.SeedanceAssetValidationStatusPending,
			"last_error":             "",
		})
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, errors.New("validation session expired while creating upstream session")
	}
	session.BytedTokenHash = &tokenHash
	session.BytedTokenCiphertext = ciphertext
	session.LaunchURL = upstream.H5Link
	session.Status = model.SeedanceAssetValidationStatusPending
	return &SeedanceAssetValidationStart{Session: session, BytedToken: upstream.BytedToken, LaunchURL: upstream.H5Link}, nil
}

func failSeedanceAssetValidationSession(id uint, userID int, message string) error {
	return model.DB.Model(&model.SeedanceAssetValidationSession{}).
		Where("id = ? AND user_id = ? AND status = ?", id, userID, model.SeedanceAssetValidationStatusCreating).
		Updates(map[string]any{
			"status":                 model.SeedanceAssetValidationStatusFailed,
			"last_error":             truncateSeedanceAssetMappingError(message),
			"byted_token_ciphertext": "",
		}).Error
}

// RefreshSeedanceAssetValidationSession polls the upstream result and publishes the local group exactly once.
func RefreshSeedanceAssetValidationSession(ctx context.Context, userID int, id uint) (*model.SeedanceAssetValidationSession, error) {
	return refreshSeedanceAssetValidationSession(ctx, userID, id, "")
}

func refreshSeedanceAssetValidationSession(ctx context.Context, userID int, id uint, projectNameOverride string) (*model.SeedanceAssetValidationSession, error) {
	session, err := model.GetSeedanceAssetValidationSession(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if session.Status == model.SeedanceAssetValidationStatusSucceeded || session.Status == model.SeedanceAssetValidationStatusFailed || session.Status == model.SeedanceAssetValidationStatusExpired || session.Status == model.SeedanceAssetValidationStatusCancelled {
		return session, nil
	}
	if session.ExpiresAt > 0 && session.ExpiresAt <= common.GetTimestamp() {
		_, err = model.ExpireSeedanceAssetValidationSession(ctx, id, userID, common.GetTimestamp())
		if err != nil {
			return nil, err
		}
		return model.GetSeedanceAssetValidationSession(ctx, userID, id)
	}
	if session.Status != model.SeedanceAssetValidationStatusPending {
		return session, nil
	}
	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetValidationLease/time.Second)
	claimed, err := model.ClaimSeedanceAssetValidationSession(id, userID, now, leaseUntil)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return model.GetSeedanceAssetValidationSession(ctx, userID, id)
	}
	token, err := decryptSeedanceAssetValidationToken(session.BytedTokenCiphertext)
	if err != nil {
		return updateSeedanceAssetValidationPending(ctx, id, userID, leaseUntil, session, err)
	}
	client, err := ResolveSeedanceAssetClient(ctx, session.ChannelID, session.KeyFingerprint, session.AccountFingerprint)
	if err != nil {
		return updateSeedanceAssetValidationPending(ctx, id, userID, leaseUntil, session, err)
	}
	projectName := session.ProjectName
	if projectNameOverride != "" {
		projectName = projectNameOverride
	}
	groupID, err := client.GetSeedanceAssetValidationResult(ctx, token, projectName)
	if err != nil {
		return updateSeedanceAssetValidationPending(ctx, id, userID, leaseUntil, session, err)
	}
	if _, err := model.CompleteSeedanceAssetValidationSession(ctx, id, userID, leaseUntil, now, groupID); err != nil {
		if errors.Is(err, model.ErrSeedanceAssetValidationSessionClaimed) {
			current, readErr := model.GetSeedanceAssetValidationSession(ctx, userID, id)
			if readErr != nil {
				return nil, errors.Join(err, readErr)
			}
			switch current.Status {
			case model.SeedanceAssetValidationStatusSucceeded:
				return current, nil
			case model.SeedanceAssetValidationStatusFailed, model.SeedanceAssetValidationStatusExpired, model.SeedanceAssetValidationStatusCancelled:
				cleanupErr := RollbackSeedanceAssetGroup(ctx, client, groupID)
				return nil, errors.Join(err, cleanupErr)
			default:
				return current, nil
			}
		}
		current, readErr := model.GetSeedanceAssetValidationSession(ctx, userID, id)
		if readErr == nil && current.Status == model.SeedanceAssetValidationStatusSucceeded && current.UpstreamGroupID == groupID {
			return current, nil
		}
		cleanupErr := RollbackSeedanceAssetGroup(ctx, client, groupID)
		_, updateErr := model.UpdateSeedanceAssetValidationSession(id, userID, leaseUntil, map[string]any{
			"status":                 model.SeedanceAssetValidationStatusFailed,
			"last_error":             truncateSeedanceAssetMappingError(errors.Join(err, cleanupErr).Error()),
			"byted_token_ciphertext": "",
		})
		return nil, errors.Join(err, cleanupErr, updateErr, readErr)
	}
	return model.GetSeedanceAssetValidationSession(ctx, userID, id)
}

func updateSeedanceAssetValidationPending(ctx context.Context, id uint, userID int, leaseUntil int64, previous *model.SeedanceAssetValidationSession, pollErr error) (*model.SeedanceAssetValidationSession, error) {
	updates := map[string]any{"poll_attempts": previous.PollAttempts + 1}
	if errors.Is(pollErr, ErrSeedanceAssetValidationPending) {
		updates["last_error"] = ""
	} else {
		updates["last_error"] = truncateSeedanceAssetMappingError(pollErr.Error())
	}
	_, err := model.UpdateSeedanceAssetValidationSession(id, userID, leaseUntil, updates)
	if err != nil {
		return nil, errors.Join(pollErr, err)
	}
	return model.GetSeedanceAssetValidationSession(ctx, userID, id)
}

func RefreshSeedanceAssetValidationSessionByToken(ctx context.Context, userID int, bytedToken, projectName string) (*model.SeedanceAssetValidationSession, error) {
	bytedToken = strings.TrimSpace(bytedToken)
	if bytedToken == "" {
		return nil, errors.New("BytedToken is required")
	}
	tokenHash, err := seedanceAssetValidationTokenHash(bytedToken)
	if err != nil {
		return nil, err
	}
	session, err := model.FindSeedanceAssetValidationSessionByTokenHash(ctx, userID, tokenHash)
	if err != nil {
		return nil, err
	}
	projectName = strings.TrimSpace(projectName)
	if len(projectName) > seedanceAssetValidationProjectNameLimit {
		return nil, errors.New("validation project name is too long")
	}
	if projectName != "" && session.ProjectName != "" && projectName != session.ProjectName {
		return nil, ErrSeedanceAssetValidationProject
	}
	return refreshSeedanceAssetValidationSession(ctx, userID, session.ID, projectName)
}

func BuildSeedanceAssetValidationCallbackURL(sessionID uint, state string) (string, error) {
	return buildSeedanceAssetValidationCallbackURL(sessionID, state)
}

func HashSeedanceAssetValidationState(state string) (string, error) {
	return seedanceAssetValidationHMAC("seedance-validation-state-v1:" + strings.TrimSpace(state))
}

func (client *SeedanceAssetClient) CreateSeedanceAssetValidationSession(ctx context.Context, callbackURL, projectName string) (struct {
	BytedToken string
	H5Link     string
}, error) {
	var response seedanceAssetResponse
	request := map[string]string{"CallbackURL": callbackURL}
	if strings.TrimSpace(projectName) != "" {
		request["ProjectName"] = strings.TrimSpace(projectName)
	}
	if err := client.call(ctx, "CreateVisualValidateSession", request, &response); err != nil {
		if isSeedanceAssetValidationUnsupportedError(err) {
			return struct {
				BytedToken string
				H5Link     string
			}{}, errors.Join(ErrSeedanceAssetValidationUnsupported, err)
		}
		return struct {
			BytedToken string
			H5Link     string
		}{}, err
	}
	if strings.TrimSpace(response.Result.BytedToken) == "" || strings.TrimSpace(response.Result.H5Link) == "" {
		return struct {
			BytedToken string
			H5Link     string
		}{}, errors.New("upstream returned an incomplete validation session")
	}
	return struct {
		BytedToken string
		H5Link     string
	}{BytedToken: response.Result.BytedToken, H5Link: response.Result.H5Link}, nil
}

// isSeedanceAssetValidationUnsupportedError limits fallback to errors that identify a missing Action.
func isSeedanceAssetValidationUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSeedanceAssetNotFound) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"status 404",
		"status 405",
		"status 501",
		"not implemented",
		"unsupported action",
		"invalid action",
		"unknown action",
		"action not found",
		"not support",
		"unsupported",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (client *SeedanceAssetClient) GetSeedanceAssetValidationResult(ctx context.Context, bytedToken, projectName string) (string, error) {
	var response seedanceAssetResponse
	request := map[string]string{"BytedToken": bytedToken}
	if strings.TrimSpace(projectName) != "" {
		request["ProjectName"] = strings.TrimSpace(projectName)
	}
	if err := client.call(ctx, "GetVisualValidateResult", request, &response); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "visual face token is not found") {
			return "", ErrSeedanceAssetValidationPending
		}
		if bytedToken != "" && strings.Contains(err.Error(), bytedToken) {
			return "", errors.New(strings.ReplaceAll(err.Error(), bytedToken, "[redacted]"))
		}
		return "", err
	}
	if strings.TrimSpace(response.Result.GroupID) == "" {
		return "", ErrSeedanceAssetValidationPending
	}
	return response.Result.GroupID, nil
}
