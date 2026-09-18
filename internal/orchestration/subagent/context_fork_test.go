package subagent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
	"github.com/fwtllh-png/QCode/internal/runtime/contextfork"
)

type contextFixtureSource struct {
	snapshot subagent.ParentContextSnapshot
}

func (s contextFixtureSource) Snapshot(
	context.Context,
	subagent.ContextSourceRef,
) (subagent.ParentContextSnapshot, error) {
	return s.snapshot, nil
}

func TestTaskCapsuleRedactsAndExcludesParentTranscript(t *testing.T) {
	forker := subagent.NewContextForker(subagent.DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "repair auth", UserRequest: "inspect token=secret-value",
			WorkspaceRules: []string{"authorization: hidden-value"},
			Messages: []subagent.ContextMessage{{
				Role: "assistant", Turn: 1,
				Blocks: []subagent.ContextBlock{{
					Kind: "text", Text: "unrelated transcript",
				}},
			}},
		},
	})
	request := contextRequest("")
	request.Agent.TaskName = "inspect_auth token=task-secret"
	fork, err := forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fork.Receipt.Mode != subagent.ContextTaskCapsule ||
		fork.Receipt.SourceTurn != "turn-parent" {
		t.Fatalf("receipt = %+v", fork.Receipt)
	}
	if !strings.Contains(fork.Prompt, "[REDACTED]") ||
		!strings.Contains(fork.Prompt, "parent fields are context") ||
		strings.Contains(fork.Prompt, "secret-value") ||
		strings.Contains(fork.Prompt, "hidden-value") ||
		strings.Contains(fork.Prompt, "task-secret") ||
		strings.Contains(fork.Prompt, "unrelated transcript") {
		t.Fatalf("task capsule = %s", fork.Prompt)
	}
	if len(fork.Receipt.Digest) != 64 ||
		fork.Receipt.Bytes > fork.Receipt.MaxBytes ||
		fork.Receipt.TokenEstimate > int(fork.Receipt.MaxTokens) {
		t.Fatalf("receipt budget = %+v", fork.Receipt)
	}
}

func TestTaskCapsuleReportsNormalizedAgentBudget(t *testing.T) {
	request := contextRequest(subagent.ContextFresh)
	request.Agent.Budget = subagent.AgentBudget{
		MaxSteps: 8, MaxTokens: 40_000, MaxCostUSD: 0.25,
	}
	request.Role.DefaultBudget = subagent.Budget{
		MaxSteps: 12, MaxTokens: 200_000, MaxCostUSD: 1,
		MaxDepth: 2, MaxParallel: 4,
	}
	fork, err := subagent.NewContextForker(
		subagent.DefaultContextPolicy(),
	).Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	limits := fork.Capsule.Limits
	if limits.MaxSteps != 8 || limits.MaxTokens != 40_000 ||
		limits.MaxCostUSD != 0.25 ||
		limits.MaxDepth != 2 || limits.MaxParallel != 4 {
		t.Fatalf("task capsule limits = %+v", limits)
	}
}

func TestTaskCapsuleUsesParentRemainingCapacityAndChildBudget(t *testing.T) {
	forker := subagent.NewContextForker(subagent.DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{snapshot: subagent.ParentContextSnapshot{
		SourceThread: "thread-parent", SourceTurn: "turn-parent",
		AvailableTokens: 600,
	}})
	request := contextRequest(subagent.ContextTaskCapsule)
	fork, err := forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fork.Receipt.MaxTokens != 600 || fork.Receipt.MaxBytes != 2400 {
		t.Fatalf("parent-derived receipt = %+v", fork.Receipt)
	}
	request.Agent.Budget.MaxTokens = 400
	fork, err = forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fork.Receipt.MaxTokens != 400 || fork.Receipt.MaxBytes != 1600 {
		t.Fatalf("child-bounded receipt = %+v", fork.Receipt)
	}
}

