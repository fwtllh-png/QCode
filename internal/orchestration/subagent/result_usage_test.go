package subagent_test

import (
	"math"
	"testing"

	"github.com/fwtllh-png/QCode/internal/orchestration/subagent"
)

func TestResultUsageTokensSaturatesInsteadOfWrapping(t *testing.T) {
	overflowing := subagent.ResultUsage{
		InputTokens: math.MaxUint64, OutputTokens: 1,
	}
	if got := overflowing.Tokens(); got != math.MaxUint64 {
		t.Fatalf("overflowing Tokens() = %d, want %d", got, uint64(math.MaxUint64))
	}
	normal := subagent.ResultUsage{InputTokens: 3, OutputTokens: 4}
	if got := normal.Tokens(); got != 7 {
		t.Fatalf("normal Tokens() = %d, want 7", got)
	}
}
