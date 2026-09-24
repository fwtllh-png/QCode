package file

import (
	"strings"
	"testing"
)

func TestReplaceExactFallsBackOnConfusablePunctuation(t *testing.T) {
	content := "if value == “ready” {\n\treturn ‘ok’\n}"
	old := "if value == \"ready\" {\n\treturn 'ok'\n}"
	next, recovery, err := replaceExact([]byte(content), old, "replaced", 1)
	if err != nil {
		t.Fatalf("replaceExact error = %v", err)
	}
	if !recovery.normalized {
		t.Fatalf("expected normalized recovery, got %+v", recovery)
	}
	// old covers the whole file once folded, including the closing brace.
	if string(next) != "replaced" {
		t.Fatalf("replaceExact = %q, want %q", next, "replaced")
	}
}

func TestReplaceExactFoldsSpecialSpacesAndTrailingWhitespace(t *testing.T) {
	content := "alpha\u00A0beta gamma   \ndelta‐epsilon\t\n"
	old := "alpha beta gamma\ndelta-epsilon\n"
	next, recovery, err := replaceExact([]byte(content), old, "X", 1)
	if err != nil {
		t.Fatalf("replaceExact error = %v", err)
	}
	if !recovery.normalized {
		t.Fatalf("expected normalized recovery, got %+v", recovery)
	}
	want := "X"
	if string(next) != want {
		t.Fatalf("replaceExact = %q, want %q", next, want)
	}
}

func TestReplaceNormalizedOnlyReplacesProjectedSpan(t *testing.T) {
	// The fold must never rewrite bytes outside the located span: untouched
	// smart quotes and full-width spaces stay byte-identical.
	content := "keep “left”\n\ttarget “mid” tail  \nkeep ‘right’\n"
	next, ok := replaceNormalized(content, "target \"mid\" tail", "REPLACED", 1)
	if !ok {
		t.Fatalf("replaceNormalized rejected a clean fold")
	}
	want := "keep “left”\n\tREPLACED  \nkeep ‘right’\n"
	if string(next) != want {
		t.Fatalf("replaceNormalized = %q, want %q", next, want)
	}
}

func TestReplaceNormalizedPreservesCRLFOutsideSpan(t *testing.T) {
	content := "one\r\ntwo–three\r\nfour\r\n"
	next, ok := replaceNormalized(content, "two-three", "X", 1)
	if !ok {
		t.Fatalf("replaceNormalized rejected a dash fold inside CRLF content")
	}
	want := "one\r\nX\r\nfour\r\n"
	if string(next) != want {
		t.Fatalf("replaceNormalized = %q, want %q", next, want)
	}
}

func TestReplaceNormalizedAdoptsSpanLineEndings(t *testing.T) {
	content := "a\r\nb\r\nc\r\n"
	next, ok := replaceNormalized(content, "a\nb", "x\ny", 1)
	if !ok {
		t.Fatalf("replaceNormalized rejected a CRLF-spanning match")
	}
	want := "x\r\ny\r\nc\r\n"
	if string(next) != want {
		t.Fatalf("replaceNormalized = %q, want %q", next, want)
	}
}

func TestReplaceNormalizedKeepsOccurrenceCountSemantics(t *testing.T) {
	content := "tag “one”\ntag “two”\n"
	if _, ok := replaceNormalized(content, "tag \"one\"", "X", 1); !ok {
		t.Fatalf("expected single-occurrence fold to apply")
	}
	if _, ok := replaceNormalized(content, "tag \"missing\"", "X", 1); ok {
		t.Fatalf("expected zero-fold-count to reject")
	}
	next, ok := replaceNormalized(content, "tag \"one\"", "X", 2)
	if ok {
		t.Fatalf("expected two-occurrence demand to reject, got %q", next)
	}
}

func TestReplaceNormalizedIdentityFoldRejects(t *testing.T) {
	// When folding changes nothing on both sides, the exact pass already
	// rejected this predicate; the fallback must not re-answer it.
	content := "plain text\n"
	if next, ok := replaceNormalized(content, "plain text", "X", 2); ok {
		t.Fatalf("identity fold with wrong count unexpectedly applied: %q", next)
	}
}

