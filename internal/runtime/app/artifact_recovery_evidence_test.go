package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func TestRecoveryDisplayPromptUnwrapsLegacyInternalPrompt(t *testing.T) {
	legacy := TurnRecoveryPromptPrefix + ` Do not infer the task.

Original model-visible request:
<source_request>
Fix the parser
</source_request>

<recovery_evidence>
{"source_turn_id":"turn-source","closed_tools":[{"call_id":"call-read"}]}
</recovery_evidence>`
	if got := RecoveryDisplayPrompt(legacy, legacy); got != "Fix the parser" {
		t.Fatalf("legacy recovery display prompt = %q", got)
	}
	nested := TurnRecoveryPromptPrefix + ` Do not infer the task.

Original model-visible request:
<source_request>
` + legacy + `
</source_request>`
	if got := RecoverySourcePrompt(nested); got != "Fix the parser" {
		t.Fatalf("nested recovery source prompt = %q", got)
	}
	withQuotedClose := TurnRecoveryPromptPrefix + ` Do not infer the task.

Original model-visible request:
<source_request>
Fix the parser
</source_request>

Recovery guidance:
<guidance>
Explain the literal </source_request> tag.
</guidance>`
	if got := RecoverySourcePrompt(withQuotedClose); got != "Fix the parser" {
		t.Fatalf("quoted close recovery source prompt = %q", got)
	}
	if got := RecoveryDisplayPrompt(
		"internal recovery context",
		"Continue: Continue: Fix the parser",
	); got != "Fix the parser" {
		t.Fatalf("nested recovery display prompt = %q", got)
	}
	if got := RecoveryDisplayPrompt(
		legacy,
		"Continue: Fix the parser\n\nGuidance: obsolete direction",
	); got != "Fix the parser" {
		t.Fatalf("recovered display prompt retained old guidance = %q", got)
	}
}

func TestRecoveryEvidenceIsCanonicalAndBounded(t *testing.T) {
	first := RecoveryDigestJSON(
		[]byte(`{"b":9223372036854775807,"a":1}`),
	)
	second := RecoveryDigestJSON(
		[]byte(`{"a":1,"b":9223372036854775807}`),
	)
	if first == "" || first != second {
		t.Fatalf("canonical argument digests = %q and %q", first, second)
	}
	tools := make([]RecoveryToolEvidence, 200)
	for index := range tools {
		tools[index] = RecoveryToolEvidence{
			Tool:            "file_read",
			CallID:          fmt.Sprintf("call-%03d", index),
			ArgumentsDigest: first,
			OutputDigest:    RecoveryDigest([]byte(strings.Repeat("x", index+1))),
		}
	}
	rendered := RenderRecoveryEvidence(
		"turn-source",
		protocol.TurnIntentWorkspaceChange,
		"failed (conflict): no changes",
		tools,
		&protocol.ExecutionReceiptData{
			ReadPaths:    []string{"parser.go", "parser_test.go"},
			Verification: protocol.ReceiptVerification{},
			WorkspaceOutcome: &protocol.ReceiptWorkspaceOutcome{
				Status: "unchanged",
			},
		},
		"partial conclusion",
	)
	if rendered == "" || len(rendered) > TurnRecoveryEvidenceLimit {
		t.Fatalf("rendered recovery evidence bytes = %d", len(rendered))
	}
	var capsule RecoveryEvidenceCapsule
	if err := json.Unmarshal([]byte(rendered), &capsule); err != nil {
		t.Fatal(err)
	}
	if capsule.Version != 3 ||
		capsule.Intent != protocol.TurnIntentWorkspaceChange ||
		capsule.SourceTurnID != "turn-source" ||
		capsule.OmittedTools == 0 ||
		len(capsule.Tools)+capsule.OmittedTools != len(tools) ||
		capsule.WorkItem == nil ||
		len(capsule.WorkItem.KnownReads) != 2 ||
		len(capsule.Outcomes) == 0 ||
		capsule.Receipt == nil ||
		len(capsule.Receipt.ReadPaths) != 2 ||
		capsule.PartialOutput != "partial conclusion" {
		t.Fatalf("recovery evidence = %+v", capsule)
	}
}

