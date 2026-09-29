package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultLanguageServerSelection(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"gopls", "clangd", "pyright-langserver"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	tests := map[string]string{
		"main.go": "gopls", "main.cpp": "clangd", "types.hpp": "clangd",
		"app.py": "pyright",
	}
	for path, want := range tests {
		spec, err := ResolveServer(path)
		if err != nil {
			t.Fatalf("ResolveServer(%q): %v", path, err)
		}
		if spec.Name != want {
			t.Fatalf("ResolveServer(%q) = %q, want %q", path, spec.Name, want)
		}
	}
	servers := strings.Join(AvailableServers(), ",")
	if servers != "clangd,gopls,pyright" {
		t.Fatalf("available servers = %q", servers)
	}
}

func TestDefaultLanguageServerSelectionRejectsMissingAndMixed(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"gopls", "clangd"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	if _, err := ResolveServer("main.rs"); err == nil ||
		!strings.Contains(err.Error(), "rust-analyzer") {
		t.Fatalf("missing server error = %v", err)
	}
	if _, err := (Checker{}).forPaths([]string{"main.go", "main.cpp"}); err == nil ||
		!strings.Contains(err.Error(), "cannot mix") {
		t.Fatalf("mixed server error = %v", err)
	}
}
