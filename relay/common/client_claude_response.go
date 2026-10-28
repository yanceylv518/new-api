package common

import (
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/tidwall/sjson"
)

// ClientClaudeResponse 仅在改名时复制响应，保留上游观测和计费仍引用的原始 DTO。
func ClientClaudeResponse(response *dto.ClaudeResponse, model string) *dto.ClaudeResponse {
	if response == nil || model == "" {
		return response
	}
	changeModel := response.Model != "" && response.Model != model
	changeMessage := response.Message != nil && response.Message.Model != "" && response.Message.Model != model
	if !changeModel && !changeMessage {
		return response
	}
	client := *response
	if changeModel {
		client.Model = model
	}
	if changeMessage {
		message := *response.Message
		message.Model = model
		client.Message = &message
	}
	return &client
}

// ClientClaudeResponseData 保留原始 JSON 的扩展字段，使写出的字节与客户端 DTO 一致。
func ClientClaudeResponseData(response *dto.ClaudeResponse, model string, data []byte) (*dto.ClaudeResponse, []byte, error) {
	client := ClientClaudeResponse(response, model)
	if client == response {
		return response, data, nil
	}
	var err error
	if client.Model != response.Model {
		data, err = sjson.SetBytes(data, "model", model)
		if err != nil {
			return nil, nil, err
		}
	}
	if client.Message != response.Message {
		data, err = sjson.SetBytes(data, "message.model", model)
		if err != nil {
			return nil, nil, err
		}
	}
	return client, data, nil
}
