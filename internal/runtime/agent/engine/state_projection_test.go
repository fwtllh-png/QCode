package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
)

// parseStringConstants extracts the string values of the constants declared
// with typeName in the Go source at path, mirroring the go/ast contract
// checks used by the turnkernel command matrix.
func parseStringConstants(t *testing.T, path, typeName string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]bool)
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != typeName {
				continue
			}
			for index := range value.Names {
				if index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				if text, unquoteErr := strconv.Unquote(literal.Value); unquoteErr == nil {
					declared[text] = true
				}
			}
		}
	}
	return declared
}

// TestPhaseStateTableCoversBothVocabularies keeps the kernel Phase and the
// host-facing State vocabularies from drifting apart: every declared phase
// must own a non-empty bucket, every declared state must appear in at least
// one bucket, and the table may not reference constants that no longer exist.
func TestPhaseStateTableCoversBothVocabularies(t *testing.T) {
	phases := parseStringConstants(t, "../turnkernel/state.go", "Phase")
	states := parseStringConstants(t, "engine.go", "State")

	if len(phases) == 0 || len(states) == 0 {
		t.Fatalf(
			"parsed vocabularies are empty: phases=%d states=%d",
			len(phases), len(states),
		)
	}

	tableStates := make(map[State]bool)
	for phase, allowed := range phaseStates {
		if !phases[string(phase)] {
			t.Errorf("phaseStates references unknown phase %q", phase)
		}
		if len(allowed) == 0 {
			t.Errorf("phase %q declares no host states", phase)
		}
		for _, state := range allowed {
			tableStates[state] = true
		}
	}
	for phase := range phases {
		if _, ok := phaseStates[turnkernel.Phase(phase)]; !ok {
			t.Errorf("phase %q is missing from phaseStates", phase)
		}
	}
	for state := range states {
		if !tableStates[State(state)] {
			t.Errorf("state %q is not declared in any phase bucket", state)
		}
	}
	for state := range tableStates {
		if !states[string(state)] {
			t.Errorf("phaseStates references unknown state %q", state)
		}
	}
}

// TestTerminalPhasesOnlyAdmitTheirTerminalState keeps the terminal boundary
// strict: a terminal kernel phase admits exactly its matching terminal host
// state plus Compacting for post-terminal context maintenance receipts, and
// no other host state.
func TestTerminalPhasesOnlyAdmitTheirTerminalState(t *testing.T) {
	for phase, want := range map[turnkernel.Phase]State{
		turnkernel.PhaseCompleted: Completed,
		turnkernel.PhaseFailed:    Failed,
		turnkernel.PhaseCanceled:  Canceled,
	} {
		allowed := phaseStates[phase]
		if len(allowed) != 2 || allowed[0] != want || allowed[1] != Compacting {
			t.Fatalf(
				"phase %q maps to %v, want exactly [%s Compacting]",
				phase, allowed, want,
			)
		}
	}
	for phase, allowed := range phaseStates {
		if !phase.Terminal() {
			continue
		}
		for _, state := range allowed {
			if state == Compacting {
				continue
			}
			terminal := state == Completed || state == Failed || state == Canceled
			if !terminal {
				t.Errorf(
					"non-terminal state %q declared for terminal phase %q",
					state, phase,
				)
			}
		}
	}
	for phase, allowed := range phaseStates {
		if phase.Terminal() {
			continue
		}
		for _, state := range allowed {
			terminal := state == Completed || state == Failed || state == Canceled
			if terminal {
				t.Errorf(
					"terminal state %q declared for non-terminal phase %q",
					state, phase,
				)
			}
		}
	}
}

func TestStateAllowedForPhase(t *testing.T) {
	cases := []struct {
		state State
		phase turnkernel.Phase
		want  bool
	}{
		{Preparing, turnkernel.PhaseCreated, true},
		{Preparing, turnkernel.PhasePreparing, true},
		{Preparing, turnkernel.PhaseVerifying, true},
		{Compacting, turnkernel.PhasePreparing, true},
		{Compacting, turnkernel.PhaseSampling, true},
		{Compacting, turnkernel.PhaseFailed, true},
		{Compacting, turnkernel.PhaseExecutingTools, false},
		{CallingModel, turnkernel.PhaseSampling, true},
		{Streaming, turnkernel.PhaseSampling, true},
		{Streaming, turnkernel.PhaseExecutingTools, true},
		{RunningTools, turnkernel.PhaseExecutingTools, true},
		{RunningTools, turnkernel.PhaseSampling, true},
		{RunningTools, turnkernel.PhaseAwaitingInput, true},
		{RunningTools, turnkernel.PhaseVerifying, false},
		{FeedingResults, turnkernel.PhaseSampling, true},
		{Verifying, turnkernel.PhaseVerifying, true},
		{Verifying, turnkernel.PhaseSampling, true},
		{AwaitingRecovery, turnkernel.PhaseCommitting, true},
		{Completed, turnkernel.PhaseCompleted, true},
		{Completed, turnkernel.PhaseSampling, false},
		{Failed, turnkernel.PhaseFailed, true},
		{Canceled, turnkernel.PhaseCanceled, true},
		{Canceled, turnkernel.PhaseCompleted, false},
	}
	for _, testCase := range cases {
		if got := stateAllowedForPhase(testCase.state, testCase.phase); got != testCase.want {
			t.Errorf(
				"stateAllowedForPhase(%s, %s) = %v, want %v",
				testCase.state, testCase.phase, got, testCase.want,
			)
		}
	}
}
