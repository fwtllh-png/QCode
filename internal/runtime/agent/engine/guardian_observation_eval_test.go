package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/security/guardian"
)

// This optional read-only census emits fixed categories and counts only. The
// historical log is never copied into testdata or sent to a model. Old approval
// events cannot reconstruct today's policy, immutable content, or source fence.
func TestGuardianHistoricalObservation(t *testing.T) {
	path := os.Getenv("QCODE_GUARDIAN_OBSERVATION_EVENTS")
	if path == "" {
		t.Skip("explicit local event path required")
	}
	out := os.Getenv("QCODE_GUARDIAN_OBSERVATION_REPORT")
	if out == "" {
		t.Fatal("observation report path required")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("cannot open local event log")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal("cannot stat local event log")
	}
	if absolute, err := filepath.Abs(out); err != nil {
		t.Fatal(err)
	} else if source, _ := filepath.Abs(path); absolute == source {
		t.Fatal("report must not overwrite event log")
	}
	// Freeze the append-only byte boundary; concurrent later events are outside
	// this observation. An incomplete record at the boundary fails the census.
	hash := sha256.New()
	decoder := json.NewDecoder(io.TeeReader(io.LimitReader(f, info.Size()), hash))
	report := struct {
		Version            int            `json:"version"`
		Origin             string         `json:"origin"`
		SourceSHA256       string         `json:"source_sha256"`
		Bytes              int64          `json:"snapshot_bytes"`
		Events             int            `json:"events"`
		ToolCalls          int            `json:"tool_calls"`
		AskCount           int            `json:"ask_count"`
		Resolved           int            `json:"resolved_count"`
		GuardianFacts      int            `json:"guardian_facts"`
		First              time.Time      `json:"first_event_at"`
		Last               time.Time      `json:"last_event_at"`
		AskTools           map[string]int `json:"asks_by_tool"`
		AskEffects         map[string]int `json:"asks_by_effect"`
		AskRisks           map[string]int `json:"asks_by_risk"`
		Content            map[string]int `json:"exec_ask_content_checks"`
		ProductionCoverage *float64       `json:"production_reviewable_fraction"`
		Unmeasured         []string       `json:"unmeasured"`
	}{Version: 1, Origin: "local_runtime_event_snapshot", Bytes: info.Size(), AskTools: map[string]int{}, AskEffects: map[string]int{}, AskRisks: map[string]int{}, Content: map[string]int{}, Unmeasured: []string{"current_policy_eligibility", "prepared_snapshot_coverage", "authorization_source_completeness", "counterfactual_model_quality", "success_notifications"}}
	requests := map[string]bool{}
	for {
		var event struct {
			Kind    string    `json:"kind"`
			Created time.Time `json:"created_at"`
			Data    struct {
				RequestID string          `json:"request_id"`
				Tool      string          `json:"tool"`
				Effect    string          `json:"effect"`
				Risk      string          `json:"risk"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"data"`
		}
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("event snapshot is malformed or incomplete; no partial census accepted")
		}
		report.Events++
		if report.First.IsZero() || event.Created.Before(report.First) {
			report.First = event.Created
		}
		if event.Created.After(report.Last) {
			report.Last = event.Created
		}
		switch event.Kind {
		case "tool.start":
			report.ToolCalls++
		case "guardian.review":
			report.GuardianFacts++
		case "approval.resolved":
			report.Resolved++
		case "approval.required":
			if event.Data.RequestID == "" || requests[event.Data.RequestID] {
				t.Fatal("missing or duplicate approval identity")
			}
			requests[event.Data.RequestID] = true
			report.AskCount++
			tool := observationCategory(event.Data.Tool, "exec_command", "run_command", "apply_patch", "file_write", "fetch_page", "web_search")
			report.AskTools[tool]++
			report.AskEffects[observationCategory(event.Data.Effect, "process.mutating", "process.read_only", "network.read", "network.mutating", "file.write", "external")]++
			report.AskRisks[observationCategory(event.Data.Risk, "low", "medium", "high", "critical")]++
			if tool == "exec_command" || tool == "run_command" {
				var args struct {
					Command string `json:"command"`
					CWD     string `json:"cwd"`
					Target  string `json:"execution_target"`
				}
				category := "arguments_unavailable"
				if json.Unmarshal(event.Data.Arguments, &args) == nil && args.Command != "" {
					switch {
					case args.Target == "host":
						category = "host_execution_excluded"
					case filepath.IsAbs(args.CWD):
						category = "cwd_needs_snapshot"
					default:
						coverage := guardian.AnalyzeContent(args.Command, ".", nil)
						category = "dependencies_or_syntax_need_evidence"
						if len(coverage.Missing) == 0 {
							category = "literal_content_closed_policy_unknown"
						}
					}
				}
				report.Content[category]++
			}
		}
	}
	report.SourceSHA256 = hex.EncodeToString(hash.Sum(nil))
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(encoded, '\n'), 0600); err != nil {
		t.Fatal("cannot write observation report")
	}
	t.Logf("events=%d calls=%d asks=%d Guardian facts=%d; production eligibility unknown", report.Events, report.ToolCalls, report.AskCount, report.GuardianFacts)
}

func observationCategory(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return candidate
		}
	}
	return "other"
}
