package intergration_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// benchmarkRatio is a measured rate with its evidence counts. Value is nil when there
// was no eligible sample, so "not measured" cannot be mistaken for zero.
type benchmarkRatio struct {
	Value       *float64 `json:"value"`
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
}

// benchmarkLatencyPercentiles reports nearest-rank task wall-clock percentiles.
type benchmarkLatencyPercentiles struct {
	Samples int   `json:"samples"`
	P50MS   int64 `json:"p50_ms"`
	P95MS   int64 `json:"p95_ms"`
}

// benchmarkBaselineMetrics is the Stage 0 upgrade baseline derived from task results.
type benchmarkBaselineMetrics struct {
	TaskSuccessRate      benchmarkRatio              `json:"task_success_rate"`
	Latency              benchmarkLatencyPercentiles `json:"latency"`
	RetryRate            benchmarkRatio              `json:"retry_rate"`
	VerificationCoverage benchmarkRatio              `json:"verification_coverage"`
	UnknownCostRate      benchmarkRatio              `json:"unknown_cost_rate"`
	RecoverySuccessRate  benchmarkRatio              `json:"recovery_success_rate"`
	RetryAttempts        int                         `json:"retry_attempts"`
}

// benchmarkAgentEvaluationMetrics summarize Multi-Agent benchmark observations.
// Ratios retain their denominators so an empty pack cannot report false green.
type benchmarkAgentEvaluationMetrics struct {
	Scenarios             int            `json:"scenarios"`
	ExplicitCompliance    benchmarkRatio `json:"explicit_compliance"`
	AdaptiveCompliance    benchmarkRatio `json:"adaptive_compliance"`
	LocalExecutionRate    benchmarkRatio `json:"local_execution_rate"`
	FalseSpawnRate        benchmarkRatio `json:"false_spawn_rate"`
	AgentCompletionRate   benchmarkRatio `json:"agent_completion_rate"`
	ParallelAdmissionRate benchmarkRatio `json:"parallel_admission_rate"`
}

func baselineMetrics(results []benchmarkResult) benchmarkBaselineMetrics {
	metrics := benchmarkBaselineMetrics{}
	durations := make([]int64, 0, len(results))
	retriedTasks := 0
	recoveredTasks := 0
	verificationApplicable := 0
	verificationCovered := 0
	usageCalls := 0
	unpricedCalls := 0
	passed := 0
	available := 0
	for _, result := range results {
		if result.Status == "unavailable" {
			continue
		}
		available++
		if result.Passed {
			passed++
		}
		if result.DurationMS >= 0 {
			durations = append(durations, result.DurationMS)
		}
		if result.RetryAttempts > 0 {
			retriedTasks++
			metrics.RetryAttempts += result.RetryAttempts
			if result.Passed {
				recoveredTasks++
			}
		}
		if result.VerificationApplicable {
			verificationApplicable++
			if result.VerificationCovered {
				verificationCovered++
			}
		}
		usageCalls += result.UsageCalls
		unpricedCalls += result.UnpricedCalls
	}
	metrics.TaskSuccessRate = ratio(passed, available)
	metrics.RetryRate = ratio(retriedTasks, available)
	metrics.VerificationCoverage = ratio(
		verificationCovered,
		verificationApplicable,
	)
	metrics.UnknownCostRate = ratio(unpricedCalls, usageCalls)
	metrics.RecoverySuccessRate = ratio(recoveredTasks, retriedTasks)
	slices.Sort(durations)
	metrics.Latency = benchmarkLatencyPercentiles{
		Samples: len(durations),
		P50MS:   nearestRank(durations, 50),
		P95MS:   nearestRank(durations, 95),
	}
	return metrics
}

