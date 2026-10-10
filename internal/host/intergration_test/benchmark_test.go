// The benchmark harness runs fixture tasks against the real runtime.
//
// A task is a declarative directory (task.json + workspace seed + provider
// fixture), so a run needs no network, no API key, and no live model: the
// fixture provider replays scripted tool calls while assertions are made
// against the resulting workspace and the protocol event stream. This keeps
// benchmark results comparable across machines and usable as a release gate.
package intergration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fwtllh-png/QCode/internal/config"
	"github.com/fwtllh-png/QCode/internal/persist/state"
	"github.com/fwtllh-png/QCode/internal/runtime/app"
	apppersistence "github.com/fwtllh-png/QCode/internal/runtime/app/persistence"
	"github.com/fwtllh-png/QCode/internal/runtime/app/wire"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

// benchmarkTaskFile is the declarative task definition read from a task directory.
const benchmarkTaskFile = "task.json"

const (
	workspaceDir = "workspace"
	providerDir  = "provider"
)

// benchmarkDefaultPosture keeps benchmark tasks deterministic when no posture is set.
const benchmarkDefaultPosture = "auto"

// Terminal outcomes a task may require.
const (
	benchmarkCompleted  = "completed"
	benchmarkFailed     = "failed"
	benchmarkCanceled   = "canceled"
	benchmarkIncomplete = "incomplete"
)

// benchmarkTask is one benchmark scenario.
type benchmarkTask struct {
	// Dir is the directory the task was loaded from; it is not part of the file.
	Dir string `json:"-"`

	Name string `json:"name"`
	// Category maps the task onto a benchmark dimension so reports can show
	// coverage rather than a single opaque score.
	Category string `json:"category"`
	// Note records why the task exists and, where it pins current behaviour
	// rather than desired behaviour, says so.
	Note   string `json:"note,omitempty"`
	Prompt string `json:"prompt"`
	// PromptFile keeps byte-sensitive benchmark prompts in one canonical file.
	PromptFile string `json:"prompt_file,omitempty"`
	// Followups are further prompts sent on the same thread after Prompt, each as
	// its own turn. Most tasks need none. A task needs them to measure anything
	// that only happens between turns — compaction cuts on whole turn groups, so a
	// single-turn thread can never reach one however large it grows.
	Followups []string `json:"followups,omitempty"`
	Tools     bool     `json:"tools"`
	// ProviderFixture may reuse another hermetic fixture relative to this task.
	ProviderFixture string `json:"provider_fixture,omitempty"`
	// Posture is the tool permission posture (suggest/auto/bypass/never). It is
	// part of the task because permission blocking is itself a measured
	// dimension, not harness plumbing.
	Posture  string `json:"posture"`
	Mode     string `json:"mode"`
	MaxSteps int    `json:"max_steps"`
	// ApprovalDecision lets a fixture exercise a parked approval journey.
	// Empty leaves approvals for an external user.
	ApprovalDecision string `json:"approval_decision,omitempty"`
	BudgetTokens     uint64 `json:"budget_tokens,omitempty"`
	// TimeoutMS bounds a single task so a stuck fixture fails instead of hanging.
	TimeoutMS int `json:"timeout_ms"`
	// Verify overrides the production soft/diagnostics defaults for tasks that
	// need a hard gate, repository command, or exact repair budget.
	Verify *benchmarkTaskVerify `json:"verify,omitempty"`
	// Index configures the repository symbol index. It is on by default, so a
	// task only sets this to turn it off or to lower a ceiling.
	Index *benchmarkTaskIndex `json:"index,omitempty"`
	// Context configures the volatile context tail. Like the index it is on by
	// default, so a task only sets it to disable a section or squeeze a budget.
	Context *benchmarkTaskContext `json:"context,omitempty"`
	// Subagent enables Multi-Agent evaluation through the production control
	// plane. Nil preserves the ordinary single-Agent benchmark defaults.
	Subagent *benchmarkTaskSubagent `json:"subagent,omitempty"`
	// benchmarkFaultProfile injects a typed fault at a specific stage during the
	// benchmark run. When set, the fixture provider is wrapped with a fault
	// injector that triggers the fault at the configured probability. Nil
	// means no fault injection — the benchmark runs normally.
	FaultProfile *benchmarkFaultProfile `json:"fault_profile,omitempty"`
	Expect       benchmarkExpectation   `json:"expect"`
}

// benchmarkFaultProfile describes a fault to inject during benchmark execution.
type benchmarkFaultProfile struct {
	// InjectAt names the stage where the fault should be injected
	// (e.g. "model_sample", "tool_exec", "guard_check").
	InjectAt string `json:"inject_at"`
	// FaultCode is the fault.Code to inject (e.g. "unavailable").
	FaultCode string `json:"fault_code"`
	// Probability is the chance (0.0–1.0) that the fault fires.
	Probability float64 `json:"probability"`
	// Retryable indicates whether the injected fault is retryable.
	Retryable bool `json:"retryable"`
}

type benchmarkTaskSubagent struct {
	Delegation  string `json:"delegation"`
	Workspace   string `json:"workspace,omitempty"`
	MaxDepth    int    `json:"max_depth,omitempty"`
	MaxParallel int    `json:"max_parallel,omitempty"`
}

// benchmarkTaskVerify mirrors the [execution.verify] config section.
type benchmarkTaskVerify struct {
	Mode           string `json:"mode"`
	Scope          string `json:"scope,omitempty"`
	OnFailure      string `json:"on_failure,omitempty"`
	Command        string `json:"command,omitempty"`
	MaxRepairSteps *int   `json:"max_repair_steps,omitempty"`
	TimeoutMS      int    `json:"timeout_ms,omitempty"`
}

