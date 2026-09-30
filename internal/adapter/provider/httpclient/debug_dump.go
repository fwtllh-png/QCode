package httpclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func providerDumpEnabled(status int) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QCODE_PROVIDER_DUMP"))) {
	case "always", "all", "1", "true":
		return true
	case "error", "errors":
		return status >= 400
	default:
		return false
	}
}
func writeProviderDump(
	request provider.ModelRequest,
	body []byte,
	path string,
	status int,
	errorText string,
) (string, error) {
	dir := strings.TrimSpace(os.Getenv("QCODE_DEBUG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			dir = filepath.Join(home, ".qcode", "debug")
		} else {
			dir = filepath.Join(os.TempDir(), "qcode-debug")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	filePath := filepath.Join(
		dir, fmt.Sprintf("provider-%d-%s.json", status, now.Format("20060102-150405")),
	)
	dump := struct {
		Time         string           `json:"time"`
		Error        string           `json:"error"`
		Protocol     string           `json:"protocol"`
		Model        string           `json:"model"`
		Provider     string           `json:"provider"`
		Path         string           `json:"request_path"`
		HowToShare   string           `json:"how_to_share"`
		DumpHint     string           `json:"dump_hint"`
		Status       int              `json:"status"`
		Messages     []messageSummary `json:"messages_summary"`
		EncodedInput []inputSummary   `json:"encoded_input_summary"`
	}{
		Time: now.Format(time.RFC3339), Status: status,
		Error: truncate(errorText, 2<<10), Protocol: string(request.Route.Protocol()),
		Model: request.Route.Model().ID, Provider: request.Route.ProviderID(), Path: path,
		Messages:     summarizeMessages(request.Messages),
		EncodedInput: summarizeEncodedBody(body, request.Route.Protocol()),
		HowToShare:   "Review this diagnostic before attaching it to a provider error report.",
		DumpHint:     "QCODE_PROVIDER_DUMP=error|always  QCODE_DEBUG_DIR=/path",
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return "", err
	}
	return filePath, nil
}

type messageSummary struct {
	Index         int            `json:"index"`
	Role          string         `json:"role"`
	Turn          uint64         `json:"turn,omitempty"`
	Adapter       string         `json:"adapter,omitempty"`
	HasReplay     bool           `json:"has_replay,omitempty"`
	ReplayDataLen int            `json:"replay_data_len,omitempty"`
	Blocks        []blockSummary `json:"blocks"`
}
type blockSummary struct {
	Type       string `json:"type"`
	TextLen    int    `json:"text_len,omitempty"`
	ID         string `json:"id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}
type inputSummary struct {
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Role     string `json:"role,omitempty"`
	ID       string `json:"id,omitempty"`
	CallID   string `json:"call_id,omitempty"`
	Name     string `json:"name,omitempty"`
	TextLen  int    `json:"text_len,omitempty"`
	ContentN int    `json:"content_parts,omitempty"`
}

func summarizeMessages(messages []provider.Message) []messageSummary {
	out := make([]messageSummary, 0, len(messages))
	for index, message := range messages {
		summary := messageSummary{
			Index: index, Role: string(message.Role), Turn: message.Turn,
			Blocks: make([]blockSummary, 0, len(message.Blocks)),
		}
		if message.Provenance != nil {
			summary.Adapter = string(message.Provenance.Adapter)
			summary.HasReplay = message.Provenance.Replay != nil
			if message.Provenance.Replay != nil {
				summary.ReplayDataLen = len(message.Provenance.Replay.Data)
			}
		}
		for _, block := range message.Blocks {
			item := blockSummary{
				Type: string(block.Type), TextLen: len(block.Text), ID: block.ID,
			}
			if block.ToolCall != nil {
				item.ToolName, item.ToolCallID = block.ToolCall.Name, block.ToolCall.ID
			}
			if block.ToolResult != nil {
				item.ToolCallID = block.ToolResult.CallID
			}
			summary.Blocks = append(summary.Blocks, item)
		}
		out = append(out, summary)
	}
	return out
}
func summarizeEncodedBody(body []byte, wire model.WireProtocol) []inputSummary {
	if len(body) == 0 || wire != model.ProtocolOpenAIResponses {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	items, _ := payload["input"].([]any)
	out := make([]inputSummary, 0, len(items))
	for index, raw := range items {
		item, _ := raw.(map[string]any)
		summary := inputSummary{
			Index: index, Type: stringValue(item["type"]), Role: stringValue(item["role"]),
			ID: stringValue(item["id"]), CallID: stringValue(item["call_id"]),
			Name: stringValue(item["name"]),
		}
		if summary.Type == "" && summary.Role != "" {
			summary.Type = "message"
		}
		switch content := item["content"].(type) {
		case string:
			summary.TextLen = len(content)
		case []any:
			summary.ContentN = len(content)
			for _, part := range content {
				value, _ := part.(map[string]any)
				summary.TextLen += len(stringValue(value["text"]))
			}
		}
		out = append(out, summary)
	}
	return out
}
func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
