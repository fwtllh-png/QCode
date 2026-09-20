package prompt

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fwtllh-png/QCode/internal/adapter/memory"
)

func TestAssembleStableOrderAndWorkspaceBoundaries(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".qcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("root rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".qcode", "instructions.md"), []byte("local rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(workspace), "AGENTS.md")
	if err := os.WriteFile(outside, []byte("must not load"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	context, err := Assemble(Options{
		BaseSystem: "base", Workspace: workspace, ToolPrefix: "tools",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"base", "root rules", "local rules", "tools"}
	if len(context.Messages) != len(want) {
		t.Fatalf("messages = %+v", context.Messages)
	}
	for index, text := range want {
		if context.Messages[index].Text() != text {
			t.Fatalf("message %d = %q, want %q", index, context.Messages[index].Text(), text)
		}
	}
}

func TestAssembleBudgetsAreDeterministicUTF8SafeAndReceipted(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("repository-rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := Options{
		BaseSystem: "你好世界-base",
		Workspace:  workspace,
		ToolPrefix: "tool-prefix",
		Budgets: map[string]Budget{
			PartitionBase:       {MaxBytes: 7},
			PartitionRepository: {MaxBytes: 8},
			PartitionToolPrefix: {MaxTokens: 1},
		},
	}
	first, err := Assemble(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Assemble(options)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Receipts) != 3 {
		t.Fatalf("receipts = %+v", first.Receipts)
	}
	for index, receipt := range first.Receipts {
		if receipt.Digest == "" || receipt.OriginalBytes < receipt.RetainedBytes ||
			receipt.OriginalTokens < receipt.RetainedTokens {
			t.Fatalf("receipt %d = %+v", index, receipt)
		}
		if !reflect.DeepEqual(receipt, second.Receipts[index]) {
			t.Fatalf("receipt is not deterministic: %+v != %+v", receipt, second.Receipts[index])
		}
	}
	if !first.Receipts[0].Truncated ||
		first.Receipts[0].TruncationReason != "byte_budget" ||
		!first.Receipts[1].Truncated ||
		first.Receipts[1].TruncationReason != "byte_budget" ||
		!first.Receipts[2].Truncated ||
		first.Receipts[2].TruncationReason != "token_budget" {
		t.Fatalf("truncation receipts = %+v", first.Receipts)
	}
	for _, message := range first.Messages {
		if !utf8.ValidString(message.Text()) {
			t.Fatalf("invalid UTF-8 retained message %q", message.Text())
		}
	}
}

func TestAssembleWorkingSetInjectionCanonicalizationAndSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	firstPath := filepath.Join(workspace, "a.go")
	secondPath := filepath.Join(workspace, "b.go")
	if err := os.WriteFile(firstPath, []byte("disk-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("disk-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstPath, err := filepath.EvalSymlinks(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err = filepath.EvalSymlinks(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	injected := "unsaved-b"
	newFile := "package newfile"
	context, err := Assemble(Options{
		Workspace: workspace,
		WorkingSet: []FileContext{
			{Path: "b.go", Content: &injected, Critical: true},
			{Path: "a.go"},
			{Path: "c.go", Content: &newFile},
		},
		Budgets: map[string]Budget{PartitionWorkingSet: {MaxBytes: 1 << 10}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(context.WorkingSet) != 3 ||
		context.WorkingSet[0] != firstPath ||
		context.WorkingSet[1] != secondPath ||
		!strings.HasSuffix(context.WorkingSet[2], string(filepath.Separator)+"c.go") ||
		len(context.CriticalPaths) != 1 ||
		context.CriticalPaths[0] != secondPath {
		t.Fatalf("working set = %+v critical=%+v", context.WorkingSet, context.CriticalPaths)
	}
	if !strings.Contains(context.Messages[1].Text(), "unsaved-b") ||
		strings.Contains(context.Messages[1].Text(), "disk-b") {
		t.Fatalf("host-injected context was not used: %q", context.Messages[1].Text())
	}
	if !strings.Contains(context.Messages[2].Text(), "package newfile") {
		t.Fatalf("new host-injected file was not used: %q", context.Messages[2].Text())
	}
	for _, receipt := range context.Receipts {
		if receipt.Kind != PartitionWorkingSet {
			continue
		}
		if !filepath.IsAbs(receipt.SourcePath) {
			t.Fatalf("non-canonical receipt path = %q", receipt.SourcePath)
		}
	}

	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workspace, "escape.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Assemble(Options{
		Workspace: workspace, WorkingSet: []FileContext{{Path: "escape.go"}},
	}); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("symlink escape error = %v", err)
	}
}

func TestAssembleUserMemoryInjectsOnlyWhenEnabled(t *testing.T) {
	workspace := t.TempDir()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("prefer short diffs"); err != nil {
		t.Fatal(err)
	}
	absent, err := Assemble(Options{Workspace: workspace, Memory: store})
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range absent.Receipts {
		if receipt.Kind == PartitionUserMemory {
			t.Fatalf("disabled memory injected: %+v", receipt)
		}
	}
	context, err := Assemble(Options{
		Workspace: workspace, MemoryEnabled: true, Memory: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, message := range context.Messages {
		if strings.Contains(message.Text(), "<user_memory") &&
			strings.Contains(message.Text(), "prefer short diffs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("messages = %+v", context.Messages)
	}
}

func TestAssembleSharesCapacityAcrossPartitions(t *testing.T) {
	context, err := Assemble(Options{
		Workspace:  t.TempDir(),
		BaseSystem: "12345678",
		ToolPrefix: "abcdefgh",
		Budgets: map[string]Budget{
			PartitionTotal: {MaxTokens: 3},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var retained uint64
	for _, receipt := range context.Receipts {
		retained += receipt.RetainedTokens
	}
	if retained > 3 {
		t.Fatalf("retained tokens = %d, want shared ceiling 3", retained)
	}
}

func TestHeuristicTokenCounterCountsDenseScriptPerRune(t *testing.T) {
	counter := HeuristicTokenCounter{}
	if got := counter.Count("配置文件"); got != 4 {
		t.Fatalf("dense count = %d, want 4", got)
	}
	// Two dense runes plus seven ASCII characters (space included) at four
	// characters per token.
	if got := counter.Count("配置 config"); got != 4 {
		t.Fatalf("mixed count = %d, want 4", got)
	}
	if got := counter.Count(""); got != 0 {
		t.Fatalf("empty count = %d", got)
	}
}

// The global user layer and the CLAUDE.md compatibility fallback extend the
// instruction stack without double-injecting repositories that maintain both
// file families.
func TestAssembleInstructionLayers(t *testing.T) {
	t.Run("global layer appends after workspace rules", func(t *testing.T) {
		workspace := t.TempDir()
		home := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("repo rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".qcode"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".qcode", "AGENTS.md"), []byte("user rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		context, err := Assemble(Options{
			BaseSystem: "base", Workspace: workspace, Home: home,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"base", "repo rules", "user rules"}
		if len(context.Messages) != len(want) {
			t.Fatalf("messages = %+v", context.Messages)
		}
		for index, text := range want {
			if context.Messages[index].Text() != text {
				t.Fatalf("message %d = %q, want %q", index, context.Messages[index].Text(), text)
			}
		}
	})

	t.Run("CLAUDE.md is a fallback, not a duplicate", func(t *testing.T) {
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, "CLAUDE.md"), []byte("claude rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		context, err := Assemble(Options{BaseSystem: "base", Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		if len(context.Messages) != 2 || context.Messages[1].Text() != "claude rules" {
			t.Fatalf("messages = %+v", context.Messages)
		}

		both := t.TempDir()
		if err := os.WriteFile(filepath.Join(both, "AGENTS.md"), []byte("agents rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(both, "CLAUDE.md"), []byte("claude rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		context, err = Assemble(Options{BaseSystem: "base", Workspace: both})
		if err != nil {
			t.Fatal(err)
		}
		if len(context.Messages) != 2 || context.Messages[1].Text() != "agents rules" {
			t.Fatalf("both present injected twice: %+v", context.Messages)
		}
	})

	t.Run("global CLAUDE.md is the last resort", func(t *testing.T) {
		workspace := t.TempDir()
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("user claude rules"), 0o600); err != nil {
			t.Fatal(err)
		}
		context, err := Assemble(Options{
			BaseSystem: "base", Workspace: workspace, Home: home,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(context.Messages) != 2 || context.Messages[1].Text() != "user claude rules" {
			t.Fatalf("messages = %+v", context.Messages)
		}
	})
}

func TestDefaultBaseSystemCarriesPersonaWorkspaceAndEnvironment(t *testing.T) {
	value := DefaultBaseSystem("/work/repo", []string{
		"os: darwin (arm64)", "git 2.39.5", "", "go 1.26.3",
	})
	for _, want := range []string{
		"software engineering agent",
		"approval and sandbox policy",
		"workspace: /work/repo",
		"os: darwin (arm64)",
		"git 2.39.5",
		"go 1.26.3",
	} {
		if !strings.Contains(value, want) {
			t.Errorf("base system missing %q:\n%s", want, value)
		}
	}
	if !strings.HasSuffix(value, "go 1.26.3") {
		t.Errorf("blank environment lines leaked or trailing newline: %q", value)
	}
	if DefaultBaseSystem("", nil) == "" {
		t.Fatal("empty inputs produced an empty persona")
	}
}
