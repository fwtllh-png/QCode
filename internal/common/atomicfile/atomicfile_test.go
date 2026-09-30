package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceWritesAndReplacesDurably(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, content := range []string{"first", "second"} {
		if err := Replace(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != content {
			t.Fatalf("content = %q, want %q", data, content)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}
