package common

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const requestTraceStoragePrefix = "zstd-base64-v1:"
const requestTraceCompressionMinBytes = 256

type RequestTraceCompressor struct {
	encoder *zstd.Encoder
	level   string
	buffer  []byte
}

func (c *RequestTraceCompressor) Close() error {
	var err error
	if c.encoder != nil {
		err = c.encoder.Close()
	}
	c.encoder, c.buffer, c.level = nil, nil, ""
	return err
}

var requestTraceStorageDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(TraceMetadataMaxBytes),
	)
})

func EncodeRequestTraceStorage(value string) (encoded string, err error) {
	var compressor RequestTraceCompressor
	defer func() { err = errors.Join(err, compressor.Close()) }()
	return compressor.Encode(value)
}

// 压缩器只由一个写入 worker 使用，切换档位时释放旧编码器和缓冲。
func (c *RequestTraceCompressor) Encode(value string) (string, error) {
	if len(value) > TraceMetadataMaxBytes {
		return "", fmt.Errorf("request trace metadata exceeds %d bytes", TraceMetadataMaxBytes)
	}
	requestTraceConfig.RLock()
	level := requestTraceConfig.value.CompressionLevel
	requestTraceConfig.RUnlock()
	if level != "fast" && level != "standard" {
		return "", fmt.Errorf("invalid request trace compression level")
	}
	if c.level != level {
		if err := c.Close(); err != nil {
			return "", err
		}
		c.level = level
	}
	// 小摘要保留原 JSON，避免初始化压缩器。
	if len(value) < requestTraceCompressionMinBytes {
		return value, nil
	}
	if c.encoder == nil {
		encoderLevel := zstd.SpeedFastest
		if level == "standard" {
			encoderLevel = zstd.SpeedDefault
		}
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encoderLevel), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(TraceMetadataMaxBytes))
		if err != nil {
			return "", fmt.Errorf("initialize request trace compressor: %w", err)
		}
		c.encoder = encoder
	}
	c.buffer = c.encoder.EncodeAll([]byte(value), c.buffer[:0])
	if len(requestTraceStoragePrefix)+base64.StdEncoding.EncodedLen(len(c.buffer)) >= len(value) {
		return value, nil
	}
	return requestTraceStoragePrefix + base64.StdEncoding.EncodeToString(c.buffer), nil
}

func DecodeRequestTraceStorage(value string) (string, error) {
	if !strings.HasPrefix(value, requestTraceStoragePrefix) {
		return value, nil
	}
	encoded := strings.TrimPrefix(value, requestTraceStoragePrefix)
	if len(encoded) > base64.StdEncoding.EncodedLen(2*TraceMetadataMaxBytes) {
		return "", fmt.Errorf("compressed request trace metadata exceeds the storage limit")
	}
	compressed, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode request trace base64: %w", err)
	}
	decoder, err := requestTraceStorageDecoder()
	if err != nil {
		return "", fmt.Errorf("initialize request trace decompressor: %w", err)
	}
	decoded, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return "", fmt.Errorf("decompress request trace metadata: %w", err)
	}
	return string(decoded), nil
}
