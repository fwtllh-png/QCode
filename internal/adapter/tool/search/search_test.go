package search

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/security/controlmatrix"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

func TestSearchTextScopedFileOverResultBudgetStillMatches(t *testing.T) {
	root := repositoryRoot(t)
	run(t, root, "git", "init", "-q")
	needle := "HandlePromise_unique_token"
	// Result budget 10_000 tokens allows 40_000 scan bytes. A scoped file just
	// over that cap must still be searched; empty matches previously looked like
	// a missing symbol and pushed the model into whole-file paging.
	write(t, filepath.Join(root, "core.cpp"), strings.Repeat("x", 45_000)+"\n"+needle+"\n")
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "search_text", map[string]any{
		"pattern": needle, "path": "core.cpp", "limit": 20,
	})
	matches := decodeMatches(t, result.Content)
	if len(matches) != 1 || matches[0]["file"] != "core.cpp" {
		t.Fatalf("scoped search = %s", result.Content)
	}
	if strings.Contains(result.Content, `"skipped"`) ||
		!strings.Contains(result.Content, "read-only analysis or edits") {
		t.Fatalf("scoped search = %s", result.Content)
	}
}

func TestSearchTextReportsSkippedLargeInBodyWhenEmpty(t *testing.T) {
	root := repositoryRoot(t)
	run(t, root, "git", "init", "-q")
	write(t, filepath.Join(root, "core.cpp"), strings.Repeat("HandlePromise\n", 80))
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "search_text", map[string]any{
		"query": "HandlePromise", "max_file_bytes": 40, "max_results": 10,
	})
	if !strings.Contains(result.Content, `"matches":[]`) ||
		!strings.Contains(result.Content, `"large":1`) {
		t.Fatalf("empty search hid skipped file: %s", result.Content)
	}
}

func TestSearchHonorsGitIgnoreFiltersAndFilePolicies(t *testing.T) {
	root := repositoryRoot(t)
	run(t, root, "git", "init", "-q")
	globalIgnore := filepath.Join(root, "global-ignore")
	write(t, globalIgnore, "global.txt\n")
	run(t, root, "git", "config", "core.excludesFile", globalIgnore)
	write(t, filepath.Join(root, ".gitignore"), "ignored.txt\nignored-dir/\n")
	write(t, filepath.Join(root, ".git", "info", "exclude"), "info.txt\n")
	for name, content := range map[string]string{
		"visible.txt":             "target\n",
		"excluded.txt":            "target\n",
		"ignored.txt":             "target\n",
		"ignored-dir/nested.txt":  "target\n",
		"info.txt":                "target\n",
		"global.txt":              "target\n",
		"nested/also-visible.txt": "target\n",
		"large.txt":               strings.Repeat("target", 100),
	} {
		write(t, filepath.Join(root, name), content)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.txt"), []byte("target\x00data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid.txt"), []byte{0xff, 't'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("visible.txt", filepath.Join(root, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "search_text", map[string]any{
		"query": "target", "include": []string{"*.txt"}, "exclude": []string{"excluded*"},
		"max_file_bytes": 100, "max_results": 10,
	})
	matches := decodeMatches(t, result.Content)
	if len(matches) != 2 ||
		matches[0]["file"] != "nested/also-visible.txt" ||
		matches[1]["file"] != "visible.txt" {
		t.Fatalf("search matches = %#v", matches)
	}
	// Ignored files never reach the walk, so they are absent from the matches
	// rather than counted: git enumerated the file set and its rules decided.
	if result.Metadata["enumeration"] != "git" {
		t.Fatalf("enumeration = %#v", result.Metadata["enumeration"])
	}
	for key := range map[string]struct{}{
		"skipped_binary": {}, "skipped_large": {},
		"skipped_encoding": {}, "skipped_symlink": {},
	} {
		if value, ok := result.Metadata[key].(int); !ok || value == 0 {
			t.Fatalf("%s = %#v", key, result.Metadata[key])
		}
	}
}

