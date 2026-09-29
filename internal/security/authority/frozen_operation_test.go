package authority

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	securitymodel "github.com/fwtllh-png/QCode/internal/security/model"
)

func TestLateEvidenceDoesNotReadChangedFilesystem(t *testing.T) {
	input := fixtureCompileInput(t)
	host := t.TempDir()
	file := filepath.Join(host, "frozen.txt")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	appendFixtureResources(&input.Prepared, tool.Resource{Kind: "file", Path: file, Access: tool.AccessRead})
	compiled, err := Compile(resolveCompileFixture(input))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	// Replacing the path with a symlink would redirect any accidental reread.
	if err := os.Symlink(input.SandboxPolicy.WorkspaceRoot, file); err != nil {
		t.Fatal(err)
	}
	bound, err := compiled.Bind(Evidence{Artifact: &ArtifactIntent{ManifestDigest: strings.Repeat("a", 64), Generation: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled.Operation.Resources, bound.Operation.Resources) {
		t.Fatal("Bind reread resource identity")
	}
	rebound, err := bound.WithProfile(bound.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bound.Operation.Resources, rebound.Operation.Resources) {
		t.Fatal("WithProfile reread resource identity")
	}
}

func TestCompileRequiresThePreparedSnapshot(t *testing.T) {
	input := resolveCompileFixture(fixtureCompileInput(t))
	input.Prepared.Assessment = securitymodel.Assessment{}
	if _, err := Compile(input); err == nil {
		t.Fatal("missing prepared assessment was accepted")
	}
	input = resolveCompileFixture(fixtureCompileInput(t))
	changed := input.Prepared.Assessment.Input()
	changed.Resources = nil
	input.Invocation.Assessment = securitymodel.Assess(changed)
	if _, err := Compile(input); err == nil {
		t.Fatal("different assessed resource set was accepted")
	}
}
