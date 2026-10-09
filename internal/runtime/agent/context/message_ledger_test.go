package agentcontext

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestSnapshotPlacesBackgroundBeforeCurrentTurnAndPreservesFeedback(t *testing.T) {
	old := provider.TextMessage(provider.RoleUser, "old request")
	old.Turn = 1
	current := provider.TextMessage(provider.RoleUser, "current request")
	current.Turn = 2
	latest := provider.TextMessage(provider.RoleAssistant, "latest progress")
	latest.Turn = 2
	background := []provider.Message{provider.TextMessage(provider.RoleSystem, "checkpoint")}
	feedback := []provider.Message{provider.TextMessage(provider.RoleUser, "continue incomplete output")}
	ledger := NewMessageLedger(LedgerInput{
		Stable:  []provider.Message{provider.TextMessage(provider.RoleSystem, "policy")},
		History: []provider.Message{old, current, latest}, Dynamic: background,
		Continuation: feedback, DynamicBeforeTurn: 2,
	})
	initial := ledger.Snapshot()
	check := func(snapshot MessageSnapshot, want []string) {
		t.Helper()
		messages, items, refs := snapshot.Messages(), snapshot.Items(), snapshot.ItemRefs()
		var got []string
		for i, message := range messages {
			got = append(got, message.Text())
			if !reflect.DeepEqual(message, items[i].Message) || items[i].ID != refs[i].ID {
				t.Fatal("message order differs from item/prefix order")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("order=%v want=%v", got, want)
		}
		encoded, err := json.Marshal(struct {
			Messages []provider.Message `json:"messages"`
		}{messages})
		if err != nil {
			t.Fatal(err)
		}
		digest, err := snapshot.Digest()
		if err != nil || digest != digestString(string(encoded)) {
			t.Fatalf("digest does not bind the physical message order: %s %v", digest, err)
		}
	}
	want := []string{"policy", "old request", "checkpoint", "current request", "latest progress", "continue incomplete output"}
	check(initial, want)
	normalized, _, err := initial.Normalize(model.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	check(normalized, want)
	// Omitting an earlier turn changes the insertion index, not the anchor.
	folded := normalized.WithHistory([]provider.Message{current, latest})
	check(folded, []string{"policy", "checkpoint", "current request", "latest progress", "continue incomplete output"})
	replaced := folded.WithDynamic([]provider.Message{provider.TextMessage(provider.RoleSystem, "updated checkpoint")})
	check(replaced, []string{"policy", "updated checkpoint", "current request", "latest progress", "continue incomplete output"})
	check(initial, want)
	// If the current request is absent, background stays before the retained
	// history rather than splitting a surviving tool group at a stale offset.
	check(initial.WithHistory([]provider.Message{old}), []string{"policy", "checkpoint", "old request", "continue incomplete output"})
	check(initial.WithHistory(nil), []string{"policy", "checkpoint", "continue incomplete output"})
}

func TestLedgerProjectsOneOrderedImmutableSnapshot(t *testing.T) {
	stable := []provider.Message{provider.TextMessage(provider.RoleSystem, "stable")}
	history := []provider.Message{provider.TextMessage(provider.RoleUser, "history")}
	dynamic := []provider.Message{provider.TextMessage(provider.RoleSystem, "dynamic")}
	continuation := []provider.Message{
		provider.TextMessage(provider.RoleAssistant, "continuation"),
	}
	definitions := []provider.ToolDefinition{{
		Name: "read", Description: "read a file",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		},
	}}
	ledger := NewMessageLedger(LedgerInput{
		Stable: stable, History: history, Dynamic: dynamic,
		Continuation: continuation, Definitions: definitions,
	})
	initial := ledger.Snapshot()
	if initial.Revision() != 1 {
		t.Fatalf("initial revision=%d", initial.Revision())
	}
	messages := initial.Messages()
	if got := []string{
		messages[0].Text(), messages[1].Text(),
		messages[2].Text(), messages[3].Text(),
	}; !reflect.DeepEqual(got, []string{
		"stable", "history", "dynamic", "continuation",
	}) {
		t.Fatalf("message order=%v", got)
	}
	items := initial.Items()
	if got := []MessageKind{items[0].Kind, items[1].Kind, items[2].Kind, items[3].Kind}; !reflect.DeepEqual(got, orderedKinds[:]) {
		t.Fatalf("item kinds=%v", got)
	}
	ids := []string{items[0].ID, items[1].ID, items[2].ID, items[3].ID}

	unchanged := ledger.Project(LedgerProjection{
		Stable: stable, History: history, Dynamic: dynamic,
		Continuation: continuation, Definitions: definitions,
	})
	if unchanged.Revision() != 1 {
		t.Fatalf("unchanged revision=%d", unchanged.Revision())
	}

	changedDynamic := []provider.Message{
		provider.TextMessage(provider.RoleSystem, "dynamic changed"),
	}
	changed := ledger.Project(LedgerProjection{
		Stable: stable, History: history, Dynamic: changedDynamic,
		Continuation: continuation, Definitions: definitions,
	})
	if changed.Revision() != 2 {
		t.Fatalf("changed revision=%d", changed.Revision())
	}
	changedItems := changed.Items()
	if changedItems[0].ID != ids[0] || changedItems[1].ID != ids[1] ||
		changedItems[2].ID == ids[2] || changedItems[3].ID != ids[3] {
		t.Fatalf("item identities changed unexpectedly: before=%v after=%v", ids, changedItems)
	}

	messages = changed.Messages()
	messages[0].Blocks[0].Text = "mutated"
	tools := changed.Definitions()
	tools[0].InputSchema["type"] = "mutated"
	next := ledger.Snapshot()
	if next.Messages()[0].Text() != "stable" ||
		next.Definitions()[0].InputSchema["type"] != "object" {
		t.Fatalf("snapshot mutation leaked: messages=%+v tools=%+v", next.Messages(), next.Definitions())
	}

	rewritten := changed.WithHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "compacted"),
	})
	if rewritten.Revision() != 3 ||
		rewritten.Partition(KindHistory)[0].Text() != "compacted" ||
		changed.Partition(KindHistory)[0].Text() != "history" {
		t.Fatalf("history rewrite changed source snapshot: old=%+v new=%+v", changed, rewritten)
	}
}

