package mcp

import (
	"context"
	"encoding/json"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
)

const traceMetadataKey = "qcode_trace"

func withTraceMetadata(
	ctx context.Context,
	params json.RawMessage,
) json.RawMessage {
	carrier := make(map[string]string, 2)
	if !tracecontext.InjectMap(ctx, carrier) {
		return params
	}
	values := make(map[string]json.RawMessage)
	if len(params) != 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &values); err != nil {
			return params
		}
	}
	metadata := make(map[string]json.RawMessage)
	if raw := values["_meta"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return params
		}
	}
	encodedTrace, err := json.Marshal(carrier)
	if err != nil {
		return params
	}
	metadata[traceMetadataKey] = encodedTrace
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return params
	}
	values["_meta"] = encodedMetadata
	encoded, err := json.Marshal(values)
	if err != nil {
		return params
	}
	return encoded
}
