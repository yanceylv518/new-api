package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const seedanceAssetReferencePrefix = "asset://"

var seedanceAssetMediaFields = map[string]struct{}{
	"audio":           {},
	"audio_url":       {},
	"audio_urls":      {},
	"image":           {},
	"image_url":       {},
	"image_urls":      {},
	"images":          {},
	"input_reference": {},
	"video":           {},
	"video_url":       {},
	"video_urls":      {},
	"videos":          {},
}

var seedanceAssetJSONContainerFields = map[string]struct{}{
	"content":  {},
	"input":    {},
	"metadata": {},
}

func seedanceAssetField(name string) bool {
	_, ok := seedanceAssetMediaFields[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func seedanceAssetJSONContainer(name string) bool {
	_, ok := seedanceAssetJSONContainerFields[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func appendSeedanceAssetReference(raw string, ids *[]string, seen map[string]struct{}) error {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, seedanceAssetReferencePrefix) {
		return nil
	}
	assetID := strings.TrimPrefix(raw, seedanceAssetReferencePrefix)
	if assetID == "" || len(assetID) > 128 || strings.IndexFunc(assetID, func(r rune) bool { return r <= ' ' }) >= 0 {
		return errors.New("invalid private asset reference")
	}
	if _, exists := seen[assetID]; exists {
		return nil
	}
	if len(*ids) >= model.MaxSeedanceAssetAffinityReferences {
		return fmt.Errorf("private asset references exceed %d", model.MaxSeedanceAssetAffinityReferences)
	}
	seen[assetID] = struct{}{}
	*ids = append(*ids, assetID)
	return nil
}

// collectSeedanceAssetReferences only treats media URL fields as asset
// references, so a prompt that merely mentions "asset://..." is not routed.
func collectSeedanceAssetReferences(value any, mediaField bool, ids *[]string, seen map[string]struct{}) error {
	switch current := value.(type) {
	case string:
		if mediaField {
			return appendSeedanceAssetReference(current, ids, seen)
		}
	case []any:
		for _, item := range current {
			if err := collectSeedanceAssetReferences(item, mediaField, ids, seen); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range current {
			field := strings.ToLower(strings.TrimSpace(key))
			childMedia := mediaField || seedanceAssetField(field)
			if err := collectSeedanceAssetReferences(item, childMedia, ids, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractSeedanceAssetIDsFromJSON(raw []byte) ([]string, error) {
	if !bytes.Contains(raw, []byte(seedanceAssetReferencePrefix)) {
		return nil, nil
	}
	var value any
	if err := common.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	ids := make([]string, 0, 1)
	if err := collectSeedanceAssetReferences(value, false, &ids, make(map[string]struct{})); err != nil {
		return nil, err
	}
	return ids, nil
}

func extractSeedanceAssetIDsFromMultipart(c *gin.Context) ([]string, error) {
	form, err := common.ParseMultipartFormReusable(c)
	if err != nil {
		return nil, err
	}
	defer form.RemoveAll()
	ids := make([]string, 0, 1)
	seen := make(map[string]struct{})
	for field, values := range form.Value {
		field = strings.ToLower(strings.TrimSpace(field))
		for _, value := range values {
			value = strings.TrimSpace(value)
			if seedanceAssetField(field) {
				if err := appendSeedanceAssetReference(value, &ids, seen); err != nil {
					return nil, err
				}
				continue
			}
			if !seedanceAssetJSONContainer(field) || (len(value) == 0 || (value[0] != '{' && value[0] != '[')) {
				continue
			}
			parsed, parseErr := extractSeedanceAssetIDsFromJSON([]byte(value))
			if parseErr != nil {
				return nil, parseErr
			}
			for _, assetID := range parsed {
				if err := appendSeedanceAssetReference(seedanceAssetReferencePrefix+assetID, &ids, seen); err != nil {
					return nil, err
				}
			}
		}
	}
	return ids, nil
}

func extractSeedanceAssetIDs(c *gin.Context) ([]string, error) {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		mediaType = c.GetHeader("Content-Type")
	}
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		storage, storageErr := common.GetBodyStorage(c)
		if storageErr != nil {
			return nil, storageErr
		}
		raw, readErr := storage.Bytes()
		if readErr != nil {
			return nil, readErr
		}
		return extractSeedanceAssetIDsFromJSON(raw)
	}
	if mediaType == "multipart/form-data" {
		return extractSeedanceAssetIDsFromMultipart(c)
	}
	return nil, nil
}

func applySeedanceAssetAffinity(c *gin.Context) error {
	assetIDs, err := extractSeedanceAssetIDs(c)
	if err != nil {
		return fmt.Errorf("failed to inspect private asset references: %w", err)
	}
	if len(assetIDs) == 0 {
		return nil
	}
	binding, err := model.FindSeedanceAssetBinding(c.Request.Context(), c.GetInt("id"), assetIDs)
	if err != nil {
		return err
	}
	common.SetContextKey(c, constant.ContextKeySeedanceAssetChannelId, binding.ChannelID)
	common.SetContextKey(c, constant.ContextKeySeedanceAssetKeyFingerprint, binding.KeyFingerprint)
	common.SetContextKey(c, constant.ContextKeyChannelConstraints, func() *dto.ChannelConstraints {
		constraints, _ := common.GetContextKeyType[*dto.ChannelConstraints](c, constant.ContextKeyChannelConstraints)
		if constraints == nil {
			constraints = &dto.ChannelConstraints{}
		}
		constraints.AddPin(dto.ChannelPin{
			ChannelId: binding.ChannelID,
			Source:    dto.PinSourceSeedanceAsset,
			Rank:      dto.PinRankSeedanceAsset,
			RetryMode: dto.PinRetrySameChannel,
		})
		return constraints
	}())
	return nil
}
