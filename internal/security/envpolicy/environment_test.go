package envpolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestMergePrecedenceConflictsAndEmptyValues(t *testing.T) {
	got, err := Merge([]string{"MODE=source", "LANG=C"}, []string{"MODE=trusted"}, []string{"MODE=", "MODE="})
	if err != nil || !slices.Equal(got, []string{"LANG=C", "MODE="}) {
		t.Fatalf("merged=%v err=%v", got, err)
	}
	for _, values := range [][]string{{"MODE=one", "MODE=two"}, {"broken"}, {"X=bad\x00value"}} {
		if _, err := Merge(values, []string{"MODE=override"}); err == nil {
			t.Fatalf("accepted invalid layer %q", values)
		}
	}
}

func TestSelectionAndDeclarationSafety(t *testing.T) {
	source := []string{"LANG=C", "LC_MESSAGES=en_US", "UNRECOGNIZED=value", "API_TOKEN=fixture", "BASH_ENV=fixture"}
	got, err := Merge(Baseline(source))
	if err != nil || !slices.Equal(got, []string{"LANG=C", "LC_MESSAGES=en_US"}) {
		t.Fatalf("baseline=%v err=%v", got, err)
	}
	for _, name := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "API_TOKEN", "SIGNING_KEY", "BASH_ENV", "NODE_OPTIONS", "PYTHONSTARTUP"} {
		err := ValidateDeclaredEnvironment([]string{name + "=do-not-print-this-value"})
		if err == nil || strings.Contains(err.Error(), "do-not-print-this-value") {
			t.Fatalf("invalid declaration error for %s: %v", name, err)
		}
	}
	if err := ValidateDeclaredEnvironment([]string{"UNKNOWN_TOOL_SETTING=value"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePreparedEnvironment([]string{"HOME=/private/home", "TMPDIR=/private/tmp"}); err != nil {
		t.Fatal(err)
	}
}