func agentEvaluationMetrics(results []benchmarkResult) benchmarkAgentEvaluationMetrics {
	var metrics benchmarkAgentEvaluationMetrics
	var explicitOK, explicitTotal int
	var adaptiveOK, adaptiveTotal int
	var localOK, localTotal, falseSpawns int
	var spawned, terminal int
	var parallelOK, parallelTotal int
	for _, result := range results {
		if result.ExpectedAgentSpawns == nil || result.Status == "unavailable" {
			continue
		}
		metrics.Scenarios++
		compliant := result.AgentSpawns == *result.ExpectedAgentSpawns
		switch result.DelegationMode {
		case "explicit":
			explicitTotal++
			if compliant {
				explicitOK++
			}
		case "adaptive":
			adaptiveTotal++
			if compliant {
				adaptiveOK++
			}
		}
		if *result.ExpectedAgentSpawns == 0 {
			localTotal++
			if result.AgentSpawns == 0 {
				localOK++
			} else {
				falseSpawns++
			}
		}
		spawned += result.AgentSpawns
		terminal += result.AgentTerminals
		if result.ExpectedAgentConcurrency != nil {
			parallelTotal++
			if result.AgentMaxConcurrency >= *result.ExpectedAgentConcurrency {
				parallelOK++
			}
		}
	}
	metrics.ExplicitCompliance = ratio(explicitOK, explicitTotal)
	metrics.AdaptiveCompliance = ratio(adaptiveOK, adaptiveTotal)
	metrics.LocalExecutionRate = ratio(localOK, localTotal)
	metrics.FalseSpawnRate = ratio(falseSpawns, localTotal)
	metrics.AgentCompletionRate = ratio(terminal, spawned)
	metrics.ParallelAdmissionRate = ratio(parallelOK, parallelTotal)
	return metrics
}

func ratio(numerator, denominator int) benchmarkRatio {
	result := benchmarkRatio{Numerator: numerator, Denominator: denominator}
	if denominator == 0 {
		return result
	}
	value := float64(numerator) / float64(denominator)
	result.Value = &value
	return result
}

func nearestRank(sorted []int64, percentile int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (percentile*len(sorted) + 99) / 100
	rank = max(rank, 1)
	return sorted[rank-1]
}

func TestCheckedInBaselineMatchesTaskInventory(t *testing.T) {
	root := filepath.Join("..", "..", "..", "testdata", "benchmarks")
	tasks, err := discoverBenchmarkTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "baseline-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var baseline struct {
		SchemaVersion int `json:"schema_version"`
		TaskInventory struct {
			Total             int            `json:"total"`
			Categories        map[string]int `json:"categories"`
			ExpectedTerminals map[string]int `json:"expected_terminals"`
			Assertions        map[string]int `json:"tasks_with_assertions"`
		} `json:"task_inventory"`
	}
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatal(err)
	}
	if baseline.SchemaVersion != 1 {
		t.Fatalf("baseline schema version=%d want 1", baseline.SchemaVersion)
	}
	categories := make(map[string]int)
	terminals := make(map[string]int)
	assertions := map[string]int{
		"files": 0, "unchanged": 0, "absent": 0, "tools_used": 0,
		"tools_failed": 0, "output_contains": 0, "receipt_changes": 0,
		"verification": 0, "context": 0, "approval": 0, "compaction": 0,
	}
	for _, task := range tasks {
		categories[task.Category]++
		terminals[task.Expect.Terminal]++
		incrementIf(assertions, "files", len(task.Expect.Files) > 0)
		incrementIf(assertions, "unchanged", len(task.Expect.Unchanged) > 0)
		incrementIf(assertions, "absent", len(task.Expect.Absent) > 0)
		incrementIf(assertions, "tools_used", len(task.Expect.ToolsUsed) > 0)
		incrementIf(assertions, "tools_failed", len(task.Expect.ToolsFailed) > 0)
		incrementIf(assertions, "output_contains", len(task.Expect.OutputContains) > 0)
		incrementIf(assertions, "receipt_changes", len(task.Expect.ReceiptChanges) > 0)
		incrementIf(
			assertions, "verification",
			task.Expect.VerifyStatus != "" || task.Expect.VerifyAction != "",
		)
		incrementIf(
			assertions, "context",
			len(task.Expect.ContextSections) > 0 ||
				len(task.Expect.ContextSelections) > 0,
		)
		incrementIf(assertions, "approval", task.Expect.Approvals != nil)
		incrementIf(assertions, "compaction", task.Expect.Compactions != nil)
	}
	if baseline.TaskInventory.Total != len(tasks) ||
		!reflect.DeepEqual(baseline.TaskInventory.Categories, categories) ||
		!reflect.DeepEqual(baseline.TaskInventory.ExpectedTerminals, terminals) ||
		!reflect.DeepEqual(baseline.TaskInventory.Assertions, assertions) {
		t.Fatalf(
			"baseline inventory=(%d,%v,%v,%v) tasks=(%d,%v,%v,%v)",
			baseline.TaskInventory.Total,
			baseline.TaskInventory.Categories,
			baseline.TaskInventory.ExpectedTerminals,
			baseline.TaskInventory.Assertions,
			len(tasks), categories, terminals, assertions,
		)
	}
}

