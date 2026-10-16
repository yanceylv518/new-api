package service

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/klauspost/compress/zstd"
)

const requestTraceCompressionCodec = "zstd-base64"

type requestTraceCompressionEnvelope struct {
	Codec    string `json:"codec"`
	RawBytes int    `json:"raw_bytes"`
	Payload  string `json:"payload"`
}

// 编码器和缓冲区只由异步写入协程持有，切换档位时释放旧编码器。
type requestTraceCompressor struct {
	encoder *zstd.Encoder
	level   string
	buffer  []byte
}

func (c *requestTraceCompressor) Encode(raw []byte, level string) (string, error) {
	if c.encoder == nil || c.level != level {
		var encoderLevel zstd.EncoderLevel
		switch level {
		case "fastest":
			encoderLevel = zstd.SpeedFastest
		case "default":
			encoderLevel = zstd.SpeedDefault
		case "better":
			encoderLevel = zstd.SpeedBetterCompression
		case "best":
			encoderLevel = zstd.SpeedBestCompression
		default:
			return "", errors.New("unsupported request trace compression level")
		}
		if err := c.Close(); err != nil {
			return "", err
		}
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(encoderLevel), zstd.WithEncoderCRC(true))
		if err != nil {
			return "", err
		}
		c.encoder, c.level = encoder, level
	}
	c.buffer = c.encoder.EncodeAll(raw, c.buffer[:0])
	if base64.StdEncoding.EncodedLen(len(c.buffer)) >= len(raw) {
		return string(raw), nil
	}
	// JSON 编码器直接生成相同的 Base64 字符串，读取侧仍使用严格校验。
	encoded, err := common.Marshal(struct {
		Codec    string `json:"codec"`
		RawBytes int    `json:"raw_bytes"`
		Payload  []byte `json:"payload"`
	}{
		Codec: requestTraceCompressionCodec, RawBytes: len(raw), Payload: c.buffer,
	})
	if err != nil {
		return "", err
	}
	if len(encoded) >= len(raw) {
		return string(raw), nil
	}
	return string(encoded), nil
}

func (c *requestTraceCompressor) Close() error {
	if c.encoder == nil {
		return nil
	}
	err := c.encoder.Close()
	c.encoder = nil
	return err
}

// DecodeRequestTraceData 兼容原始 JSON；压缩记录恢复为同一份 JSON 后再交给原有版本校验。
func DecodeRequestTraceData(stored string) ([]byte, error) {
	if len(stored) == 0 || len(stored) > maxRequestTraceBytes {
		return nil, errors.New("request trace data exceeds size limit or is empty")
	}
	var envelope requestTraceCompressionEnvelope
	if err := common.UnmarshalJsonStr(stored, &envelope); err != nil {
		return nil, fmt.Errorf("invalid request trace storage data: %w", err)
	}
	if envelope.Codec == "" {
		return []byte(stored), nil
	}
	if envelope.Codec != requestTraceCompressionCodec {
		return nil, errors.New("unsupported request trace storage codec")
	}
	if envelope.RawBytes <= 0 || envelope.RawBytes > maxRequestTraceBytes {
		return nil, errors.New("invalid request trace uncompressed size")
	}
	compressed, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		return nil, fmt.Errorf("invalid request trace compressed payload: %w", err)
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxRequestTraceBytes), zstd.WithDecoderMaxWindow(maxRequestTraceBytes), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return nil, fmt.Errorf("initialize request trace decoder: %w", err)
	}
	defer decoder.Close()
	raw, err := decoder.DecodeAll(compressed, make([]byte, 0, envelope.RawBytes))
	if err != nil {
		return nil, fmt.Errorf("decode request trace payload: %w", err)
	}
	if len(raw) != envelope.RawBytes {
		return nil, errors.New("request trace uncompressed size mismatch")
	}
	return raw, nil
}
