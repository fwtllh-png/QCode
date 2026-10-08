package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestCompactionReceiptReportsModelDownshiftAndDropsNarrative(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.SummaryMaxBytes = 4 << 10
	previous := agentcontext.TruthCapsule{
		SchemaVersion: agentcontext.TruthSchemaVersion, Generation: 1,
		CompatibilityHash: "sha256:larger-model",
		ModelID:           "larger-model", ContextTokens: 8192,
		DownshiftPolicy: agentcontext.DownshiftRuntimeTruthOnly,
		Entities: []agentcontext.TruthEntity{
			agentcontext.NewTruthEntity(
				agentcontext.EntityFact,
				"legacy.go\x00definition\x0010",
				"legacy.go:10 definition",
				"runtime.evidence",
			),
		},
	}
	previous.Seal()
	rendered, err := agentcontext.RenderStructured(
		agentcontext.Summary{Window: 2},
		previous,
		agentcontext.Narrative{Items: []agentcontext.NarrativeItem{{Kind: agentcontext.NarrativeCritical, Text: "old discussion"}}},
		4<<10,
	)
	if err != nil {
		t.Fatal(err)
	}
	engine.history = []provider.Message{
		provider.TextMessage(provider.RoleSystem, rendered.Text),
		messageWithText(provider.RoleUser, strings.Repeat("old ", 500), 1),
		messageWithText(provider.RoleAssistant, strings.Repeat("answer ", 500), 1),
		messageWithText(provider.RoleUser, "current", 2),
	}
	receipt := engine.compactForcedForTest()
	if receipt == nil || !receipt.ModelDownshifted ||
		receipt.NarrativeIncluded ||
		receipt.DownshiftPolicy != agentcontext.DownshiftRuntimeTruthOnly ||
		receipt.TruthGeneration != 2 ||
		receipt.CriticalFacts != 0 {
		t.Fatalf("downshift receipt=%+v", receipt)
	}
	capsule, found, err := agentcontext.ParseTruthCapsule(engine.history[0].Text())
	if err != nil || !found ||
		truthEntityContains(capsule, agentcontext.EntityFact, "legacy.go:10") {
		t.Fatalf("downshift capsule=%+v found=%t err=%v", capsule, found, err)
	}
}

func TestTruthCapsuleDowngradesVerifiedChangeWhenWorkspaceCannotBind(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	path := filepath.Join(t.TempDir(), "outside.go")
	engine.context.Evidence().MarkChanged(path, 1, true)
	engine.context.Evidence().MarkVerified([]string{path})

	capsule := engine.buildTruthCapsule(engine.buildCompactSummary(nil), nil)
	if err := capsule.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, entity := range capsule.Entities {
		if entity.Kind != agentcontext.EntityChange {
			continue
		}
		if entity.Verified || entity.Retention != agentcontext.RetentionMandatory ||
			entity.WorkspaceClaimStatus != "" {
			t.Fatalf("unbound change retained an invalid claim: %+v", entity)
		}
		return
	}
	t.Fatal("change truth entity was not emitted")
}

// compactForcedForTest exercises history replacement without a durable store.
func (e *Engine) compactForcedForTest() *CompactionReceipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resetViewFold()
	receipt := e.compactHistory(&e.history, true)
	e.reconcileWorldBaseline(e.history)
	return receipt
}
