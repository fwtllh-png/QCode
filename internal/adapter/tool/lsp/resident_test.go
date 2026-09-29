package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/platform/symbols"
)

// The resident tests drive the fixture process the checker tests use: its
// lifecycle log records every notification and request, which is exactly what
// a pool has to prove — the second question reuses the first question's
// server instead of paying for another one.

func fixtureSpec(logPath string) ServerSpec {
	return ServerSpec{
		Name: "fixture", Binary: os.Args[0],
		Args: []string{
			"-test.run=^TestLSPFixtureProcess$",
			"-lsp-fixture-log=" + logPath,
		},
	}
}

func newResidentWithFixture(t *testing.T, options ResidentOptions) (*Resident, string) {
	t.Helper()
	root := t.TempDir()
	source := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "resident.log")
	resident := NewResident(Checker{Root: root, Sandbox: lspTestBackend{}}, options)
	resident.resolver = func(string) (ServerSpec, error) {
		return fixtureSpec(logPath), nil
	}
	t.Cleanup(func() { _ = resident.Close() })
	return resident, logPath
}

func countEvents(t *testing.T, logPath, event string) int {
	t.Helper()
	content, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(content), event+"\n")
}

func TestResidentReusesTheSessionAcrossQueries(t *testing.T) {
	resident, logPath := newResidentWithFixture(t, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	query := symbols.SemanticQuery{Path: "main.go", Line: 3, Character: 6}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := resident.Definition(t.Context(), query)
		if err != nil {
			t.Fatalf("definition %d: %v", attempt, err)
		}
		if result.Source != "lsp:fixture-lsp" || len(result.Locations) != 1 {
			t.Fatalf("definition %d = %+v", attempt, result)
		}
	}
	// One server answered both questions: initialize and didOpen appear once,
	// definition twice. The second query found the document already open with
	// the same digest, so no didOpen and no didChange.
	if got := countEvents(t, logPath, "initialize"); got != 1 {
		t.Fatalf("initialize count = %d, want 1 (the session is reused)", got)
	}
	if got := countEvents(t, logPath, "didOpen"); got != 1 {
		t.Fatalf("didOpen count = %d, want 1 (unchanged digest skips reopening)", got)
	}
	if got := countEvents(t, logPath, "didChange"); got != 0 {
		t.Fatalf("didChange count = %d, want 0", got)
	}
}

func TestResidentCachesRepeatedQueries(t *testing.T) {
	resident, logPath := newResidentWithFixture(t, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	query := symbols.SemanticQuery{Path: "main.go", Line: 3, Character: 6}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := resident.References(t.Context(), query, true); err != nil {
			t.Fatalf("references %d: %v", attempt, err)
		}
	}
	// Three identical questions, one server, one exchange: the second and
	// third answers came from the cache.
	if got := countEvents(t, logPath, "references"); got != 1 {
		t.Fatalf("references count = %d, want 1 (repeats served from cache)", got)
	}
}

func TestResidentSendsDidChangeWhenTheDocumentMoves(t *testing.T) {
	resident, logPath := newResidentWithFixture(t, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	root := filepath.Dir(logPath)
	query := symbols.SemanticQuery{Path: "main.go", Line: 3, Character: 6}
	if _, err := resident.Definition(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	edited := "package main\n\nfunc main() { changed() }\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resident.Definition(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(t, logPath, "didChange"); got != 1 {
		t.Fatalf("didChange count = %d, want 1 after the edit", got)
	}
}

func TestResidentRestartsAKilledSessionOnce(t *testing.T) {
	resident, _ := newResidentWithFixture(t, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	query := symbols.SemanticQuery{Path: "main.go", Line: 3, Character: 6}
	if _, err := resident.Definition(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	resident.mu.Lock()
	for _, session := range resident.sessions {
		if session.client.command.Process != nil {
			_ = session.client.command.Process.Kill()
		}
	}
	resident.mu.Unlock()
	// A crash with a successful query on either side is bad luck, not a
	// pattern: the next query restarts the server and answers.
	if _, err := resident.Definition(t.Context(), query); err != nil {
		t.Fatalf("query after the crash: %v", err)
	}
}

func TestResidentDisablesAServerThatCannotAnswer(t *testing.T) {
	root := t.TempDir()
	source := "package main\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fixture that matches no test exits at once: the process starts and
	// dies before the handshake, which is what a broken server looks like.
	resident := NewResident(Checker{Root: root, Sandbox: lspTestBackend{}}, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	resident.resolver = func(string) (ServerSpec, error) {
		return ServerSpec{
			Name: "fixture", Binary: os.Args[0],
			Args: []string{"-test.run=^TestLSPFixtureNeverMatches$"},
		}, nil
	}
	t.Cleanup(func() { _ = resident.Close() })
	query := symbols.SemanticQuery{Path: "main.go", Line: 1, Character: 1}
	for attempt := 1; attempt <= maxConsecutiveFailures; attempt++ {
		if _, err := resident.Definition(t.Context(), query); err == nil {
			t.Fatalf("attempt %d should fail", attempt)
		}
	}
	// The pool has now watched this server die twice without a successful
	// query between; it refuses a third start, and the caller falls back to
	// the lexical index.
	if _, err := resident.Definition(t.Context(), query); err == nil {
		t.Fatal("repeated failures should disable the server")
	} else if !strings.Contains(err.Error(), "consecutive crashes") {
		t.Fatalf("error = %v", err)
	}
}

func TestResidentCloseRetiresEverySession(t *testing.T) {
	resident, _ := newResidentWithFixture(t, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	query := symbols.SemanticQuery{Path: "main.go", Line: 3, Character: 6}
	if _, err := resident.Definition(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if err := resident.Close(); err != nil {
		t.Fatal(err)
	}
	resident.mu.Lock()
	live := len(resident.sessions)
	resident.mu.Unlock()
	if live != 0 {
		t.Fatalf("sessions after close = %d, want 0", live)
	}
	// Close is idempotent — a resource stack may call it twice.
	if err := resident.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResidentSerializesQueriesOnOneSession(t *testing.T) {
	resident, _ := newResidentWithFixture(t, ResidentOptions{
		IdleTimeout: time.Minute,
	})
	query := symbols.SemanticQuery{Path: "main.go", Line: 3, Character: 6}
	// The JSON-RPC client drops responses it does not recognize, so
	// concurrency on one session would corrupt answers; the pool must
	// serialize. Fire a burst and require every answer complete.
	var group sync.WaitGroup
	results := make([]error, 8)
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			_, results[slot] = resident.Definition(t.Context(), query)
		}(index)
	}
	group.Wait()
	for slot, err := range results {
		if err != nil {
			t.Fatalf("concurrent query %d: %v", slot, err)
		}
	}
}
