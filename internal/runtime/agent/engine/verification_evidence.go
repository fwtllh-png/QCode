package engine

import (
	"maps"
	"slices"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
)

func (e *Engine) bindVerificationEvidence(
	call provider.ToolCall,
	result *tool.Result,
	batchMutated bool,
	mutationRevision uint64,
) {
	if result == nil || result.Outcome == nil || result.Outcome.Facts == nil ||
		result.Execution == nil ||
		!result.Execution.VerificationEvidenceAuthorized {
		return
	}
	source := result.Outcome.Facts.Verification
	scope := e.executionScope()
	session := result.Outcome.Facts.ProcessSession
	if source == nil && session != nil && session.SourceSessionID != "" && scope != nil {
		scope.mu.Lock()
		pending, exists := scope.state.pendingVerification[session.SourceSessionID]
		if !session.Running {
			delete(scope.state.pendingVerification, session.SourceSessionID)
		}
		scope.mu.Unlock()
		if !exists || session.Running {
			return
		}
		pending.Status, pending.ExitCode = verify.StatusFailed, session.ExitCode
		if session.ExitCode == 0 && !session.Terminated {
			pending.Status = verify.StatusPassed
		}
		if pending.MutationRevision != mutationRevision || batchMutated {
			pending.Status = verify.StatusUnavailable
		}
		source = &pending
	}
	if source == nil || source.SchemaVersion != 1 {
		return
	}
	evidence := *source
	evidence.CoveredPaths = append([]string(nil), source.CoveredPaths...)
	result.Metadata = maps.Clone(result.Metadata)
	if result.Metadata == nil {
		result.Metadata = make(map[string]any)
	}
	reject := func(reason string) {
		result.Outcome.Facts.Verification = nil
		delete(result.Metadata, verify.EvidenceMetadataKey)
		result.Metadata["verification_evidence_accepted"] = false
		result.Metadata["verification_evidence_rejection"] = reason
	}
	if batchMutated {
		reject("same_batch_mutation")
		return
	}
	switch evidence.Status {
	case verify.StatusPassed, verify.StatusFailed, verify.StatusUnavailable, verify.StatusRunning:
	default:
		reject("invalid_status")
		return
	}
	if len(evidence.CoveredPaths) == 0 {
		reject("missing_covered_paths")
		return
	}
	covered := make([]string, 0, len(evidence.CoveredPaths))
	for _, path := range evidence.CoveredPaths {
		relative, ok := verify.CanonicalEvidencePath(path)
		if !ok {
			reject("invalid_covered_path")
			return
		}
		if !slices.Contains(covered, relative) {
			covered = append(covered, relative)
		}
	}
	slices.Sort(covered)
	evidence.CoveredPaths = covered
	if evidence.StartedCallID == "" {
		evidence.StartedCallID = call.ID
	}
	evidence = evidence.Bind(call.ID, e.sessionRevision, mutationRevision)
	result.Outcome.Facts.Verification = &evidence
	result.Metadata[verify.EvidenceMetadataKey] = evidence
	if evidence.Status == verify.StatusRunning {
		if scope != nil && session != nil && session.Running && session.SourceSessionID != "" {
			scope.mu.Lock()
			if scope.state.pendingVerification == nil {
				scope.state.pendingVerification = make(map[string]verify.Evidence)
			}
			scope.state.pendingVerification[session.SourceSessionID] = evidence
			scope.mu.Unlock()
		}
		result.Metadata["verification_evidence_accepted"] = false
		result.Metadata["verification_evidence_rejection"] = "process_running"
		return
	}
	if evidence.InputDigest != "" {
		digest, err := verify.InputDigest(e.options.Workspace, covered)
		if err != nil || digest != evidence.InputDigest {
			evidence.Status = verify.StatusUnavailable
			result.Metadata["verification_evidence_rejection"] = "inputs_changed"
		}
	}
	result.Outcome.Facts.Verification = &evidence
	result.Metadata[verify.EvidenceMetadataKey] = evidence
	result.Metadata["verification_evidence_accepted"] = true
	if scope == nil {
		return
	}
	scope.mu.Lock()
	scope.state.verification = append(scope.state.verification, evidence)
	scope.mu.Unlock()
}

func (e *Engine) verificationEvidence() []verify.Evidence {
	scope := e.currentScope()
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	evidence := append([]verify.Evidence(nil), scope.state.verification...)
	scope.mu.Unlock()
	// Full Access processes and external editors may change covered inputs
	// without a file-tool mutation receipt. Recheck the authoritative bytes
	// before using prior evidence to approve completion.
	for index := range evidence {
		item := &evidence[index]
		if item.Status != verify.StatusPassed || item.InputDigest == "" {
			continue
		}
		digest, err := verify.InputDigest(e.options.Workspace, item.CoveredPaths)
		if err != nil || digest != item.InputDigest {
			item.Status = verify.StatusInvalidated
			item.InvalidationReason = "covered_paths changed after verification"
		}
	}
	return evidence
}
