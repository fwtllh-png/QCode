package agentcontext

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestConversationMarkdownSourceRanges(t *testing.T) {
	for _, tc := range []struct {
		name, text     string
		labels, bodies []string
	}{
		{"ordered", "1. 一号问题\n7. 原始七号\n", []string{"1", "7"}, []string{"1. 一号问题\n", "7. 原始七号\n"}},
		{"nested", "1. 父条件\n   1. 子问题\n   2. 子问题二\n2. 下一项", []string{"1", "1", "2", "2"}, []string{"1. 父条件\n   1. 子问题\n   2. 子问题二\n", "   1. 子问题\n", "   2. 子问题二\n", "2. 下一项"}},
		{"fenced", "1. ```go\n   2. not an item\n   ```\n2. 第二项", []string{"1", "2"}, []string{"1. ```go\n   2. not an item\n   ```\n", "2. 第二项"}},
		{"fence_after_list", "1. 首项\n\n```\n2. not an item\n```\n", []string{"1"}, []string{"1. 首项\n\n"}},
		{"empty_fence", "1. ```\n   ```\n2. 尾项", []string{"1", "2"}, []string{"1. ```\n   ```\n", "2. 尾项"}},
		{"empty_item", "1.\n2. 定义", []string{"1", "2"}, []string{"1.\n", "2. 定义"}},
		{"headings", "# 报告\n前言\n\n## 问题\n1. 定义\n\n# 另一报告\n内容", []string{"报告", "问题", "1", "另一报告"}, []string{"# 报告\n前言\n\n## 问题\n1. 定义\n\n", "## 问题\n1. 定义\n\n", "1. 定义\n\n", "# 另一报告\n内容"}},
		{"setext", "报告\n====\n正文\n\n另一报告\n====\n另一正文", []string{"报告", "另一报告"}, []string{"报告\n====\n正文\n\n", "另一报告\n====\n另一正文"}},
		{"multiple_lists", "1. 第一组\n\n分隔段\n\n1. 第二组", []string{"1", "1"}, []string{"1. 第一组\n\n", "1. 第二组"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := IndexConversationAnswer("thread", "turn", 1, tc.text)
			if source.Text != tc.text {
				t.Fatal("source text changed")
			}
			state := &ConversationState{}
			if err := state.Add(source); err != nil {
				t.Fatal(err)
			}
			var labels, bodies []string
			for _, item := range source.Items {
				labels = append(labels, item.Label)
				bodies = append(bodies, source.Text[item.Start:item.End])
			}
			if !reflect.DeepEqual(labels, tc.labels) || !reflect.DeepEqual(bodies, tc.bodies) {
				t.Fatalf("labels=%q bodies=%q items=%+v", labels, bodies, source.Items)
			}
			if tc.name == "nested" && source.Items[1].ParentID != source.Items[0].ID {
				t.Fatal("nested item lost parent")
			}
		})
	}
}

func TestConversationStableIdentitySelectionAndReplacement(t *testing.T) {
	text := "1. 同名问题\n2. 同名问题"
	a := IndexConversationAnswer("thread", "first", 1, text)
	b := IndexConversationAnswer("thread", "second", 2, text)
	foreign := IndexConversationAnswer("another-thread", "first", 1, text)
	if a.ID == b.ID || a.ID == foreign.ID || a.ID != IndexConversationAnswer("thread", "first", 1, text).ID {
		t.Fatal("source identity is not scoped or stable")
	}
	state := &ConversationState{}
	for _, source := range []ConversationSource{a, b} {
		if err := state.Add(source); err != nil {
			t.Fatal(err)
		}
	}
	if len(state.CandidateSources(Plan{})) != 2 {
		t.Fatal("ambiguous report was silently discarded")
	}
	if err := state.ValidateReferences(nil, []string{foreign.Items[1].ID}); err == nil {
		t.Fatal("foreign reference accepted")
	}
	selection := NewConversationSelection(nil, []string{a.Items[1].ID}, 3, "select", "继续第一份报告第2项")
	if err := state.Select(selection, nil); err != nil {
		t.Fatal(err)
	}
	clone := CloneConversation(state)
	clone.Selection.ItemIDs[0] = "mutated"
	if state.Selection.ItemIDs[0] != a.Items[1].ID {
		t.Fatal("selection aliased")
	}
	plan := Plan{Steps: []PlanStep{{ID: "work", ReferenceItemIDs: []string{a.Items[1].ID}, Title: "renamed", Status: StepPending}}}
	replacement := ConversationReplacement{OldGroupID: a.ID, NewGroupID: b.ID, SourceTurn: 4, SourceTurnID: "correct", UserRequestDigest: selection.UserRequestDigest}
	if err := state.Select(NewConversationSelection([]string{b.ID}, nil, 4, "correct", "改用新版"), []ConversationReplacement{replacement}); err != nil {
		t.Fatal(err)
	}
	if len(state.Sources) != 2 || !state.Superseded(a.ID) {
		t.Fatal("replacement destroyed audit source")
	}
	if err := state.ValidatePlan(&plan); err == nil {
		t.Fatal("active obsolete reference accepted")
	}
	plan.Steps[0].Status = StepDone
	if err := state.ValidatePlan(&plan); err != nil {
		t.Fatalf("historical reference lost: %v", err)
	}
}

