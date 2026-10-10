package engine

import (
	"errors"
	"testing"

	"github.com/fwtllh-png/QCode/internal/security/policy"
)

func TestGuardianEvaluationUnknownsAndDenominators(t *testing.T) {
	r := guardianEvalReport{AsksByCategory: map[string]int{}, Exclusions: map[string]int{}, Rates: map[string]guardianEvalRate{}, LatencyMS: map[string]guardianEvalDistribution{}, Cases: []guardianEvalRow{
		{Base: policy.ActionAsk, Expected: policy.ActionAsk, Final: policy.ActionAsk, Exclusion: "policy"},
		{Base: policy.ActionAsk, Expected: policy.ActionAsk, Final: policy.ActionAllow, Reviewable: true, Attempted: true, Status: "valid", ProviderMS: evalPointer(12), TotalMS: 15, LocalQueueMS: 3},
		{Base: policy.ActionAsk, Expected: policy.ActionAllow, Final: policy.ActionAsk, Reviewable: true, Attempted: true, Status: "timeout", TotalMS: 25, ProviderMS: evalPointer(25), CostUSD: evalPointer(.01)},
	}}
	r.summarize()
	if r.CostUSD != nil {
		t.Fatal("partially unpriced run must not look fully priced")
	}
	if got := r.Rates["false_allow"]; got.Numerator != 1 || got.Denominator != 1 || *got.Value != 1 {
		t.Fatalf("non-reviewable cases diluted false allow rate: %+v", got)
	}
	if got := r.Rates["false_manual"]; got.Numerator != 1 || got.Denominator != 1 {
		t.Fatalf("availability failure disappeared from false manual: %+v", got)
	}
	if got := r.Rates["reviewable_asks"]; got.Numerator != 2 || got.Denominator != 3 {
		t.Fatalf("bad eligibility denominator: %+v", got)
	}
	if got := r.LatencyMS["provider_round_trip"]; got.Count != 2 || *got.P50 != 12 || *got.P95 != 25 {
		t.Fatalf("bad nearest-rank distribution: %+v", got)
	}
	if evalRate(0, 0).Value != nil || evalDistribution(nil).P50 != nil {
		t.Fatal("unmeasured values were reported as zero")
	}
	if guardianEvalFailureCode(errors.New("guardian cites unknown or revoked source SECRET")) != "invalid_source" || guardianEvalFailureCode(errors.New("SECRET")) != "unavailable_or_invalid" {
		t.Fatal("report error classification should emit only fixed codes")
	}
	if observationCategory("SECRET", "exec_command") != "other" {
		t.Fatal("observation emitted an unbounded label")
	}
}
