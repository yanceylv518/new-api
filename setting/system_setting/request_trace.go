package system_setting

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
)

const RequestTraceCompressionOptionKey = "request_trace.compression_level"
const DefaultRequestTraceCompressionLevel = "fastest"

func ValidateRequestTraceCompressionLevel(level string) error {
	switch level {
	case "fastest", "default", "better", "best":
		return nil
	default:
		return errors.New("request trace compression level must be fastest, default, better, or best")
	}
}

func GetRequestTraceCompressionLevel() string {
	common.OptionMapRWMutex.RLock()
	level := common.OptionMap[RequestTraceCompressionOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if level == "" {
		return DefaultRequestTraceCompressionLevel
	}
	return level
}