func TestConversationManifestRoundTripAndBodyReuse(t *testing.T) {
	store := &manifestMemoryStore{}
	snapshot := manifestSnapshot(t, 1, nil)
	source := IndexConversationAnswer("thread", "report", 1, "1. "+strings.Repeat("长定义", 2000)+"\n2. 尾部定义")
	snapshot.Conversation = &ConversationState{}
	if err := snapshot.Conversation.Add(source); err != nil {
		t.Fatal(err)
	}
	snapshot.Plan = &Plan{Steps: []PlanStep{{ID: "work", Title: "实现第二项", Status: StepPending, ReferenceItemIDs: []string{source.Items[1].ID}}}}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	first, err := BuildContextManifest(t.Context(), store, "thread", "report", snapshot, nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	bodyRef := first.Conversation.Bodies[source.ID]
	if !slices.Contains(first.ContentIDs(), bodyRef.Handle) {
		t.Fatal("GC roots omit body")
	}
	indexBytes := store.values[first.Conversation.Index.BaseRef.Handle]
	if strings.Contains(string(indexBytes), "尾部定义") {
		t.Fatal("index duplicated source body")
	}
	loaded, err := LoadContextManifest(t.Context(), store, first)
	if err != nil || !reflect.DeepEqual(loaded.Conversation, snapshot.Conversation) || loaded.Digest != snapshot.Digest {
		t.Fatalf("restore: %v", err)
	}
	snapshot.Revision++
	if err := snapshot.Conversation.Select(NewConversationSelection(nil, []string{source.Items[1].ID}, 2, "focus", "继续第2项"), nil); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Seal(); err != nil {
		t.Fatal(err)
	}
	second, err := BuildContextManifest(t.Context(), store, "thread", "focus", snapshot, &first, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	if second.Conversation.Bodies[source.ID] != bodyRef {
		t.Fatal("focus rewrote body reference")
	}
	if _, err := LoadContextManifest(t.Context(), store, second); err != nil {
		t.Fatal(err)
	}
	accounting, err := PrepareAccountingDelta("focus", provider.Usage{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := NewSessionDelta(snapshot, accounting)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(delta)
	decoded, err := DecodeSessionDelta(raw)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := PrepareSessionRestore(decoded, 0, "", true)
	if err != nil || !reflect.DeepEqual(restored.State.Conversation, snapshot.Conversation) {
		t.Fatalf("delta restore: %v", err)
	}
	delete(store.values, bodyRef.Handle)
	if _, err := LoadContextManifest(t.Context(), store, second); err == nil {
		t.Fatal("missing source body accepted")
	}
}

func TestConversationOptionalEncodingAndCorruptRestore(t *testing.T) {
	snapshot := manifestSnapshot(t, 1, nil)
	raw, _ := json.Marshal(snapshot)
	if strings.Contains(string(raw), "conversation") {
		t.Fatal("nil optional field changed encoding")
	}
	source := IndexConversationAnswer("thread", "turn", 1, "1. 定义")
	state := &ConversationState{Sources: map[string]ConversationSource{source.ID: source}}
	source.Items[0].End = len(source.Text) + 1
	state.Sources[source.ID] = source
	if _, err := PrepareSessionRestore(SessionDelta{Conversation: state}, 0, "", true); err == nil {
		t.Fatal("corrupt source restored")
	}
}
