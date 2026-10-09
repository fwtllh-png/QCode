package policy

import (
	"strings"
)

// CommandSafetyRule declares a shell command prefix that is safe to
// auto-allow under the Auto posture without human approval. Rules are
// deterministic prefix matches against the first token(s) of the command.
type CommandSafetyRule struct {
	// Prefix is the leading text of a safe command, case-sensitive.
	Prefix string
	// Description explains why the prefix is safe for auditability.
	Description string
}

// DefaultCommandSafetyRules returns the built-in safe command prefixes.
// These cover routine development operations that modify only workspace
// state (git metadata, build artifacts, lock files) and are reversible
// through version control or re-running the command.
//
// Destructive operations (git push --force, rm, dd, mkfs, chmod 777,
// curl | sh, etc.) are intentionally absent: they retain the Ask posture.
func DefaultCommandSafetyRules() []CommandSafetyRule {
	return []CommandSafetyRule{
		{Prefix: "git add", Description: "stages tracked files; reversible via git reset"},
		{Prefix: "git commit", Description: "creates a local commit; reversible via git reset"},
		{Prefix: "git status", Description: "read-only working tree inspection"},
		{Prefix: "git diff", Description: "read-only difference inspection"},
		{Prefix: "git log", Description: "read-only history inspection"},
		{Prefix: "git branch", Description: "local branch management; reversible"},
		{Prefix: "git checkout", Description: "switches branches or restores files from index"},
		{Prefix: "git stash", Description: "temporary workspace snapshot; reversible"},
		{Prefix: "git restore", Description: "restores files from a commit or stash"},
		{Prefix: "git switch", Description: "switches branches"},
		{Prefix: "git tag", Description: "local tag management"},
		{Prefix: "git merge", Description: "local merge; reversible via git reset"},
		{Prefix: "git rebase", Description: "local rebase; reversible via reflog"},
		{Prefix: "git cherry-pick", Description: "applies a specific commit; reversible"},
		{Prefix: "git worktree", Description: "manages linked worktrees"},
		{Prefix: "go build", Description: "compiles Go packages; writes build cache only"},
		{Prefix: "go test", Description: "runs Go tests in a sandbox; may write temp files"},
		{Prefix: "go vet", Description: "static analysis; read-only"},
		{Prefix: "go fmt", Description: "formats Go source; reversible via git checkout"},
		{Prefix: "go mod tidy", Description: "updates go.mod/go.sum; reversible"},
		{Prefix: "go mod download", Description: "downloads module dependencies"},
		{Prefix: "go generate", Description: "runs code generation directives"},
		{Prefix: "gofmt", Description: "formats Go source; reversible"},
		{Prefix: "npm test", Description: "runs package tests"},
		{Prefix: "npm run", Description: "runs package.json scripts"},
		{Prefix: "npm ci", Description: "installs dependencies from lock file"},
		{Prefix: "npm install", Description: "installs dependencies"},
		{Prefix: "npx ", Description: "executes package binaries"},
		{Prefix: "yarn test", Description: "runs package tests via yarn"},
		{Prefix: "yarn build", Description: "builds via yarn"},
		{Prefix: "yarn install", Description: "installs dependencies via yarn"},
		{Prefix: "pnpm test", Description: "runs package tests via pnpm"},
		{Prefix: "pnpm build", Description: "builds via pnpm"},
		{Prefix: "pnpm install", Description: "installs dependencies via pnpm"},
		{Prefix: "cargo test", Description: "runs Rust tests"},
		{Prefix: "cargo build", Description: "compiles Rust crate"},
		{Prefix: "cargo check", Description: "type-checks Rust crate without producing binaries"},
		{Prefix: "cargo clippy", Description: "runs Rust linter"},
		{Prefix: "cargo fmt", Description: "formats Rust source; reversible"},
		{Prefix: "cargo add", Description: "adds a dependency to Cargo.toml; reversible"},
		{Prefix: "rustfmt", Description: "formats Rust source; reversible"},
		{Prefix: "make", Description: "runs Makefile targets"},
		{Prefix: "pytest", Description: "runs Python tests"},
		{Prefix: "python -m pytest", Description: "runs Python tests via module"},
		{Prefix: "python -m pip install", Description: "installs Python packages"},
		{Prefix: "pip install", Description: "installs Python packages"},
		{Prefix: "tsc", Description: "TypeScript compiler; writes declaration files"},
		{Prefix: "eslint", Description: "JavaScript/TypeScript linter"},
		{Prefix: "prettier", Description: "code formatter; reversible"},
		{Prefix: "vitest", Description: "runs Vitest tests"},
		{Prefix: "jest", Description: "runs Jest tests"},
		{Prefix: "terraform validate", Description: "validates Terraform configuration; read-only"},
		{Prefix: "terraform plan", Description: "previews Terraform changes; read-only"},
		{Prefix: "docker build", Description: "builds container image; local only"},
		{Prefix: "docker compose build", Description: "builds compose services; local only"},
		{Prefix: "ls ", Description: "lists directory contents"},
		{Prefix: "cat ", Description: "reads file contents"},
		{Prefix: "head ", Description: "reads first lines of a file"},
		{Prefix: "tail ", Description: "reads last lines of a file"},
		{Prefix: "grep ", Description: "searches file contents"},
		{Prefix: "find ", Description: "finds files by criteria"},
		{Prefix: "wc ", Description: "counts lines/words/bytes"},
		{Prefix: "sort ", Description: "sorts input lines"},
		{Prefix: "uniq ", Description: "filters adjacent duplicates"},
		{Prefix: "diff ", Description: "compares files"},
		{Prefix: "which ", Description: "locates executables"},
		{Prefix: "file ", Description: "identifies file types"},
		{Prefix: "du ", Description: "reports disk usage"},
		{Prefix: "df ", Description: "reports filesystem usage"},
		{Prefix: "stat ", Description: "displays file metadata"},
		{Prefix: "pwd", Description: "prints working directory"},
		{Prefix: "echo ", Description: "prints arguments"},
		{Prefix: "printf ", Description: "formats and prints"},
		{Prefix: "date", Description: "displays date/time"},
		{Prefix: "env", Description: "displays or sets environment"},
		{Prefix: "whoami", Description: "displays current user"},
		{Prefix: "uname ", Description: "displays system information"},
		{Prefix: "true", Description: "no-op"},
		{Prefix: "false", Description: "no-op"},
		{Prefix: "test ", Description: "evaluates conditional expressions"},
		{Prefix: "[ ", Description: "evaluates conditional expressions (POSIX test)"},
	}
}

