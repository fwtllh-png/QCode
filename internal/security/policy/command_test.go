package policy

import (
	"reflect"
	"testing"
)

func TestAnalyzeCommandUsesTypedSegmentsAndInterpreterBoundaries(t *testing.T) {
	analysis, err := AnalyzeCommand(
		`env MODE=test git status && bash -lc 'printf ok; rm -rf ./tmp'`,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := make([][]string, 0, len(analysis.Segments))
	for _, segment := range analysis.Segments {
		got = append(got, segment.Argv)
	}
	want := [][]string{
		{"git", "status"},
		{"bash", "-lc", "printf ok; rm -rf ./tmp"},
		{"printf", "ok"},
		{"rm", "-rf", "./tmp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("segments = %#v, want %#v", got, want)
	}
	if !analysis.Segments[1].InterpreterPayload {
		t.Fatal("shell payload boundary was not recorded")
	}
}

func TestCommandRuleCannotCrossSegmentsOrInterpreterPayload(t *testing.T) {
	for _, test := range []struct {
		command, prefix string
		action          Action
		want            bool
	}{
		{`git status`, `git status`, ActionAllow, true},
		{`git status && rm -rf .`, `git status`, ActionAllow, false},
		{`git status | cat`, `git status`, ActionAllow, false},
		{`git status >out`, `git status`, ActionAllow, false},
		{`bash -lc 'git status'`, `bash`, ActionAllow, false},
		{`bash -lc 'git status; rm -rf .'`, `rm`, ActionDeny, true},
		{`python3 -c 'import os'`, `python3`, ActionAllow, false},
		{`env X=1 rm -rf .`, `rm`, ActionDeny, true},
		// Ask gates composites: matching can only add friction, and a
		// chained command touching a gated prefix must still gate.
		{`git status && rm -rf .`, `git status`, ActionAsk, true},
		{`git status | cat`, `git status`, ActionAsk, true},
		{`echo done && curl https://example.internal`, `curl`, ActionAsk, true},
		{`go build ./... && printf done`, `go build`, ActionAsk, true},
		{`echo done`, `go build`, ActionAsk, false},
	} {
		if got := commandRuleMatches(test.command, test.prefix, test.action); got != test.want {
			t.Fatalf(
				"commandRuleMatches(%q, %q, %q) = %t, want %t",
				test.command, test.prefix, test.action, got, test.want,
			)
		}
	}
}

func TestUnsafePersistentPrefixRejectsBroadExecutables(t *testing.T) {
	for _, prefix := range []string{
		"sh", "bash -lc 'echo ok'", "python3 script.py", "node app.js", "git", "rm",
		"git status | cat", "echo >out",
	} {
		if !unsafePersistentPrefix(prefix) {
			t.Fatalf("unsafe prefix accepted: %q", prefix)
		}
	}
	for _, prefix := range []string{"git status", "rm ./generated.txt", "go test ./pkg"} {
		if unsafePersistentPrefix(prefix) {
			t.Fatalf("bounded prefix rejected: %q", prefix)
		}
	}
}

func TestCommandGrantIdentityIsASTCanonical(t *testing.T) {
	first, ok := commandGrantIdentity(`git   status`)
	if !ok {
		t.Fatal("first command was not analyzed")
	}
	second, ok := commandGrantIdentity(`git status`)
	if !ok || first != second {
		t.Fatalf("canonical identities differ: %q != %q", first, second)
	}
	third, ok := commandGrantIdentity(`git status && true`)
	if !ok || third == first {
		t.Fatal("additional command segment retained grant identity")
	}
}
