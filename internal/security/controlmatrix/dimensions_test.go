package controlmatrix

import (
	"reflect"
	"strings"
	"testing"
)

// TestDimensionSpecsCoverEveryMatrixField keeps the spec table and the Matrix
// struct in lockstep: every field (by JSON tag) must have exactly one spec
// row, every spec row must name a field, and both sides must have the same
// size. A new dimension without a spec row — or a stale row after a field is
// removed — fails here instead of drifting through StringMap, Identity,
// Validate, and SatisfiedBy.
func TestDimensionSpecsCoverEveryMatrixField(t *testing.T) {
	specsByKeys := make(map[string]bool, len(dimensionSpecs))
	for _, spec := range dimensionSpecs {
		if spec.key == "" || spec.label == "" || spec.value == nil || spec.satisfies == nil {
			t.Fatalf("incomplete dimension spec for key %q", spec.key)
		}
		if len(spec.values) == 0 {
			t.Fatalf("dimension %q declares no values", spec.key)
		}
		if specsByKeys[spec.key] {
			t.Fatalf("duplicate dimension spec %q", spec.key)
		}
		specsByKeys[spec.key] = true
	}

	matrixType := reflect.TypeOf(Matrix{})
	for index := range matrixType.NumField() {
		field := matrixType.Field(index)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "" {
			t.Errorf("Matrix field %s has no json tag", field.Name)
			continue
		}
		if !specsByKeys[tag] {
			t.Errorf("Matrix field %s (json %q) has no dimension spec", field.Name, tag)
			continue
		}
		delete(specsByKeys, tag)
	}
	for key := range specsByKeys {
		t.Errorf("dimension spec %q names no Matrix field", key)
	}
	if got := matrixType.NumField(); got != len(dimensionSpecs) {
		t.Fatalf("Matrix has %d fields, spec table has %d rows", got, len(dimensionSpecs))
	}
}

// TestDimensionSpecsValidateEveryDeclaredValue keeps the values lists honest:
// every declared value must pass its own spec's validity, and no spec may
// declare a duplicate value.
func TestDimensionSpecsValidateEveryDeclaredValue(t *testing.T) {
	for _, spec := range dimensionSpecs {
		seen := make(map[string]bool, len(spec.values))
		for _, value := range spec.values {
			if seen[value] {
				t.Errorf("dimension %q declares duplicate value %q", spec.key, value)
			}
			seen[value] = true
			if !spec.valid(value) {
				t.Errorf("dimension %q value %q fails its own validity", spec.key, value)
			}
		}
	}
}

// TestIdentityOrderIsLocked pins the dimension order that Identity joins.
// Reordering or inserting dimensions changes persisted identities and must
// update this expectation deliberately.
func TestIdentityOrderIsLocked(t *testing.T) {
	matrix := Matrix{
		FilesystemRead:  FilesystemReadDeclaredRoots,
		FilesystemWrite: FilesystemWriteExactPaths,
		Network:         NetworkProxyTargets,
		ProcessTree:     ProcessTreeGroupKill,
		CrossProcess:    CrossProcessRestricted,
		Syscall:         SyscallDenyDangerous,
		IPC:             IPCUnixOnly,
		PathIdentity:    PathIdentityCanonical,
		ArtifactOrigin:  ArtifactOriginVerifiedManifest,
		DurableRecovery: DurableRecoveryExternalJournal,
	}
	const want = "declared_roots/exact_paths/proxy_targets/" +
		"group_kill/restricted/deny_dangerous/unix_only/" +
		"canonical/verified_manifest/external_journal"
	if got := matrix.Identity(); got != want {
		t.Fatalf("Identity() = %q, want %q", got, want)
	}
	projected := matrix.StringMap()
	if len(projected) != len(dimensionSpecs) {
		t.Fatalf("StringMap has %d keys, want %d", len(projected), len(dimensionSpecs))
	}
	for key, value := range projected {
		spec, ok := func() (dimensionSpec, bool) {
			for _, candidate := range dimensionSpecs {
				if candidate.key == key {
					return candidate, true
				}
			}
			return dimensionSpec{}, false
		}()
		if !ok {
			t.Fatalf("StringMap key %q has no dimension spec", key)
		}
		if spec.value(matrix) != value {
			t.Fatalf("StringMap[%q] = %q disagrees with spec getter", key, value)
		}
	}
}
