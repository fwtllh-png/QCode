package turnhistory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestTurnHistoryReadsBoundedTurnAndIsIdempotentRegister(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	lookup := func(_ context.Context, turn uint64) ([]provider.Message, error) {
		if turn != 2 {
			return nil, nil
		}
		return []provider.Message{{
			Role: provider.RoleUser, Turn: 2,
			Blocks: []provider.ContentBlock{{
				Type: provider.ContentText,
				Text: strings.Repeat("explore ", 40) + "P2: missing overflow test",
			}},
		}}, nil
	}
	if err := Register(registry, lookup); err != nil {
		t.Fatal(err)
	}
	if err := Register(registry, lookup); err != nil {
		t.Fatal(err)
	}
	_, _, executor, err := registry.Resolve(Name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(input{Turn: 2, MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Content) > 64 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "P2:") {
		t.Fatalf("content = %q", result.Content)
	}
	if _, err := executor.Execute(t.Context(), []byte(`{"turn":1}`)); err == nil {
		t.Fatal("missing turn succeeded")
	}
}

func TestTurnHistoryFirstPageKeepsFindingsAfterTail(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	lookup := func(_ context.Context, turn uint64) ([]provider.Message, error) {
		return []provider.Message{
			{
				Role: provider.RoleUser, Turn: 7,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentText,
					Text: "audit the parser " + strings.Repeat("explore ", 80),
				}},
			},
			{
				Role: provider.RoleAssistant, Turn: 7,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentText,
					Text: "summary without paths",
				}},
			},
		}, nil
	}
	findings := func(_ context.Context, turn uint64) (agentcontext.TurnFindings, bool) {
		return agentcontext.TurnFindings{
			Conclusion: "hasGlobalLock is the root cause",
			Sites:      []string{"eds_metaserver.cc:88 hasGlobalLock"},
		}, turn == 7
	}
	if err := Register(registry, lookup, findings); err != nil {
		t.Fatal(err)
	}
	_, _, executor, err := registry.Resolve(Name)
	if err != nil {
		t.Fatal(err)
	}
	page, err := executor.Execute(t.Context(), []byte(`{"turn":7,"max_bytes":160}`))
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated ||
		!strings.Contains(page.Content, "hasGlobalLock is the root cause") ||
		!strings.Contains(page.Content, "eds_metaserver.cc:88 hasGlobalLock") ||
		strings.Contains(page.Content, "audit the parser") {
		t.Fatalf("findings page = %+v", page)
	}
}

func TestTurnHistoryFirstPagePrefersTailConclusions(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	lookup := func(_ context.Context, turn uint64) ([]provider.Message, error) {
		return []provider.Message{
			{
				Role: provider.RoleUser, Turn: 1,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentText,
					Text: "audit the parser " + strings.Repeat("explore ", 80),
				}},
			},
			{
				Role: provider.RoleAssistant, Turn: 1,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentText,
					Text: "five P2s: missing overflow test",
				}},
			},
		}, nil
	}
	if err := Register(registry, lookup); err != nil {
		t.Fatal(err)
	}
	_, _, executor, err := registry.Resolve(Name)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := executor.Execute(t.Context(), []byte(`{"turn":1,"max_bytes":80}`))
	if err != nil {
		t.Fatal(err)
	}
	if !tail.Truncated ||
		!strings.Contains(tail.Content, "five P2s: missing overflow test") ||
		strings.Contains(tail.Content, "audit the parser") {
		t.Fatalf("tail page = %+v", tail)
	}
	head, err := executor.Execute(t.Context(), []byte(`{"turn":1,"from":"head","max_bytes":80}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(head.Content, "audit the parser") ||
		strings.Contains(head.Content, "five P2s") {
		t.Fatalf("head page = %+v", head)
	}
}
