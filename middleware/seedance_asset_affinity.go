package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const seedanceAssetReferencePrefix = "asset://"

// ErrInvalidSeedanceAssetRequest 表示引用格式无效或所选接口不支持本地素材引用。
var ErrInvalidSeedanceAssetRequest = errors.New("invalid private asset request")

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
		return fmt.Errorf("%w: malformed reference", ErrInvalidSeedanceAssetRequest)
	}
	if _, exists := seen[assetID]; exists {
		return nil
	}
	if len(*ids) >= model.MaxSeedanceAssetAffinityReferences {
		return fmt.Errorf("%w: too many references", ErrInvalidSeedanceAssetRequest)
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
	if !bytes.Contains(raw, []byte("asset:")) {
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

func prepareSeedanceAssetRequest(c *gin.Context, channel *model.Channel, key string) error {
	assetIDs, exists := common.GetContextKeyType[[]string](c, constant.ContextKeySeedanceAssetReferences)
	if c.Request == nil {
		if exists && len(assetIDs) > 0 {
			return fmt.Errorf("%w: request body is unavailable", ErrInvalidSeedanceAssetRequest)
		}
		return nil
	}
	if !exists {
		var err error
		assetIDs, err = extractSeedanceAssetIDs(c)
		if err != nil {
			return fmt.Errorf("%w: failed to inspect references: %v", ErrInvalidSeedanceAssetRequest, err)
		}
		if len(assetIDs) == 0 {
			return nil
		}
		common.SetContextKey(c, constant.ContextKeySeedanceAssetReferences, assetIDs)
	}
	if !service.SupportsSeedanceAssets(channel) {
		return fmt.Errorf("%w: selected channel does not support Seedance assets", ErrInvalidSeedanceAssetRequest)
	}
	client, err := service.NewSeedanceAssetClientForSelectedKey(channel, key)
	if err != nil {
		return err
	}
	newMapping, err := service.MapSeedanceAssetsToAccount(c.Request.Context(), c.GetInt("id"), client, assetIDs)
	if err != nil {
		return err
	}
	previousMapping, _ := common.GetContextKeyType[map[string]string](c, constant.ContextKeySeedanceAssetMapping)
	if maps.Equal(previousMapping, newMapping) && previousMapping != nil {
		return nil
	}
	if err := rewriteSeedanceRequestBody(c, previousMapping, newMapping); err != nil {
		return fmt.Errorf("failed to rewrite private asset references: %w", err)
	}
	common.SetContextKey(c, constant.ContextKeySeedanceAssetMapping, newMapping)
	return nil
}

func rewriteSeedanceRequestBody(c *gin.Context, previousMapping, newMapping map[string]string) error {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		mediaType = c.GetHeader("Content-Type")
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return err
		}
		raw, err := storage.Bytes()
		if err != nil {
			return err
		}
		rewritten, changed, err := rewriteSeedanceJSON(raw, previousMapping, newMapping)
		if err != nil || !changed {
			return err
		}
		bodyStorage, err := common.CreateBodyStorage(rewritten)
		if err != nil {
			return err
		}
		return replaceSeedanceRequestBody(c, bodyStorage, c.GetHeader("Content-Type"))
	case mediaType == "multipart/form-data":
		bodyStorage, contentType, changed, err := rewriteSeedanceMultipart(c, previousMapping, newMapping)
		if err != nil || !changed {
			return err
		}
		return replaceSeedanceRequestBody(c, bodyStorage, contentType)
	default:
		return fmt.Errorf("%w: asset references require a JSON or multipart request", ErrInvalidSeedanceAssetRequest)
	}
}

func rewriteSeedanceJSON(raw []byte, previousMapping, newMapping map[string]string) ([]byte, bool, error) {
	var value json.RawMessage = raw
	rewritten, changed, err := rewriteSeedanceJSONValue(value, false, previousMapping, newMapping)
	if err != nil || !changed {
		return raw, changed, err
	}
	return rewritten, true, nil
}

func rewriteSeedanceJSONValue(raw json.RawMessage, mediaField bool, previousMapping, newMapping map[string]string) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return raw, false, nil
	}
	switch raw[0] {
	case '"':
		if !mediaField {
			return raw, false, nil
		}
		var value string
		if err := common.Unmarshal(raw, &value); err != nil {
			return nil, false, err
		}
		rewritten, changed, err := rewriteSeedanceAssetReference(value, previousMapping, newMapping)
		if err != nil || !changed {
			return raw, changed, err
		}
		encoded, err := common.Marshal(rewritten)
		return encoded, true, err
	case '{':
		var fields map[string]json.RawMessage
		if err := common.Unmarshal(raw, &fields); err != nil {
			return nil, false, err
		}
		changed := false
		for key, child := range fields {
			rewritten, childChanged, err := rewriteSeedanceJSONValue(child, mediaField || seedanceAssetField(key), previousMapping, newMapping)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				fields[key] = rewritten
				changed = true
			}
		}
		if !changed {
			return raw, false, nil
		}
		encoded, err := common.Marshal(fields)
		return encoded, true, err
	case '[':
		var items []json.RawMessage
		if err := common.Unmarshal(raw, &items); err != nil {
			return nil, false, err
		}
		changed := false
		for index, child := range items {
			rewritten, childChanged, err := rewriteSeedanceJSONValue(child, mediaField, previousMapping, newMapping)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				items[index] = rewritten
				changed = true
			}
		}
		if !changed {
			return raw, false, nil
		}
		encoded, err := common.Marshal(items)
		return encoded, true, err
	default:
		return raw, false, nil
	}
}

