package agentcontext

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// The estimator is deliberately non-additive: a floor of one token per call
// and per-call ceiling rounding. MeasureDetailed must still reproduce the
// exact call structure the attribution contract was built on — per-message
// estimates for items and history roles, whole-slice estimates for the other
// partitions.
func nonAdditiveEstimator(messages []provider.Message) (uint64, error) {
	var base uint64
	for _, message := range messages {
		for _, block := range message.Blocks {
			base += uint64(len(block.Text))
		}
	}
	return uint64(max(1, int64(math.Ceil(float64(base)*1.5)))), nil
}

func TestMeasureDetailedMatchesLegacyCallStructure(t *testing.T) {
	estimate := EstimatorFunc(nonAdditiveEstimator)
	empty := provider.Message{Role: provider.RoleUser}
	snapshot := NewMessageLedger(LedgerInput{
		Stable: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "stable one"),
			provider.TextMessage(provider.RoleSystem, "stable two"),
		},
		History: []provider.Message{
			provider.TextMessage(provider.RoleUser, "question"),
			provider.TextMessage(provider.RoleAssistant, "answer"),
			provider.TextMessage(provider.RoleTool, "result"),
			provider.TextMessage(provider.RoleSystem, "misc"),
			empty,
		},
		Dynamic: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "dynamic"),
		},
		Continuation: []provider.Message{
			provider.TextMessage(provider.RoleAssistant, "partial"),
		},
		Definitions: []provider.ToolDefinition{{
			Name: "read", Description: "read a file",
		}},
	}).Snapshot()

	got, err := snapshot.MeasureDetailed("reason", "effort", estimate)
	if err != nil {
		t.Fatal(err)
	}

	// Reference: the pre-restructure algorithm with identical granularity.
	perMessage := func(messages ...provider.Message) uint64 {
		value, err := estimate.Estimate(messages)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	var wantMaxItem uint64
	for _, item := range snapshot.items {
		wantMaxItem = max(wantMaxItem, perMessage(item.Message))
	}
	var wantUser, wantAssistant, wantTool, wantOther uint64
	for _, message := range snapshot.partitions[KindHistory] {
		tokens := perMessage(message)
		switch message.Role {
		case provider.RoleUser:
			wantUser += tokens
		case provider.RoleAssistant:
			wantAssistant += tokens
		case provider.RoleTool:
			wantTool += tokens
		default:
			wantOther += tokens
		}
	}
	want := protocol.SampleContextData{
		Reason: "reason", ReasoningEffort: "effort",
		ContextRevision: snapshot.Revision(),
		MessageCount: len(snapshot.partitions[KindStable]) +
			len(snapshot.partitions[KindHistory]) +
			len(snapshot.partitions[KindDynamic]) +
			len(snapshot.partitions[KindContinuation]),
		ToolDefinitionCount:    1,
		MaxItemTokens:          wantMaxItem,
		StableTokens:           perMessage(snapshot.partitions[KindStable]...),
		HistoryUserTokens:      wantUser,
		HistoryAssistantTokens: wantAssistant,
		HistoryToolTokens:      wantTool,
		HistoryOtherTokens:     wantOther,
		DynamicTokens:          perMessage(snapshot.partitions[KindDynamic]...),
		ContinuationTokens:     perMessage(snapshot.partitions[KindContinuation]...),
	}
	if want.MaxItemTokens < tokenestimateText(t, snapshot.definitions[0]) {
		want.MaxItemTokens = tokenestimateText(t, snapshot.definitions[0])
	}
	definitionsJSON, marshalErr := json.Marshal(snapshot.definitions)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	want.ToolDefinitionTokens = tokenestimate.Text(string(definitionsJSON))
	want.EstimatedTokens = want.StableTokens + want.HistoryUserTokens +
		want.HistoryAssistantTokens + want.HistoryToolTokens +
		want.HistoryOtherTokens + want.DynamicTokens +
		want.ContinuationTokens + want.ToolDefinitionTokens
	want.ProviderFramingTokens = (want.EstimatedTokens*12 + 99) / 100
	want.EstimatedTokens += want.ProviderFramingTokens

	if got.Data.MaxItemTokens != want.MaxItemTokens ||
		got.Data.StableTokens != want.StableTokens ||
		got.Data.HistoryUserTokens != want.HistoryUserTokens ||
		got.Data.HistoryAssistantTokens != want.HistoryAssistantTokens ||
		got.Data.HistoryToolTokens != want.HistoryToolTokens ||
		got.Data.HistoryOtherTokens != want.HistoryOtherTokens ||
		got.Data.DynamicTokens != want.DynamicTokens ||
		got.Data.ContinuationTokens != want.ContinuationTokens ||
		got.Data.EstimatedTokens != want.EstimatedTokens ||
		got.Data.ProviderFramingTokens != want.ProviderFramingTokens ||
		got.Data.MessageCount != want.MessageCount {
		t.Fatalf(
			"attribution diverged from legacy call structure:\ngot  %+v\nwant %+v",
			got.Data, want,
		)
	}
	if len(got.ItemTokens) != len(snapshot.items) {
		t.Fatalf(
			"item tokens = %d entries, want %d",
			len(got.ItemTokens), len(snapshot.items),
		)
	}
	for index, item := range snapshot.items {
		if got.ItemTokens[index] != perMessage(item.Message) {
			t.Fatalf("item %d tokens = %d, want %d",
				index, got.ItemTokens[index], perMessage(item.Message))
		}
	}
}

