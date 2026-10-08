package agentcontext

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestNarrativeCoverageRejectsMissingAndForgedRanges(t *testing.T) {
	now := time.Now().UTC()
	input, err := BuildNarrativeInput("thread", "window", "truth", "route", []provider.Message{messageAt(provider.RoleAssistant, "1. 第一项\n\n2. 第二项\n\n```go\nfunc Tail() {}\n```", 1)}, NarrativeLimits{}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{}
	for _, name := range []string{"technical_concepts", "files_and_code", "errors_and_fixes", "pending_jobs", "current_work", "next_steps", "critical_context", "decisions", "rationale", "preferences", "unresolved"} {
		base[name] = []any{}
	}
	base["critical_context"] = []map[string]any{{"text": "第一项", "source_message_ids": []string{input.Excerpts[0].MessageID}}}
	raw, _ := json.Marshal(base)
	if _, err := ValidateNarrativeJSON(raw, input, NarrativeLimits{}, 2, now); err == nil || !strings.Contains(err.Error(), "uncovered") {
		t.Fatalf("missing coverage accepted: %v", err)
	}
	ids := make([]string, len(input.Excerpts))
	for i, x := range input.Excerpts {
		ids[i] = x.MessageID
	}
	base["critical_context"] = []map[string]any{{"text": "两个编号与 Tail 函数。", "source_message_ids": ids}}
	raw, _ = json.Marshal(base)
	artifact, err := ValidateNarrativeJSON(raw, input, NarrativeLimits{}, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	artifact.Coverage[0].Digest = "forged"
	artifact.Digest = artifact.digest()
	if err := artifact.Validate(now); err == nil {
		t.Fatal("forged range digest accepted")
	}
	input.Excerpts = input.Excerpts[1:]
	input.Digest = input.digest()
	if err := input.Validate(now); err == nil {
		t.Fatal("incomplete source advertised as complete")
	}
}

func TestNarrativeRepresentationReplacementAndClone(t *testing.T) {
	now := time.Now().UTC()
	input, err := BuildNarrativeInput("thread", "window", "truth", "route", []provider.Message{messageAt(provider.RoleAssistant, "source", 1)}, NarrativeLimits{}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	item := NarrativeItem{Kind: NarrativeCritical, Text: "summary", SourceMessageIDs: []string{input.Excerpts[0].MessageID}, SourceDigest: digestString(input.Excerpts[0].Digest), CreatedTurn: 1}
	item.ID = stableNarrativeItemID(item)
	coverage, _ := narrativeCoverage(input, []NarrativeItem{item})
	artifact := NarrativeArtifact{Version: NarrativeSchemaVersion, ThreadID: input.ThreadID, WindowID: input.SourceWindowID, AuthorityDigest: input.AuthorityDigest, InputDigest: input.Digest, RouteDigest: input.RouteDigest, Body: Narrative{Items: []NarrativeItem{item}}, Coverage: coverage, CreatedAt: now, ExpiresAt: input.ExpiresAt}
	artifact.Digest = artifact.digest()
	merged := MergeNarrativeRepresentation(&artifact, artifact)
	if err := merged.Validate(now); err != nil || len(merged.Coverage) != 1 || len(merged.Body.Items) != 1 {
		t.Fatalf("duplicate representation: %+v, %v", merged, err)
	}
	state := Compaction{Digest: &artifact, NarrativeAttempts: []string{"attempt"}}
	cloned := CloneCompaction(state)
	cloned.Digest.Coverage[0].Source.Parents[0].Label = "changed"
	cloned.NarrativeAttempts[0] = "changed"
	if state.Digest.Coverage[0].Source.Parents[0].Label == "changed" || state.NarrativeAttempts[0] != "attempt" {
		t.Fatal("clone shares mutable coverage")
	}
}
