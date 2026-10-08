package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

const (
	VerifyModeOff  = verify.ModeOff
	VerifyModeSoft = verify.ModeSoft
	VerifyModeHard = verify.ModeHard

	VerifyOnFailureFail   = verify.OnFailureFail
	VerifyOnFailureRevert = verify.OnFailureRevert
)

type VerifyOptions = verify.GateOptions
type VerificationReceipt = verify.GateReceipt
type VerificationWorkspace = verify.Workspace

type verifyAction string

const (
	// verifyActionSkipped means the gate had nothing to judge: it is off, has no
	// runner. An unchanged workspace gets an explicit not_required receipt.
	verifyActionSkipped verifyAction = "skipped"
	verifyActionPassed  verifyAction = "passed"
	verifyActionRepair  verifyAction = "repair"
	// verifyActionReported is a soft-mode failure: recorded, turn unaffected.
	verifyActionReported verifyAction = "reported"
	verifyActionBlocked  verifyAction = "blocked"
	verifyActionFailed   verifyAction = "failed"
	verifyActionReverted verifyAction = "reverted"
)

type verifyOutcome struct {
	action  verifyAction
	receipt *VerificationReceipt
}

// verifyGate holds the per-turn gate state: how much of the repair budget the
// turn has spent, which is also the extra step allowance it has earned.
type verifyGate struct {
	engine   *Engine
	kernel   *turnkernel.RuntimeKernel
	attempts []verify.Receipt
}

// extraSteps keeps repair rounds out of the model's normal step budget, so a
// gate failure never costs the model a step it would otherwise have used.
func (g *verifyGate) extraSteps() int {
	if g == nil {
		return 0
	}
	return int(g.kernel.RepairSteps(turnkernel.RepairVerification))
}

// evaluate runs one verification pass over the files the turn changed.
func (g *verifyGate) evaluate(
	ctx context.Context, send func(State, Event) error,
) (verifyOutcome, error) {
	options := g.engine.options.Verify
	if !options.Enabled() && (g.kernel == nil || !g.kernel.Snapshot().Policy.VerificationRequired) {
		return verifyOutcome{action: verifyActionSkipped}, nil
	}
	paths := changedPaths(g.engine.TurnDiff())
	if g.kernel == nil {
		return verifyOutcome{}, protocol.NewProblem(
			protocol.CodeInternal,
			"turn kernel is required for verification",
			false,
			nil,
		)
	}
	// Restored turns retain the policy that admitted their work.
	policy := g.kernel.Snapshot().Policy
	if policy.VerificationMode != "" {
		options.Mode = policy.VerificationMode
	}
	if err := g.kernel.BeginVerification(); err != nil {
		return verifyOutcome{}, err
	}
	scope := options.Scope
	if scope == "" {
		scope = verify.ScopeDiagnostics
	}
	verifyCtx := ctx
	if options.Timeout > 0 {
		var cancel context.CancelFunc
		verifyCtx, cancel = context.WithTimeout(ctx, options.Timeout)
		defer cancel()
	}
	span := g.engine.tracer().Start(trace.NameVerify, 0, map[string]any{
		"scope": string(scope), "repair_step": g.extraSteps(),
	})
	mutationRevision := g.kernel.MutationRevision()
	var receipt verify.Receipt
	var err error
	if len(paths) == 0 {
		// Absence of net changes is a recorded applicability decision, not a test pass.
		receipt = verify.Receipt{
			Scope: scope, Status: verify.StatusNotRequired,
			Message: "no net workspace changes remain; no verification checks were run",
		}
	} else if options.Runner == nil {
		receipt = verify.Receipt{Scope: scope, Status: verify.StatusUnavailable, Message: "verification runner is not configured", UncoveredPaths: append([]string(nil), paths...)}
	} else {
		receipt, err = options.Runner.Verify(verifyCtx, verify.Request{
			RequireConfiguredCommand: options.Mode == VerifyModeHard,
			Scope:                    scope, Paths: paths, Diagnostics: g.engine.turnDiagnostics(),
			WorkspaceRevision: g.engine.sessionRevision, MutationRevision: mutationRevision,
			Evidence: g.engine.verificationEvidence(),
		})
	}
	if err != nil {
		span.Set("error", errorText(err))
		span.End(trace.StatusError)
		receipt = verify.Receipt{
			Scope: scope, Status: verify.StatusUnavailable, Message: err.Error(),
			UncoveredPaths: append([]string(nil), paths...),
		}
	} else {
		span.Set("status", receipt.Status)
		span.End(trace.StatusOK)
	}
	actionValue, err := g.kernel.FinishVerification(g.verificationCommand(receipt))
	if err != nil {
		return verifyOutcome{}, err
	}
	action := verifyAction(actionValue)
	g.attempts = append(g.attempts, receipt)
	// Only a pass clears the evidence gap. A failed or unavailable run leaves the
	// change exactly as unproved as it was before the gate ran.
	if receipt.Status == verify.StatusPassed {
		for _, path := range paths {
			g.engine.contextAuthority().ObservePath(
				g.engine.options.Workspace, agentcontext.SourceVerified, g.engine.turn, path,
			)
		}
		g.engine.contextAuthority().ObserveVerified(
			g.engine.options.Workspace,
			paths,
		)
	} else if receipt.Status != verify.StatusNotRequired {
		// An unavailable run is recorded too, and says so: "nobody could check
		// this" is a different instruction to the next turn than "the check ran
		// and it is broken", and neither is silence.
		g.engine.contextAuthority().Failures().NoteVerify(
			g.engine.turn,
			string(receipt.Scope),
			string(receipt.Status),
			verify.FailureDetail(receipt),
		)
	}
	observed := &VerificationReceipt{
		Receipt: receipt, Mode: options.Mode, Action: string(action),
		RepairSteps: g.extraSteps(), Paths: paths,
		Attempts: append([]verify.Receipt(nil), g.attempts...),
	}
	if len(paths) == 0 {
		observed.Workspace = &VerificationWorkspace{Status: "unchanged"}
	}
	if err := send(Verifying, Event{Verification: observed}); err != nil {
		return verifyOutcome{}, err
	}
	return verifyOutcome{action: action, receipt: observed}, nil
}

