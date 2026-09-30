package openai

import (
	"encoding/json"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	providerwire "github.com/fwtllh-png/QCode/internal/adapter/provider/wire"
)

func TestResponsesProjectionComparesEveryWireRequestProperty(t *testing.T) {
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	base := incrementalRequest(t, "https://api.openai.test")
	base.Projection = provider.ProjectionContext{
		ContextRevision: 3, WindowID: "window_1", WindowNumber: 1,
	}
	call, err := adapter.Prepare(base)
	if err != nil {
		t.Fatal(err)
	}
	baseline := call.Projection
	boolPointer := func(value bool) *bool { return &value }
	floatPointer := func(value float64) *float64 { return &value }
	tests := []struct {
		name   string
		mutate func(*provider.ModelRequest)
	}{
		{"max_output_tokens", func(value *provider.ModelRequest) { value.MaxOutputTokens++ }},
		{"temperature", func(value *provider.ModelRequest) { value.Temperature = floatPointer(0.2) }},
		{"reasoning_effort", func(value *provider.ModelRequest) { value.ReasoningEffort = "high" }},
		{"native_search", func(value *provider.ModelRequest) { value.NativeSearch = true }},
		{"tools", func(value *provider.ModelRequest) {
			value.Tools = []provider.ToolDefinition{{
				Name: "read", Description: "Read a file",
				InputSchema: map[string]any{"type": "object"},
			}}
		}},
		{"prompt_cache_key", func(value *provider.ModelRequest) { value.PromptCacheKey = "other" }},
		{"store", func(value *provider.ModelRequest) { value.Store = boolPointer(true) }},
		{"parallel_tools", func(value *provider.ModelRequest) { value.ParallelTools = boolPointer(false) }},
		{"include", func(value *provider.ModelRequest) { value.Include = []string{"reasoning.encrypted_content"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			changed, err := adapter.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Projection.PropertyDigest == baseline.PropertyDigest {
				t.Fatalf(
					"property digest unchanged: baseline=%+v changed=%+v",
					baseline,
					changed.Projection,
				)
			}
		})
	}
	history := base
	history.Messages = append(
		history.Messages,
		provider.TextMessage(provider.RoleUser, "next"),
	)
	changed, err := adapter.Prepare(history)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Projection.PropertyDigest != baseline.PropertyDigest ||
		changed.Projection.InputDigest == baseline.InputDigest {
		t.Fatalf(
			"history/property partition drift: baseline=%+v changed=%+v",
			baseline,
			changed.Projection,
		)
	}
}

func TestResponsesProjectionReportsEveryContinuityFallback(t *testing.T) {
	request := incrementalRequest(t, "https://api.openai.test")
	request.Projection = provider.ProjectionContext{
		ContextRevision: 4, WindowID: "window_1", WindowNumber: 1,
	}
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)}
	extended := append(
		append([]json.RawMessage(nil), prefix...),
		json.RawMessage(`{"role":"user","content":"next"}`),
	)
	base := responsesSession{
		previous: "resp-1", property: call.Projection.PropertyDigest,
		routeDigest: call.Projection.RouteDigest,
		windowID:    request.Projection.WindowID,
		prefix:      prefix,
	}
	tests := []struct {
		name     string
		reason   provider.ProjectionFallbackReason
		mutate   func(*responsesSession, *provider.ModelRequest, *providerwire.PreparedCall)
		input    []json.RawMessage
		property string
	}{
		{
			name: "retry", reason: provider.ProjectionFallbackRetry,
			mutate: func(_ *responsesSession, request *provider.ModelRequest, _ *providerwire.PreparedCall) {
				request.Projection.Retry = true
			},
		},
		{
			name: "resume", reason: provider.ProjectionFallbackResume,
			mutate: func(_ *responsesSession, request *provider.ModelRequest, _ *providerwire.PreparedCall) {
				request.Projection.RecoveryID = "continue\x00turn_1"
			},
		},
		{
			name: "route", reason: provider.ProjectionFallbackRouteChanged,
			mutate: func(_ *responsesSession, _ *provider.ModelRequest, call *providerwire.PreparedCall) {
				call.Projection.RouteDigest = "changed"
			},
		},
		{
			name: "compaction", reason: provider.ProjectionFallbackCompaction,
			mutate: func(_ *responsesSession, request *provider.ModelRequest, _ *providerwire.PreparedCall) {
				request.Projection.WindowID = "window_2"
				request.Projection.WindowNumber = 2
			},
		},
		{
			name: "properties", reason: provider.ProjectionFallbackPropertyChanged,
			property: "changed",
		},
		{
			name: "history", reason: provider.ProjectionFallbackHistoryRebased,
			input: []json.RawMessage{json.RawMessage(`{"role":"user","content":"rebased"}`)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := responsesSession{
				previous:    base.previous,
				property:    base.property,
				routeDigest: base.routeDigest,
				windowID:    base.windowID,
				recoveryID:  base.recoveryID,
				prefix:      append([]json.RawMessage(nil), base.prefix...),
			}
			currentRequest, currentCall := request, call
			if test.mutate != nil {
				test.mutate(&session, &currentRequest, &currentCall)
			}
			input := extended
			if test.input != nil {
				input = test.input
			}
			property := call.Projection.PropertyDigest
			if test.property != "" {
				property = test.property
			}
			receipt := evaluateProjection(
				&session,
				currentRequest,
				currentCall,
				input,
				property,
			)
			if receipt.IncrementalEligible ||
				receipt.FallbackReason != test.reason {
				t.Fatalf("receipt=%+v", receipt)
			}
		})
	}
	eligible := evaluateProjection(
		&base,
		request,
		call,
		extended,
		call.Projection.PropertyDigest,
	)
	if !eligible.IncrementalEligible ||
		eligible.FallbackReason != "" ||
		eligible.StablePrefixDigest == "" {
		t.Fatalf("eligible receipt=%+v", eligible)
	}
}