func TestSearchKeepsSkippingVendorDirectoriesGitWouldTrack(t *testing.T) {
	root := repositoryRoot(t)
	run(t, root, "git", "init", "-q")
	for name, content := range map[string]string{
		"main.go":                 "target\n",
		"vendor/dep/dep.go":       "target\n",
		"node_modules/pkg/mod.js": "target\n",
		"target/debug/deps.rs":    "target\n",
	} {
		write(t, filepath.Join(root, name), content)
	}
	run(t, root, "git", "add", ".")
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}

	result := execute(t, registry, "search_text", map[string]any{"query": "target"})
	matches := decodeMatches(t, result.Content)
	if len(matches) != 1 || matches[0]["file"] != "main.go" {
		t.Fatalf("matches = %#v", matches)
	}
	// Checked-in dependency trees and build artifacts are skipped by name
	// whatever git thinks of them, and that is what skipped_ignored now counts.
	if value, ok := result.Metadata["skipped_ignored"].(int); !ok || value != 3 {
		t.Fatalf("skipped_ignored = %#v", result.Metadata["skipped_ignored"])
	}
}

func TestSearchTextSkipsMultiplyLinkedFiles(t *testing.T) {
	root := repositoryRoot(t)
	run(t, root, "git", "init", "-q")
	write(t, filepath.Join(root, "kept.txt"), "needle\n")
	write(t, filepath.Join(root, "original.txt"), "needle\n")
	if err := os.Link(
		filepath.Join(root, "original.txt"),
		filepath.Join(root, "linked.txt"),
	); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "add", ".")
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "search_text", map[string]any{"query": "needle"})
	if result.IsError {
		t.Fatalf("multiply linked search failed: %s", result.Content)
	}
	matches := decodeMatches(t, result.Content)
	if len(matches) != 1 || matches[0]["file"] != "kept.txt" {
		t.Fatalf("matches = %#v content=%s", matches, result.Content)
	}
	if !strings.Contains(result.Content, `"linked":2`) &&
		!strings.Contains(result.Content, `"linked": 2`) {
		t.Fatalf("linked skip missing: %s", result.Content)
	}
}

func TestSearchResultOrderAndTruncationAreDeterministic(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "b.txt"), "hit\n")
	write(t, filepath.Join(root, "a.txt"), "hit\n")
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}
	result := execute(t, registry, "search_text", map[string]any{
		"query": "hit", "max_results": 1,
	})
	matches := decodeMatches(t, result.Content)
	if !result.Truncated || len(matches) != 1 || matches[0]["file"] != "a.txt" ||
		result.Metadata["matches"] != 2 || result.Metadata["returned"] != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestSearchReturnsContextCaseControlAndFuzzyFileRanking(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "src", "HTTPClient.go"), "before\nNeedle\nAFTER\n")
	write(t, filepath.Join(root, "src", "http_client_test.go"), "test\n")
	write(t, filepath.Join(root, "docs", "client-http.md"), "docs\n")
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}

	sensitive := execute(t, registry, "search_text", map[string]any{"query": "needle"})
	if matches := decodeMatches(t, sensitive.Content); len(matches) != 0 {
		t.Fatalf("case-sensitive matches = %#v", matches)
	}
	insensitive := execute(t, registry, "search_text", map[string]any{
		"query": "needle", "case_insensitive": true, "before": 1, "after": 1,
	})
	matches := decodeMatches(t, insensitive.Content)
	if len(matches) != 1 || matches[0]["line"] != float64(2) || matches[0]["text"] != "Needle" {
		t.Fatalf("text matches = %#v", matches)
	}
	contextValue := matches[0]["context"].(map[string]any)
	before := contextValue["before"].([]any)
	after := contextValue["after"].([]any)
	if before[0].(map[string]any)["text"] != "before" ||
		after[0].(map[string]any)["text"] != "AFTER" {
		t.Fatalf("context = %#v", contextValue)
	}

	files := execute(t, registry, "search_files", map[string]any{"query": "httpclient"})
	fileMatches := decodeMatches(t, files.Content)
	if len(fileMatches) < 2 || fileMatches[0]["path"] != "src/HTTPClient.go" {
		t.Fatalf("fuzzy files = %#v", fileMatches)
	}
	for index := 1; index < len(fileMatches); index++ {
		previous := int(fileMatches[index-1]["score"].(float64))
		current := int(fileMatches[index]["score"].(float64))
		if current > previous {
			t.Fatalf("fuzzy scores are not descending: %#v", fileMatches)
		}
	}

	regexFiles := execute(t, registry, "search_files", map[string]any{
		"query": `HTTPClient\.go$`, "regex": true,
	})
	regexMatches := decodeMatches(t, regexFiles.Content)
	if len(regexMatches) != 1 || regexMatches[0]["path"] != "src/HTTPClient.go" {
		t.Fatalf("regex files = %#v", regexMatches)
	}
}

