package contextview

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

func TestNarrativeSelectionUsesOnlyValidNonduplicatedSources(t *testing.T) {
	now := time.Now().UTC()
	history := []provider.Message{provider.TextMessage(provider.RoleUser, strings.Repeat("完整来源；", 500))}
	history[0].Turn = 1
	input, err := agentcontext.BuildNarrativeInput("thread", "window", "authority", "route", history, agentcontext.NarrativeLimits{}, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, source := range input.Excerpts {
		ids = append(ids, source.MessageID)
	}
	text, _ := json.Marshal(map[string]any{
		"technical_concepts": []any{}, "files_and_code": []any{}, "errors_and_fixes": []any{},
		"pending_jobs": []any{}, "current_work": []any{}, "next_steps": []any{}, "critical_context": []any{},
		"decisions": []any{}, "rationale": []any{}, "unresolved": []any{},
		"preferences": []any{map[string]any{"text": "保留完整定义。", "source_message_ids": ids}},
	})
	artifact, err := agentcontext.ValidateNarrativeJSON(text, input, agentcontext.NarrativeLimits{}, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, reason                             string
		enabled, raw, extract, fits, unavailable bool
		at                                       time.Time
		route                                    string
	}{
		{name: "cached with generation off", enabled: true, fits: true, at: now, route: "route"},
		{name: "ledger only", reason: "digest_disabled", fits: true, at: now, route: "route"},
		{name: "raw duplicate", reason: "duplicate_representation", enabled: true, raw: true, fits: true, at: now, route: "route"},
		{name: "extract duplicate", reason: "duplicate_representation", enabled: true, extract: true, fits: true, at: now, route: "route"},
		{name: "expired", reason: "invalid_or_expired", enabled: true, fits: true, at: now.Add(2 * time.Hour), route: "route"},
		{name: "model switch", reason: "route_or_window_changed", enabled: true, fits: true, at: now, route: "other"},
		{name: "capacity", reason: "context_capacity", enabled: true, at: now, route: "route"},
		{name: "withdrawn source", reason: "source_unavailable", enabled: true, unavailable: true, fits: true, at: now, route: "route"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			projection := agentcontext.ProjectionResult{}
			if scenario.unavailable {
				projection.Omissions = []agentcontext.ProjectionOmission{{Source: agentcontext.ProjectionSource{Index: 0}, Reason: agentcontext.OmittedSourceUnavailable}}
			}
			if scenario.raw {
				projection.Selected = []agentcontext.ProjectionSource{{Index: 0}}
			}
			if scenario.extract {
				projection.References = []agentcontext.ReferenceCoverage{{ContentDigest: input.Excerpts[0].Source.Digest}}
			}
			message, rows, err := SelectNarrative(&artifact, scenario.enabled, scenario.route, "window", history, projection, scenario.at, func(provider.Message) (bool, error) { return scenario.fits, nil })
			if err != nil || len(rows) == 0 {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			if (message != nil) != (scenario.reason == "") || rows[0].Reason != scenario.reason {
				t.Fatalf("message=%v rows=%+v", message, rows)
			}
		})
	}
}
