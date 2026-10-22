package requesttrace

import (
	"hash/fnv"
	"strings"
)

var relayPathPrefixes = [...]string{"/v1", "/v1beta", "/pg", "/mj"}

func IsRelayPath(path string) bool {
	for _, prefix := range relayPathPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	if !strings.HasPrefix(path, "/") {
		return false
	}
	mode, rest, found := strings.Cut(path[1:], "/")
	return found && mode != "" && (rest == "mj" || strings.HasPrefix(rest, "mj/"))
}

type SamplingPolicy struct {
	SuccessRate     int
	SlowRequestUS   int64
	SlowFirstByteUS int64
}

func (p SamplingPolicy) Keep(record *Record) bool {
	if record.Outcome != "success" || record.CaptureStatus == "partial" || record.AttemptCount > 1 {
		return true
	}
	// 长流的正常生成时间不作为总耗时异常，首字节阈值仍适用。
	if p.SlowRequestUS > 0 && (record.IsStream == nil || !*record.IsStream) && record.DurationUS >= p.SlowRequestUS {
		return true
	}
	if p.SlowFirstByteUS > 0 && record.FinalAttemptFirstByteOffsetUS != nil && *record.FinalAttemptFirstByteOffsetUS >= p.SlowFirstByteUS {
		return true
	}
	if p.SuccessRate >= 100 {
		return true
	}
	if p.SuccessRate <= 0 {
		return false
	}
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(record.RequestID))
	return digest.Sum64()%100 < uint64(p.SuccessRate)
}