func TestSearchProjectAcceptsCommonModelAliases(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "src", "a.go"), "needle alpha\n")
	write(t, filepath.Join(root, "src", "b.go"), "other\n")
	write(t, filepath.Join(root, "docs", "a.md"), "needle docs\n")
	registry := tool.NewRegistry(nil, nil)
	if err := RegisterWithBackend(registry, root, searchTestBackend{}); err != nil {
		t.Fatal(err)
	}

	result := execute(t, registry, "search_project", map[string]any{
		"pattern": "needle", "path": "src", "include": "*.go", "limit": 5,
		"context": 1, "case_sensitive": false, "description": "find needle",
	})
	matches := decodeMatches(t, result.Content)
	if len(matches) != 1 || matches[0]["file"] != "src/a.go" {
		t.Fatalf("alias search = %#v", matches)
	}

	globbed := execute(t, registry, "search_project", map[string]any{
		"query": "needle", "glob": "*.md",
	})
	if matches := decodeMatches(t, globbed.Content); len(matches) != 1 || matches[0]["file"] != "docs/a.md" {
		t.Fatalf("glob search = %#v", decodeMatches(t, globbed.Content))
	}
}

func TestSearchSchemaRequiresQueryOrPattern(t *testing.T) {
	schema := (&Tool{kind: "search_files"}).Descriptor().InputSchema
	if err := tool.ValidateArguments(schema, json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty search arguments passed schema validation")
	}
	for _, arguments := range []string{
		`{"query":"needle"}`,
		`{"pattern":"needle"}`,
	} {
		if err := tool.ValidateArguments(
			schema,
			json.RawMessage(arguments),
		); err != nil {
			t.Fatalf("arguments %s failed schema validation: %v", arguments, err)
		}
	}
}

func TestPatternDefaultsToRegexUnlessExplicitlyDisabled(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "pattern", raw: `{"pattern":"^# "}`, want: true},
		{name: "literal query", raw: `{"query":"^# "}`, want: false},
		{name: "explicit literal pattern", raw: `{"pattern":"^# ","regex":false}`, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := parseSearchInput(json.RawMessage(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if input.Regex != test.want {
				t.Fatalf("regex = %t, want %t", input.Regex, test.want)
			}
		})
	}
}

func decodeMatches(t *testing.T, content string) []map[string]any {
	t.Helper()
	var payload struct {
		Matches []map[string]any `json:"matches"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Matches
}

type searchTestBackend struct{}

func (searchTestBackend) Capability() sandbox.Capability {
	return sandbox.Capability{
		Platform: "test", Backend: "passthrough", Available: true,
		Effective: controlmatrix.Matrix{FilesystemRead: controlmatrix.FilesystemReadDeclaredRoots,

			FilesystemWrite: controlmatrix.
				FilesystemWriteExactPaths,

			Network:     controlmatrix.NetworkDenied,
			ProcessTree: controlmatrix.ProcessTreeGroupKill, CrossProcess: controlmatrix.
					CrossProcessUnrestricted, Syscall: controlmatrix.SyscallDenyDangerous,
			IPC: controlmatrix.
				IPCUnrestricted, PathIdentity: controlmatrix.PathIdentityDescriptorRelative, ArtifactOrigin: controlmatrix.ArtifactOriginUnverifiedPath,
			DurableRecovery: controlmatrix.DurableRecoveryMemoryOnly,
		},
	}
}

func (searchTestBackend) Prepare(_ context.Context, command sandbox.Command) (sandbox.Command, error) {
	return command, nil
}

func execute(t *testing.T, registry *tool.Registry, name string, input map[string]any) tool.Result {
	t.Helper()
	data, _ := json.Marshal(input)
	ctx := tool.WithResultTokenBudget(t.Context(), 10_000)
	result, err := tooltest.Execute(ctx, registry, tool.Call{
		Name: name, Arguments: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repositoryRoot returns a temporary directory for a real git repository. It is
// deliberately not t.TempDir: git wrappers in the wild keep writing under .git
// after the command returns (logs, telemetry), and testing's own cleanup fails
// the test when such a write lands between its walk and its rmdir. Removal is
// retried here instead, because a straggler write says nothing about the search
// behaviour under test.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "search-repository-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var removeErr error
		for attempt := range 5 {
			if removeErr = os.RemoveAll(root); removeErr == nil {
				return
			}
			time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
		}
		t.Logf("left %s behind: %v", root, removeErr)
	})
	return root
}

func run(t *testing.T, directory, name string, arguments ...string) {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, arguments, err, output)
	}
}
