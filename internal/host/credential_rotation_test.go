package host

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/credential"
	oskeyring "github.com/zalando/go-keyring"
)

// TestCredentialProtocolLivesInRotation keeps stage → activate → commit in
// one transaction type. Control's protocol methods take a context, which
// separates them from credentialRotation's own zero-argument steps.
func TestCredentialProtocolLivesInRotation(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	protocol := map[string]bool{"StageKeyring": true, "Activate": true, "Commit": true, "Restore": true}
	fileSet := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "credential_rotation.go" {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if ok && protocol[selector.Sel.Name] && isContextArgument(call.Args[0]) {
				t.Errorf("%s: %s drives the credential protocol outside credentialRotation",
					fileSet.Position(call.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
}

func isContextArgument(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name == "ctx"
	case *ast.CallExpr:
		selector, ok := value.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && pkg.Name == "context"
	}
	return false
}

func openTestCredentialControl(t *testing.T) (*credential.Control, credential.Reference) {
	t.Helper()
	oskeyring.MockInit()
	base := credential.Reference{Kind: "env", Name: "QCODE_TEST_KEY"}
	control, effective, err := credential.OpenControl(
		t.Context(), t.TempDir(), "rotation-test", "openai", base, credential.Reference{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return control, effective
}

func effectiveCredential(t *testing.T, control *credential.Control) credential.Reference {
	t.Helper()
	reference, err := control.Reference(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestCredentialRotationCommitRequiresActivation(t *testing.T) {
	control, current := openTestCredentialControl(t)
	rotation, err := stageCredentialRotation(t.Context(), control, current, "sk-rotated")
	if err != nil {
		t.Fatal(err)
	}
	if !rotation.Pending() || rotation.Reference() == current {
		t.Fatalf("staged rotation = pending %t reference %+v", rotation.Pending(), rotation.Reference())
	}
	if err := rotation.Commit(); !errors.Is(err, errCredentialRotationNotActivated) {
		t.Fatalf("Commit() before Activate() error = %v", err)
	}
	if got := effectiveCredential(t, control); got != current {
		t.Fatalf("effective credential after refused commit = %+v, want %+v", got, current)
	}
	if err := rotation.Rollback(); err != nil {
		t.Fatalf("Rollback() of a staged rotation: %v", err)
	}
	if got := effectiveCredential(t, control); got != current {
		t.Fatalf("effective credential after rollback = %+v, want %+v", got, current)
	}
}

func TestCredentialRotationActivateCommitThenRollbackIsNoop(t *testing.T) {
	control, current := openTestCredentialControl(t)
	rotation, err := stageCredentialRotation(t.Context(), control, current, "sk-rotated")
	if err != nil {
		t.Fatal(err)
	}
	next := rotation.Reference()
	if err := rotation.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := rotation.Activate(); err != nil {
		t.Fatalf("second Activate() = %v", err)
	}
	if got := effectiveCredential(t, control); got != next {
		t.Fatalf("effective credential after activate = %+v, want %+v", got, next)
	}
	if err := rotation.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := rotation.Rollback(); err != nil {
		t.Fatalf("Rollback() after commit = %v", err)
	}
	if rotation.Pending() {
		t.Fatal("committed rotation still pending")
	}
	if got := effectiveCredential(t, control); got != next {
		t.Fatalf("effective credential after committed rollback = %+v, want %+v", got, next)
	}
}

func TestCredentialRotationRollbackRestoresActivatedCredential(t *testing.T) {
	control, current := openTestCredentialControl(t)
	rotation, err := stageCredentialRotation(t.Context(), control, current, "sk-rotated")
	if err != nil {
		t.Fatal(err)
	}
	if err := rotation.Activate(); err != nil {
		t.Fatal(err)
	}
	if err := rotation.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := effectiveCredential(t, control); got != current {
		t.Fatalf("effective credential after rollback = %+v, want %+v", got, current)
	}
	if err := rotation.Activate(); !errors.Is(err, errCredentialRotationSettled) {
		t.Fatalf("Activate() after rollback error = %v", err)
	}
	if err := rotation.Commit(); !errors.Is(err, errCredentialRotationSettled) {
		t.Fatalf("Commit() after rollback error = %v", err)
	}
}

func TestCredentialRotationWithoutSecretIsIdle(t *testing.T) {
	control, current := openTestCredentialControl(t)
	rotation, err := stageCredentialRotation(t.Context(), control, current, "")
	if err != nil {
		t.Fatal(err)
	}
	if rotation.Pending() || rotation.Reference() != current || rotation.Control() != control {
		t.Fatalf("idle rotation = pending %t reference %+v", rotation.Pending(), rotation.Reference())
	}
	for name, step := range map[string]func() error{
		"activate": rotation.Activate, "commit": rotation.Commit, "rollback": rotation.Rollback,
	} {
		if err := step(); err != nil {
			t.Fatalf("%s on idle rotation: %v", name, err)
		}
	}
	var missing *credentialRotation
	if missing.Pending() || missing.Activate() != nil || missing.Commit() != nil || missing.Rollback() != nil {
		t.Fatal("nil rotation is not a no-op")
	}
}

func TestCredentialRotationBindsOnlyTheOwningConnection(t *testing.T) {
	control, current := openTestCredentialControl(t)
	rotation, err := stageCredentialRotation(t.Context(), control, current, "sk-rotated")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rotation.Rollback() }()
	defaultReference := credential.Reference{Kind: "keyring", Name: "default"}
	selection := webSetupSelection{
		Connections: []webSetupConnection{
			{ID: "openai", Provider: "openai", Credential: &defaultReference},
			{ID: "second", Provider: "second"},
		},
		DefaultConnection: "openai",
	}
	if !rotation.bindTo(selection, "second") {
		t.Fatal("pending rotation did not bind")
	}
	if got := selection.Connection("second").Credential; got == nil || *got != rotation.Reference() {
		t.Fatalf("second connection credential = %+v", got)
	}
	if got := selection.Active().Credential; got == nil || *got != defaultReference {
		t.Fatalf("default connection credential changed to %+v", got)
	}
	if !rotation.bindTo(selection, "") {
		t.Fatal("pending rotation did not bind the active connection")
	}
	if got := selection.Active().Credential; got == nil || *got != rotation.Reference() {
		t.Fatalf("active connection credential = %+v", got)
	}
	idle, err := stageCredentialRotation(t.Context(), control, current, "")
	if err != nil {
		t.Fatal(err)
	}
	if idle.bindTo(selection, "second") {
		t.Fatal("idle rotation rewrote a connection credential")
	}
}