func TestSnapshotPreservesEmptySchemaArrays(t *testing.T) {
	snapshot := NewMessageLedger(LedgerInput{Definitions: []provider.ToolDefinition{{
		Name: "capabilities", Description: "probe capabilities",
		InputSchema: map[string]any{
			"type": "object", "required": []string{},
		},
	}}}).Snapshot()
	required, ok := snapshot.Definitions()[0].InputSchema["required"].([]string)
	if !ok || required == nil || len(required) != 0 {
		t.Fatalf("required = %#v, want non-nil empty array", required)
	}
	encoded, err := json.Marshal(snapshot.Definitions()[0].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"required":[],"type":"object"}` {
		t.Fatalf("schema = %s", encoded)
	}
}

func TestSnapshotMeasureAttributesExactlyTheProjectedRequest(t *testing.T) {
	estimate := func(messages []provider.Message) (uint64, error) {
		return uint64(len(messages) * 10), nil
	}
	snapshot := NewMessageLedger(LedgerInput{
		Stable: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "stable"),
		},
		History: []provider.Message{
			provider.TextMessage(provider.RoleUser, "question"),
			provider.TextMessage(provider.RoleAssistant, "answer"),
			provider.TextMessage(provider.RoleTool, "result"),
		},
		Dynamic: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "dynamic"),
		},
		Definitions: []provider.ToolDefinition{{
			Name: "read", Description: "read a file",
		}},
	}).Snapshot()
	got, err := snapshot.Measure("normal", "high", EstimatorFunc(estimate))
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != "normal" || got.ReasoningEffort != "high" ||
		got.ContextRevision != snapshot.Revision() || got.ContextDigest == "" ||
		got.StableTokens != 10 || got.HistoryUserTokens != 10 ||
		got.HistoryAssistantTokens != 10 || got.HistoryToolTokens != 10 ||
		got.DynamicTokens != 10 || got.ToolDefinitionTokens == 0 ||
		got.ProviderFramingTokens == 0 || got.EstimatedTokens == 0 ||
		got.MessageCount != len(snapshot.Messages()) {
		t.Fatalf("attribution=%+v", got)
	}
}

func TestSnapshotClonesAttachmentsAndReplayState(t *testing.T) {
	message := provider.Message{
		Role: provider.RoleAssistant,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentImage,
			Attachment: &provider.Attachment{
				MediaType: "image/png", Data: []byte("image"),
			},
		}},
		Provenance: &provider.AssistantProvenance{
			Adapter: "openai", Provider: "provider", Model: "model",
			Replay: &provider.ReplayState{Version: 1, Data: []byte(`{"id":"replay"}`)},
		},
	}
	resultMessage := provider.Message{
		Role: provider.RoleTool,
		Blocks: []provider.ContentBlock{{
			Type: provider.ContentToolResult,
			ToolResult: &provider.ToolResult{
				CallID: "call-1", Content: "bounded",
				Admission: &provider.AdmissionReceipt{
					Digest: "sha256:original", Handle: "result_original",
				},
			},
		}},
	}
	ledger := NewMessageLedger(LedgerInput{History: []provider.Message{message, resultMessage}})
	snapshot := ledger.Snapshot()
	projected := snapshot.Messages()
	projected[0].Blocks[0].Attachment.Data[0] = 'X'
	projected[0].Provenance.Replay.Data[0] = '['
	projected[1].Blocks[0].ToolResult.Admission.Handle = "mutated"
	again := ledger.Snapshot().Messages()[0]
	againResult := ledger.Snapshot().Messages()[1]
	if string(again.Blocks[0].Attachment.Data) != "image" ||
		string(again.Provenance.Replay.Data) != `{"id":"replay"}` ||
		againResult.Blocks[0].ToolResult.Admission.Handle != "result_original" {
		t.Fatalf(
			"nested model content was not cloned: %+v %+v",
			again,
			againResult,
		)
	}
}

func TestItemIdentitySurvivesUnrelatedHistoryPrefixRemoval(t *testing.T) {
	prefix := provider.TextMessage(provider.RoleUser, "remove me")
	prefix.Turn = 1
	retained := provider.TextMessage(provider.RoleAssistant, "retain me")
	retained.Turn = 2
	snapshot := NewMessageLedger(LedgerInput{History: []provider.Message{prefix, retained}}).Snapshot()
	before := snapshot.Items()[1].ID
	rewritten := snapshot.WithHistory([]provider.Message{retained})
	after := rewritten.Items()[0].ID
	if before != after {
		t.Fatalf("retained item identity changed: before=%s after=%s", before, after)
	}
}

func TestApplyTransportAddsWireReceiptWithoutChangingContextIdentity(t *testing.T) {
	context := &protocol.SampleContextData{
		ContextRevision: 3, ContextDigest: "sha256:context",
	}
	ApplyTransport(context, provider.TransportMetadata{
		RequestBytes: 42, LogicalRequestDigest: "logical",
		TransportPayloadDigest: "transport", Incremental: true,
		Projection: provider.ProjectionReceipt{
			Mode:                provider.ProjectionModeIncrementalSession,
			IncrementalEligible: true, StablePrefixDigest: "prefix",
			LogicalTransportEquivalent: true,
		},
	})
	if context.ContextRevision != 3 || context.ContextDigest != "sha256:context" ||
		context.RequestBytes != 42 || context.LogicalRequestDigest != "logical" ||
		context.TransportPayloadDigest != "transport" || !context.IncrementalTransport ||
		context.ProviderProjection == nil ||
		context.ProviderProjection.Mode != string(provider.ProjectionModeIncrementalSession) ||
		context.ProviderProjection.StablePrefixDigest != "prefix" ||
		!context.ProviderProjection.LogicalTransportEquivalent {
		t.Fatalf("transport receipt=%+v", context)
	}
}

func TestEmptyProjectionDoesNotAdvanceRevision(t *testing.T) {
	ledger := NewMessageLedger(LedgerInput{})
	if got := ledger.Project(LedgerProjection{}).Revision(); got != 1 {
		t.Fatalf("empty projection revision=%d", got)
	}
}

func TestDefinitionsDeterministicAcrossInputOrders(t *testing.T) {
	toolSet := []provider.ToolDefinition{
		{
			Name: "zeta", Description: "z tool",
			InputSchema: map[string]any{"type": "object"},
		},
		{
			Name: "alpha", Description: "a tool",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
			},
		},
		{Name: "mid", Description: "m tool"},
	}
	shuffled := []provider.ToolDefinition{toolSet[2], toolSet[0], toolSet[1]}

	fromBase := NewMessageLedger(LedgerInput{
		Definitions: toolSet,
	}).Snapshot().Definitions()
	fromShuffled := NewMessageLedger(LedgerInput{
		Definitions: shuffled,
	}).Snapshot().Definitions()

	// The projected definitions must not depend on caller-supplied order.
	if !reflect.DeepEqual(fromBase, fromShuffled) {
		t.Fatalf("definitions differ by input order: base=%+v shuffled=%+v", fromBase, fromShuffled)
	}
	// Output must be name-ascending.
	for index := 1; index < len(fromBase); index++ {
		if fromBase[index-1].Name > fromBase[index].Name {
			t.Fatalf("definitions not name-ascending at %d: %+v", index, fromBase)
		}
	}
	// Byte sequence must be identical for the same tool set.
	encodedBase, err := json.Marshal(fromBase)
	if err != nil {
		t.Fatal(err)
	}
	encodedShuffled, err := json.Marshal(fromShuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encodedBase, encodedShuffled) {
		t.Fatalf("definitions byte sequence differs: %s vs %s", encodedBase, encodedShuffled)
	}
}

func TestProjectIgnoresDefinitionInputOrder(t *testing.T) {
	first := []provider.ToolDefinition{{Name: "b"}, {Name: "a"}}
	reordered := []provider.ToolDefinition{{Name: "a"}, {Name: "b"}}
	ledger := NewMessageLedger(LedgerInput{Definitions: first})
	before := ledger.Snapshot().Revision()
	if got := ledger.Project(LedgerProjection{Definitions: reordered}).Revision(); got != before {
		t.Fatalf("order-only definitions change advanced revision: before=%d after=%d", before, got)
	}
	got := ledger.Snapshot().Definitions()
	if got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("projected definitions not canonical: %+v", got)
	}
}

func TestMessagesDeterministicByteSequence(t *testing.T) {
	build := func() MessageSnapshot {
		return NewMessageLedger(LedgerInput{
			Stable:       []provider.Message{provider.TextMessage(provider.RoleSystem, "stable")},
			History:      []provider.Message{provider.TextMessage(provider.RoleUser, "question")},
			Dynamic:      []provider.Message{provider.TextMessage(provider.RoleSystem, "dynamic")},
			Continuation: []provider.Message{provider.TextMessage(provider.RoleAssistant, "continue")},
			Definitions:  []provider.ToolDefinition{{Name: "b"}, {Name: "a"}},
		}).Snapshot()
	}
	first, err := json.Marshal(build().Messages())
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(build().Messages())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("messages byte sequence is not deterministic: %s vs %s", first, second)
	}
}

func TestDigestIndependentOfDefinitionInputOrder(t *testing.T) {
	ledgerA := NewMessageLedger(LedgerInput{
		Definitions: []provider.ToolDefinition{{Name: "zeta"}, {Name: "alpha"}},
	})
	ledgerB := NewMessageLedger(LedgerInput{
		Definitions: []provider.ToolDefinition{{Name: "alpha"}, {Name: "zeta"}},
	})
	digestA, err := ledgerA.Snapshot().Digest()
	if err != nil {
		t.Fatal(err)
	}
	digestB, err := ledgerB.Snapshot().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("digest differs by definition input order: %s vs %s", digestA, digestB)
	}
}
