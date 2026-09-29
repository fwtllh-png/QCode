package policy

import "testing"

func TestPathPatternMatchesSubtrees(t *testing.T) {
	tests := []struct {
		pattern string
		match   []string
		miss    []string
	}{
		{"secrets", []string{"secrets", "secrets/a/b"}, []string{"secretsx", "a/secrets"}},
		{"*.pem", []string{"key.pem", "key.pem/child"}, []string{"dir/key.pem", "key.pem.bak"}},
		{"**/*.pem", []string{"key.pem", "a/b/key.pem"}, []string{"a/key.pem.bak"}},
		{"**/.env", []string{".env", "a/.env", "a/b/.env"}, []string{"a/.envrc", ".env.local"}},
		{"secrets/**", []string{"secrets", "secrets/x", "secrets/x/y"}, []string{"secret/x"}},
		{"a/**/b", []string{"a/b", "a/x/b", "a/x/y/b/c"}, []string{"a/x/c"}},
		{"a/?.txt", []string{"a/b.txt"}, []string{"a/bc.txt", "a/.txt"}},
		{"[ab].go", []string{"a.go", "b.go"}, []string{"c.go"}},
		{"/ws/src/*.go", []string{"/ws/src/x.go"}, []string{"/ws/src/x/y.go", "/ws/x.go"}},
		{`/ws/a\[1\]/x`, []string{"/ws/a[1]/x"}, []string{"/ws/a1/x"}},
	}
	for _, test := range tests {
		pattern, err := CompilePathPattern(test.pattern)
		if err != nil {
			t.Fatalf("CompilePathPattern(%q) error = %v", test.pattern, err)
		}
		for _, path := range test.match {
			if !pattern.Match(path) {
				t.Errorf("%q did not match %q", test.pattern, path)
			}
		}
		for _, path := range test.miss {
			if pattern.Match(path) {
				t.Errorf("%q matched %q", test.pattern, path)
			}
		}
	}
}

func TestPathPatternRejectsUnsupportedSyntax(t *testing.T) {
	for _, pattern := range []string{"", "a**b", "**x", "{a,b}", "a[", "src/{x}"} {
		if _, err := CompilePathPattern(pattern); err == nil {
			t.Errorf("CompilePathPattern(%q) accepted unsupported syntax", pattern)
		}
	}
}

func TestEscapedLiteralMatchesOnlyItself(t *testing.T) {
	literal := "/tmp/a[1]**?{literal}/b"
	pattern, err := CompilePathPattern(EscapePathPattern(literal))
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.Match(literal+"/c") || pattern.Match("/tmp/a1xy/b") {
		t.Fatal("escaped literal pattern interpreted wildcards")
	}
}

func TestPathPatternIntersectsDirectoryAuthority(t *testing.T) {
	for _, tc := range []struct {
		pattern, root string
		want          bool
	}{
		{"**/.env", "app", true},
		{"secrets/token", "secrets", true},
		{"secrets/token", "src", false},
		{"/ws/a/**/b", "/ws/a/x", true},
		{"/ws/a/**/b", "/other/a", false},
		{"a/?.pem", "a/long.txt", false},
		{"a/?.pem", "a", true},
		{"a", "a/b", true},
		{"a", "ab", false},
	} {
		pattern, err := CompilePathPattern(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := pattern.IntersectsTree(tc.root); got != tc.want {
			t.Errorf("%q intersects tree %q = %v, want %v", tc.pattern, tc.root, got, tc.want)
		}
	}
}

func TestRuleValidationRejectsUnsupportedPathSyntax(t *testing.T) {
	if err := ValidateRules(SourceRepository, []Rule{{
		Tool: "*", Resource: "src/{a,b}", Action: ActionDeny,
	}}); err == nil {
		t.Fatal("brace pattern was accepted")
	}
	if err := ValidateRules(SourceRepository, []Rule{{
		Tool: "*", Resource: "https://example.com/search?q=1", Action: ActionDeny,
	}}); err != nil {
		t.Fatalf("non-path resource was parsed as a path pattern: %v", err)
	}
}

func TestPathRuleGlobGatesWrites(t *testing.T) {
	runtime := DefaultRuntime(ModeAct, PermissionBypass)
	runtime.Repository = []Rule{{
		Tool: "*", Resource: "**/*.pem", Action: ActionHold, Code: "pem_hold", RequireWrite: true,
	}}
	if decision := runtime.Decide(resolveFixture(writeCall("certs/key.pem"))); decision.Code != "pem_hold" {
		t.Fatalf("nested pem write decision = %+v", decision)
	}
	if decision := runtime.Decide(resolveFixture(writeCall("certs/key.txt"))); decision.Action != ActionAllow {
		t.Fatalf("non-matching write decision = %+v", decision)
	}
}
