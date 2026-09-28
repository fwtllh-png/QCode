package admission_test

import (
	"errors"
	"math"
	"testing"

	"github.com/fwtllh-png/QCode/internal/orchestration/admission"
)

func TestGovernorEnforcesDepthConcurrencyAndTokenSpend(t *testing.T) {
	governor := admission.NewGovernor(admission.Limits{
		MaxTokens: 10, MaxDepth: 1, MaxConcurrency: 1,
	})
	if _, err := governor.Admit(2, 0, 0); !errors.Is(err, admission.ErrDepthBudget) {
		t.Fatalf("depth error = %v", err)
	}
	lease, err := governor.Admit(1, 4, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := governor.Admit(1, 0, 0); !errors.Is(err, admission.ErrConcurrency) {
		t.Fatalf("concurrency error = %v", err)
	}
	governor.Release(lease)
	if err := governor.Record(7, 0); !errors.Is(err, admission.ErrTokenBudget) {
		t.Fatalf("token error = %v", err)
	}
	snapshot := governor.Snapshot()
	if snapshot.SpentTokens != 11 || snapshot.SpentCostUSD != 0.5 ||
		snapshot.InFlight != 0 || snapshot.MaxDepthSeen != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGovernorEnforcesCostSpend(t *testing.T) {
	governor := admission.NewGovernor(admission.Limits{MaxCostUSD: 2})
	if err := governor.Record(0, 2.5); !errors.Is(err, admission.ErrCostBudget) {
		t.Fatalf("cost error = %v", err)
	}
}

func TestGovernorAllowsUnboundedZeroLimits(t *testing.T) {
	governor := admission.NewGovernor(admission.Limits{})
	lease, err := governor.Admit(100, 1000, 25)
	if err != nil {
		t.Fatal(err)
	}
	governor.Release(lease)
	if err := governor.Record(1000, 25); err != nil {
		t.Fatal(err)
	}
}

func TestGovernorTokenOverflowSaturatesInsteadOfWrapping(t *testing.T) {
	governor := admission.NewGovernor(admission.Limits{MaxTokens: 100})
	if err := governor.Record(1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := governor.Admit(1, math.MaxUint64, 0); !errors.Is(
		err, admission.ErrTokenBudget,
	) {
		t.Fatalf("overflow admit error = %v", err)
	}
	if err := governor.Record(math.MaxUint64, 0); !errors.Is(
		err, admission.ErrTokenBudget,
	) {
		t.Fatalf("overflow record error = %v", err)
	}
	if snapshot := governor.Snapshot(); snapshot.SpentTokens != math.MaxUint64 {
		t.Fatalf("spent tokens = %d, want %d", snapshot.SpentTokens, uint64(math.MaxUint64))
	}

	unbounded := admission.NewGovernor(admission.Limits{})
	if err := unbounded.Record(math.MaxUint64, 0); err != nil {
		t.Fatal(err)
	}
	if err := unbounded.Record(1, 0); err != nil {
		t.Fatal(err)
	}
	if snapshot := unbounded.Snapshot(); snapshot.SpentTokens != math.MaxUint64 {
		t.Fatalf(
			"saturated spent tokens = %d, want %d",
			snapshot.SpentTokens, uint64(math.MaxUint64),
		)
	}
}
