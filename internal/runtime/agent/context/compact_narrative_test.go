package agentcontext

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestNarrativeArtifactIsSourceBoundAndNonAuthoritative(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	removed := []provider.Message{
		messageAt(provider.RoleUser, "I prefer deterministic ledgers", 1),
		messageAt(provider.RoleAssistant, "Decision: retain structured truth because it is verifiable", 1),
	}
	input, err := BuildNarrativeInput(
		"thread-1",
		"window-1",
		"sha256:authority",
		"sha256:route",
		removed,
		NarrativeLimits{},
		now,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"technical_concepts": []any{},
		"files_and_code": []map[string]any{{
			"text":               "parser/lex.go exposes Lex.",
			"source_message_ids": []string{input.Excerpts[1].MessageID},
		}},
		"errors_and_fixes": []any{},
		"pending_jobs":     []any{},
		"current_work":     []any{},
		"next_steps":       []any{},
		"critical_context": []any{},
		"decisions": []map[string]any{{
			"text":               "Use a deterministic truth ledger.",
			"source_message_ids": []string{input.Excerpts[1].MessageID},
		}},
		"rationale": []any{},
		"preferences": []map[string]any{{
			"text":               "Prefer deterministic state.",
			"source_message_ids": []string{input.Excerpts[0].MessageID},
		}},
		"unresolved": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ValidateNarrativeJSON(
		raw,
		input,
		NarrativeLimits{},
		2,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Body.Items) != 3 ||
		artifact.AuthorityDigest != input.AuthorityDigest ||
		artifact.Digest == "" ||
		artifact.Body.Items[0].ID == artifact.Body.Items[1].ID {
		t.Fatalf("artifact=%+v", artifact)
	}
}

func TestNarrativeValidatorRejectsUnknownSourceAndFields(t *testing.T) {
	now := time.Now().UTC()
	input, err := BuildNarrativeInput(
		"thread-1",
		"window-1",
		"sha256:authority",
		"sha256:route",
		[]provider.Message{messageAt(provider.RoleUser, "constraint", 1)},
		NarrativeLimits{},
		now,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"technical_concepts":[],"files_and_code":[],"errors_and_fixes":[],"pending_jobs":[],"current_work":[],"next_steps":[],"critical_context":[],"decisions":[],"rationale":[],"preferences":[],"unresolved":[]}`
	for name, raw := range map[string]string{
		"unknown source": strings.Replace(
			valid,
			`"files_and_code":[]`,
			`"files_and_code":[{"text":"x","source_message_ids":["msg_unknown"]}]`,
			1,
		),
		"unknown field": strings.TrimSuffix(valid, "}") + `,"verified":true}`,
		"missing array": strings.Replace(valid, `"next_steps":[],`, "", 1),
		"trailing":      valid + ` true`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateNarrativeJSON(
				[]byte(raw),
				input,
				NarrativeLimits{},
				2,
				now,
			); err == nil {
				t.Fatal("invalid narrative was accepted")
			}
		})
	}
}

func TestStableMessageIDDoesNotContainMessageText(t *testing.T) {
	message := messageAt(provider.RoleUser, "private content", 7)
	id := StableMessageID("thread-1", message, 3)
	if id == "" || id == "private content" {
		t.Fatalf("message id = %q", id)
	}
	if id != StableMessageID("thread-1", message, 3) {
		t.Fatal("message id changed across retry")
	}
	if message.Turn != 7 {
		t.Fatalf("message turn mutated to %d", message.Turn)
	}
	withoutTurn := message
	withoutTurn.Turn = 0
	if id == StableMessageID("thread-1", withoutTurn, 3) {
		t.Fatalf("message id %q ignored the turn", id)
	}
}

func TestNarrativeInputIncludesPairedToolResults(t *testing.T) {
	now := time.Now().UTC()
	input, err := BuildNarrativeInput(
		"thread-1",
		"window-1",
		"sha256:authority",
		"sha256:route",
		[]provider.Message{
			messageAt(provider.RoleUser, "retain this preference", 1),
			messageAt(provider.RoleSystem, "old generated narrative", 1),
			{
				Role: provider.RoleAssistant,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentToolCall,
					ToolCall: &provider.ToolCall{
						ID: "call-read", Name: "file_read",
						Arguments: `{"path":"parser.go"}`,
					},
				}},
				Turn: 1,
			},
			{
				Role: provider.RoleTool,
				Blocks: []provider.ContentBlock{{
					Type: provider.ContentToolResult,
					ToolResult: &provider.ToolResult{
						CallID: "call-read", Content: "func Parse() {}",
					},
				}},
				Turn: 1,
			},
		},
		NarrativeLimits{},
		now,
		time.Hour,
		[]string{
			NarrativeCurrent,
			NarrativeFileCode,
			NarrativeNextStep,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if input.PrivacyClass != NarrativePrivacyClass ||
		len(input.Excerpts) != 2 ||
		len(input.RequiredKinds) != 3 ||
		input.Excerpts[0].Role != provider.RoleUser ||
		input.Excerpts[1].Role != provider.RoleTool ||
		!strings.Contains(input.Excerpts[1].Text, "file_read") ||
		!strings.Contains(input.Excerpts[1].Text, "parser.go") ||
		!strings.Contains(input.Excerpts[1].Text, "func Parse()") {
		t.Fatalf("input=%+v", input)
	}
	source := input.Excerpts[1].MessageID
	raw, err := json.Marshal(map[string]any{
		"technical_concepts": []any{},
		"files_and_code": []map[string]any{{
			"text": "parser.go defines Parse.", "source_message_ids": []string{source, input.Excerpts[0].MessageID},
		}},
		"errors_and_fixes": []any{},
		"pending_jobs":     []any{},
		"current_work": []map[string]any{{
			"text": "Implement the parser.", "source_message_ids": []string{source},
		}},
		"next_steps": []map[string]any{{
			"text": "Write parser.go.", "source_message_ids": []string{source},
		}},
		"critical_context": []any{},
		"decisions":        []any{},
		"rationale":        []any{},
		"preferences":      []any{},
		"unresolved":       []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ValidateNarrativeJSON(
		raw, input, NarrativeLimits{}, 2, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	missing, _ := json.Marshal(map[string]any{
		"technical_concepts": []any{},
		"files_and_code":     []any{},
		"errors_and_fixes":   []any{},
		"pending_jobs":       []any{},
		"current_work":       []any{},
		"next_steps":         []any{},
		"critical_context":   []any{},
		"decisions":          []any{},
		"rationale":          []any{},
		"preferences":        []any{},
		"unresolved":         []any{},
	})
	if _, err := ValidateNarrativeJSON(
		missing, input, NarrativeLimits{}, 2, now,
	); err == nil {
		t.Fatal("tool-heavy narrative accepted without continuation context")
	}
}

func TestNarrativeInputRetainsAllSourcesForLaterBudgetPartition(t *testing.T) {
	now := time.Now().UTC()
	var removed []provider.Message
	for index := 0; index < 20; index++ {
		removed = append(
			removed,
			messageAt(provider.RoleUser, "short preference", uint64(index+1)),
		)
	}
	limits := NarrativeLimits{}
	limits.MaxInputBytes = 900
	input, err := BuildNarrativeInput(
		"thread-1",
		"window-1",
		"sha256:authority",
		"sha256:route",
		removed,
		limits,
		now,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= limits.MaxInputBytes || len(input.Excerpts) != len(removed) {
		t.Fatalf(
			"artifact bytes=%d excerpts=%d",
			len(raw),
			len(input.Excerpts),
		)
	}
}

func TestNarrativeJSONValidatesAgainstBuiltInput(t *testing.T) {
	now := time.Now().UTC()
	input, err := BuildNarrativeInput(
		"thread-1",
		"window-1",
		"sha256:authority",
		"sha256:route",
		[]provider.Message{
			messageAt(provider.RoleUser, "continue", 1),
		},
		NarrativeLimits{},
		now,
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Excerpts) == 0 {
		t.Fatal("expected excerpts")
	}
	raw := strings.Replace(
		`{"technical_concepts":[],"files_and_code":[],"errors_and_fixes":[],"pending_jobs":[],"current_work":[],"next_steps":[],"critical_context":[],"decisions":[],"rationale":[],"preferences":[],"unresolved":[]}`,
		`"preferences":[]`,
		`"preferences":[{"text":"continue","source_message_ids":["`+
			input.Excerpts[0].MessageID+`"]}]`,
		1,
	)
	if _, err := ValidateNarrativeJSON(
		[]byte(raw), input, NarrativeLimits{}, 2, now,
	); err != nil {
		t.Fatal(err)
	}
}

func TestNarrativeZeroLimitsKeepMoreThanFormerItemAndByteCaps(t *testing.T) {
	now := time.Now().UTC()
	input, err := BuildNarrativeInput("thread", "window", "authority", "route", []provider.Message{messageAt(provider.RoleUser, "完整来源", 1)}, NarrativeLimits{}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	for i := 0; i < 40; i++ {
		items = append(items, map[string]any{"text": fmt.Sprint(i) + strings.Repeat("长定义", 200), "source_message_ids": []string{input.Excerpts[0].MessageID}})
	}
	output := map[string]any{"technical_concepts": []any{}, "files_and_code": []any{}, "errors_and_fixes": []any{}, "pending_jobs": []any{}, "current_work": []any{}, "next_steps": []any{}, "critical_context": []any{}, "decisions": []any{}, "rationale": []any{}, "unresolved": []any{}, "preferences": items}
	raw, _ := json.Marshal(output)
	artifact, err := ValidateNarrativeJSON(raw, input, NarrativeLimits{}, 2, now)
	if err != nil || len(artifact.Body.Items) != 40 {
		t.Fatalf("zero reintroduced a hidden limit: items=%d err=%v", len(artifact.Body.Items), err)
	}
	for _, limits := range []NarrativeLimits{{MaxItems: 32}, {ItemMaxBytes: 512}} {
		if _, err := ValidateNarrativeJSON(raw, input, limits, 2, now); err == nil {
			t.Fatal("explicit ceiling ignored")
		}
	}
}

func messageAt(role provider.Role, text string, turn uint64) provider.Message {
	message := provider.TextMessage(role, text)
	message.Turn = turn
	return message
}
