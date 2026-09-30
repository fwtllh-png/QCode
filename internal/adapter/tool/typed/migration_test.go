package typed_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTierOneToolsUseTypedBoundary(t *testing.T) {
	root := filepath.Clean("..")
	files := []string{
		"completion/completion.go",
		"lsp/lsp.go",
		"../memory/tools.go",
		"revert/revert.go",
		"../skill/adapter.go",
		"toolsearch/tool_search.go",
	}
	for _, relative := range files {
		t.Run(relative, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, relative))
			if err != nil {
				t.Fatal(err)
			}
			source := string(data)
			if !strings.Contains(source, "typed.Define(") {
				t.Fatal("Tier-1 tool does not construct a typed executor")
			}
			if strings.Contains(source, "json.Unmarshal(") {
				t.Fatal("Tier-1 tool reintroduced local JSON decoding")
			}
		})
	}
}

func TestTierTwoToolsUseTypedBoundaryOrDocumentException(t *testing.T) {
	root := filepath.Clean("..")
	cases := []struct {
		path          string
		typed         bool
		exception     bool
		forbidRawRoot bool
	}{
		{path: "shell/protocol.go", typed: true, forbidRawRoot: true},
		{path: "handle/handle.go", typed: true},
		{path: "shell/shell.go", typed: true, forbidRawRoot: true},
		{path: "../mcp/adapter.go", typed: true, exception: true},
	}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, test.path))
			if err != nil {
				t.Fatal(err)
			}
			source := string(data)
			if test.typed && !strings.Contains(source, "typed.Define(") {
				t.Fatal("Tier-2 tool lost its typed executor")
			}
			if test.exception &&
				!strings.Contains(source, "typed-boundary-exception:") {
				t.Fatal("Tier-2 explicit executor lacks a reviewed exception")
			}
			if test.forbidRawRoot &&
				(strings.Contains(source, "json.Unmarshal(raw") ||
					strings.Contains(source, "json.Unmarshal(arguments")) {
				t.Fatal("Tier-2 typed root reintroduced local JSON decoding")
			}
		})
	}
}

func TestRemainingBuiltinsEmbedTypedContract(t *testing.T) {
	root := filepath.Clean("..")
	files := []string{
		"agent/ops.go",
		"content/content.go",
		"file/file.go",
		"git/git.go",
		"interact/interact.go",
		"search/search.go",
		"search/symbol.go",
		"web/web.go",
		"web/browser.go",
	}
	for _, relative := range files {
		t.Run(relative, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, relative))
			if err != nil {
				t.Fatal(err)
			}
			source := string(data)
			if !strings.Contains(source, "typed.Contract[") ||
				!strings.Contains(source, "typed.NewResultContract(") {
				t.Fatal("builtin does not embed the typed execution contract")
			}
			if strings.Contains(source, "typed.ExecuteOutcome(") {
				t.Fatal("builtin retained the legacy Outcome adapter")
			}
		})
	}
}