func TestRecoveryEvidenceBoundsWorkItemWithoutLosingConclusion(t *testing.T) {
	for _, kind := range []string{"reads", "edits", "sessions"} {
		for _, count := range []int{1, 128} {
			t.Run(fmt.Sprintf("%s/%d", kind, count), func(t *testing.T) {
				tools := make([]RecoveryToolEvidence, count)
				for i := range tools {
					path := fmt.Sprintf("%03d/%s", i, strings.Repeat("路径\"\\", TurnRecoveryEvidenceLimit/(count*4)))
					tools[i] = RecoveryToolEvidence{Tool: "file_read", Path: path}
					switch kind {
					case "edits":
						tools[i] = RecoveryToolEvidence{Tool: "file_edit", Changes: []protocol.FileChange{{Path: path}}}
					case "sessions":
						tools[i] = RecoveryToolEvidence{Tool: "exec_command", Path: path, Reason: "running"}
					}
				}
				before, err := json.Marshal(tools)
				if err != nil {
					t.Fatal(err)
				}
				rendered := RenderRecoveryEvidence("turn-source", protocol.TurnIntentWorkspaceChange,
					"interrupted", tools, nil, "保留这条结论")
				if rendered == "" || len(rendered) > TurnRecoveryEvidenceLimit {
					t.Fatalf("rendered recovery evidence bytes = %d", len(rendered))
				}
				var capsule RecoveryEvidenceCapsule
				if err := json.Unmarshal([]byte(rendered), &capsule); err != nil {
					t.Fatal(err)
				}
				if capsule.SourceTurnID != "turn-source" || capsule.Terminal != "interrupted" ||
					capsule.Intent != protocol.TurnIntentWorkspaceChange || capsule.Version != 3 ||
					capsule.PartialOutput != "保留这条结论" || capsule.WorkItem == nil {
					t.Fatalf("recovery lost source or conclusion: %+v", capsule)
				}
				// Inspect the wire format so omitted counts remain explicit to the model.
				var wire struct {
					WorkItem map[string]json.RawMessage `json:"work_item"`
				}
				if err := json.Unmarshal([]byte(rendered), &wire); err != nil {
					t.Fatal(err)
				}
				var omitted int
				if err := json.Unmarshal(wire.WorkItem["omitted_"+kind], &omitted); err != nil {
					t.Fatalf("missing omitted count: %v", err)
				}
				retained := len(capsule.WorkItem.KnownReads) + len(capsule.WorkItem.KnownEdits) + len(capsule.WorkItem.OpenSessions)
				if omitted == 0 || omitted+retained != count || (retained == 0 && capsule.WorkItem.RequiredAction != "turn_history") {
					t.Fatalf("work item = %+v, omitted=%d", capsule.WorkItem, omitted)
				}
				after, err := json.Marshal(tools)
				if err != nil || string(after) != string(before) {
					t.Fatal("rendering mutated input evidence")
				}
			})
		}
	}
}

func TestRecoveryEvidenceBoundsOversizedReceipt(t *testing.T) {
	for _, section := range []string{"workspace", "verification"} {
		t.Run(section, func(t *testing.T) {
			receipt := &protocol.ExecutionReceiptData{
				ReadPaths: []string{"source.go"},
				Changes:   []protocol.ReceiptChange{{Path: "target.go"}},
			}
			large := strings.Repeat("长内容", TurnRecoveryEvidenceLimit)
			if section == "workspace" {
				receipt.WorkspaceOutcome = &protocol.ReceiptWorkspaceOutcome{Status: "changed", Note: large}
			} else {
				receipt.Verification.Tests = large
			}
			before, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			rendered := RenderRecoveryEvidence("turn-source", protocol.TurnIntentWorkspaceChange,
				"interrupted", nil, receipt, "partial conclusion")
			if rendered == "" || len(rendered) > TurnRecoveryEvidenceLimit {
				t.Fatalf("rendered recovery evidence bytes = %d", len(rendered))
			}
			var capsule RecoveryEvidenceCapsule
			if err := json.Unmarshal([]byte(rendered), &capsule); err != nil {
				t.Fatal(err)
			}
			if capsule.SourceTurnID != "turn-source" || capsule.PartialOutput != "partial conclusion" ||
				capsule.WorkItem == nil || capsule.WorkItem.OmittedReads != 1 ||
				capsule.WorkItem.OmittedEdits != 1 || capsule.WorkItem.RequiredAction != "turn_history" {
				t.Fatalf("recovery lost source or conclusion: %+v", capsule)
			}
			after, err := json.Marshal(receipt)
			if err != nil || string(after) != string(before) {
				t.Fatal("rendering mutated input receipt")
			}
		})
	}
}
