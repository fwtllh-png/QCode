package envprobe

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestFingerprintDoesNotExecuteHostTools(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "probes")
	t.Setenv("QCODE_TEST_PROBE_MARKER", marker)
	t.Setenv("PATH", bin)
	t.Setenv("SHELL", "/host/login-shell")
	for _, name := range []string{"git", "go", "node", "python3"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(`#!/bin/sh
printf '%s\n' "$0" >> "$QCODE_TEST_PROBE_MARKER"
exit 1
`), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"os: " + runtime.GOOS + " (" + runtime.GOARCH + ")"}
	if got := Fingerprint(); !slices.Equal(got, want) {
		t.Fatalf("platform fingerprint = %v, want %v", got, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fingerprint executed a host tool: marker stat = %v", err)
	}
}
