// Package verify reduces execution and diagnostic evidence into receipts.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/observability/diagnostics"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

// Scope selects what a verification pass looks at.
type Scope string

const (
	// ScopeDiagnostics reuses the post-edit diagnostics already collected for
	// the files a turn touched, so it costs no extra process.
	ScopeDiagnostics Scope = "diagnostics"
	// ScopeRepository requires recorded repository verification commands.
	ScopeRepository Scope = "repository"
	// ScopeAffected requires recorded verification covering changed files.
	ScopeAffected Scope = "affected"
	// ScopeCommands is a model-invoked verification command whose exact covered paths
	// the engine bound to the current workspace mutation revision.
	ScopeCommands Scope = "commands"
)

const (
	StatusPassed       = "passed"
	StatusFailed       = "failed"
	StatusUnavailable  = "unavailable"
	StatusNotEvaluated = "not_evaluated"
	// StatusInvalidated marks evidence that exited successfully but was
	// disqualified after execution because the command changed the very
	// inputs it claimed to cover. It never counts as coverage.
	StatusInvalidated = "invalidated"
)

// Check is one verification command and, once run, its outcome.
type Check struct {
	CallID            string `json:"call_id,omitempty"`
	Name              string `json:"name"`
	Command           string `json:"command"`
	Reason            string `json:"reason"`
	Category          string `json:"category,omitempty"`
	Status            string `json:"status,omitempty"`
	ExitCode          int    `json:"exit_code,omitempty"`
	Stdout            string `json:"stdout,omitempty"`
	Stderr            string `json:"stderr,omitempty"`
	InputDigest       string `json:"input_digest,omitempty"`
	WorkspaceRevision uint64 `json:"workspace_revision,omitempty"`
	MutationRevision  uint64 `json:"mutation_revision,omitempty"`
}

// Receipt is the verdict of one verification pass.
type Receipt struct {
	UncoveredPaths []string `json:"uncovered_paths,omitempty"`
	Scope          Scope    `json:"scope"`
	Status         string   `json:"status"`
	Checks         []Check  `json:"checks,omitempty"`
	Errors         int      `json:"errors,omitempty"`
	Warnings       int      `json:"warnings,omitempty"`
	Message        string   `json:"message,omitempty"`
}

// Failed reports whether the pass produced a verdict the gate must act on.
// Unavailable is not a failure: a workspace without verification commands must
// not block every turn.
func (r Receipt) Failed() bool { return r.Status == StatusFailed }

// Feedback renders the receipt for the model, bounded so a noisy test suite
// cannot flood the context.
func (r Receipt) Feedback(limit int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "verification (%s) failed", r.Scope)
	if r.Message != "" {
		fmt.Fprintf(&builder, ": %s", r.Message)
	}
	builder.WriteByte('\n')
	for _, check := range r.Checks {
		if check.Status == StatusPassed || check.Status == StatusUnavailable {
			continue
		}
		fmt.Fprintf(&builder, "$ %s (exit %d)\n", check.Command, check.ExitCode)
		if output := strings.TrimSpace(check.Stdout + "\n" + check.Stderr); output != "" {
			builder.WriteString(output)
			builder.WriteByte('\n')
		}
	}
	text := builder.String()
	if limit > 0 && len(text) > limit {
		return text[:limit] + "\n[verification output truncated]"
	}
	return text
}

// Request describes one verification pass. Diagnostics carries the post-edit
// receipts the turn already collected, so ScopeDiagnostics needs no new process.
type Request struct {
	Scope             Scope
	Paths             []string
	Diagnostics       []diagnostics.Receipt
	WorkspaceRevision uint64
	MutationRevision  uint64
	Evidence          []Evidence
}

// Runner performs one verification pass.
type Runner interface {
	Verify(context.Context, Request) (Receipt, error)
}