func incrementIf(counts map[string]int, name string, condition bool) {
	if condition {
		counts[name]++
	}
}

func TestBaselineMetricsExposeRatesAndPercentiles(t *testing.T) {
	results := []benchmarkResult{
		{
			Passed: true, DurationMS: 100, UsageCalls: 2,
			VerificationApplicable: true, VerificationCovered: true,
		},
		{
			Passed: true, DurationMS: 200, UsageCalls: 2, UnpricedCalls: 1,
			RetryAttempts: 2, VerificationApplicable: true,
			VerificationCovered: true,
		},
		{
			Passed: false, DurationMS: 1000, UsageCalls: 1, UnpricedCalls: 1,
			VerificationApplicable: true,
		},
	}

	got := baselineMetrics(results)
	assertRatio(t, got.TaskSuccessRate, 2, 3)
	assertRatio(t, got.RetryRate, 1, 3)
	assertRatio(t, got.VerificationCoverage, 2, 3)
	assertRatio(t, got.UnknownCostRate, 2, 5)
	assertRatio(t, got.RecoverySuccessRate, 1, 1)
	if got.RetryAttempts != 2 {
		t.Fatalf("retry attempts=%d want 2", got.RetryAttempts)
	}
	if got.Latency.Samples != 3 ||
		got.Latency.P50MS != 200 ||
		got.Latency.P95MS != 1000 {
		t.Fatalf("latency=%+v", got.Latency)
	}
}

func TestBaselineMetricsKeepEmptyDenominatorsUnknown(t *testing.T) {
	got := baselineMetrics(nil)
	for name, ratio := range map[string]benchmarkRatio{
		"success":  got.TaskSuccessRate,
		"retry":    got.RetryRate,
		"verify":   got.VerificationCoverage,
		"cost":     got.UnknownCostRate,
		"recovery": got.RecoverySuccessRate,
	} {
		if ratio.Value != nil || ratio.Denominator != 0 {
			t.Fatalf("%s ratio=%+v want unknown", name, ratio)
		}
	}
}

func TestReportJSONCarriesVersionedBaselineMetrics(t *testing.T) {
	report := benchmarkReport{
		SchemaVersion: 1,
		Total:         1,
		Passed:        1,
		Results:       []benchmarkResult{{Passed: true, DurationMS: 10}},
		Metrics:       baselineMetrics([]benchmarkResult{{Passed: true, DurationMS: 10}}),
		GeneratedAt:   time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
	}
	var encoded bytes.Buffer
	if err := report.encode(&encoded); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema_version"] != float64(1) {
		t.Fatalf("schema_version=%v", payload["schema_version"])
	}
	metrics, ok := payload["metrics"].(map[string]any)
	if !ok || metrics["task_success_rate"] == nil ||
		metrics["recovery_success_rate"] == nil {
		t.Fatalf("metrics=%v", payload["metrics"])
	}
}

func assertRatio(t *testing.T, got benchmarkRatio, numerator, denominator int) {
	t.Helper()
	if got.Numerator != numerator || got.Denominator != denominator ||
		got.Value == nil {
		t.Fatalf("ratio=%+v want %d/%d", got, numerator, denominator)
	}
	want := float64(numerator) / float64(denominator)
	if *got.Value != want {
		t.Fatalf("ratio value=%f want %f", *got.Value, want)
	}
}