// MatchesSafeCommand reports whether the command starts with any safe
// prefix. Matching is on the literal command text after leading
// whitespace. Compound commands (a && b) only match if every segment
// is safe.
func MatchesSafeCommand(rules []CommandSafetyRule, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	// Split compound commands on && and || and check every segment.
	for _, segment := range splitCompound(command) {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		if !matchesAnyPrefix(rules, segment) {
			return false
		}
	}
	return true
}

// splitCompound splits on shell logical operators, preserving quoted
// segments. This is a conservative approximation: if splitting is
// ambiguous the command is not safe.
func splitCompound(command string) []string {
	var segments []string
	var current strings.Builder
	depth := 0
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '\'', '"':
			depth ^= 1
			current.WriteByte(command[i])
		case '&', '|':
			if depth == 0 && i+1 < len(command) && command[i+1] == command[i] {
				segments = append(segments, current.String())
				current.Reset()
				i++ // skip the second & or |
			} else {
				current.WriteByte(command[i])
			}
		default:
			current.WriteByte(command[i])
		}
	}
	segments = append(segments, current.String())
	return segments
}

func matchesAnyPrefix(rules []CommandSafetyRule, command string) bool {
	for _, rule := range rules {
		if strings.HasPrefix(command, rule.Prefix) {
			// Ensure the prefix ends at a word boundary (space, end,
			// or flag delimiter) to prevent "git addx" matching "git add".
			rest := command[len(rule.Prefix):]
			if rest == "" || rest[0] == ' ' || rest[0] == '\t' ||
				(len(rule.Prefix) > 0 && rule.Prefix[len(rule.Prefix)-1] == ' ') {
				return true
			}
		}
	}
	return false
}
