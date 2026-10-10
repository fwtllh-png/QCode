package verify

const (
	StatusNotRequired  = "not_required"
	StatusPassed       = "passed"
	StatusFailed       = "failed"
	StatusUnavailable  = "unavailable"
	StatusNotEvaluated = "not_evaluated"
	// StatusInvalidated marks evidence that exited successfully but was
	// disqualified after execution because the command changed the very
	// inputs it claimed to cover. It never counts as coverage.
	StatusInvalidated = "invalidated"
)

const EvidenceMetadataKey = "verification_evidence"
const StatusRunning = "running"

// Evidence decodes historical tool results. New executions never produce it.
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
