package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.v1.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validCatalog = `{
  "version": 1,
  "providers": [
    {
      "id": "fixture", "adapter": "openai_compatible",
      "endpoint": "https://fixture.example/v1",
      "protocol": "openai_chat",
      "credential": {"kind": "env", "name": "FIXTURE_KEY"},
      "provenance": "bundled",
      "models": {
        "fixture-1": {
          "id": "fixture-1", "canonical_id": "fixture-1", "wire_id": "fixture-1",
          "limits": {"context_tokens": 8192, "max_output_tokens": 1024},
          "capabilities": {"tool_calls": true},
          "pricing": {"input_per_million": 1, "output_per_million": 2, "currency": "USD", "known": true},
          "provenance": "bundled"
        }
      }
    }
  ]
}`

func TestCheckerAcceptsValidCatalog(t *testing.T) {
	if err := run(writeCatalog(t, validCatalog)); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
}

func TestCheckerRejectsOutputBeyondContext(t *testing.T) {
	corrupted := strings.Replace(
		validCatalog, `"max_output_tokens": 1024`, `"max_output_tokens": 9999999`, 1,
	)
	err := run(writeCatalog(t, corrupted))
	// The runtime per-provider validation also rejects this shape; either
	// message proves the gate fires.
	if err == nil || (!strings.Contains(err.Error(), "exceeds context window") &&
		!strings.Contains(err.Error(), "output limit exceeds context")) {
		t.Fatalf("output-limit error = %v", err)
	}
}

func TestCheckerRejectsNegativePricing(t *testing.T) {
	corrupted := strings.Replace(
		validCatalog, `"input_per_million": 1`, `"input_per_million": -1`, 1,
	)
	err := run(writeCatalog(t, corrupted))
	if err == nil || !strings.Contains(err.Error(), "negative pricing") {
		t.Fatalf("pricing error = %v", err)
	}
}

func TestCheckerRejectsMissingProvenance(t *testing.T) {
	corrupted := strings.Replace(
		validCatalog,
		`"pricing": {"input_per_million": 1, "output_per_million": 2, "currency": "USD", "known": true},
          "provenance": "bundled"`,
		`"pricing": {"input_per_million": 1, "output_per_million": 2, "currency": "USD", "known": true},
          "provenance": "guessed"`,
		1,
	)
	err := run(writeCatalog(t, corrupted))
	if err == nil || !strings.Contains(err.Error(), "unknown provenance") {
		t.Fatalf("provenance error = %v", err)
	}
}

func TestCheckerRejectsDuplicateCanonicalIDs(t *testing.T) {
	corrupted := strings.Replace(
		validCatalog, `"wire_id": "fixture-1"`, `"wire_id": "fixture-alt"`, 1,
	)
	corrupted = strings.Replace(
		corrupted,
		`"fixture-1": {
          "id": "fixture-1", "canonical_id": "fixture-1", "wire_id": "fixture-alt",`,
		`"fixture-1": {
          "id": "fixture-1", "canonical_id": "fixture-1", "wire_id": "fixture-alt",
          "limits": {"context_tokens": 8192, "max_output_tokens": 1024},
          "capabilities": {"tool_calls": true},
          "pricing": {"input_per_million": 1, "output_per_million": 2, "currency": "USD", "known": true},
          "provenance": "bundled"
        },
        "fixture-1-alt": {
          "id": "fixture-1-alt", "canonical_id": "fixture-1", "wire_id": "fixture-1-alt",`,
		1,
	)
	err := run(writeCatalog(t, corrupted))
	if err == nil || !strings.Contains(err.Error(), "canonical id") {
		t.Fatalf("canonical duplicate error = %v", err)
	}
}
