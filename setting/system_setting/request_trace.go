package system_setting

import (
	"errors"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
)

const RequestTraceEnabledOptionKey = "request_trace.enabled"
const RequestTraceCompressionOptionKey = "request_trace.compression_level"
const DefaultRequestTraceCompressionLevel = "fastest"

var requestTraceEnabled atomic.Bool

func IsRequestTraceEnabled() bool {
	return requestTraceEnabled.Load()
}

func SetRequestTraceEnabled(enabled bool) {
	requestTraceEnabled.Store(enabled)
}

func ValidateRequestTraceEnabled(value string) error {
	if value != "true" && value != "false" {
		return errors.New("request trace enabled must be true or false")
	}
	return nil
}

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
