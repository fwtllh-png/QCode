package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/common/tracecontext"
)

func TestTraceMetadataPreservesExistingMCPMetadata(t *testing.T) {
	ctx, err := tracecontext.NewRoot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := tracecontext.Current(ctx)
	encoded := withTraceMetadata(
		ctx,
		json.RawMessage(`{"value":"ok","_meta":{"existing":true}}`),
	)
	var params map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &params); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(params["_meta"], &metadata); err != nil {
		t.Fatal(err)
	}
	if string(metadata["existing"]) != "true" {
		t.Fatalf("metadata = %s", params["_meta"])
	}
	var carrier map[string]string
	if err := json.Unmarshal(metadata[traceMetadataKey], &carrier); err != nil {
		t.Fatal(err)
	}
	extracted, err := tracecontext.ExtractMap(context.Background(), carrier)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tracecontext.Current(extracted)
	if !ok || got.TraceID != want.TraceID || got.SpanID != want.SpanID {
		t.Fatalf("want=%+v got=%+v", want, got)
	}
}
