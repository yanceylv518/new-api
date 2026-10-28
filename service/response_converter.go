package service

import (
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

func ConvertResponse(c *gin.Context, info *relaycommon.RelayInfo, target types.RelayFormat, response any) (*relayconvert.ResponseResult, error) {
	result, err := relayconvert.ConvertResponse(c, info, target, response)
	if result != nil {
		restoreClientClaudeModel(info, result)
		info.RecordConversionDiagnostics(c, result.Diagnostics)
	}
	return result, err
}

func ConvertStreamResponse(c *gin.Context, info *relaycommon.RelayInfo, target types.RelayFormat, response any) (*relayconvert.ResponseResult, error) {
	result, err := relayconvert.ConvertStreamResponse(c, info, target, response)
	if result != nil {
		restoreClientClaudeModel(info, result)
		info.RecordConversionDiagnostics(c, result.Diagnostics)
	}
	return result, err
}

func ConvertStreamResponseChunk(c *gin.Context, info *relaycommon.RelayInfo, state *relayconvert.ResponseStreamState, response any) ([]relayconvert.ResponseResult, error) {
	results, err := relayconvert.ConvertStreamResponseChunk(c, info, state, response)
	for index := range results {
		restoreClientClaudeModel(info, &results[index])
	}
	if state != nil {
		info.RecordConversionDiagnostics(c, state.Diagnostics())
	}
	return results, err
}

func FinalizeStreamResponse(c *gin.Context, info *relaycommon.RelayInfo, state *relayconvert.ResponseStreamState) ([]relayconvert.ResponseResult, error) {
	results, err := relayconvert.FinalizeStreamResponse(c, info, state)
	for index := range results {
		restoreClientClaudeModel(info, &results[index])
	}
	if state != nil {
		info.RecordConversionDiagnostics(c, state.Diagnostics())
	}
	return results, err
}

func restoreClientClaudeModel(info *relaycommon.RelayInfo, result *relayconvert.ResponseResult) {
	if info == nil || info.RelayFormat != types.RelayFormatClaude || info.OriginModelName == "" || result == nil || result.To != types.RelayFormatClaude {
		return
	}
	switch response := result.Value.(type) {
	case *dto.ClaudeResponse:
		result.Value = relaycommon.ClientClaudeResponse(response, info.OriginModelName)
	case dto.ClaudeResponse:
		if client := relaycommon.ClientClaudeResponse(&response, info.OriginModelName); client != &response {
			result.Value = *client
		}
	case []*dto.ClaudeResponse:
		var clients []*dto.ClaudeResponse
		for index, entry := range response {
			client := relaycommon.ClientClaudeResponse(entry, info.OriginModelName)
			if client == entry {
				continue
			}
			if clients == nil {
				clients = append([]*dto.ClaudeResponse(nil), response...)
			}
			clients[index] = client
		}
		if clients != nil {
			result.Value = clients
		}
	}
}
