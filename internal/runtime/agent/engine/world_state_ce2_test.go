package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestWorldStatePersistsAcrossTurnsAndEmitsOnlyChanges(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("one"), textStream("two"), textStream("three"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	engine.options.RepoContext = &stubRepoContext{}

	if _, err := engine.Run(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if !agentcontext.WorldBaselineValid(engine.History(), engine.context.World()) ||
		engine.context.World().Revision != 1 {
		t.Fatalf("first baseline=%+v history=%+v", engine.context.World(), engine.History())
	}
	if _, err := engine.Run(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	second := runtime.requests[1].Messages
	if countWorldSection(second, "policy") != 1 ||
		countWorldMode(second, "patch") != 0 ||
		engine.context.World().Revision != 1 {
		t.Fatalf("unchanged request=%+v baseline=%+v", second, engine.context.World())
	}

	engine.observePath(agentcontext.SourceRead, "found.go")
	if _, err := engine.Run(t.Context(), "third", nil); err != nil {
		t.Fatal(err)
	}
	third := runtime.requests[2].Messages
	if countWorldSection(third, "working_set_ledger") != 1 ||
		countWorldSection(third, promptcontext.PartitionSessionState) != 1 ||
		countWorldMode(third, "patch") != 2 ||
		engine.context.World().Revision != 2 {
		t.Fatalf("patched request=%+v baseline=%+v", third, engine.context.World())
	}
	if countWorldSection(engine.History(), "working_set_ledger") != 2 {
		t.Fatalf("durable world history was collapsed: %+v", engine.History())
	}
}

func TestWorldStateFreezesWithinTurnAndRefreshesAtNextTurn(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("echo-1", "echo", `{"text":"one"}`),
		textStream("done"),
		textStream("next"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	repository := &stubRepoContext{}
	engine := newEngine(t, runtime, registry)
	engine.options.RepoContext = repository

	if _, err := engine.Run(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 2 {
		t.Fatalf("first Turn requests = %d, want 2", len(runtime.requests))
	}
	if len(repository.evidence) != 1 {
		t.Fatalf("World State builds = %d, want one per Turn", len(repository.evidence))
	}
	if countWorldMode(runtime.requests[1].Messages, "patch") != 0 {
		t.Fatalf("second sample rewrote World State: %+v", runtime.requests[1].Messages)
	}

	engine.observePath(agentcontext.SourceRead, "deferred.go")
	if _, err := engine.Run(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if len(repository.evidence) != 2 {
		t.Fatalf("World State builds = %d, want one for each Turn", len(repository.evidence))
	}
	if countWorldSection(runtime.requests[2].Messages, "working_set_ledger") != 1 ||
		countWorldSection(runtime.requests[2].Messages, promptcontext.PartitionSessionState) != 1 ||
		countWorldMode(runtime.requests[2].Messages, "patch") != 2 {
		t.Fatalf("next Turn did not publish deferred World changes: %+v",
			runtime.requests[2].Messages)
	}
}

func TestWorldStateDefersEvidenceChangesWithinTurn(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		toolCallStream("echo-1", "echo", `{"text":"same"}`),
		toolCallStream("echo-2", "echo", `{"text":"same"}`),
		textStream("done"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	repository := &stubRepoContext{}
	engine.options.RepoContext = repository

	reminded := false
	if _, err := engine.Run(t.Context(), "repeat one call", func(Event) error {
		for _, reminder := range engine.evidenceSet().Snapshot(100).Reminders {
			reminded = reminded || strings.Contains(reminder.Detail, "echo ran 2 times")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(runtime.requests))
	}
	if len(repository.evidence) != 1 || !reminded {
		t.Fatalf("projected evidence = %+v", repository.evidence)
	}
}

func TestWorldBaselineSurvivesSessionDeltaRestart(t *testing.T) {
	sourceRuntime := &scriptedProvider{streams: []provider.Stream{textStream("one")}}
	source := newEngine(t, sourceRuntime, tool.NewRegistry(nil, nil))
	source.options.RepoContext = &stubRepoContext{}
	if _, err := source.Run(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	fork, err := source.Fork()
	if err != nil {
		t.Fatal(err)
	}
	fork.ReplaceHistory(source.History())
	if !agentcontext.WorldBaselineValid(fork.History(), fork.context.World()) {
		t.Fatalf("fork baseline=%+v history=%+v", fork.context.World(), fork.History())
	}
	delta, err := prepareSessionDeltaForTest(
		"turn-1", 0, source.History(), provider.Usage{}, 0,
		SessionStateDelta{Turn: source.turn, World: source.context.World()},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}

	targetRuntime := &scriptedProvider{streams: []provider.Stream{textStream("two")}}
	target := newEngine(t, targetRuntime, tool.NewRegistry(nil, nil))
	target.options.RepoContext = &stubRepoContext{}
	if err := target.RestoreSessionDelta(raw); err != nil {
		t.Fatal(err)
	}
	if !agentcontext.WorldBaselineValid(target.History(), target.context.World()) {
		t.Fatalf("restored baseline=%+v history=%+v", target.context.World(), target.History())
	}
	if _, err := target.Run(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	request := targetRuntime.requests[0].Messages
	if countWorldSection(request, "policy") != 1 ||
		countWorldMode(request, "patch") != 0 {
		t.Fatalf("restart reinjected unchanged world state: %+v", request)
	}
}

func TestHistoryReplacementAndCompactionInvalidateWorldBaseline(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("one"), textStream("two"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	if _, err := engine.Run(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	engine.ReplaceHistory([]provider.Message{
		provider.TextMessage(provider.RoleUser, "replacement"),
	})
	if engine.context.World().Revision != 0 {
		t.Fatalf("replacement retained stale baseline=%+v", engine.context.World())
	}
	if _, err := engine.Run(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	if countWorldMode(runtime.requests[1].Messages, "full") != 3 {
		t.Fatalf("replacement did not force full state: %+v", runtime.requests[1].Messages)
	}

	engine.history = append(
		engine.history,
		messageWithText(provider.RoleUser, strings.Repeat("old context ", 200), 2),
		messageWithText(provider.RoleAssistant, strings.Repeat("old answer ", 200), 2),
		messageWithText(provider.RoleUser, "current", 3),
	)
	if receipt := engine.CompactForced(); receipt == nil {
		t.Fatal("forced compaction did not produce a receipt")
	}
	if engine.context.World().Revision != 0 {
		t.Fatalf("compaction retained stale baseline=%+v", engine.context.World())
	}
	for _, message := range engine.History() {
		if _, _, ok := agentcontext.InspectWorldMessage(message); ok {
			t.Fatalf("compaction retained world fragment: %+v", engine.History())
		}
	}
}

func TestPolicyAndSkillsChangesProduceTypedPatches(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("one"), textStream("two"), textStream("three"),
	}}
	engine := newEngine(t, runtime, tool.NewRegistry(nil, nil))
	skills := []SkillSummary{{
		Name: "review", Description: "review code", Source: "builtin",
		Path: "skills/review/SKILL.md",
	}}
	engine.options.TurnSnapshots.Skills = func() []SkillSummary {
		return append([]SkillSummary(nil), skills...)
	}
	if _, err := engine.Run(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}

	engine.SetPermission(policy.PermissionAuto)
	if _, err := engine.Run(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	second := runtime.requests[1].Messages
	if countWorldSection(second, "policy") != 1 ||
		countWorldSection(second, "skills") != 1 ||
		countWorldMode(second, "patch") != 1 {
		t.Fatalf("policy patch request=%+v", second)
	}

	skills = append(skills, SkillSummary{
		Name: "test", Description: "run tests", Source: "builtin",
		Path: "skills/test/SKILL.md",
	})
	if _, err := engine.Run(t.Context(), "third", nil); err != nil {
		t.Fatal(err)
	}
	third := runtime.requests[2].Messages
	if countWorldSection(third, "skills") != 1 ||
		countWorldSection(third, promptcontext.PartitionSessionState) != 1 ||
		countWorldMode(third, "patch") != 3 {
		t.Fatalf("skills patch request=%+v", third)
	}
}

func TestToolCatalogChangeProducesTypedPatch(t *testing.T) {
	runtime := &scriptedProvider{streams: []provider.Stream{
		textStream("one"), textStream("two"),
	}}
	registry := tool.NewRegistry(nil, nil)
	if err := registry.Register(&echoTool{}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine(t, runtime, registry)
	if _, err := engine.Run(t.Context(), "first", nil); err != nil {
		t.Fatal(err)
	}
	descriptor := echoDescriptor()
	descriptor.Name = "lookup"
	descriptor.Description = "lookup a value"
	if err := registry.Register(&countingCatalogExecutor{descriptor: descriptor}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), "second", nil); err != nil {
		t.Fatal(err)
	}
	request := runtime.requests[1].Messages
	if countWorldSection(request, "tool_catalog") != 1 ||
		countWorldMode(request, "patch") != 1 {
		t.Fatalf("catalog patch request=%+v", request)
	}
}

func TestSkillWorldUsesSingleBudgetedAuthority(t *testing.T) {
	engine := newEngine(t, &scriptedProvider{}, tool.NewRegistry(nil, nil))
	engine.options.ContextBudgets = map[string]promptcontext.Budget{
		promptcontext.PartitionSkills: {MaxBytes: 256, MaxTokens: 128},
	}
	spec := TurnSpec{
		Mode: policy.ModeAct,
		Policy: policy.DefaultRuntime(
			policy.ModeAct,
			policy.PermissionBypass,
		),
		Skills: []SkillSummary{{
			Name: "large", Description: strings.Repeat("description ", 100),
			Source: "workspace", Path: "skills/large/SKILL.md",
		}},
	}
	sections, receipts := engine.frozenWorldSections(spec, 1)
	var skill *agentcontext.WorldSection
	for index := range sections {
		if sections[index].ID == promptcontext.PartitionSkills {
			skill = &sections[index]
			break
		}
	}
	if skill == nil || skill.Message == nil ||
		len(skill.Message.Text()) > 256 {
		t.Fatalf("skill section=%+v", skill)
	}
	var receipt *promptcontext.Receipt
	for index := range receipts {
		if receipts[index].Kind == promptcontext.PartitionSkills {
			receipt = &receipts[index]
			break
		}
	}
	if receipt == nil || !receipt.Truncated ||
		receipt.TruncationReason == "" {
		t.Fatalf("skill receipt=%+v", receipt)
	}
}

func TestSkillWorldPreservesSourcePathsAndBoundsDescriptions(t *testing.T) {
	rendered := promptcontext.RenderSkillWorld([]SkillSummary{{
		Name: "review", Description: strings.Repeat("界", 200),
		Source: "workspace", Path: "/private/skills/review/SKILL.md",
		Handle: "skh_handle", PackageHandle: "skp_package",
		ResourceHandle: "skr_resource",
	}})
	if !strings.Contains(rendered, `source_path="/private/skills/review/SKILL.md"`) ||
		!strings.Contains(rendered, "...") ||
		!strings.Contains(rendered, "any exact advertised handle") ||
		!strings.Contains(rendered, `handle="skh_handle"`) ||
		!strings.Contains(rendered, `package="skp_package"`) ||
		!strings.Contains(rendered, `resource="skr_resource"`) {
		t.Fatalf("rendered skills=%q", rendered)
	}
}

func TestWorldReceiptsReflectOnlyProjectedChanges(t *testing.T) {
	receipts := promptcontext.ProjectWorldReceipts([]promptcontext.Receipt{
		{
			Kind:          promptcontext.PartitionMode,
			RetainedBytes: 10, RetainedTokens: 3,
		},
		{
			Kind:          promptcontext.PartitionPolicy,
			RetainedBytes: 20, RetainedTokens: 5,
		},
	}, []string{promptcontext.PartitionPolicy})
	if receipts[0].RetainedBytes != 0 ||
		receipts[0].RetainedTokens != 0 ||
		receipts[1].RetainedBytes != 20 ||
		receipts[1].RetainedTokens != 5 {
		t.Fatalf("projected receipts=%+v", receipts)
	}
}

func countWorldSection(messages []provider.Message, id string) int {
	count := 0
	for _, message := range messages {
		entry, _, ok := agentcontext.InspectWorldMessage(message)
		if ok && entry.ID == id {
			count++
		}
	}
	return count
}

func countWorldMode(messages []provider.Message, mode string) int {
	count := 0
	for _, message := range messages {
		_, value, ok := agentcontext.InspectWorldMessage(message)
		if ok && value == agentcontext.WorldMode(mode) {
			count++
		}
	}
	return count
}
