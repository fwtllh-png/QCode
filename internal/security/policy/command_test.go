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

func TestRestrictiveCommandRulesFailClosedOnEvasion(t *testing.T) {
	for _, command := range []string{
		`/usr/bin/git push`,
		`\git push`,
		`g\it push`,
		`"git" push`,
		`command git push`,
		`exec git push`,
		`env -i git push`,
		`env -u HOME git push`,
		`nice -n 5 git push`,
		`timeout 10 git push`,
		`sudo -u root git push`,
		`nohup git push origin main`,
		`$(echo git) push`,
		`git $(echo push)`,
		`$GIT push`,
		`git -C ./repo push`,
		`git -c core.sshCommand=ssh push --force`,
		`echo push | xargs git`,
		`eval git push`,
		`eval "git push"`,
		`bash -c 'git push'`,
		`bash -c 'git push; if'`,
		`python3 -c "import subprocess; subprocess.run(['git', 'push'])"`,
		`git push "unterminated`,
	} {
		for _, action := range []Action{ActionDeny, ActionHold, ActionAsk} {
			if !commandRuleMatches(command, `git push`, action) {
				t.Errorf("%s rule `git push` missed %q", action, command)
			}
		}
	}
	for _, command := range []string{
		`find . -name '*.tmp' -exec rm -f {} +`,
		`find . -execdir /bin/rm {} ;`,
		`xargs -0 rm`,
	} {
		if !commandRuleMatches(command, `rm`, ActionDeny) {
			t.Errorf("deny rule `rm` missed %q", command)
		}
	}
	for _, command := range []string{
		`git status`,
		`git log --oneline`,
		`echo git push`,
		`printf '%s\n' "git push"`,
		`gitk push`,
		`go test ./...`,
	} {
		if commandRuleMatches(command, `git push`, ActionDeny) {
			t.Errorf("deny rule `git push` matched unrelated %q", command)
		}
	}
}

func TestAllowCommandRulesStayLiteral(t *testing.T) {
	for _, command := range []string{
		`/tmp/evil/git status`,
		`command git status`,
		`$(echo git) status`,
		`git status "unterminated`,
	} {
		if commandRuleMatches(command, `git status`, ActionAllow) {
			t.Errorf("allow rule `git status` widened to %q", command)
		}
	}
}

func TestEnvOptionsUnwrapToTheCommand(t *testing.T) {
	for command, want := range map[string][]string{
		`env -i PATH=/bin git status`:           {"git", "status"},
		`env -u HOME -- git status`:             {"git", "status"},
		`env --unset=HOME --chdir=/ git status`: {"git", "status"},
	} {
		analysis, err := AnalyzeCommand(command)
		if err != nil {
			t.Fatal(err)
		}
		if got := analysis.Segments[0].Argv; !reflect.DeepEqual(got, want) {
			t.Fatalf("%q argv = %#v, want %#v", command, got, want)
		}
	}
	analysis, err := AnalyzeCommand(`env -S 'git push' status`)
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.Segments[0].Dynamic {
		t.Fatal("env -S split string must be treated as dynamic")
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
