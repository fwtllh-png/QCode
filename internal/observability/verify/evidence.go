package verify

import (
	"path/filepath"
	"slices"
	"strings"
)

const EvidenceMetadataKey = "verification_evidence"
const StatusRunning = "running"

// Evidence is a successful or failed declared verification command together with
// the exact workspace paths it claims to cover. The engine adds CallID and
// MutationRevision after execution; tool arguments cannot choose either value.
type Evidence struct {
	SchemaVersion     int      `json:"schema_version"`
	Kind              string   `json:"kind"`
	Status            string   `json:"status"`
	CoveredPaths      []string `json:"covered_paths"`
	CommandDigest     string   `json:"command_digest"`
	Command           string   `json:"command,omitempty"`
	CWD               string   `json:"cwd,omitempty"`
	InputDigest       string   `json:"input_digest,omitempty"`
	CallID            string   `json:"call_id,omitempty"`
	StartedCallID     string   `json:"started_call_id,omitempty"`
	ExitCode          int      `json:"exit_code"`
	WorkspaceRevision uint64   `json:"workspace_revision,omitempty"`
	MutationRevision  uint64   `json:"mutation_revision,omitempty"`
	// InvalidationReason explains a post-execution downgrade to
	// StatusInvalidated; empty for every other status.
	InvalidationReason string `json:"invalidation_reason,omitempty"`
}

func (e Evidence) Bind(callID string, workspaceRevision, mutationRevision uint64) Evidence {
	e.CallID, e.WorkspaceRevision, e.MutationRevision =
		callID, workspaceRevision, mutationRevision
	return e
}

// CanonicalEvidencePath normalizes one workspace-relative evidence path.
func CanonicalEvidencePath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) {
		return "", false
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(clean), true
}

// CommandEvidenceReceipt reduces current-revision command evidence into a
// verification verdict. Failed commands remain visible but never cover paths.
func CommandEvidenceReceipt(
	paths []string,
	mutationRevision uint64,
	inputs []Evidence,
) (Receipt, []string) {
	covered := make(map[string]struct{})
	latest := make(map[string]int, len(inputs))
	for index, evidence := range inputs {
		if evidence.MutationRevision == mutationRevision {
			latest[evidenceKey(evidence)] = index
		}
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	checks := make([]Check, 0, len(keys))
	failed := 0
	for _, key := range keys {
		index := latest[key]
		evidence := inputs[index]
		category := ""
		reason := "declared verification evidence covers exact changed paths"
		switch evidence.Status {
		case StatusPassed:
			for _, path := range evidence.CoveredPaths {
				covered[path] = struct{}{}
			}
		case StatusFailed:
			if failedEvidenceSuperseded(evidence, index, latest, inputs) {
				continue
			}
			failed++
			category, reason = "command_failure", "declared verification command failed"
		case StatusUnavailable:
			category = "verification_unavailable"
			reason = "declared verification command was unavailable"
		}
		command := evidence.Command
		if command == "" {
			command = evidence.CommandDigest
		}
		checks = append(checks, Check{
			CallID: evidence.CallID,
			Name:   evidence.Kind, Command: command,
			Reason: reason, Category: category,
			Status: evidence.Status, ExitCode: evidence.ExitCode,
			InputDigest:       evidence.InputDigest,
			WorkspaceRevision: evidence.WorkspaceRevision,
			MutationRevision:  evidence.MutationRevision,
		})
	}
	uncovered := uncoveredEvidencePaths(paths, covered)
	if failed != 0 {
		message := "declared verification command failed"
		if len(uncovered) != 0 {
			message += " and does not cover every changed path"
		}
		return Receipt{
			Scope: ScopeCommands, Status: StatusFailed,
			UncoveredPaths: uncovered,
			Checks:         checks, Errors: failed, Message: message,
		}, uncovered
	}
	if len(uncovered) != 0 {
		return Receipt{
			Scope: ScopeCommands, Status: StatusUnavailable, Checks: checks,
			UncoveredPaths: uncovered,
			Message:        "declared verification evidence does not cover every changed path",
		}, uncovered
	}
	return Receipt{
		Scope: ScopeCommands, Status: StatusPassed, Checks: checks,
		Message: "post-mutation declared verification evidence covers every changed path",
	}, nil
}

func evidenceKey(evidence Evidence) string {
	paths := append([]string(nil), evidence.CoveredPaths...)
	slices.Sort(paths)
	return evidence.Kind + "\x00" + evidence.CommandDigest + "\x00" +
		strings.Join(slices.Compact(paths), "\x00")
}

func failedEvidenceSuperseded(
	failed Evidence,
	index int,
	latest map[string]int,
	inputs []Evidence,
) bool {
	remaining := make(map[string]struct{}, len(failed.CoveredPaths))
	for _, path := range failed.CoveredPaths {
		remaining[path] = struct{}{}
	}
	for _, candidateIndex := range latest {
		candidate := inputs[candidateIndex]
		if candidateIndex <= index || candidate.Kind != failed.Kind ||
			candidate.Status != StatusPassed {
			continue
		}
		for _, path := range candidate.CoveredPaths {
			delete(remaining, path)
		}
	}
	return len(remaining) == 0
}

func uncoveredEvidencePaths(paths []string, covered map[string]struct{}) []string {
	uncovered := make([]string, 0)
	for _, path := range paths {
		canonical, ok := CanonicalEvidencePath(path)
		if !ok {
			uncovered = append(uncovered, path)
		} else if _, ok := covered[canonical]; !ok {
			uncovered = append(uncovered, canonical)
		}
	}
	slices.Sort(uncovered)
	return uncovered
}
