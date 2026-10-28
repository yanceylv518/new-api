package system_setting

import (
	"errors"
	"strconv"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/requesttrace"
)

const RequestTraceEnabledOptionKey = "request_trace.enabled"
const RequestTraceCompressionOptionKey = "request_trace.compression_level"
const DefaultRequestTraceCompressionLevel = "fastest"
const RequestTraceSuccessSampleRateOptionKey = "request_trace.success_sample_rate"
const RequestTraceSlowRequestOptionKey = "request_trace.slow_request_ms"
const RequestTraceSlowFirstByteOptionKey = "request_trace.slow_first_byte_ms"
const RequestTraceRetentionOptionKey = "request_trace.retention_days"
const defaultRequestTraceRetentionDays = 30

var requestTraceEnabled atomic.Bool
var requestTraceSampling atomic.Pointer[requesttrace.SamplingPolicy]

func DefaultRequestTraceRetentionDays() int {
	days := common.GetEnvOrDefault("REQUEST_TRACE_RETENTION_DAYS", defaultRequestTraceRetentionDays)
	if days < 1 || days > 3650 {
		return defaultRequestTraceRetentionDays
	}
	return days
}

func GetRequestTraceRetentionDays() int {
	common.OptionMapRWMutex.RLock()
	value, exists := common.OptionMap[RequestTraceRetentionOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if !exists {
		return DefaultRequestTraceRetentionDays()
	}
	days, err := strconv.Atoi(value)
	if err != nil || days < 0 || days > 3650 {
		return DefaultRequestTraceRetentionDays()
	}
	return days
}

func ValidateRequestTraceRetentionDays(value string) error {
	days, err := strconv.Atoi(value)
	if err != nil || days < 0 || days > 3650 {
		return errors.New("request timeline retention must be an integer between 0 and 3650 days")
	}
	return nil
}

func IsRequestTraceSamplingOption(key string) bool {
	return key == RequestTraceSuccessSampleRateOptionKey || key == RequestTraceSlowRequestOptionKey || key == RequestTraceSlowFirstByteOptionKey
}

func ValidateRequestTraceSamplingOption(key, value string) error {
	if !IsRequestTraceSamplingOption(key) {
		return nil
	}
	maximum := 86400000
	if key == RequestTraceSuccessSampleRateOptionKey {
		maximum = 100
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 || number > int64(maximum) {
		return errors.New(key + " must be an integer between 0 and " + strconv.Itoa(maximum))
	}
	return nil
}

func GetRequestTraceSamplingPolicy() requesttrace.SamplingPolicy {
	if policy := requestTraceSampling.Load(); policy != nil {
		return *policy
	}
	return requesttrace.SamplingPolicy{SuccessRate: 100}
}

func SetRequestTraceSamplingOption(key, value string) error {
	if err := ValidateRequestTraceSamplingOption(key, value); err != nil {
		return err
	}
	if !IsRequestTraceSamplingOption(key) {
		return errors.New("unsupported request trace sampling option")
	}
	number, _ := strconv.ParseInt(value, 10, 64)
	for {
		previous := requestTraceSampling.Load()
		next := requesttrace.SamplingPolicy{SuccessRate: 100}
		if previous != nil {
			next = *previous
		}
		switch key {
		case RequestTraceSuccessSampleRateOptionKey:
			next.SuccessRate = int(number)
		case RequestTraceSlowRequestOptionKey:
			next.SlowRequestUS = number * 1000
		case RequestTraceSlowFirstByteOptionKey:
			next.SlowFirstByteUS = number * 1000
		}
		if requestTraceSampling.CompareAndSwap(previous, &next) {
			return nil
		}
	}
}

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