func (g *verifyGate) verificationCommand(
	receipt verify.Receipt,
) turnkernel.VerificationFinished {
	var evidenceCalls []string
	stableChecks := append([]verify.Check(nil), receipt.Checks...)
	for index := range stableChecks {
		if stableChecks[index].CallID != "" {
			evidenceCalls = append(evidenceCalls, stableChecks[index].CallID)
		}
		stableChecks[index].CallID = ""
	}
	sort.Strings(evidenceCalls)
	// Execution IDs are provenance, not progress: a fresh ID must not reset
	// the repair budget for an unchanged failed check.
	signature, _ := json.Marshal(stableChecks)
	key := fmt.Sprintf("mutation=%d;status=%s;checks=%x",
		g.kernel.MutationRevision(), receipt.Status, sha256.Sum256(signature))
	return turnkernel.VerificationFinished{
		Status:        kernelVerificationStatus(receipt.Status),
		EvidenceCalls: evidenceCalls,
		Message:       receipt.Message,
		RepairKey:     key,
	}
}

func kernelVerificationStatus(
	status string,
) turnkernel.VerificationStatus {
	switch status {
	case verify.StatusNotRequired:
		return turnkernel.VerificationNotRequired
	case verify.StatusPassed:
		return turnkernel.VerificationPassed
	case verify.StatusFailed:
		return turnkernel.VerificationFailed
	default:
		return turnkernel.VerificationUnavailable
	}
}

// feedback is the repair prompt for the model. It uses a user message with the
// [verify] prefix (the convention mailbox already follows) rather than a faked
// tool result: the model never issued this call, and inventing a tool_call /
// tool_result pair pollutes the history and can trip provider-side pairing
// checks.
func verifyFeedback(receipt *VerificationReceipt, turn uint64) provider.Message {
	return verify.FeedbackMessage(receipt, turn)
}

func changedPaths(entries []turnkernel.TurnDiffEntry) []string {
	unique := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Path == "" {
			continue
		}
		unique[entry.Path] = struct{}{}
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
