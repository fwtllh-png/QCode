package engine

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// CurrentTurnSpec returns an isolated copy of the active or most recently
// completed TurnSpec.
func (e *Engine) CurrentTurnSpec() TurnSpec {
	if scope := e.currentScope(); scope != nil {
		return scope.Spec()
	}
	return TurnSpec{}
}

// WorkspaceExcerpt reads a bounded prefix of a workspace-relative regular
// file for context delegation. It shares workspace facts only — never parent
// conversation — so a delegated child can start from the file's current
// content without re-reading it. Path escapes, missing files, and non-text
// prefixes report false instead of degrading the capsule.
func (e *Engine) WorkspaceExcerpt(relPath string, maxBytes int) (string, bool) {
	if e == nil || maxBytes <= 0 {
		return "", false
	}
	// Workspace is fixed at construction. Taking e.mu here would deadlock:
	// Execute holds it for the whole turn, including the spawn_agent tool call
	// that forks parent context through this method.
	root := e.options.Workspace
	if root == "" || relPath == "" {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(relPath))
	if filepath.IsAbs(clean) || clean == "." ||
		clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(clean)))
	if err != nil {
		return "", false
	}
	if len(data) > maxBytes {
		data = data[:maxBytes]
	}
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	text := strings.TrimSpace(string(data))
	// NUL is valid UTF-8 but only occurs in binary payloads; it is a
	// reliable sentinel that the prefix is not shareable text.
	if text == "" || strings.ContainsRune(text, 0) {
		return "", false
	}
	return text, true
}