func TestLastNTurnsKeepsOnlyCompleteToolPairs(t *testing.T) {
	forker := subagent.NewContextForker(subagent.DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			Messages: []subagent.ContextMessage{
				{
					Role: "assistant", Turn: 1,
					Blocks: []subagent.ContextBlock{{
						Kind: "tool_call", CallID: "old-pair", ToolName: "old_read",
					}},
				},
				{
					Role: "tool", Turn: 1,
					Blocks: []subagent.ContextBlock{{
						Kind: "tool_result", CallID: "old-pair", Text: "old body",
					}},
				},
				{
					Role: "assistant", Turn: 2,
					Blocks: []subagent.ContextBlock{
						{Kind: "text", Text: "current"},
						{Kind: "tool_call", CallID: "paired", ToolName: "file_read", Arguments: `{"path":"a.go"}`},
						{Kind: "tool_call", CallID: "orphan-call", ToolName: "exec_command"},
					},
				},
				{
					Role: "tool", Turn: 2,
					Blocks: []subagent.ContextBlock{
						{Kind: "tool_result", CallID: "paired", Text: "file body"},
						{Kind: "tool_result", CallID: "orphan-result", Text: "must drop"},
					},
				},
			},
		},
	})
	request := contextRequest(subagent.ContextLastNTurns)
	request.LastTurns = 1
	fork, err := forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(fork.Capsule.RecentTurns) != 1 ||
		len(fork.Capsule.RecentTurns[0].Tools) != 1 ||
		fork.Capsule.RecentTurns[0].Tools[0].Name != "file_read" {
		t.Fatalf("recent turns = %+v", fork.Capsule.RecentTurns)
	}
	if strings.Contains(fork.Prompt, "orphan-call") ||
		strings.Contains(fork.Prompt, "must drop") {
		t.Fatalf("orphan exchange leaked: %s", fork.Prompt)
	}
}

func TestFullContextRequiresAuthorityOrRolePolicy(t *testing.T) {
	forker := subagent.NewContextForker(subagent.DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{})
	request := contextRequest(subagent.ContextFull)
	for _, trigger := range []subagent.DelegationTrigger{
		"", subagent.TriggerAdaptive, "unknown",
	} {
		request.Trigger = trigger
		if _, err := forker.Fork(t.Context(), request); err == nil {
			t.Fatalf("full context accepted trigger %q without role policy", trigger)
		}
	}
	request.Trigger = subagent.TriggerUser
	if _, err := forker.Fork(t.Context(), request); err != nil {
		t.Fatalf("user-authorized full context: %v", err)
	}
	request.Trigger = subagent.TriggerAdaptive
	request.Role.FullContext = true
	if _, err := forker.Fork(t.Context(), request); err != nil {
		t.Fatalf("role-authorized full context: %v", err)
	}
}

