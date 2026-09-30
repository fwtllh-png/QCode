package sqlkit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigratedRepositoriesDoNotReimplementSQLKit(t *testing.T) {
	root := filepath.Clean("../..")
	files := map[string][]string{
		"persist/session/lifecycle.go": {
			".BeginTx(",
		},
		"persist/session/profile.go": {
			".BeginTx(", "func normalizedJSON(", "func nullableTime(",
			"func timestamp(",
		},
		"persist/session/repository.go": {
			".BeginTx(", "func normalizedJSON(", "func nullableTime(",
			"func timestamp(",
		},
		"persist/snapshot/repository.go": {
			".BeginTx(", "func normalizedObject(", "func timestamp(",
		},
	}
	for relative, forbidden := range files {
		t.Run(relative, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, relative))
			if err != nil {
				t.Fatal(err)
			}
			source := string(data)
			for _, token := range forbidden {
				if strings.Contains(source, token) {
					t.Fatalf("migrated repository contains forbidden helper %q", token)
				}
			}
		})
	}
}
