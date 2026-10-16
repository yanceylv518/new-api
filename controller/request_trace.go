package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/requesttrace"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type requestTraceSummary struct {
	RequestId                 string `json:"request_id"`
	StartedAtUS               int64  `json:"started_at_us"`
	FinishedAtUS              int64  `json:"finished_at_us"`
	DurationUS                int64  `json:"duration_us"`
	FirstPayloadWriteOffsetUS *int64 `json:"first_payload_write_offset_us"`
	HTTPStatus                *int   `json:"http_status"`
	Outcome                   string `json:"outcome"`
	IsStream                  *bool  `json:"is_stream"`
	CaptureStatus             string `json:"capture_status"`
	SchemaVersion             int    `json:"schema_version"`
}

type requestTraceDetail struct {
	Timeline                      *requesttrace.Data `json:"timeline"`
	AttemptCount                  int                `json:"attempt_count"`
	FinalChannelId                *int64             `json:"final_channel_id"`
	FinalAttemptFirstByteOffsetUS *int64             `json:"final_attempt_first_byte_offset_us"`
	NodeId                        string             `json:"node_id"`
	Status                        string             `json:"status"`
}

type requestTraceQueryResult struct {
	Found         bool                 `json:"found"`
	Enabled       bool                 `json:"enabled"`
	RetentionDays int                  `json:"retention_days"`
	Summary       *requestTraceSummary `json:"summary"`
	Detail        *requestTraceDetail  `json:"detail"`
}

func GetRequestTrace(c *gin.Context) { getRequestTrace(c, true) }

func GetSelfRequestTrace(c *gin.Context) { getRequestTrace(c, false) }

func getRequestTrace(c *gin.Context, admin bool) {
	requestID := c.Param("request_id")
	if len(requestID) == 0 || len(requestID) > 64 || strings.Trim(requestID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "invalid_request_id", "message": "Invalid request ID"})
		return
	}
	userID := 0
	if !admin {
		userID = c.GetInt("id")
		if userID <= 0 {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
	}
	status := service.GetRequestTraceWriterStatus()
	result := requestTraceQueryResult{Enabled: status.Enabled, RetentionDays: model.RequestTraceRetentionDays()}
	queryCtx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	findTrace := model.FindRequestTraceSummary
	if admin {
		findTrace = model.FindRequestTrace
	}
	entry, err := findTrace(queryCtx, requestID, userID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiSuccess(c, result)
		return
	}
	if err != nil {
		common.SysError("request trace query failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "code": "trace_query_failed", "message": "Unable to load request timeline"})
		return
	}
	result.Found = true
	result.Summary = &requestTraceSummary{
		RequestId: entry.RequestId, StartedAtUS: entry.StartedAtUS, FinishedAtUS: entry.FinishedAtUS,
		DurationUS: entry.DurationUS, FirstPayloadWriteOffsetUS: entry.FirstPayloadWriteOffsetUS,
		HTTPStatus: entry.HTTPStatus, Outcome: entry.Outcome, IsStream: entry.IsStream,
		CaptureStatus: entry.CaptureStatus, SchemaVersion: entry.SchemaVersion,
	}
	if admin {
		result.Detail = &requestTraceDetail{AttemptCount: entry.AttemptCount, FinalChannelId: entry.FinalChannelId,
			FinalAttemptFirstByteOffsetUS: entry.FinalAttemptFirstByteOffsetUS, NodeId: entry.NodeId, Status: "available"}
		if entry.SchemaVersion != requesttrace.SchemaVersion {
			result.Detail.Status = "unsupported_version"
		} else {
			var data requesttrace.Data
			raw, err := service.DecodeRequestTraceData(entry.TraceData)
			if err != nil {
				result.Detail.Status = "invalid_data"
				common.SysError("request trace decoding failed (request_id=" + entry.RequestId + "): " + err.Error())
			} else if err := common.Unmarshal(raw, &data); err != nil || data.Phases == nil || data.Attempts == nil || data.Errors == nil || data.Capture.Missing == nil {
				result.Detail.Status = "invalid_data"
			} else {
				result.Detail.Timeline = &data
			}
		}
	}
	common.ApiSuccess(c, result)
}

func GetRequestTraceStatus(c *gin.Context) {
	common.ApiSuccess(c, service.GetRequestTraceWriterStatus())
}