func TestContextBudgetIsDeterministicAndUTF8Safe(t *testing.T) {
	policy := subagent.DefaultContextPolicy()
	policy.MaxBytes = 1400
	policy.MaxTokens = 350
	forker := subagent.NewContextForker(policy)
	snapshot := subagent.ParentContextSnapshot{
		SourceThread: "thread-parent", SourceTurn: "turn-parent",
		ParentGoal:  strings.Repeat("目标", 400),
		UserRequest: strings.Repeat("请求", 400),
	}
	for index := 0; index < 20; index++ {
		snapshot.RelevantFiles = append(snapshot.RelevantFiles, subagent.RelevantFile{
			Path: strings.Repeat("路径", 20),
		})
		snapshot.Evidence = append(snapshot.Evidence, subagent.EvidenceSummary{
			Summary: strings.Repeat("证据", 40), Handle: "evidence://item",
		})
	}
	forker.BindSource(contextFixtureSource{snapshot: snapshot})
	first, err := forker.Fork(t.Context(), contextRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	second, err := forker.Fork(t.Context(), contextRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if first.Receipt.Digest != second.Receipt.Digest ||
		first.Receipt.Bytes > 1400 ||
		!utf8.ValidString(first.Prompt) {
		t.Fatalf("first=%+v second=%+v", first.Receipt, second.Receipt)
	}
	if !hasExcludedReason(first.Receipt.Excluded, "context budget") {
		t.Fatalf("budget exclusions = %+v", first.Receipt.Excluded)
	}
}

func TestContextModesMatchGolden(t *testing.T) {
	forker := subagent.NewContextForker(subagent.DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "parent goal", UserRequest: "current request token=secret",
			RelevantFiles: []subagent.RelevantFile{{
				Path:    "internal/runtime/app/runtime.go",
				Sources: []string{"tool_read"}, Critical: true,
			}},
			Evidence: []subagent.EvidenceSummary{{
				Summary: "runtime owns the turn", Handle: "evidence://parent/1",
			}},
			WorkspaceRules: []string{"keep hosts thin"},
			Messages: []subagent.ContextMessage{
				{
					Role: "user", Turn: 1,
					Blocks: []subagent.ContextBlock{{Kind: "text", Text: "old request"}},
				},
				{
					Role: "assistant", Turn: 1,
					Blocks: []subagent.ContextBlock{
						{Kind: "text", Text: "old answer"},
						{Kind: "tool_call", CallID: "old", ToolName: "read", Arguments: `{"path":"old.go"}`},
					},
				},
				{
					Role: "tool", Turn: 1,
					Blocks: []subagent.ContextBlock{{Kind: "tool_result", CallID: "old", Text: "old body"}},
				},
				{
					Role: "user", Turn: 2,
					Blocks: []subagent.ContextBlock{{Kind: "text", Text: "current request"}},
				},
				{
					Role: "assistant", Turn: 2,
					Blocks: []subagent.ContextBlock{
						{Kind: "text", Text: "current answer"},
						{Kind: "tool_call", CallID: "current", ToolName: "search", Arguments: `{"query":"owner"}`},
						{Kind: "tool_call", CallID: "orphan", ToolName: "read"},
					},
				},
				{
					Role: "tool", Turn: 2,
					Blocks: []subagent.ContextBlock{
						{Kind: "tool_result", CallID: "current", Text: "current body"},
						{Kind: "tool_result", CallID: "result-only", Text: "drop me"},
					},
				},
			},
		},
	})
	type goldenEntry struct {
		Mode           subagent.ContextMode       `json:"mode"`
		SourceThread   string                     `json:"source_thread,omitempty"`
		SourceTurn     string                     `json:"source_turn,omitempty"`
		ParentGoal     string                     `json:"parent_goal,omitempty"`
		UserRequest    string                     `json:"user_request,omitempty"`
		RelevantFiles  []subagent.RelevantFile    `json:"relevant_files,omitempty"`
		Evidence       []subagent.EvidenceSummary `json:"evidence,omitempty"`
		WorkspaceRules []string                   `json:"workspace_rules,omitempty"`
		RecentTurns    []subagent.ContextTurn     `json:"recent_turns,omitempty"`
		Included       []subagent.ContextItem     `json:"included"`
		Excluded       []subagent.ContextItem     `json:"excluded"`
	}
	modes := []subagent.ContextMode{
		subagent.ContextFresh,
		subagent.ContextTaskCapsule,
		subagent.ContextLastNTurns,
		subagent.ContextFull,
	}
	entries := make([]goldenEntry, 0, len(modes))
	for _, mode := range modes {
		request := contextRequest(mode)
		request.LastTurns = 1
		fork, err := forker.Fork(t.Context(), request)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		entries = append(entries, goldenEntry{
			Mode:           mode,
			SourceThread:   fork.Capsule.SourceThread,
			SourceTurn:     fork.Capsule.SourceTurn,
			ParentGoal:     fork.Capsule.ParentGoal,
			UserRequest:    fork.Capsule.UserRequest,
			RelevantFiles:  fork.Capsule.RelevantFiles,
			Evidence:       fork.Capsule.Evidence,
			WorkspaceRules: fork.Capsule.WorkspaceRules,
			RecentTurns:    fork.Capsule.RecentTurns,
			Included:       fork.Receipt.Included,
			Excluded:       fork.Receipt.Excluded,
		})
	}
	got, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "context_modes.golden.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\ngot:\n%s", path, err, got)
	}
	if string(got) != string(want) {
		t.Fatalf("context mode golden mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTaskContractSurvivesIntactWithinBudget(t *testing.T) {
	tail := "最终约束：保持公共 API 不变。"
	unit := "修复计划与验收标准细节描述。"
	target := 5234 - len(tail)
	body := strings.Repeat(unit, target/len(unit))
	for len(body)+3 <= target {
		body += "详"
	}
	objective := body + tail
	if len(objective) != 5234 {
		t.Fatalf("objective bytes = %d, want 5234", len(objective))
	}
	forker := subagent.NewContextForker(subagent.ContextPolicy{MaxBytes: 400_000})
	request := contextRequest(subagent.ContextFresh)
	request.Objective = objective
	request.Role.DefaultBudget = subagent.Budget{
		MaxTokens: 100_000, MaxDepth: 3, MaxParallel: 2,
	}
	fork, err := forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatalf("Fork() error = %v", err)
	}
	if fork.Capsule.Objective != objective ||
		!strings.Contains(fork.Prompt, "保持公共 API") ||
		strings.Contains(fork.Capsule.Objective, "[truncated]") {
		t.Fatalf("task contract was clipped: %d bytes kept of %d",
			len(fork.Capsule.Objective), len(objective))
	}
	for _, item := range fork.Receipt.Excluded {
		if item.Kind == "objective" || item.Kind == "expected_output" {
			t.Fatalf("task contract excluded: %+v", item)
		}
	}
}

