package agentcontext

import (
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
)

func TestRenderTurnCheckpointIsStructuredAndBounded(t *testing.T) {
	checkpoint, err := RenderTurnCheckpoint(CheckpointRenderInput{
		Turn:   3,
		Status: CheckpointCompleted,
		Goal:   "teach the parser",
		Plan: Plan{Steps: []PlanStep{{
			Title: "update the lexer", Status: StepInProgress,
		}}},
		Items: []NarrativeItem{{
			Kind: NarrativeUnresolved, Text: "P2: missing overflow test",
			SourceMessageIDs: []string{"msg_a"},
		}},
		Budget: 4 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(checkpoint.Text, CheckpointMarkerStart) ||
		!strings.Contains(checkpoint.Text, "P2: missing overflow test") ||
		!strings.Contains(checkpoint.Text, TurnHistoryToolName) {
		t.Fatalf("checkpoint = %s", checkpoint.Text)
	}
}

func TestRenderTurnCheckpointOverflowKeepsTitleAndHandle(t *testing.T) {
	checkpoint, err := RenderTurnCheckpoint(CheckpointRenderInput{
		Turn:   4,
		Status: CheckpointCompleted,
		Goal:   strings.Repeat("goal ", 80),
		Items: []NarrativeItem{{
			Kind: NarrativeNextStep, Text: strings.Repeat("next ", 80),
			SourceMessageIDs: []string{"msg_b"},
		}},
		HistoryHandle: "result_turn_4",
		Budget:        280,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Text) > 280 {
		t.Fatalf("checkpoint bytes = %d", len(checkpoint.Text))
	}
	if !strings.Contains(checkpoint.Text, "result_turn_4") &&
		!strings.Contains(checkpoint.Text, TurnHistoryToolName) {
		t.Fatalf("overflow lost retrieval pointer: %s", checkpoint.Text)
	}
}

func TestRenderTurnCheckpointCanceledKeepsNextPlanAndReadPaths(t *testing.T) {
	checkpoint, err := RenderTurnCheckpoint(CheckpointRenderInput{
		Turn:   5,
		Status: CheckpointCanceled,
		Plan: Plan{Steps: []PlanStep{
			{Title: "audit accept()", Status: StepDone},
			{Title: "fix overflow in accept()", Status: StepPending},
		}},
		Items: []NarrativeItem{{
			Kind: NarrativeUnresolved, Text: "half-open tool chain",
			SourceMessageIDs: []string{"msg_c"},
		}},
		ReadPaths: []string{"multi_paxos_node.h", "snapshot_store.h"},
		Budget:    1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(checkpoint.Text, "half-open tool chain") {
		t.Fatalf("canceled checkpoint kept open tool chain: %s", checkpoint.Text)
	}
	if !strings.Contains(checkpoint.Text, "next: fix overflow in accept()") ||
		!strings.Contains(checkpoint.Text, `"read_paths"`) ||
		!strings.Contains(checkpoint.Text, "multi_paxos_node.h") {
		t.Fatalf("canceled checkpoint = %s", checkpoint.Text)
	}
}

func TestRenderTurnCheckpointOverflowNotesOmittedReadPaths(t *testing.T) {
	paths := make([]string, 0, 12)
	for index := 0; index < 12; index++ {
		paths = append(paths, strings.Repeat("already_read_path_", 4)+string(rune('a'+index))+".go")
	}
	checkpoint, err := RenderTurnCheckpoint(CheckpointRenderInput{
		Turn:      6,
		Status:    CheckpointCanceled,
		Goal:      strings.Repeat("goal ", 40),
		Plan:      Plan{Steps: []PlanStep{{Title: "fix overflow", Status: StepPending}}},
		ReadPaths: paths,
		Budget:    400,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Text) > 400 {
		t.Fatalf("checkpoint bytes = %d", len(checkpoint.Text))
	}
	if !strings.Contains(checkpoint.Text, "more already-read paths omitted") {
		t.Fatalf("overflow lost omitted-read notice: %s", checkpoint.Text)
	}
}

func TestRenderTurnCheckpointFailedKeepsPlanButOmitsUncommittedNarrative(t *testing.T) {
	checkpoint, err := RenderTurnCheckpoint(CheckpointRenderInput{
		Turn:    2,
		Status:  CheckpointFailed,
		Failure: "provider timeout",
		Plan: Plan{Steps: []PlanStep{{
			Title: "finish parser tests", Status: StepInProgress,
		}}},
		Items: []NarrativeItem{{
			Kind: NarrativeUnresolved, Text: "half-open tool chain",
			SourceMessageIDs: []string{"pending"},
		}},
		Budget: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(checkpoint.Text, "half-open tool chain") {
		t.Fatalf("failed checkpoint kept open work: %s", checkpoint.Text)
	}
	if !strings.Contains(checkpoint.Text, "provider timeout") ||
		!strings.Contains(checkpoint.Text, "next: finish parser tests") {
		t.Fatalf("failed checkpoint lost failure: %s", checkpoint.Text)
	}
}

func TestMessagesForTurnSkipsWorldAndOtherTurns(t *testing.T) {
	history := []provider.Message{
		{Role: provider.RoleUser, Turn: 1, Blocks: []provider.ContentBlock{{
			Type: provider.ContentText, Text: "old",
		}}},
		{Role: provider.RoleUser, Turn: 2, Blocks: []provider.ContentBlock{{
			Type: provider.ContentText, Text: "current",
		}}},
	}
	got := MessagesForTurn(history, 2)
	if len(got) != 1 || got[0].Text() != "current" {
		t.Fatalf("messages = %+v", got)
	}
}

func TestOmittedTurnHintIsMandatoryAndDoesNotInventLists(t *testing.T) {
	hint := FormatOmittedTurnHint([]uint64{1})
	if !strings.Contains(hint, TurnHistoryToolName) ||
		!strings.Contains(hint, "turn=1") ||
		strings.Contains(hint, "P2") {
		t.Fatalf("hint = %q", hint)
	}
	rangeHint := FormatOmittedTurnHint([]uint64{1, 2, 3})
	if !strings.Contains(rangeHint, "1-3") ||
		!strings.Contains(rangeHint, "turn=1") {
		t.Fatalf("range hint = %q", rangeHint)
	}
	entity, ok := OmittedTurnRetrievalEntity([]uint64{1})
	if !ok || entity.Retention != RetentionMandatory ||
		entity.Source != TurnHistorySource ||
		strings.Contains(entity.Value, "P2") {
		t.Fatalf("entity = %+v ok=%v", entity, ok)
	}
	capsule := MandatorySessionState(BuildTruthCapsule(TruthProjection{
		Compatibility: Compatibility{SchemaVersion: TruthSchemaVersion},
		ModelID:       "model",
		ContextTokens: 4096,
		ExtraEntities: []TruthEntity{entity},
	}))
	rendered, err := RenderSessionState(capsule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.Text, hint) {
		t.Fatalf("session state missing retrieval hint: %s", rendered.Text)
	}
}

func TestValidateTurnCheckpointsRejectsRewrite(t *testing.T) {
	err := ValidateTurnCheckpoints([]TurnCheckpoint{
		{Turn: 1, Status: CheckpointCompleted, Text: "a"},
		{Turn: 1, Status: CheckpointCompleted, Text: "b"},
	})
	if err == nil {
		t.Fatal("duplicate turn accepted")
	}
}
