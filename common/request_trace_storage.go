package common

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const requestTraceStoragePrefix = "zstd-base64-v1:"
const requestTraceCompressionMinBytes = 256

var requestTraceStorageEncoders = map[string]func() (*zstd.Encoder, error){
	"fast":     requestTraceStorageEncoder(zstd.SpeedFastest),
	"standard": requestTraceStorageEncoder(zstd.SpeedDefault),
	"high":     requestTraceStorageEncoder(zstd.SpeedBetterCompression),
	"best":     requestTraceStorageEncoder(zstd.SpeedBestCompression),
}

func requestTraceStorageEncoder(level zstd.EncoderLevel) func() (*zstd.Encoder, error) {
	return sync.OnceValues(func() (*zstd.Encoder, error) {
		return zstd.NewWriter(nil,
			zstd.WithEncoderLevel(level),
			zstd.WithEncoderConcurrency(min(runtime.GOMAXPROCS(0), 4)),
			zstd.WithWindowSize(TraceMetadataMaxBytes),
		)
	})
}

var requestTraceStorageDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(TraceMetadataMaxBytes),
	)
})

func EncodeRequestTraceStorage(value string) (string, error) {
	if len(value) > TraceMetadataMaxBytes {
		return "", fmt.Errorf("request trace metadata exceeds %d bytes", TraceMetadataMaxBytes)
	}
	requestTraceConfig.RLock()
	level := requestTraceConfig.value.CompressionLevel
	requestTraceConfig.RUnlock()
	createEncoder, ok := requestTraceStorageEncoders[level]
	if !ok {
		return "", fmt.Errorf("invalid request trace compression level")
	}
	// 小摘要省去压缩；读取端兼容无前缀 JSON，四档设置仍用于较大的记录。
	if len(value) < requestTraceCompressionMinBytes {
		return value, nil
	}
	encoder, err := createEncoder()
	if err != nil {
		return "", fmt.Errorf("initialize request trace compressor: %w", err)
	}
	compressed := encoder.EncodeAll([]byte(value), nil)
	if len(requestTraceStoragePrefix)+base64.StdEncoding.EncodedLen(len(compressed)) >= len(value) {
		return value, nil
	}
	return requestTraceStoragePrefix + base64.StdEncoding.EncodeToString(compressed), nil
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