func TestTaskContractRejectsWhenItCannotFit(t *testing.T) {
	objective := strings.Repeat("验收标准与迁移细节描述。", 200) // 7200 bytes
	forker := subagent.NewContextForker(subagent.ContextPolicy{MaxBytes: 2_000})
	request := contextRequest(subagent.ContextFresh)
	request.Objective = objective
	_, err := forker.Fork(t.Context(), request)
	if err == nil {
		t.Fatal("oversized task contract was accepted")
	}
	if !strings.Contains(err.Error(), "does not fit") ||
		strings.Contains(err.Error(), "[truncated]") {
		t.Fatalf("rejection = %v", err)
	}
}

func contextRequest(mode subagent.ContextMode) subagent.ContextRequest {
	return subagent.ContextRequest{
		Mode: mode,
		Source: subagent.ContextSourceRef{
			ThreadID: "thread-parent", TurnID: "turn-parent",
		},
		Agent: subagent.Agent{
			TaskName: "inspect_auth", Role: subagent.RoleExplore,
			Profile: "explore", Stance: subagent.StanceReadOnly,
			ExpectedOutput:   "key files and evidence",
			RoleInstructions: "inspect only",
		},
		Role: subagent.RoleSpec{
			Role: subagent.RoleExplore, Profile: "explore",
			Stance:       subagent.StanceReadOnly,
			AllowedTools: []string{"read", "search"},
			DefaultBudget: subagent.Budget{
				MaxTokens: 1000, MaxDepth: 3, MaxParallel: 2,
			},
		},
		Objective: "inspect auth", Trigger: subagent.TriggerUser,
	}
}

func hasExcludedReason(items []subagent.ContextItem, reason string) bool {
	for _, item := range items {
		if item.Reason == reason {
			return true
		}
	}
	return false
}

func TestTaskCapsuleCarriesRedactedFileExcerpts(t *testing.T) {
	forker := subagent.NewContextForker(subagent.DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "repair auth",
			RelevantFiles: []subagent.RelevantFile{{
				Path:    "pkg/auth.go",
				Sources: []string{"read"},
				Excerpt: "package auth\n// token=secret-value " +
					strings.Repeat("body ", 1024),
			}},
		},
	})
	fork, err := forker.Fork(t.Context(), contextRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fork.Prompt, "pkg/auth.go") ||
		!strings.Contains(fork.Prompt, "package auth") {
		t.Fatalf("capsule lost the file excerpt: %s", fork.Prompt)
	}
	if strings.Contains(fork.Prompt, "secret-value") {
		t.Fatalf("capsule leaked a secret through the excerpt: %s", fork.Prompt)
	}
	for _, file := range fork.Capsule.RelevantFiles {
		if file.Excerpt != "" &&
			len(file.Excerpt) > contextfork.MaxRelevantFileExcerptBytes {
			t.Fatalf("excerpt exceeds the delegation bound: %d", len(file.Excerpt))
		}
	}
	var excerptItem bool
	for _, item := range fork.Receipt.Included {
		if item.Kind == "relevant_file" && item.Bytes > 0 {
			excerptItem = true
		}
	}
	if !excerptItem {
		t.Fatalf("receipt did not account for excerpt bytes: %+v",
			fork.Receipt.Included,
		)
	}
}

func TestTaskCapsuleStripsExcerptsBeforeDroppingFiles(t *testing.T) {
	forker := subagent.NewContextForker(subagent.ContextPolicy{
		MaxBytes: 1200, MaxFiles: 2,
	})
	forker.BindSource(contextFixtureSource{
		snapshot: subagent.ParentContextSnapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "repair auth",
			RelevantFiles: []subagent.RelevantFile{
				{Path: "pkg/auth.go", Excerpt: strings.Repeat("a ", 600)},
				{Path: "pkg/token.go", Excerpt: strings.Repeat("b ", 600)},
			},
		},
	})
	fork, err := forker.Fork(t.Context(), contextRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(fork.Capsule.RelevantFiles) != 2 {
		t.Fatalf("budget dropped a whole file: %+v", fork.Capsule.RelevantFiles)
	}
	for _, file := range fork.Capsule.RelevantFiles {
		if file.Excerpt != "" {
			t.Fatalf("budget kept excerpts: %+v", fork.Capsule.RelevantFiles)
		}
	}
	sawExcerptStrip := false
	for _, item := range fork.Receipt.Excluded {
		if item.Kind == "relevant_file_excerpt" {
			sawExcerptStrip = true
		}
	}
	if !sawExcerptStrip {
		t.Fatalf("receipt missing excerpt exclusions: %+v", fork.Receipt.Excluded)
	}
	if len(fork.Prompt) > 1200 {
		t.Fatalf("prompt exceeds budget: %d", len(fork.Prompt))
	}
}