func TestResponsesProjectionRetryOwnsForcedHTTPFallback(t *testing.T) {
	request := incrementalRequest(t, "https://api.openai.test")
	request.Projection.Retry = true
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	session := adapter.session(request)
	session.forceHTTP = true
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if call.Projection.Mode != provider.ProjectionModeFullHTTP ||
		call.Projection.FallbackReason != provider.ProjectionFallbackRetry ||
		session.forceHTTP {
		t.Fatalf(
			"projection=%+v force_http=%t",
			call.Projection,
			session.forceHTTP,
		)
	}
}

func TestResponsesStablePrefixReuseAfterSampleThreeExceeds95Percent(t *testing.T) {
	request := incrementalRequest(t, "https://api.openai.test")
	request.Projection.WindowID = "window_1"
	adapter, err := NewAdapter(model.AdapterOpenAI)
	if err != nil {
		t.Fatal(err)
	}
	call, err := adapter.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]json.RawMessage, 100)
	for index := range prefix {
		encoded, err := json.Marshal(map[string]any{
			"type": "message", "role": "user", "content": index,
		})
		if err != nil {
			t.Fatal(err)
		}
		prefix[index] = encoded
	}
	logical := append(
		append([]json.RawMessage(nil), prefix...),
		json.RawMessage(`{"type":"message","role":"user","content":"delta"}`),
	)
	session := responsesSession{
		previous: "resp-3", property: call.Projection.PropertyDigest,
		routeDigest: call.Projection.RouteDigest,
		windowID:    request.Projection.WindowID,
		prefix:      prefix,
	}
	receipt := evaluateProjection(
		&session,
		request,
		call,
		logical,
		call.Projection.PropertyDigest,
	)
	transport, receipt, err := projectTransportInput(
		&session,
		logical,
		receipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	reuseBasisPoints := (len(logical) - len(transport)) * 10_000 /
		len(logical)
	if !receipt.IncrementalEligible ||
		!receipt.LogicalTransportEquivalent ||
		reuseBasisPoints < 9_500 {
		t.Fatalf(
			"receipt=%+v logical=%d transport=%d reuse_bps=%d",
			receipt,
			len(logical),
			len(transport),
			reuseBasisPoints,
		)
	}
}
