package subagent

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/common/contextsnapshot"
)

type contextFixtureSource struct {
	snapshot contextsnapshot.Snapshot
}

func (s contextFixtureSource) Snapshot(
	context.Context,
	contextsnapshot.SourceRef,
) (contextsnapshot.Snapshot, error) {
	return s.snapshot, nil
}

func TestTaskCapsuleRedactsAndExcludesParentTranscript(t *testing.T) {
	forker := NewContextForker(DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: contextsnapshot.Snapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "repair auth", UserRequest: "inspect token=secret-value",
			WorkspaceRules: []string{"authorization: hidden-value"},
			Messages: []contextsnapshot.Message{{
				Role: "assistant", Turn: 1,
				Blocks: []contextsnapshot.Block{{
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
	if fork.Receipt.Mode != ContextTaskCapsule ||
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
		fork.Receipt.MaxBytes > 0 && fork.Receipt.Bytes > fork.Receipt.MaxBytes ||
		fork.Receipt.TokenEstimate > int(fork.Receipt.MaxTokens) {
		t.Fatalf("receipt budget = %+v", fork.Receipt)
	}
}

func TestTaskCapsuleReportsNormalizedAgentBudget(t *testing.T) {
	request := contextRequest(ContextFresh)
	request.Agent.Budget = AgentBudget{
		MaxSteps: 8, MaxTokens: 40_000, MaxCostUSD: 0.25,
	}
	request.Role.DefaultBudget = Budget{
		MaxSteps: 12, MaxTokens: 200_000, MaxCostUSD: 1,
		MaxDepth: 2, MaxParallel: 4,
	}
	fork, err := NewContextForker(
		DefaultContextPolicy(),
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
	forker := NewContextForker(DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{snapshot: contextsnapshot.Snapshot{
		SourceThread: "thread-parent", SourceTurn: "turn-parent",
		AvailableTokens: 600,
	}})
	request := contextRequest(ContextTaskCapsule)
	fork, err := forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fork.Receipt.MaxTokens != 600 || fork.Receipt.MaxBytes != 0 {
		t.Fatalf("parent-derived receipt = %+v", fork.Receipt)
	}
	request.Agent.Budget.MaxTokens = 400
	fork, err = forker.Fork(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fork.Receipt.MaxTokens != 400 || fork.Receipt.MaxBytes != 0 {
		t.Fatalf("child-bounded receipt = %+v", fork.Receipt)
	}
}

func TestLastNTurnsKeepsOnlyCompleteToolPairs(t *testing.T) {
	forker := NewContextForker(DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: contextsnapshot.Snapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			Messages: []contextsnapshot.Message{
				{
					Role: "assistant", Turn: 1,
					Blocks: []contextsnapshot.Block{{
						Kind: "tool_call", CallID: "old-pair", ToolName: "old_read",
					}},
				},
				{
					Role: "tool", Turn: 1,
					Blocks: []contextsnapshot.Block{{
						Kind: "tool_result", CallID: "old-pair", Text: "old body",
					}},
				},
				{
					Role: "assistant", Turn: 2,
					Blocks: []contextsnapshot.Block{
						{Kind: "text", Text: "current"},
						{Kind: "tool_call", CallID: "paired", ToolName: "file_read", Arguments: `{"path":"a.go"}`},
						{Kind: "tool_call", CallID: "orphan-call", ToolName: "exec_command"},
					},
				},
				{
					Role: "tool", Turn: 2,
					Blocks: []contextsnapshot.Block{
						{Kind: "tool_result", CallID: "paired", Text: "file body"},
						{Kind: "tool_result", CallID: "orphan-result", Text: "must drop"},
					},
				},
			},
		},
	})
	request := contextRequest(ContextLastNTurns)
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
	forker := NewContextForker(DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{})
	request := contextRequest(ContextFull)
	for _, trigger := range []DelegationTrigger{
		"", TriggerAdaptive, "unknown",
	} {
		request.Trigger = trigger
		if _, err := forker.Fork(t.Context(), request); err == nil {
			t.Fatalf("full context accepted trigger %q without role policy", trigger)
		}
	}
	request.Trigger = TriggerUser
	if _, err := forker.Fork(t.Context(), request); err != nil {
		t.Fatalf("user-authorized full context: %v", err)
	}
	request.Trigger = TriggerAdaptive
	request.Role.FullContext = true
	if _, err := forker.Fork(t.Context(), request); err != nil {
		t.Fatalf("role-authorized full context: %v", err)
	}
}

func TestContextBudgetIsDeterministicAndUTF8Safe(t *testing.T) {
	policy := DefaultContextPolicy()
	policy.MaxBytes = 1400
	policy.MaxTokens = 350
	forker := NewContextForker(policy)
	snapshot := contextsnapshot.Snapshot{
		SourceThread: "thread-parent", SourceTurn: "turn-parent",
		ParentGoal:  "完整目标",
		UserRequest: "完整请求",
	}
	for index := 0; index < 20; index++ {
		snapshot.RelevantFiles = append(snapshot.RelevantFiles, contextsnapshot.RelevantFile{
			Path: strings.Repeat("路径", 20),
		})
		snapshot.Evidence = append(snapshot.Evidence, contextsnapshot.Evidence{
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
	forker := NewContextForker(DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: contextsnapshot.Snapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "parent goal", UserRequest: "current request token=secret",
			RelevantFiles: []contextsnapshot.RelevantFile{{
				Path:    "internal/runtime/app/runtime.go",
				Sources: []string{"tool_read"}, Critical: true,
			}},
			Evidence: []contextsnapshot.Evidence{{
				Summary: "runtime owns the turn", Handle: "evidence://parent/1",
			}},
			WorkspaceRules: []string{"keep hosts thin"},
			Messages: []contextsnapshot.Message{
				{
					Role: "user", Turn: 1,
					Blocks: []contextsnapshot.Block{{Kind: "text", Text: "old request"}},
				},
				{
					Role: "assistant", Turn: 1,
					Blocks: []contextsnapshot.Block{
						{Kind: "text", Text: "old answer"},
						{Kind: "tool_call", CallID: "old", ToolName: "read", Arguments: `{"path":"old.go"}`},
					},
				},
				{
					Role: "tool", Turn: 1,
					Blocks: []contextsnapshot.Block{{Kind: "tool_result", CallID: "old", Text: "old body"}},
				},
				{
					Role: "user", Turn: 2,
					Blocks: []contextsnapshot.Block{{Kind: "text", Text: "current request"}},
				},
				{
					Role: "assistant", Turn: 2,
					Blocks: []contextsnapshot.Block{
						{Kind: "text", Text: "current answer"},
						{Kind: "tool_call", CallID: "current", ToolName: "search", Arguments: `{"query":"owner"}`},
						{Kind: "tool_call", CallID: "orphan", ToolName: "read"},
					},
				},
				{
					Role: "tool", Turn: 2,
					Blocks: []contextsnapshot.Block{
						{Kind: "tool_result", CallID: "current", Text: "current body"},
						{Kind: "tool_result", CallID: "result-only", Text: "drop me"},
					},
				},
			},
		},
	})
	type goldenEntry struct {
		Mode           ContextMode                    `json:"mode"`
		SourceThread   string                         `json:"source_thread,omitempty"`
		SourceTurn     string                         `json:"source_turn,omitempty"`
		ParentGoal     string                         `json:"parent_goal,omitempty"`
		UserRequest    string                         `json:"user_request,omitempty"`
		RelevantFiles  []contextsnapshot.RelevantFile `json:"relevant_files,omitempty"`
		Evidence       []contextsnapshot.Evidence     `json:"evidence,omitempty"`
		WorkspaceRules []string                       `json:"workspace_rules,omitempty"`
		RecentTurns    []ContextTurn                  `json:"recent_turns,omitempty"`
		Included       []ContextItem                  `json:"included"`
		Excluded       []ContextItem                  `json:"excluded"`
	}
	modes := []ContextMode{
		ContextFresh,
		ContextTaskCapsule,
		ContextLastNTurns,
		ContextFull,
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
	forker := NewContextForker(ContextPolicy{MaxBytes: 400_000})
	request := contextRequest(ContextFresh)
	request.Objective = objective
	request.Role.DefaultBudget = Budget{
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
	forker := NewContextForker(ContextPolicy{MaxBytes: 2_000})
	request := contextRequest(ContextFresh)
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

func contextRequest(mode ContextMode) ContextRequest {
	return ContextRequest{
		Mode: mode,
		Source: contextsnapshot.SourceRef{
			ThreadID: "thread-parent", TurnID: "turn-parent",
		},
		Agent: Agent{
			TaskName: "inspect_auth", Role: RoleExplore,
			Profile: "explore", Stance: StanceReadOnly,
			ExpectedOutput:   "key files and evidence",
			RoleInstructions: "inspect only",
		},
		Role: RoleSpec{
			Role: RoleExplore, Profile: "explore",
			Stance:       StanceReadOnly,
			AllowedTools: []string{"read", "search"},
			DefaultBudget: Budget{
				MaxTokens: 1000, MaxDepth: 3, MaxParallel: 2,
			},
		},
		Objective: "inspect auth", Trigger: TriggerUser,
	}
}

func hasExcludedReason(items []ContextItem, reason string) bool {
	for _, item := range items {
		if item.Reason == reason {
			return true
		}
	}
	return false
}

func TestTaskCapsuleCarriesRedactedFileExcerpts(t *testing.T) {
	forker := NewContextForker(DefaultContextPolicy())
	forker.BindSource(contextFixtureSource{
		snapshot: contextsnapshot.Snapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "repair auth",
			RelevantFiles: []contextsnapshot.RelevantFile{{
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
			len(file.Excerpt) > contextsnapshot.MaxRelevantFileExcerptBytes {
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
	forker := NewContextForker(ContextPolicy{
		MaxBytes: 1200, MaxFiles: 2,
	})
	forker.BindSource(contextFixtureSource{
		snapshot: contextsnapshot.Snapshot{
			SourceThread: "thread-parent", SourceTurn: "turn-parent",
			ParentGoal: "repair auth",
			RelevantFiles: []contextsnapshot.RelevantFile{
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

func TestSubagentDoesNotImportRuntimeImplementation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const runtimePath = "github.com/fwtllh-png/QCode/internal/runtime/"
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(imported, runtimePath) && imported != runtimePath+"protocol" {
				t.Errorf("%s imports %s: subagent must use shared contracts and injected interfaces", name, imported)
			}
		}
	}
}

func TestP4ContextModesPreserveSelectedReferences(t *testing.T) {
	for _, mode := range []ContextMode{ContextFresh, ContextTaskCapsule, ContextLastNTurns, ContextFull} {
		t.Run(string(mode), func(t *testing.T) {
			snapshot := contextsnapshot.Snapshot{SourceThread: "parent", SourceTurn: "last", References: []contextsnapshot.Reference{{
				SourceID: "report", SourceThread: "parent", SourceTurn: "old", ContentDigest: "source-digest",
				ItemIDs: []string{"item:2"}, Start: 10, End: 40, Text: "2. 保留原编号的定义 token=private-value",
			}}}
			for i := 1; i <= 20; i++ {
				snapshot.Messages = append(snapshot.Messages, contextsnapshot.Message{Role: "assistant", Turn: uint64(i), Blocks: []contextsnapshot.Block{{Kind: "text", Text: strings.Repeat("optional history ", 500)}}})
			}
			forker := NewContextForker(ContextPolicy{MaxBytes: 4096, MaxTokens: 10000})
			forker.BindSource(contextFixtureSource{snapshot: snapshot})
			request := contextRequest(mode)
			request.Role.FullContext = true
			fork, err := forker.Fork(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if mode == ContextFresh {
				if len(fork.Capsule.References) != 0 {
					t.Fatal("fresh inherited parent definitions")
				}
				return
			}
			if len(fork.Capsule.References) != 1 {
				t.Fatal("budget dropped necessary definition")
			}
			ref := fork.Capsule.References[0]
			if !strings.Contains(ref.Text, "保留原编号的定义") || strings.Contains(ref.Text, "private-value") || !ref.Redacted || ref.ContentDigest != "source-digest" || ref.ItemIDs[0] != "item:2" {
				t.Fatalf("invalid inherited reference: %+v", ref)
			}
			fork.Capsule.References[0].ItemIDs[0] = "mutated"
			if snapshot.References[0].ItemIDs[0] != "item:2" {
				t.Fatal("child mutated parent reference")
			}
			if fork.Receipt.MaxTokens != request.Role.DefaultBudget.MaxTokens {
				t.Fatal("explicit policy bypassed child budget")
			}
		})
	}
}

func TestP4ContextRefusesToClipRequiredMaterial(t *testing.T) {
	for _, kind := range []string{"reference", "user_request", "parent_goal", "role_instructions", "workspace_rule"} {
		t.Run(kind, func(t *testing.T) {
			text := strings.Repeat("不可静默丢失的约束", 500)
			snapshot := contextsnapshot.Snapshot{}
			request := contextRequest(ContextTaskCapsule)
			switch kind {
			case "reference":
				snapshot.References = []contextsnapshot.Reference{{SourceID: "report", Text: text}}
			case "user_request":
				snapshot.UserRequest = text
			case "parent_goal":
				snapshot.ParentGoal = text
			case "role_instructions":
				request.Agent.RoleInstructions = text
			case "workspace_rule":
				snapshot.WorkspaceRules = []string{text}
			}
			forker := NewContextForker(ContextPolicy{MaxBytes: 2000})
			forker.BindSource(contextFixtureSource{snapshot: snapshot})
			if _, err := forker.Fork(t.Context(), request); err == nil || !strings.Contains(err.Error(), "does not fit") {
				t.Fatalf("required material clipped: %v", err)
			}
		})
	}
}
