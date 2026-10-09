// Package policy adjudicates every tool invocation against the security
// model. This file documents why command-prefix matching is not a valid
// authorization basis and preserves the adversarial fixtures that proved it.
//
// The safe_command_allowed pathway was removed by the Guardian design
// document (docs/zh-CN/guardian-auto-review-design.md §3). Command text
// prefixes cannot prove safety: shell syntax allows execution through
// substitution, chaining, and indirect scripts that a prefix cannot see.
//
// This file intentionally exports no policy behavior. The adversarial
// inputs below are consumed by command_safety_test.go to verify that the
// removed pathway stays removed and that these commands still produce Ask.
package policy

// adversarialPrefixInputs are shell commands whose leading text matches a
// prefix that was previously classified as "safe". Each demonstrates a
// distinct bypass vector that prefix matching cannot detect.
var adversarialPrefixInputs = []struct {
	Command string
	Prefix  string // the prefix it would have matched
	Bypass  string // why the prefix classification is wrong
}{
	{Command: "find . -name '*.env' -exec cat {} \\;", Prefix: "find ", Bypass: "find -exec runs an arbitrary command"},
	{Command: "echo ok; rm -rf scratch", Prefix: "echo ", Bypass: "semicolon introduces a second command"},
	{Command: "echo $(rm -rf scratch)", Prefix: "echo ", Bypass: "command substitution executes within echo"},
	{Command: "npm run deploy", Prefix: "npm run", Bypass: "script name does not constrain side effects"},
	{Command: "make clean", Prefix: "make", Bypass: "Makefile target content is unconstrained"},
	{Command: "git restore --worktree -- source.go", Prefix: "git restore", Bypass: "may discard uncommitted work"},
	{Command: "git checkout -- .", Prefix: "git checkout", Bypass: "discards uncommitted changes to tracked files"},
	{Command: "cat ~/.ssh/id_rsa", Prefix: "cat ", Bypass: "reads credentials outside the workspace"},
	{Command: "grep -r 'password' /etc/", Prefix: "grep ", Bypass: "searches system configuration outside workspace"},
	{Command: "go test -run TestThatCallsNetwork", Prefix: "go test", Bypass: "test code may perform network operations"},
	{Command: "python -m pip install malicious-package", Prefix: "python -m pip install", Bypass: "installs arbitrary code from PyPI"},
	{Command: "cargo add malicious-crate", Prefix: "cargo add", Bypass: "adds arbitrary dependency from crates.io"},
}