func TestReplaceNormalizedRejectsDisproportionateSpan(t *testing.T) {
	// oldLines > 1 with a span far beyond the byte ratio guard. Constructing a
	// legitimately folded but oversized span is not possible by construction,
	// so exercise the guard through its public boundary: a multi-line old
	// whose folded match would need more than 4x old's bytes. The guard must
	// fail closed rather than edit an oversized region.
	old := "head line\n" + strings.Repeat("x", 200) + "\ntail line\n"
	span := "head line\n" + strings.Repeat("x", 200) + strings.Repeat(" y", 100) + "\ntail line\n"
	if disproportionateEditSpan(span, old) {
		t.Fatalf("guard rejected a proportionate span")
	}
	huge := "head line\n" + strings.Repeat("z", 4000) + "\ntail line\n"
	if !disproportionateEditSpan(huge, old) {
		t.Fatalf("guard accepted a disproportionate span")
	}
}

func TestStripLineNumberPrefixes(t *testing.T) {
	cases := []struct {
		name string
		old  string
		want string
		ok   bool
	}{
		{"colon block", "  12: first\n13: second\n", " first\n second\n", true},
		{"arrow block", "  7→alpha\n8→beta\n", "alpha\nbeta\n", true},
		{"pipe block", "3|one\n4|two\n", "one\ntwo\n", true},
		{"blank lines kept", "10:a\n\n11:b\n", "a\n\nb\n", true},
		{"single line refused", "80: port\n", "", false},
		{"partial block refused", "12:ok\nplain line\n", "", false},
		{"no prefixes", "alpha\nbeta\n", "", false},
		{"bare digits refused", "12 alpha\n34 beta\n", "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := stripLineNumberPrefixes(testCase.old)
			if ok != testCase.ok {
				t.Fatalf("ok = %v, want %v", ok, testCase.ok)
			}
			if ok && got != testCase.want {
				t.Fatalf("stripped = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestReplaceExactRecoversLineNumberPrefixPaste(t *testing.T) {
	content := "func main() {\n\tfmt.Println(\"hi\")\n}\n"
	old := "  1→func main() {\n2→\tfmt.Println(\"hi\")\n3→}\n"
	next, recovery, err := replaceExact([]byte(content), old, "RENAMED", 1)
	if err != nil {
		t.Fatalf("replaceExact error = %v", err)
	}
	if !recovery.prefixStripped {
		t.Fatalf("expected prefix-stripped recovery, got %+v", recovery)
	}
	if !strings.Contains(string(next), "RENAMED") || strings.Contains(string(next), "2→") {
		t.Fatalf("replaceExact = %q", next)
	}
}

func TestReplaceExactFailureKeepsOriginalMissError(t *testing.T) {
	content := "alpha\nbeta\n"
	next, _, err := replaceExact([]byte(content), "gamma\ndelta", "X", 1)
	if err == nil {
		t.Fatalf("expected miss error, got %q", next)
	}
	if !strings.Contains(err.Error(), "matched 0 times") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeEditMapsBoundaries(t *testing.T) {
	view := "abc“def”   \ngh–ij\n"
	normalized := normalizeEdit(view)
	if normalized.text != "abc\"def\"\ngh-ij\n" {
		t.Fatalf("normalized text = %q", normalized.text)
	}
	// Every folded rune must project back into the original view such that
	// re-folding the projected bytes reproduces the folded text.
	if len(normalized.originAtByte) != len(normalized.text)+1 {
		t.Fatalf("origin mapping length = %d, want %d",
			len(normalized.originAtByte), len(normalized.text)+1)
	}
	if normalized.originAtByte[len(normalized.text)] != len(view) {
		t.Fatalf("sentinel = %d, want %d",
			normalized.originAtByte[len(normalized.text)], len(view))
	}
}
