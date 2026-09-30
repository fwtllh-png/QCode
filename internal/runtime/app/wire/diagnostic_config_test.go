package wire

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fwtllh-png/QCode/internal/observability/verify"
)

func TestDiagnosticCommandReadRootsIncludeScriptAndInterpreterTrees(t *testing.T) {
	root := t.TempDir()
	commandDir := filepath.Join(root, "bin")
	commandPackage := filepath.Join(root, "command-package")
	nodeBin := filepath.Join(root, "node", "bin")
	for _, directory := range []string{commandDir, commandPackage, nodeBin} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(commandPackage, "cli.mjs"): "#!/usr/bin/env node\n",
		filepath.Join(nodeBin, "node"):           "#!/bin/sh\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(
		filepath.Join(commandPackage, "cli.mjs"),
		filepath.Join(commandDir, "fixture-lint"),
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", commandDir+string(os.PathListSeparator)+nodeBin)

	roots := diagnosticCommandReadRoots(map[string]verify.DiagnosticCommand{
		".md": {Name: "fixture-lint", Args: []string{"{path}"}},
	})
	canonicalPackage, err := filepath.EvalSymlinks(commandPackage)
	if err != nil {
		t.Fatal(err)
	}
	canonicalNode, err := filepath.EvalSymlinks(filepath.Join(root, "node"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(roots, canonicalPackage) ||
		!slices.Contains(roots, canonicalNode) {
		t.Fatalf(
			"read roots %q do not contain command package %q and Node tree %q",
			roots,
			canonicalPackage,
			canonicalNode,
		)
	}
}

func TestDiagnosticDependencyReadRootsUsesStructuredLibraryClosure(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "bin", "node")
	first := filepath.Join(root, "opt", "llhttp", "lib", "libllhttp.dylib")
	second := filepath.Join(root, "opt", "uv", "lib", "libuv.dylib")
	for _, path := range []string{executable, first, second} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	canonicalExecutable, _ := filepath.EvalSymlinks(executable)
	canonicalFirst, _ := filepath.EvalSymlinks(first)
	graph := map[string][]string{
		canonicalExecutable: {first},
		canonicalFirst:      {second},
	}
	files := diagnosticDependencyReadFilesWith(
		executable,
		func(path string) ([]string, error) { return graph[path], nil },
	)
	for _, want := range []string{first, second} {
		want, _ = filepath.EvalSymlinks(want)
		if !slices.Contains(files, want) {
			t.Fatalf("dependency files %q do not contain %q", files, want)
		}
	}
}