// InputDigest binds a verification node to the exact bytes of its declared
// workspace inputs. An empty path set deliberately disables reuse because the
// runtime cannot prove which workspace content the command consumed.
func InputDigest(root string, paths []string) (string, error) {
	normalized := relativePaths(root, paths)
	for _, path := range normalized {
		if _, valid := CanonicalEvidencePath(path); !valid {
			return "", fmt.Errorf("verification path is outside the workspace: %q", path)
		}
	}
	paths = normalized
	if len(paths) == 0 {
		return "", nil
	}
	sort.Strings(paths)
	hash := sha256.New()
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		hash.Write([]byte(path))
		hash.Write([]byte{0})
		file, err := workspace.OpenFile(path)
		if errors.Is(err, os.ErrNotExist) {
			hash.Write([]byte("missing"))
		} else if err != nil {
			return "", err
		} else {
			hash.Write([]byte("file\x00"))
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return "", errors.Join(copyErr, closeErr)
			}
		}
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// FromDiagnostics turns the post-edit diagnostics of a turn into a verdict.
// Only error-severity diagnostics fail the pass; warnings are reported so the
// receipt still shows why a pass was noisy. When no receipt covers the changed
// paths the pass is unavailable rather than passed, so a missing diagnostics
// runner never reads as a green light.
func FromDiagnostics(receipts []diagnostics.Receipt, paths []string) Receipt {
	receipt := Receipt{Scope: ScopeDiagnostics, Status: StatusPassed}
	evaluated := 0
	covered := make(map[string]bool)
	for _, item := range receipts {
		if len(paths) > 0 && !containsPath(paths, item.Path) {
			continue
		}
		switch item.Status {
		case "unavailable":
			continue
		case "failed":
			receipt.Status = StatusFailed
			receipt.Errors++
			evaluated++
			receipt.Checks = append(receipt.Checks, Check{
				Name: diagnosticsCheckName(item), Command: "diagnostics " + item.Path,
				Reason:   "post-edit diagnostics cover the changed file",
				Category: "diagnostic_failure", Status: StatusFailed, Stderr: item.Message,
			})
			continue
		case "completed":
		default:
			continue
		}
		evaluated++
		covered[item.Path] = true
		var messages []string
		errors := 0
		for _, diagnostic := range item.Diagnostics {
			switch diagnostic.Severity {
			case "error":
				errors++
				receipt.Errors++
				messages = append(messages, formatDiagnostic(item.Path, diagnostic))
			case "warning":
				receipt.Warnings++
			}
		}
		if errors == 0 {
			continue
		}
		receipt.Status = StatusFailed
		receipt.Checks = append(receipt.Checks, Check{
			Name: diagnosticsCheckName(item), Command: "diagnostics " + item.Path,
			Reason:   "post-edit diagnostics cover the changed file",
			Category: "diagnostic_failure",
			Status:   StatusFailed, Stderr: strings.Join(messages, "\n"),
		})
	}
	for _, path := range paths {
		matched := false
		for candidate := range covered {
			matched = matched || samePath(path, candidate)
		}
		if !matched {
			receipt.UncoveredPaths = append(receipt.UncoveredPaths, path)
		}
	}
	if receipt.Status != StatusFailed && (evaluated == 0 || len(receipt.UncoveredPaths) != 0) {
		receipt.Status = StatusUnavailable
		receipt.Message = "post-edit diagnostics do not cover every changed file"
	}
	return receipt
}

func diagnosticsCheckName(receipt diagnostics.Receipt) string {
	if receipt.Runner != "" {
		return receipt.Runner
	}
	return "diagnostics"
}

func formatDiagnostic(path string, diagnostic diagnostics.Diagnostic) string {
	location := path
	if diagnostic.Path != "" {
		location = diagnostic.Path
	}
	return fmt.Sprintf(
		"%s:%d:%d: %s", location,
		diagnostic.Range.Start.Line+1, diagnostic.Range.Start.Character+1, diagnostic.Message,
	)
}

func containsPath(paths []string, candidate string) bool {
	for _, path := range paths {
		if samePath(path, candidate) {
			return true
		}
	}
	return false
}

// samePath tolerates one side being absolute: the guard canonicalises the paths
// it hands to diagnostics, while the turn diff records workspace-relative ones.
func samePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if left == right {
		return true
	}
	separator := string(filepath.Separator)
	return strings.HasSuffix(left, separator+right) ||
		strings.HasSuffix(right, separator+left)
}

// UnavailableRunner stands in when verification is not configured.
type UnavailableRunner struct{ Message string }

func (r UnavailableRunner) Verify(_ context.Context, request Request) (Receipt, error) {
	message := r.Message
	if message == "" {
		message = "verification runner is not configured"
	}
	return Receipt{
		Scope: request.Scope, Status: StatusUnavailable, Message: message,
		UncoveredPaths: append([]string(nil), request.Paths...),
	}, nil
}