func rewriteSeedanceAssetReference(value string, previousMapping, newMapping map[string]string) (string, bool, error) {
	if !strings.HasPrefix(value, seedanceAssetReferencePrefix) {
		return value, false, nil
	}
	assetID := strings.TrimPrefix(value, seedanceAssetReferencePrefix)
	localID := assetID
	for candidateLocalID, previousRemoteID := range previousMapping {
		if previousRemoteID == assetID {
			localID = candidateLocalID
			break
		}
	}
	remoteID, exists := newMapping[localID]
	if !exists {
		return "", false, fmt.Errorf("%w: reference has no mapping for selected account", ErrInvalidSeedanceAssetRequest)
	}
	rewritten := seedanceAssetReferencePrefix + remoteID
	return rewritten, rewritten != value, nil
}

func rewriteSeedanceMultipart(c *gin.Context, previousMapping, newMapping map[string]string) (common.BodyStorage, string, bool, error) {
	form, err := common.ParseMultipartFormReusable(c)
	if err != nil {
		return nil, "", false, fmt.Errorf("%w: malformed multipart request: %v", ErrInvalidSeedanceAssetRequest, err)
	}
	defer form.RemoveAll()
	changed := false
	for field, values := range form.Value {
		for index, value := range values {
			if seedanceAssetField(field) {
				rewritten, valueChanged, err := rewriteSeedanceAssetReference(value, previousMapping, newMapping)
				if err != nil {
					return nil, "", false, err
				}
				if valueChanged {
					values[index] = rewritten
					changed = true
				}
				continue
			}
			trimmed := strings.TrimSpace(value)
			if !seedanceAssetJSONContainer(field) || len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
				continue
			}
			rewritten, valueChanged, err := rewriteSeedanceJSON([]byte(value), previousMapping, newMapping)
			if err != nil {
				return nil, "", false, err
			}
			if valueChanged {
				values[index] = string(rewritten)
				changed = true
			}
		}
		form.Value[field] = values
	}
	if !changed {
		return nil, "", false, nil
	}
	previous, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, "", false, err
	}
	maxMB := constant.MaxRequestBodyMB
	if maxMB <= 0 {
		maxMB = 128
	}
	reader, pipeWriter := io.Pipe()
	contentType := ""
	writerErr := make(chan error, 1)
	go func() {
		writer := multipart.NewWriter(pipeWriter)
		for field, values := range form.Value {
			for _, value := range values {
				if writeErr := writer.WriteField(field, value); writeErr != nil {
					_ = pipeWriter.CloseWithError(writeErr)
					writerErr <- writeErr
					return
				}
			}
		}
		for field, files := range form.File {
			for _, fileHeader := range files {
				file, openErr := fileHeader.Open()
				if openErr != nil {
					_ = pipeWriter.CloseWithError(openErr)
					writerErr <- openErr
					return
				}
				var part io.Writer
				if len(fileHeader.Header) == 0 {
					part, openErr = writer.CreateFormFile(field, fileHeader.Filename)
				} else {
					part, openErr = writer.CreatePart(fileHeader.Header)
				}
				if openErr == nil {
					_, openErr = io.Copy(part, file)
				}
				closeErr := file.Close()
				if openErr == nil {
					openErr = closeErr
				}
				if openErr != nil {
					_ = pipeWriter.CloseWithError(openErr)
					writerErr <- openErr
					return
				}
			}
		}
		contentType = writer.FormDataContentType()
		writeErr := writer.Close()
		if writeErr != nil {
			_ = pipeWriter.CloseWithError(writeErr)
		} else {
			_ = pipeWriter.Close()
		}
		writerErr <- writeErr
	}()
	storage, storageErr := common.CreateBodyStorageFromReader(reader, previous.Size(), int64(maxMB)<<20)
	if storageErr != nil {
		_ = reader.CloseWithError(storageErr)
	}
	writeErr := <-writerErr
	if storageErr != nil {
		return nil, "", false, storageErr
	}
	if writeErr != nil {
		_ = storage.Close()
		return nil, "", false, writeErr
	}
	return storage, contentType, true, nil
}

func replaceSeedanceRequestBody(c *gin.Context, storage common.BodyStorage, contentType string) error {
	previous, err := common.GetBodyStorage(c)
	if err != nil {
		storage.Close()
		return err
	}
	if err := previous.Close(); err != nil {
		storage.Close()
		return err
	}
	c.Set(common.KeyBodyStorage, storage)
	c.Set(common.KeyRequestBody, nil)
	c.Request.Body = io.NopCloser(storage)
	c.Request.ContentLength = storage.Size()
	c.Request.Header.Set("Content-Length", strconv.FormatInt(storage.Size(), 10))
	c.Request.Header.Set("Content-Type", contentType)
	c.Request.GetBody = storage.NewReader
	if strings.HasPrefix(contentType, "multipart/form-data") {
		c.Set("_original_multipart_ct", contentType)
	}
	return nil
}
