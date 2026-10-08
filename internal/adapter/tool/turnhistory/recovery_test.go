package turnhistory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
)

func TestTurnHistoryP4PagesCompleteSourcesAndRejectsChangedDigest(t *testing.T) {
	registry := tool.NewRegistry(nil, nil)
	body := strings.Repeat("第一项定义\n", 20) + "2. 必须完整保留的尾部问题\n"
	err := Register(registry, func(context.Context, uint64) (*Entry, error) {
		return &Entry{Transcript: body, FindingsIndex: "索引：第二项在尾部"}, nil
	}, func(_ context.Context, request ReferenceRequest) (*Entry, error) {
		if request.ItemID != "item:2" {
			return nil, nil
		}
		return &Entry{Transcript: body}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, executor, err := registry.Resolve(Name)
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt strings.Builder
	request := input{ReferenceRequest: ReferenceRequest{ItemID: "item:2"}, MaxBytes: 31}
	var digest string
	for {
		raw, _ := json.Marshal(request)
		result, err := executor.Execute(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Content) > request.MaxBytes || !utf8.ValidString(result.Content) {
			t.Fatal("unbounded or invalid UTF-8 page")
		}
		rebuilt.WriteString(result.Content)
		digest = result.Metadata["content_digest"].(string)
		next, more := result.Metadata["next_offset"].(int)
		if !more {
			break
		}
		if request.Offset != nil && next <= *request.Offset {
			t.Fatal("paging made no progress")
		}
		request.Offset = &next
		request.Digest = digest
	}
	if rebuilt.String() != body {
		t.Fatal("paged recovery lost source bytes")
	}
	body += "changed"
	raw, _ := json.Marshal(request)
	if _, err := executor.Execute(t.Context(), raw); err == nil {
		t.Fatal("stale digest accepted")
	}
	for _, raw := range []string{`{}`, `{"turn":1,"item_id":"item:2"}`, `{"catalog":false}`, `{"item_id":"item:2","index_only":true}`, `{"item_id":"item:2","offset":1}`, `{"turn":1,"preferred_turn":2}`} {
		if _, err := executor.Execute(t.Context(), []byte(raw)); err == nil {
			t.Fatalf("invalid selector accepted: %s", raw)
		}
	}
}

func TestTurnHistoryP4LongFindingsRespectCap(t *testing.T) {
	entry := &Entry{Transcript: strings.Repeat("old output ", 100), FindingsIndex: strings.Repeat("索引", 100)}
	for _, from := range []string{"head", "tail"} {
		for _, limit := range []int{4, 17, 256} {
			result, err := referencePage(entry, input{Turn: 1, From: from, MaxBytes: limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Content) > limit || !result.Truncated || !utf8.ValidString(result.Content) {
				t.Fatalf("invalid bounded page: %+v", result)
			}
		}
	}
}

func TestTurnHistoryP4RejectsSubRunePagesAndInvalidOffsets(t *testing.T) {
	entry := &Entry{Transcript: "甲乙"}
	for _, from := range []string{"head", "tail"} {
		if _, err := referencePage(entry, input{Turn: 1, From: from, MaxBytes: 2}); err == nil {
			t.Fatalf("%s returned an empty nonterminal page", from)
		}
	}
	page, err := referencePage(entry, input{Turn: 1, From: "head", MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{1, 7} {
		if _, err := referencePage(entry, input{Turn: 1, Offset: &offset, Digest: page.Metadata["content_digest"].(string), MaxBytes: 3}); err == nil {
			t.Fatalf("invalid offset %d accepted", offset)
		}
	}
}
