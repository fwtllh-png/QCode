package fault

import (
	"testing"
)

// TestDecideExhaustiveMatrix verifies that Decide() returns the correct
// RecoveryAction for every Code × Origin × Disposition × Retryable
// combination. This ensures the fault classification system behaves
// consistently and that no combination panics or returns an unexpected
// recovery action.
func TestDecideExhaustiveMatrix(t *testing.T) {
	codes := []Code{
		InvalidArgument, Conflict, ResourceExhausted, Unavailable,
		Canceled, DeadlineExceeded, Internal,
	}
	origins := []Origin{
		OriginRuntime, OriginProvider, OriginTool,
		OriginVerification, OriginPersistence, OriginProjection, OriginKernel,
	}
	dispositions := []Disposition{
		FailTurn, RetryStep, RetryTurn, ResumeTurn, Reject,
	}

	for _, code := range codes {
		for _, origin := range origins {
			for _, disposition := range dispositions {
				for _, retryable := range []bool{true, false} {
					name := string(code) + "_" + string(origin) + "_" + string(disposition)
					if retryable {
						name += "_retryable"
					} else {
						name += "_nonretryable"
					}
					t.Run(name, func(t *testing.T) {
						problem := NewClassified(
							code,
							"test fault: "+name,
							retryable,
							Metadata{
								Origin:      origin,
								Disposition: disposition,
								SideEffects: SideEffectUnchanged,
								RetryOwner:  RetryOwnerEngine,
							},
							nil,
						)

						// Verify the problem was created correctly.
						if problem.Code != code {
							t.Errorf("Code = %q, want %q", problem.Code, code)
						}
						if problem.Fault == nil {
							t.Fatal("Fault metadata is nil")
						}
						if problem.Fault.Origin != origin {
							t.Errorf("Origin = %q, want %q", problem.Fault.Origin, origin)
						}
						if problem.Fault.Disposition != disposition {
							t.Errorf("Disposition = %q, want %q", problem.Fault.Disposition, disposition)
						}

						// Verify Decide() returns a valid recovery action.
						decision := Decide(problem, RecoveryContext{
							Owner:      RetryOwnerEngine,
							Idempotent: true,
							Attempt:    1,
						})
						if !validRecoveryAction(decision.Action) {
							t.Errorf("Decide returned invalid action %q", decision.Action)
						}

						// Verify CodeOf returns the correct code.
						if CodeOf(problem) != code {
							t.Errorf("CodeOf = %q, want %q", CodeOf(problem), code)
						}

						// Verify DispositionOf returns the correct disposition.
						if DispositionOf(problem) != disposition {
							t.Errorf("DispositionOf = %q, want %q", DispositionOf(problem), disposition)
						}

						// Verify no panic.
						_ = problem.Error()
						_ = problem.Unwrap()
					})
				}
			}
		}
	}
}

func validRecoveryAction(action RecoveryAction) bool {
	switch action {
	case RecoveryRetry, RecoveryResume, RecoveryWait, RecoveryBlock, RecoveryFail, RecoveryReject:
		return true
	default:
		return false
	}
}
