package turnhistory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestTurnHistoryLookupErrorAndEmptyTranscript(t *testing.T) {
	wantErr := errors.New("archive unavailable")
	registry := tool.NewRegistry(nil, nil)
	if err := Register(registry, func(_ context.Context, turn uint64) (*Entry, error) {
		if turn == 1 {
			return nil, wantErr
		}
		return &Entry{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, _, executor, err := registry.Resolve(Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(t.Context(), []byte(`{"turn":1}`)); !errors.Is(err, wantErr) {
		t.Fatalf("lookup error = %v, want %v", err, wantErr)
	}
	if result, err := executor.Execute(t.Context(), []byte(`{"turn":2}`)); err != nil || result.Content != "" {
		t.Fatalf("existing turn with empty transcript = %+v, %v", result, err)
	}
}

func TestAssemblePageUTF8AndFindingsBudget(t *testing.T) {
	for _, test := range []struct {
		name, transcript, index, from string
		limit                         int
		want                          string
	}{
		{name: "head", transcript: "甲乙丙", from: "head", limit: 4, want: "甲"},
		{name: "tail", transcript: "甲乙丙", limit: 4, want: "丙"},
		{name: "head sub-rune", transcript: "甲乙丙", from: "head", limit: 1},
		{name: "tail sub-rune", transcript: "甲乙丙", limit: 1},
		{name: "head with index", transcript: "甲乙丙", index: "sites", from: "head", limit: 11, want: "甲\n\nsites\n"},
		{name: "tail with index", transcript: "甲乙丙", index: "sites", limit: 11, want: "丙\n\nsites\n"},
		{name: "oversized index head", transcript: "甲乙丙", index: "sites", from: "head", limit: 1, want: "\n"},
		{name: "oversized index tail", transcript: "甲乙丙", index: "sites", limit: 1, want: "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, truncated, original := assemblePage(test.transcript, test.index, test.from, test.limit)
			wantOriginal := len(test.transcript)
			if test.index != "" {
				wantOriginal += len(test.index) + 3
			}
			if got != test.want || len(got) > test.limit || !utf8.ValidString(got) || !truncated || original != wantOriginal {
				t.Fatalf("page = %q, truncated=%v, original=%d; want %q, true, %d", got, truncated, original, test.want, wantOriginal)
			}
		})
	}
}

func TestTurnHistoryReadsBoundedTurnAndIsIdempotentRegister(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	lookup := func(_ context.Context, turn uint64) (*Entry, error) {
		if turn != 2 {
			return nil, nil
		}
		return &Entry{Transcript: "[turn 2 user] " + strings.Repeat("explore ", 40) + "P2: missing overflow test\n"}, nil
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
	lookup := func(_ context.Context, turn uint64) (*Entry, error) {
		return &Entry{
			Transcript: "[turn 7 user] audit the parser " + strings.Repeat("explore ", 80) +
				"\n[turn 7 assistant] summary without paths\n",
			FindingsIndex: "[turn 7 findings]\nconclusion: hasGlobalLock is the root cause\nsites: eds_metaserver.cc:88 hasGlobalLock\n",
		}, nil
	}
	if err := Register(registry, lookup); err != nil {
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
	lookup := func(_ context.Context, turn uint64) (*Entry, error) {
		return &Entry{Transcript: "[turn 1 user] audit the parser " + strings.Repeat("explore ", 80) +
			"\n[turn 1 assistant] five P2s: missing overflow test\n"}, nil
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