// benchmarkTaskIndex mirrors the [context.index] config section.
type benchmarkTaskIndex struct {
	Enabled      *bool `json:"enabled,omitempty"`
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`
	MaxFiles     int   `json:"max_files,omitempty"`
}

// benchmarkTaskContext mirrors the [context.repo_map], [context.working_set],
// [context.evidence], [context.coding_policy] and [context.compact] config
// sections.
type benchmarkTaskContext struct {
	RepoMap      *benchmarkTaskRepoMap      `json:"repo_map,omitempty"`
	WorkingSet   *benchmarkTaskWorkingSet   `json:"working_set,omitempty"`
	Evidence     *benchmarkTaskEvidence     `json:"evidence,omitempty"`
	CodingPolicy *benchmarkTaskCodingPolicy `json:"coding_policy,omitempty"`
	Compact      *benchmarkTaskCompact      `json:"compact,omitempty"`
}

// benchmarkTaskRepoMap mirrors the [context.repo_map] config section.
type benchmarkTaskRepoMap struct {
	Enabled        *bool `json:"enabled,omitempty"`
	MaxBytes       int   `json:"max_bytes,omitempty"`
	MaxDirectories int   `json:"max_directories,omitempty"`
}

// benchmarkTaskWorkingSet mirrors the [context.working_set] config section.
type benchmarkTaskWorkingSet struct {
	Enabled    *bool `json:"enabled,omitempty"`
	MaxEntries int   `json:"max_entries,omitempty"`
	MaxBytes   int   `json:"max_bytes,omitempty"`
}

// benchmarkTaskEvidence mirrors the [context.evidence] config section.
type benchmarkTaskEvidence struct {
	Enabled    *bool `json:"enabled,omitempty"`
	MaxEntries int   `json:"max_entries,omitempty"`
	MaxBytes   int   `json:"max_bytes,omitempty"`
}

// benchmarkTaskCodingPolicy mirrors the [context.coding_policy] config section.
type benchmarkTaskCodingPolicy struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// benchmarkTaskCompact mirrors the [context.compact] config section. A benchmark task
// runs a single turn, so the only way it can reach a compaction is to lower
// AutoCompactTokens until the in-turn gate fires between tool calls.
type benchmarkTaskCompact struct {
	AutoCompactTokens int    `json:"auto_compact_tokens,omitempty"`
	Scope             string `json:"scope,omitempty"`
	SummaryMaxBytes   int    `json:"summary_max_bytes,omitempty"`
	MaxDigestEntries  int    `json:"max_digest_entries,omitempty"`
}

// benchmarkExpectation describes the observable outcome a task requires. Every field is
// optional; an empty Expectation only requires the turn to reach a terminal.
type benchmarkExpectation struct {
	Terminal string `json:"terminal"`
	// TerminalContains requires substrings in the failure message, so a task can
	// assert a turn failed for the intended reason instead of any reason.
	TerminalContains  []string `json:"terminal_contains,omitempty"`
	AgentSpawns       *int     `json:"agent_spawns,omitempty"`
	AgentTerminals    *int     `json:"agent_terminals,omitempty"`
	AgentConcurrency  *int     `json:"agent_max_concurrency,omitempty"`
	IntegrationStates []string `json:"integration_states,omitempty"`
	// Files maps workspace-relative paths to their exact required content.
	Files map[string]string `json:"files,omitempty"`
	// Unchanged lists paths that must be byte-identical to the seed. This is how
	// guardrail tasks prove a rejected edit left nothing behind.
	Unchanged []string `json:"unchanged,omitempty"`
	Absent    []string `json:"absent,omitempty"`
	// ToolsUsed requires a successful result for each tool.
	ToolsUsed []string `json:"tools_used,omitempty"`
	// ToolsFailed requires an error result for each tool, so a task can assert
	// that a policy or fingerprint check actually blocked a call.
	ToolsFailed    []string `json:"tools_failed,omitempty"`
	OutputContains []string `json:"output_contains,omitempty"`
	// ReceiptChanges requires the execution receipt to report exactly these
	// changed paths, which checks the receipt against the real workspace effect.
	ReceiptChanges []string `json:"receipt_changes,omitempty"`
	// VerifyStatus and VerifyAction assert the verdict of the last verification
	// gate evaluation, and what the gate did with it.
	VerifyStatus string `json:"verify_status,omitempty"`
	VerifyAction string `json:"verify_action,omitempty"`
	// VerifyRepairs requires the gate to have spent exactly this many repair
	// rounds, which is how a task proves self-correction actually happened.
	VerifyRepairs    *int   `json:"verify_repairs,omitempty"`
	Approvals        *int   `json:"approvals,omitempty"`
	ApprovalDecision string `json:"approval_decision,omitempty"`
	// ContextSections requires the receipt to report each of these context
	// partitions, which is how a task proves the volatile tail was assembled at
	// all rather than silently skipped.
	ContextSections   []string                                     `json:"context_sections,omitempty"`
	ContextSelections map[string]benchmarkExpectedContextSelection `json:"context_selections,omitempty"`
	// ContextTruncated requires each of these partitions to report a budget cut.
	ContextTruncated []string `json:"context_truncated,omitempty"`
	// ReceiptReadPaths requires the receipt to report exactly these read paths.
	ReceiptReadPaths []string `json:"receipt_read_paths,omitempty"`
	// ReceiptEvidenceKinds requires the receipt's evidence to classify at least
	// one hit as each of these kinds, which is how a task proves a lookup was
	// sorted rather than just counted.
	ReceiptEvidenceKinds []string `json:"receipt_evidence_kinds,omitempty"`
	// ReceiptEvidenceRisks requires each of these risk kinds to be reported.
	ReceiptEvidenceRisks []string `json:"receipt_evidence_risks,omitempty"`
	// ReceiptEvidenceReminders requires each of these substrings to appear in a
	// reminder, so a task can pin the advice the model was actually given.
	ReceiptEvidenceReminders []string `json:"receipt_evidence_reminders,omitempty"`
	// ReceiptNotCollectedExcludes requires the receipt to stop claiming these
	// sections are uncollected, which keeps the coverage list honest as sections
	// are implemented.
	ReceiptNotCollectedExcludes []string `json:"receipt_not_collected_excludes,omitempty"`
	// Compactions requires exactly this many in-turn compactions. A task only
	// reaches one by lowering [context.compact] auto_compact_tokens, so this is how
	// it proves the gate fired instead of the turn simply fitting.
	Compactions *int `json:"compactions,omitempty"`
	// CompactionSections requires the last compaction to report each of these
	// summary sections, which checks the structured summary against what the
	// ledgers actually held.
	CompactionSections []string `json:"compaction_sections,omitempty"`
	// CompactionSectionsExclude requires each of these sections to be absent,
	// which is how a squeezed-budget task proves the cheapest section was the one
	// dropped. What the summary text itself must contain is asserted by the
	// fixture's expected_request_fragments, against the request the model saw.
	CompactionSectionsExclude []string `json:"compaction_sections_exclude,omitempty"`
	// CompactionTruncated requires the last summary to report a budget cut.
	CompactionTruncated *bool `json:"compaction_truncated,omitempty"`
}

type benchmarkExpectedContextSelection struct {
	Kind          string   `json:"kind,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
	EvidenceKinds []string `json:"evidence_kinds,omitempty"`
	Truncated     *bool    `json:"truncated,omitempty"`
}

