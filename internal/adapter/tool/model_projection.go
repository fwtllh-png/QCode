package tool

import (
	"encoding/json"
	"errors"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func ProjectModelResults(
	calls []provider.ToolCall,
	results []Result,
	turn uint64,
) ([]provider.Message, error) {
	if len(calls) != len(results) {
		return nil, errors.New("tool call and result counts differ")
	}
	messages := make([]provider.Message, 0, len(calls))
	var attachments []provider.Attachment
	for index, call := range calls {
		data, err := json.Marshal(ModelResult(call.Name, results[index]))
		if err != nil {
			return nil, err
		}
		messages = append(messages, provider.Message{
			Role: provider.RoleTool,
			Turn: turn,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentToolResult,
				ToolResult: &provider.ToolResult{
					CallID:  call.ID,
					Content: string(data),
					IsError: results[index].IsError,
					Admission: provider.CloneAdmissionReceipt(
						results[index].Admission,
					),
				},
			}},
		})
		attachments = append(attachments, results[index].Attachments...)
	}
	if len(attachments) != 0 {
		blocks := make([]provider.ContentBlock, 0, len(attachments))
		for index := range attachments {
			attachment := attachments[index]
			blocks = append(blocks, provider.ContentBlock{
				Type: provider.ContentImage, Attachment: &attachment,
			})
		}
		messages = append(messages, provider.Message{
			Role: provider.RoleUser, Turn: turn, Blocks: blocks,
		})
	}
	return messages, nil
}