// Digest must keep its exact encoding contract: partition order by kind and
// a nil message slice for the empty snapshot.
func TestSnapshotDigestEncodingIsPinned(t *testing.T) {
	empty, err := NewMessageLedger(LedgerInput{}).Snapshot().Digest()
	if err != nil {
		t.Fatal(err)
	}
	encodedEmpty, marshalErr := json.Marshal(struct {
		Messages    []provider.Message        `json:"messages"`
		Definitions []provider.ToolDefinition `json:"definitions,omitempty"`
	}{})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	sum := sha256.Sum256(encodedEmpty)
	if empty != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("empty digest = %s, want sha256 of %s", empty, encodedEmpty)
	}

	snapshot := NewMessageLedger(LedgerInput{
		Stable: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "stable"),
		},
		History: []provider.Message{
			provider.TextMessage(provider.RoleUser, "question"),
		},
		Definitions: []provider.ToolDefinition{{
			Name: "read", Description: "read a file",
		}},
	}).Snapshot()
	var flat []provider.Message
	for _, kind := range orderedKinds {
		flat = append(flat, snapshot.Partition(kind)...)
	}
	encoded, marshalErr := json.Marshal(struct {
		Messages    []provider.Message        `json:"messages"`
		Definitions []provider.ToolDefinition `json:"definitions,omitempty"`
	}{Messages: flat, Definitions: snapshot.Definitions()})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	want := "sha256:" + hex.EncodeToString(func() []byte {
		digest := sha256.Sum256(encoded)
		return digest[:]
	}())
	got, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest = %s, want %s (encoding %s)", got, want, encoded)
	}
}

func TestItemRefsAlignWithItems(t *testing.T) {
	snapshot := NewMessageLedger(LedgerInput{
		Stable: []provider.Message{
			provider.TextMessage(provider.RoleSystem, "stable"),
		},
		History: []provider.Message{
			provider.TextMessage(provider.RoleUser, "question"),
			provider.TextMessage(provider.RoleUser, "again"),
		},
	}).Snapshot()
	items := snapshot.Items()
	refs := snapshot.ItemRefs()
	if len(refs) != len(items) {
		t.Fatalf("refs = %d, items = %d", len(refs), len(items))
	}
	for index := range items {
		if refs[index].ID != items[index].ID ||
			refs[index].Kind != items[index].Kind {
			t.Fatalf(
				"ref %d = %+v, want item identity %+v",
				index, refs[index], items[index],
			)
		}
	}
}

func tokenestimateText(
	t *testing.T,
	definition provider.ToolDefinition,
) uint64 {
	t.Helper()
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	return tokenestimate.Text(string(encoded))
}