// benchmarkResult is the outcome and cost of one task run.
type benchmarkResult struct {
	Task     string   `json:"task"`
	Category string   `json:"category"`
	Status   string   `json:"status"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
	// Error is set when the harness itself could not run the task, which is
	// reported separately from an assertion failure.
	Error              string   `json:"error,omitempty"`
	UnavailableReason  string   `json:"unavailable_reason,omitempty"`
	Terminal           string   `json:"terminal"`
	DurationMS         int64    `json:"duration_ms"`
	ToolsSucceeded     []string `json:"tools_succeeded,omitempty"`
	ToolsFailed        []string `json:"tools_failed,omitempty"`
	ToolFailureDetails []string `json:"tool_failure_details,omitempty"`
	ReceiptChanges     []string `json:"receipt_changes,omitempty"`
	// ReceiptDiagnostics is the receipt's diagnostics verdict, reported so a run
	// shows whether changes were checked at all.
	ReceiptDiagnostics       string               `json:"receipt_diagnostics,omitempty"`
	VerifyStatus             string               `json:"verify_status,omitempty"`
	VerifyAction             string               `json:"verify_action,omitempty"`
	VerifyRepairs            int                  `json:"verify_repairs,omitempty"`
	InputTokens              uint64               `json:"input_tokens"`
	UncachedInputTokens      uint64               `json:"uncached_input_tokens"`
	OutputTokens             uint64               `json:"output_tokens"`
	ReasoningTokens          uint64               `json:"reasoning_tokens"`
	CachedTokens             uint64               `json:"cached_tokens"`
	CostMicrounits           uint64               `json:"cost_microunits"`
	UsageCalls               int                  `json:"usage_calls"`
	UnpricedCalls            int                  `json:"unpriced_calls"`
	RetryAttempts            int                  `json:"retry_attempts"`
	Approvals                int                  `json:"approvals"`
	ApprovalDecision         string               `json:"approval_decision,omitempty"`
	AgentSpawns              int                  `json:"agent_spawns"`
	AgentTerminals           int                  `json:"agent_terminals"`
	AgentMaxConcurrency      int                  `json:"agent_max_concurrency"`
	IntegrationStates        []string             `json:"integration_states,omitempty"`
	DelegationMode           string               `json:"delegation_mode,omitempty"`
	ExpectedAgentSpawns      *int                 `json:"expected_agent_spawns,omitempty"`
	ExpectedAgentConcurrency *int                 `json:"expected_agent_concurrency,omitempty"`
	VerificationApplicable   bool                 `json:"verification_applicable"`
	VerificationCovered      bool                 `json:"verification_covered"`
	Samples                  []protocol.UsageData `json:"samples,omitempty"`
}

// benchmarkReport aggregates a suite run.
type benchmarkReport struct {
	SchemaVersion       int                             `json:"schema_version"`
	Platform            string                          `json:"platform"`
	Total               int                             `json:"total"`
	Available           int                             `json:"available"`
	Unavailable         int                             `json:"unavailable"`
	Failed              int                             `json:"failed"`
	Passed              int                             `json:"passed"`
	Results             []benchmarkResult               `json:"results"`
	Categories          map[string]int                  `json:"categories"`
	InputTokens         uint64                          `json:"input_tokens"`
	UncachedInputTokens uint64                          `json:"uncached_input_tokens"`
	OutputTokens        uint64                          `json:"output_tokens"`
	ReasoningTokens     uint64                          `json:"reasoning_tokens"`
	CachedTokens        uint64                          `json:"cached_tokens"`
	CostMicrounits      uint64                          `json:"cost_microunits"`
	DurationMS          int64                           `json:"duration_ms"`
	Metrics             benchmarkBaselineMetrics        `json:"metrics"`
	AgentMetrics        benchmarkAgentEvaluationMetrics `json:"agent_metrics"`
	GeneratedAt         time.Time                       `json:"generated_at"`
}

// ok reports whether every task passed.
func (r benchmarkReport) ok() bool { return r.Total > 0 && r.Passed == r.Total }

// baselineOK reports that every runnable task passed. Capability-specific
// unavailable tasks remain visible but do not turn a host baseline into a code
// failure. Release gates use ok instead.
func (r benchmarkReport) baselineOK() bool {
	return r.Available > 0 && r.Failed == 0
}

// encode writes the report as indented JSON.
func (r benchmarkReport) encode(writer io.Writer) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}

// loadBenchmarkTask reads a task definition from dir.
func loadBenchmarkTask(dir string) (benchmarkTask, error) {
	data, err := os.ReadFile(filepath.Join(dir, benchmarkTaskFile))
	if err != nil {
		return benchmarkTask{}, err
	}
	var task benchmarkTask
	if err := json.Unmarshal(data, &task); err != nil {
		return benchmarkTask{}, fmt.Errorf("decode %s: %w", filepath.Join(dir, benchmarkTaskFile), err)
	}
	task.Dir = dir
	if task.PromptFile != "" {
		prompt, readErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(task.PromptFile)))
		if readErr != nil {
			return benchmarkTask{}, fmt.Errorf("read benchmark prompt: %w", readErr)
		}
		task.Prompt = string(prompt)
	}
	if task.Name == "" {
		task.Name = filepath.Base(dir)
	}
	if task.Expect.Terminal == "" {
		task.Expect.Terminal = benchmarkCompleted
	}
	if task.Posture == "" {
		task.Posture = benchmarkDefaultPosture
	}
	return task, nil
}

// discoverBenchmarkTasks loads every immediate subdirectory of root that holds a TaskFile.
func discoverBenchmarkTasks(root string) ([]benchmarkTask, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var tasks []benchmarkTask
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, benchmarkTaskFile)); err != nil {
			continue
		}
		task, err := loadBenchmarkTask(dir)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no benchmark tasks under %s", root)
	}
	return tasks, nil
}

// runBenchmarkSuite runs every task under root and aggregates the results. A task that
// fails does not stop the suite, because a partial score is more useful than a
// first-failure abort when comparing runs.
func runBenchmarkSuite(ctx context.Context, root string) (benchmarkReport, error) {
	tasks, err := discoverBenchmarkTasks(root)
	if err != nil {
		return benchmarkReport{}, err
	}
	report := benchmarkReport{
		SchemaVersion: 1,
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		Categories:    make(map[string]int),
		GeneratedAt:   time.Now().UTC(),
	}
	for _, task := range tasks {
		result := runBenchmarkTask(ctx, task)
		report.Results = append(report.Results, result)
		report.Total++
		if result.Status == "unavailable" {
			report.Unavailable++
		} else {
			report.Available++
		}
		if result.Passed {
			report.Passed++
		} else if result.Status != "unavailable" {
			report.Failed++
		}
		report.Categories[result.Category]++
		report.InputTokens += result.InputTokens
		report.UncachedInputTokens += result.UncachedInputTokens
		report.OutputTokens += result.OutputTokens
		report.ReasoningTokens += result.ReasoningTokens
		report.CachedTokens += result.CachedTokens
		report.CostMicrounits += result.CostMicrounits
		report.DurationMS += result.DurationMS
	}
	report.Metrics = baselineMetrics(report.Results)
	report.AgentMetrics = agentEvaluationMetrics(report.Results)
	return report, nil
}

// runBenchmarkTask executes one task in a throwaway workspace and evaluates it. Harness
// errors are recorded on the Result instead of returned so one broken task does
// not hide the rest of the suite.
func runBenchmarkTask(ctx context.Context, task benchmarkTask) benchmarkResult {
	started := time.Now()
	result := benchmarkResult{Task: task.Name, Category: task.Category}
	observed, err := executeTask(ctx, task)
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		result.Failures = append(result.Failures, "harness: "+err.Error())
		return result
	}
	result.Terminal = observed.terminal
	result.ToolsSucceeded = observed.succeeded
	result.ToolsFailed = observed.failed
	result.ToolFailureDetails = append([]string(nil), observed.toolFailureDetails...)
	if observed.verification != nil {
		result.VerifyStatus = observed.verification.Status
		result.VerifyAction = observed.verification.Action
		result.VerifyRepairs = observed.verification.RepairSteps
	}
	if observed.receipt != nil {
		result.ReceiptDiagnostics = observed.receipt.Verification.Diagnostics
		for _, change := range observed.receipt.Changes {
			result.ReceiptChanges = append(result.ReceiptChanges, change.Path)
		}
	}
	result.InputTokens = observed.inputTokens
	result.UncachedInputTokens = observed.inputTokens - min(observed.inputTokens, observed.cachedTokens)
	result.OutputTokens = observed.outputTokens
	result.ReasoningTokens = observed.reasoningTokens
	result.CachedTokens = observed.cachedTokens
	result.CostMicrounits = observed.costMicrounits
	result.UsageCalls = observed.usageCalls
	result.UnpricedCalls = observed.unpricedCalls
	result.RetryAttempts = observed.recoveredToolFailures + result.VerifyRepairs
	result.Approvals = observed.approvals
	result.ApprovalDecision = observed.approvalDecision
	result.AgentSpawns = len(observed.agents)
	result.AgentTerminals = len(observed.terminalAgents)
	result.AgentMaxConcurrency = observed.maxAgentConcurrency
	result.IntegrationStates = append([]string(nil), observed.integrationStates...)
	result.ExpectedAgentSpawns = task.Expect.AgentSpawns
	result.ExpectedAgentConcurrency = task.Expect.AgentConcurrency
	if task.Subagent != nil {
		result.DelegationMode = task.Subagent.Delegation
	}
	result.VerificationApplicable = task.Verify != nil ||
		len(result.ReceiptChanges) > 0
	result.VerificationCovered = result.VerificationApplicable &&
		result.VerifyStatus != ""
	result.Samples = append([]protocol.UsageData(nil), observed.samples...)
	result.Failures = evaluate(task, observed)
	if strings.Contains(observed.terminalDetail, "sandbox_unavailable") {
		result.Status = "unavailable"
		result.UnavailableReason = observed.terminalDetail
		result.Failures = nil
		return result
	}
	result.Passed = len(result.Failures) == 0
	if result.Passed {
		result.Status = "passed"
	} else {
		result.Status = "failed"
	}
	return result
}

// observation is everything a run reveals about a task, gathered from the event
// stream plus the workspace state after the turn.
type observation struct {
	terminal              string
	terminalDetail        string
	output                string
	succeeded             []string
	failed                []string
	toolFailureDetails    []string
	inputTokens           uint64
	outputTokens          uint64
	reasoningTokens       uint64
	cachedTokens          uint64
	costMicrounits        uint64
	usageCalls            int
	unpricedCalls         int
	recoveredToolFailures int
	approvals             int
	approvalDecision      string
	receipt               *protocol.ExecutionReceiptData
	// verification is the last gate evaluation of the turn.
	verification *protocol.TurnVerificationData
	// compactions counts the in-turn compact gates that fired, and compaction is
	// the last one. Keeping both lets a task assert the gate ran a second time
	// while still inspecting the summary that survived.
	compactions int
	compaction  *protocol.TurnCompactionData
	// seed and final are workspace snapshots keyed by slash-separated relative
	// path. Snapshotting before cleanup keeps evaluation independent of the
	// throwaway directory's lifetime.
	seed                map[string]string
	final               map[string]string
	pendingToolFailures map[string]int
	agents              map[string]struct{}
	activeAgents        map[string]struct{}
	terminalAgents      map[string]struct{}
	maxAgentConcurrency int
	integrationStates   []string
	agentTimeline       []string
	eventKinds          []string
	samples             []protocol.UsageData
}

func executeTask(ctx context.Context, task benchmarkTask) (observation, error) {
	timeout := time.Duration(task.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	temporary, err := os.MkdirTemp("", "qcode-bench-")
	if err != nil {
		return observation{}, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()

	workspace := filepath.Join(temporary, workspaceDir)
	seed, err := seedWorkspace(filepath.Join(task.Dir, workspaceDir), workspace)
	if err != nil {
		return observation{}, fmt.Errorf("seed workspace: %w", err)
	}
	dataDir := filepath.Join(temporary, "state")
	tools := task.Tools
	overrides := config.Overrides{
		Workspace: &workspace, StateDataDir: &dataDir, Tools: &tools,
	}
	if task.MaxSteps > 0 {
		steps := task.MaxSteps
		overrides.MaxSteps = &steps
	}
	if task.Mode != "" {
		mode := task.Mode
		overrides.Mode = &mode
	}
	if task.BudgetTokens > 0 {
		budget := task.BudgetTokens
		overrides.BudgetTokens = &budget
	}
	posture := task.Posture
	if posture == "" {
		posture = benchmarkDefaultPosture
	}
	// QCODE_BENCH_POSTURE and QCODE_BENCH_MODE override task
	// settings for adversarial differential testing.
	if p := os.Getenv("QCODE_BENCH_POSTURE"); p != "" {
		posture = p
	}
	if m := os.Getenv("QCODE_BENCH_MODE"); m != "" {
		overrides.Mode = &m
	}
	applyIndexOverrides(task.Index, &overrides)
	applyContextOverrides(task.Context, &overrides)
	if settings := task.Subagent; settings != nil {
		if settings.Delegation != "" {
			value := settings.Delegation
			overrides.SubagentDelegation = &value
		}
		if settings.Workspace != "" {
			value := settings.Workspace
			overrides.SubagentWorkspace = &value
		}
		if settings.MaxDepth > 0 {
			value := settings.MaxDepth
			overrides.SubagentMaxDepth = &value
		}
		if settings.MaxParallel > 0 {
			value := settings.MaxParallel
			overrides.SubagentMaxParallel = &value
		}
	}
	fixturePath := filepath.Join(task.Dir, providerDir)
	if task.ProviderFixture != "" {
		fixturePath = filepath.Join(task.Dir, filepath.FromSlash(task.ProviderFixture))
	}
	threadID, err := protocol.NewThreadID()
	if err != nil {
		return observation{}, err
	}
	persistentStore, err := state.Open(ctx, state.Options{DataDir: dataDir})
	if err != nil {
		return observation{}, fmt.Errorf("open benchmark state: %w", err)
	}
	defer func() { _ = persistentStore.CloseAll(context.Background()) }()
	if err := apppersistence.EnsureThread(
		ctx, persistentStore, threadID, "session-benchmark", workspace,
	); err != nil {
		return observation{}, fmt.Errorf("ensure benchmark thread: %w", err)
	}
	session, err := wire.NewExec(ctx, wire.ExecOptions{
		FixturePath: fixturePath, ConfigOverrides: overrides,
		Permission: posture, PersistentStore: persistentStore,
	})
	if err != nil {
		return observation{}, fmt.Errorf("wire session: %w", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = session.Close(closeCtx)
	}()

	prompts := append([]string{task.Prompt}, task.Followups...)
	observed, err := runThread(
		ctx, session.Runtime, threadID, prompts, task.ApprovalDecision,
	)
	if err != nil {
		return observed, err
	}
	final, err := readTree(workspace)
	if err != nil {
		return observation{}, fmt.Errorf("snapshot workspace: %w", err)
	}
	observed.seed = seed
	observed.final = final
	return observed, nil
}

// applyVerifyOverrides pushes a task's gate settings through the same config
// path an operator would use, so a benchmark measures the shipped wiring.

// applyIndexOverrides pushes a task's index settings through the same config
// path an operator would use. A task that turns the index off is measuring the
// degraded contract: the symbol tools must say so instead of answering wrongly.
func applyIndexOverrides(settings *benchmarkTaskIndex, overrides *config.Overrides) {
	if settings == nil {
		return
	}
	if settings.Enabled != nil {
		enabled := *settings.Enabled
		overrides.IndexEnabled = &enabled
	}
	if settings.MaxFileBytes > 0 {
		bytes := settings.MaxFileBytes
		overrides.IndexMaxBytes = &bytes
	}
	if settings.MaxFiles > 0 {
		files := settings.MaxFiles
		overrides.IndexMaxFiles = &files
	}
}

// applyContextOverrides pushes a task's tail settings through the config path an
// operator would use. A task that squeezes a budget is measuring the truncation
// contract: the section must report the cut rather than fail the turn.
func applyContextOverrides(settings *benchmarkTaskContext, overrides *config.Overrides) {
	if settings == nil {
		return
	}
	if repoMap := settings.RepoMap; repoMap != nil {
		if repoMap.Enabled != nil {
			enabled := *repoMap.Enabled
			overrides.RepoMapEnabled = &enabled
		}
		if repoMap.MaxBytes > 0 {
			bytes := repoMap.MaxBytes
			overrides.RepoMapMaxBytes = &bytes
		}
		if repoMap.MaxDirectories > 0 {
			directories := repoMap.MaxDirectories
			overrides.RepoMapMaxDirectories = &directories
		}
	}
	if workingSet := settings.WorkingSet; workingSet != nil {
		if workingSet.Enabled != nil {
			enabled := *workingSet.Enabled
			overrides.WorkingSetEnabled = &enabled
		}
		if workingSet.MaxEntries > 0 {
			entries := workingSet.MaxEntries
			overrides.WorkingSetMaxEntries = &entries
		}
		if workingSet.MaxBytes > 0 {
			bytes := workingSet.MaxBytes
			overrides.WorkingSetMaxBytes = &bytes
		}
	}
	if evidence := settings.Evidence; evidence != nil {
		if evidence.Enabled != nil {
			enabled := *evidence.Enabled
			overrides.EvidenceEnabled = &enabled
		}
		if evidence.MaxEntries > 0 {
			entries := evidence.MaxEntries
			overrides.EvidenceMaxEntries = &entries
		}
		if evidence.MaxBytes > 0 {
			bytes := evidence.MaxBytes
			overrides.EvidenceMaxBytes = &bytes
		}
	}
	if policy := settings.CodingPolicy; policy != nil && policy.Enabled != nil {
		enabled := *policy.Enabled
		overrides.CodingPolicyEnabled = &enabled
	}
	if compaction := settings.Compact; compaction != nil {
		if compaction.AutoCompactTokens > 0 {
			tokens := compaction.AutoCompactTokens
			overrides.CompactAutoTokens = &tokens
		}
		if compaction.Scope != "" {
			overrides.CompactScope = &compaction.Scope
		}
		if compaction.SummaryMaxBytes > 0 {
			bytes := compaction.SummaryMaxBytes
			overrides.CompactSummaryMax = &bytes
		}
		if compaction.MaxDigestEntries > 0 {
			entries := compaction.MaxDigestEntries
			overrides.CompactMaxDigest = &entries
		}
	}
}

// runThread sends each prompt as its own turn on one thread and folds every turn
// into a single observation. A task with no followups sees exactly what it did
// before: one turn, whose events are the whole record.
func runThread(
	ctx context.Context,
	runtime *app.Runtime,
	threadID protocol.ThreadID,
	prompts []string,
	approvalDecision string,
) (observation, error) {
	// Subscribe once, before the first submit, so no early event is missed and the
	// stream stays continuous across turns.
	events, err := runtime.Events(context.Background(), 0)
	if err != nil {
		return observation{}, err
	}
	observed := observation{
		pendingToolFailures: make(map[string]int),
		agents:              make(map[string]struct{}),
		activeAgents:        make(map[string]struct{}),
		terminalAgents:      make(map[string]struct{}),
	}
	for _, prompt := range prompts {
		if err := runTurn(
			ctx, runtime, events, threadID, prompt, approvalDecision, &observed,
		); err != nil {
			return observed, fmt.Errorf(
				"%w; events=%v; agent timeline=%v",
				err, observed.eventKinds, observed.agentTimeline,
			)
		}
		// A turn that did not complete ends the thread: later prompts would measure
		// a state the task never asked for.
		if observed.terminal != benchmarkCompleted {
			return observed, nil
		}
	}
	return observed, nil
}

func runTurn(
	ctx context.Context,
	runtime *app.Runtime,
	events <-chan protocol.Event,
	threadID protocol.ThreadID,
	prompt string,
	approvalDecision string,
	observed *observation,
) error {
	turnID, err := protocol.NewTurnID()
	if err != nil {
		return err
	}
	itemID, err := protocol.NewItemID()
	if err != nil {
		return err
	}
	operation, err := protocol.NewOperation(&protocol.StartTurnPayload{
		ThreadID: threadID, TurnID: turnID, ItemID: itemID, Prompt: prompt,
	})
	if err != nil {
		return err
	}
	if err := runtime.Submit(ctx, operation); err != nil {
		return err
	}
	var output strings.Builder
	usageSamples := make(map[uint32]protocol.UsageData)
	foldUsage := func() {
		for _, usage := range usageSamples {
			observed.inputTokens += usage.InputTokens
			observed.outputTokens += usage.OutputTokens
			observed.reasoningTokens += usage.ReasoningTokens
			observed.cachedTokens += usage.CachedTokens
			observed.costMicrounits += usage.CostMicrounits
			observed.usageCalls++
			if !usage.CostKnown {
				observed.unpricedCalls++
			}
			observed.samples = append(observed.samples, usage)
		}
		sort.Slice(observed.samples, func(i, j int) bool {
			return observed.samples[i].Sample < observed.samples[j].Sample
		})
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("task timed out before a terminal event: %w", ctx.Err())
		case event, ok := <-events:
			if !ok {
				return errors.New("event stream closed before a terminal event")
			}
			if len(observed.eventKinds) < 100 {
				observed.eventKinds = append(
					observed.eventKinds,
					fmt.Sprintf("%d:%s:%s", event.Sequence, event.Kind, event.TurnID),
				)
			}
			update, err := projectEvent(event)
			if err != nil {
				return err
			}
			if agent, ok := update.(agentUpdate); ok {
				observeAgentUpdate(observed, agent)
			}
			parentEvent := event.TurnID == turnID
			if !parentEvent {
				interaction, ok := update.(interactionUpdate)
				if !ok || interaction.ApprovalRequired == nil ||
					interaction.ApprovalRequired.Source == nil {
					continue
				}
			}
			switch data := update.(type) {
			case textUpdate:
				if data.Channel == "output" {
					output.WriteString(data.Text)
				}
			case toolUpdate:
				if data.Result == nil {
					continue
				}
				if data.Result.IsError {
					observed.failed = appendUnique(observed.failed, data.Tool)
					observed.toolFailureDetails = append(
						observed.toolFailureDetails,
						data.Tool+": "+data.Result.Output,
					)
					observed.pendingToolFailures[data.Tool]++
					continue
				}
				observed.succeeded = appendUnique(observed.succeeded, data.Tool)
				if observed.pendingToolFailures[data.Tool] > 0 {
					observed.recoveredToolFailures++
					observed.pendingToolFailures[data.Tool]--
				}
			case interactionUpdate:
				request := data.ApprovalRequired
				if request == nil || approvalDecision == "" {
					continue
				}
				decisionItemID, itemErr := protocol.NewItemID()
				if itemErr != nil {
					return itemErr
				}
				planID := ""
				if request.EditPlan != nil {
					planID = request.EditPlan.ID
				}
				decision, decisionErr := protocol.NewOperation(
					&protocol.ApprovalDecisionPayload{
						ThreadID: event.ThreadID, TurnID: event.TurnID,
						ItemID:    decisionItemID,
						RequestID: request.RequestID,
						Decision:  protocol.ApprovalDecision(approvalDecision),
						Scope:     protocol.ApprovalScopeOnce,
						ExpiresAt: request.ExpiresAt, PlanID: planID,
					},
				)
				if decisionErr != nil {
					return decisionErr
				}
				if submitErr := runtime.Submit(ctx, decision); submitErr != nil {
					return submitErr
				}
				observed.approvals++
				observed.approvalDecision = approvalDecision
			case evidenceUpdate:
				if data.Receipt != nil {
					observed.receipt = data.Receipt
				}
				if data.Verification != nil {
					observed.verification = data.Verification
				}
			case lifecycleUpdate:
				if data.TurnCompaction != nil {
					observed.compactions++
					observed.compaction = data.TurnCompaction
				}
			case accountingUpdate:
				if data.Usage != nil {
					usageSamples[data.Usage.Sample] = *data.Usage
				}
			case terminalUpdate:
				if !parentEvent {
					continue
				}
				foldUsage()
				observed.output += output.String()
				switch data.Status {
				case "completed":
					observed.terminal = benchmarkCompleted
					return nil
				case "incomplete":
					observed.terminal = benchmarkIncomplete
					observed.terminalDetail = data.Message
					return nil
				case "failed":
					observed.terminal = benchmarkFailed
					observed.terminalDetail = data.Message
					return nil
				case "canceled":
					observed.terminal = benchmarkCanceled
					return nil
				default:
					message := data.Message
					if message == "" {
						message = "operation rejected"
					}
					return errors.New(message)
				}
			}
		}
	}
}

func observeAgentUpdate(observed *observation, update agentUpdate) {
	switch {
	case update.Spawned != nil:
		id := update.Spawned.AgentID
		observed.agentTimeline = append(
			observed.agentTimeline, id+":spawned",
		)
		observed.agents[id] = struct{}{}
		observed.activeAgents[id] = struct{}{}
		observed.maxAgentConcurrency = max(
			observed.maxAgentConcurrency,
			len(observed.activeAgents),
		)
	case update.Status != nil:
		id := update.Status.AgentID
		observed.agentTimeline = append(
			observed.agentTimeline, id+":"+update.Status.Status,
		)
		if terminalAgentStatus(update.Status.Status) {
			delete(observed.activeAgents, id)
			observed.terminalAgents[id] = struct{}{}
		}
	case update.Integration != nil:
		observed.agentTimeline = append(
			observed.agentTimeline,
			update.Integration.AgentID+":integration:"+update.Integration.Status,
		)
		observed.integrationStates = append(
			observed.integrationStates,
			update.Integration.Status,
		)
	}
}

func terminalAgentStatus(status string) bool {
	switch status {
	case "completed", "failed", "interrupted", "integrated",
		"integration_failed", "closed":
		return true
	default:
		return false
	}
}

func evaluate(task benchmarkTask, observed observation) []string {
	var failures []string
	if observed.terminal != task.Expect.Terminal {
		detail := ""
		if observed.terminalDetail != "" {
			detail = ": " + observed.terminalDetail
		}
		failures = append(failures, fmt.Sprintf(
			"terminal = %q want %q%s", observed.terminal, task.Expect.Terminal, detail,
		))
	}
	for _, want := range task.Expect.TerminalContains {
		if !strings.Contains(observed.terminalDetail, want) {
			failures = append(failures, fmt.Sprintf(
				"terminal detail %q does not contain %q", observed.terminalDetail, want,
			))
		}
	}
	if task.Expect.AgentSpawns != nil &&
		len(observed.agents) != *task.Expect.AgentSpawns {
		failures = append(failures, fmt.Sprintf(
			"agent spawns = %d want %d",
			len(observed.agents), *task.Expect.AgentSpawns,
		))
	}
	if task.Expect.AgentTerminals != nil &&
		len(observed.terminalAgents) != *task.Expect.AgentTerminals {
		failures = append(failures, fmt.Sprintf(
			"agent terminals = %d want %d",
			len(observed.terminalAgents), *task.Expect.AgentTerminals,
		))
	}
	if task.Expect.AgentConcurrency != nil &&
		observed.maxAgentConcurrency < *task.Expect.AgentConcurrency {
		failures = append(failures, fmt.Sprintf(
			"agent max concurrency = %d want at least %d",
			observed.maxAgentConcurrency, *task.Expect.AgentConcurrency,
		))
	}
	if len(task.Expect.IntegrationStates) != 0 &&
		!slices.Equal(observed.integrationStates, task.Expect.IntegrationStates) {
		failures = append(failures, fmt.Sprintf(
			"integration states = %v want %v",
			observed.integrationStates, task.Expect.IntegrationStates,
		))
	}
	for path, want := range task.Expect.Files {
		got, exists := observed.final[filepath.ToSlash(path)]
		if !exists {
			failures = append(failures, fmt.Sprintf("file %s does not exist after the turn", path))
			continue
		}
		if got != want {
			failures = append(failures, fmt.Sprintf(
				"file %s content = %q want %q", path, got, want,
			))
		}
	}
	for _, path := range task.Expect.Unchanged {
		key := filepath.ToSlash(path)
		want, seeded := observed.seed[key]
		if !seeded {
			failures = append(failures, fmt.Sprintf("unchanged %s is not part of the seed", path))
			continue
		}
		got, exists := observed.final[key]
		if !exists {
			failures = append(failures, fmt.Sprintf("unchanged %s was deleted", path))
			continue
		}
		if got != want {
			failures = append(failures, fmt.Sprintf(
				"file %s was modified: %q want seed %q", path, got, want,
			))
		}
	}
	for _, path := range task.Expect.Absent {
		if _, exists := observed.final[filepath.ToSlash(path)]; exists {
			failures = append(failures, fmt.Sprintf("file %s exists but must be absent", path))
		}
	}
	for _, name := range task.Expect.ToolsUsed {
		if !contains(observed.succeeded, name) {
			failures = append(failures, fmt.Sprintf(
				"tool %s did not succeed (succeeded=%v failed=%v)",
				name, observed.succeeded, observed.failed,
			))
		}
	}
	for _, name := range task.Expect.ToolsFailed {
		if !contains(observed.failed, name) {
			failures = append(failures, fmt.Sprintf(
				"tool %s did not fail (succeeded=%v failed=%v)",
				name, observed.succeeded, observed.failed,
			))
		}
	}
	if len(task.Expect.ReceiptChanges) != 0 {
		if observed.receipt == nil {
			failures = append(failures, "no execution receipt was emitted")
		} else {
			var got []string
			for _, change := range observed.receipt.Changes {
				got = append(got, change.Path)
			}
			sort.Strings(got)
			want := append([]string(nil), task.Expect.ReceiptChanges...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				failures = append(failures, fmt.Sprintf(
					"receipt changes = %v want %v", got, want,
				))
			}
		}
	}
	failures = append(failures, evaluateContext(task.Expect, observed.receipt)...)
	failures = append(failures, evaluateEvidence(task.Expect, observed.receipt)...)
	failures = append(failures, evaluateVerification(task.Expect, observed.verification)...)
	failures = append(failures, evaluateCompaction(task.Expect, observed)...)
	if task.Expect.Approvals != nil && observed.approvals != *task.Expect.Approvals {
		failures = append(failures, fmt.Sprintf(
			"approvals = %d want %d", observed.approvals, *task.Expect.Approvals,
		))
	}
	if task.Expect.ApprovalDecision != "" &&
		observed.approvalDecision != task.Expect.ApprovalDecision {
		failures = append(failures, fmt.Sprintf(
			"approval decision = %q want %q",
			observed.approvalDecision, task.Expect.ApprovalDecision,
		))
	}
	for _, want := range task.Expect.OutputContains {
		if !strings.Contains(observed.output, want) {
			failures = append(failures, fmt.Sprintf(
				"output %q does not contain %q", observed.output, want,
			))
		}
	}
	return failures
}

// evaluateContext checks the receipt's account of the volatile context tail.
func evaluateContext(
	expect benchmarkExpectation, receipt *protocol.ExecutionReceiptData,
) []string {
	if len(expect.ContextSections) == 0 && len(expect.ContextTruncated) == 0 &&
		len(expect.ReceiptReadPaths) == 0 && len(expect.ContextSelections) == 0 {
		return nil
	}
	if receipt == nil {
		return []string{"no execution receipt was emitted"}
	}
	reported := make(map[string]protocol.ReceiptContextSection, len(receipt.ContextSections))
	var kinds []string
	for _, section := range receipt.ContextSections {
		reported[section.Kind] = section
		kinds = append(kinds, section.Kind)
	}
	var failures []string
	for _, kind := range expect.ContextSections {
		if _, found := reported[kind]; !found {
			failures = append(failures, fmt.Sprintf(
				"context section %s missing from the receipt (reported=%v)", kind, kinds,
			))
		}
	}
	for _, kind := range expect.ContextTruncated {
		section, found := reported[kind]
		switch {
		case !found:
			failures = append(failures, fmt.Sprintf(
				"context section %s missing from the receipt (reported=%v)", kind, kinds,
			))
		case !section.Truncated:
			failures = append(failures, fmt.Sprintf(
				"context section %s was not truncated (%d of %d bytes retained)",
				kind, section.RetainedBytes, section.OriginalBytes,
			))
		}
	}
	if len(expect.ReceiptReadPaths) != 0 {
		got := append([]string(nil), receipt.ReadPaths...)
		want := append([]string(nil), expect.ReceiptReadPaths...)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			failures = append(failures, fmt.Sprintf(
				"receipt read paths = %v want %v", got, want,
			))
		}
	}
	selections := make(map[string]protocol.ReceiptContextSelection)
	for _, selection := range receipt.ContextSelections {
		selections[selection.Path] = selection
	}
	for path, want := range expect.ContextSelections {
		got, found := selections[path]
		if !found {
			failures = append(failures, fmt.Sprintf(
				"context selection %s is missing", path,
			))
			continue
		}
		if want.Kind != "" && got.Kind != want.Kind {
			failures = append(failures, fmt.Sprintf(
				"context selection %s kind = %q want %q", path, got.Kind, want.Kind,
			))
		}
		for _, reason := range want.Reasons {
			if !contains(got.Reasons, reason) {
				failures = append(failures, fmt.Sprintf(
					"context selection %s reasons = %v, missing %s",
					path, got.Reasons, reason,
				))
			}
		}
		var evidenceKinds []string
		for _, fact := range got.Evidence {
			evidenceKinds = appendUnique(evidenceKinds, fact.Kind)
		}
		for _, kind := range want.EvidenceKinds {
			if !contains(evidenceKinds, kind) {
				failures = append(failures, fmt.Sprintf(
					"context selection %s evidence = %v, missing %s",
					path, evidenceKinds, kind,
				))
			}
		}
		if want.Truncated != nil && got.Truncated != *want.Truncated {
			failures = append(failures, fmt.Sprintf(
				"context selection %s truncated = %t want %t",
				path, got.Truncated, *want.Truncated,
			))
		}
	}
	return failures
}

// evaluateEvidence checks the receipt's account of what the session established
// and what it left unproved.
func evaluateEvidence(
	expect benchmarkExpectation, receipt *protocol.ExecutionReceiptData,
) []string {
	if len(expect.ReceiptEvidenceKinds) == 0 && len(expect.ReceiptEvidenceRisks) == 0 &&
		len(expect.ReceiptEvidenceReminders) == 0 &&
		len(expect.ReceiptNotCollectedExcludes) == 0 {
		return nil
	}
	if receipt == nil {
		return []string{"no execution receipt was emitted"}
	}
	var failures []string
	for _, section := range expect.ReceiptNotCollectedExcludes {
		if contains(receipt.NotCollected, section) {
			failures = append(failures, fmt.Sprintf(
				"receipt still claims %s is not collected (not_collected=%v)",
				section, receipt.NotCollected,
			))
		}
	}
	if len(expect.ReceiptEvidenceKinds) == 0 && len(expect.ReceiptEvidenceRisks) == 0 &&
		len(expect.ReceiptEvidenceReminders) == 0 {
		return failures
	}
	if receipt.Evidence == nil {
		return append(failures, "the receipt carries no evidence section")
	}
	var kinds, risks []string
	for _, fact := range receipt.Evidence.Facts {
		kinds = appendUnique(kinds, fact.Kind)
	}
	for _, risk := range receipt.Evidence.Risks {
		risks = appendUnique(risks, risk.Kind)
	}
	for _, kind := range expect.ReceiptEvidenceKinds {
		if !contains(kinds, kind) {
			failures = append(failures, fmt.Sprintf(
				"no %s evidence in the receipt (kinds=%v)", kind, kinds,
			))
		}
	}
	for _, kind := range expect.ReceiptEvidenceRisks {
		if !contains(risks, kind) {
			failures = append(failures, fmt.Sprintf(
				"risk %s missing from the receipt (risks=%v)", kind, risks,
			))
		}
	}
	for _, want := range expect.ReceiptEvidenceReminders {
		found := false
		for _, reminder := range receipt.Evidence.Reminders {
			if strings.Contains(reminder, want) {
				found = true
				break
			}
		}
		if !found {
			failures = append(failures, fmt.Sprintf(
				"no reminder contains %q (reminders=%v)", want, receipt.Evidence.Reminders,
			))
		}
	}
	return failures
}

// evaluateCompaction checks what a turn's compact gates kept. It reads the
// compaction event rather than the provider request because the event is the
// runtime's own account of the summary, and the fixture already fails the turn
// when a required fragment is missing from the request itself.
func evaluateCompaction(expect benchmarkExpectation, observed observation) []string {
	if expect.Compactions == nil && len(expect.CompactionSections) == 0 &&
		len(expect.CompactionSectionsExclude) == 0 && expect.CompactionTruncated == nil {
		return nil
	}
	var failures []string
	if expect.Compactions != nil && observed.compactions != *expect.Compactions {
		failures = append(failures, fmt.Sprintf(
			"compactions = %d want %d", observed.compactions, *expect.Compactions,
		))
	}
	if observed.compaction == nil {
		return append(failures, "no compaction event was emitted")
	}
	for _, section := range expect.CompactionSections {
		if !contains(observed.compaction.Sections, section) {
			failures = append(failures, fmt.Sprintf(
				"compaction section %s missing (sections=%v)",
				section, observed.compaction.Sections,
			))
		}
	}
	for _, section := range expect.CompactionSectionsExclude {
		if contains(observed.compaction.Sections, section) {
			failures = append(failures, fmt.Sprintf(
				"compaction section %s survived the budget (sections=%v)",
				section, observed.compaction.Sections,
			))
		}
	}
	if expect.CompactionTruncated != nil &&
		observed.compaction.SummaryTruncated != *expect.CompactionTruncated {
		failures = append(failures, fmt.Sprintf(
			"compaction summary truncated = %t want %t",
			observed.compaction.SummaryTruncated, *expect.CompactionTruncated,
		))
	}
	return failures
}

func evaluateVerification(
	expect benchmarkExpectation, observed *protocol.TurnVerificationData,
) []string {
	if expect.VerifyStatus == "" && expect.VerifyAction == "" && expect.VerifyRepairs == nil {
		return nil
	}
	if observed == nil {
		return []string{"no verification event was emitted"}
	}
	var failures []string
	if expect.VerifyStatus != "" && observed.Status != expect.VerifyStatus {
		failures = append(failures, fmt.Sprintf(
			"verify status = %q want %q", observed.Status, expect.VerifyStatus,
		))
	}
	if expect.VerifyAction != "" && observed.Action != expect.VerifyAction {
		failures = append(failures, fmt.Sprintf(
			"verify action = %q want %q", observed.Action, expect.VerifyAction,
		))
	}
	if expect.VerifyRepairs != nil && observed.RepairSteps != *expect.VerifyRepairs {
		failures = append(failures, fmt.Sprintf(
			"verify repair rounds = %d want %d", observed.RepairSteps, *expect.VerifyRepairs,
		))
	}
	return failures
}

// seedWorkspace copies the task seed into destination and returns the seeded
// contents keyed by slash-separated relative path, for unchanged assertions.
func seedWorkspace(source, destination string) (map[string]string, error) {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return nil, err
	}
	info, err := os.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", source)
	}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return nil, err
	}
	return readTree(destination)
}

// readTree snapshots regular file contents under root, keyed by
// slash-separated relative path.
func readTree(root string) (map[string]string, error) {
	tree := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[filepath.ToSlash(relative)] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tree, nil
}

func appendUnique(values []string, value string) []string {
	if value == "" || contains(values, value) {
		return values
	}
	return append(values, value)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
