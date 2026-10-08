package wire

import (
	"github.com/fwtllh-png/QCode/internal/config"
	"testing"
)

func TestContextPolicyKeepsNarrativeTokenCeilings(t *testing.T) {
	for _, output := range []int{0, 701} {
		policy := engineContextPolicy(config.Context{Compact: config.Compact{SemanticNarrativeMaxInputTokens: 4096, SemanticNarrativeMaxOutputTokens: output}})
		if policy.NarrativeLimits.MaxInputTokens != 4096 || policy.NarrativeLimits.MaxOutputTokens != uint64(output) || policy.NarrativeLimits.MaxInputBytes != 0 || policy.NarrativeLimits.MaxOutputBytes != 0 || policy.NarrativeLimits.ExcerptMaxBytes != 0 {
			t.Fatalf("token configuration was converted to bytes: %+v", policy.NarrativeLimits)
		}
	}
}
