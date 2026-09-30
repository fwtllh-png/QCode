package skill

import (
	"testing"
)

func TestManifestStrictSchemaAndCompatibility(t *testing.T) {
	_, err := ParseManifest([]byte(`
schema_version = 1
name = "review"
version = "1.2"
qcode = ">=1.0.0"
unknown = true
`))
	if err == nil {
		t.Fatal("invalid manifest was accepted")
	}
	manifest, err := ParseManifest([]byte(`
schema_version = 1
name = "review"
version = "1.2.0"
qcode = ">=1.0.0 <2.0.0"

[dependencies]
repository-context = "^2.1.0"
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkVersion(manifest.QCode, "1.5.0"); err != nil {
		t.Fatal(err)
	}
	if err := checkVersion(manifest.QCode, "2.0.0"); err == nil {
		t.Fatal("incompatible runtime was accepted")
	}
}
