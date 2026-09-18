package config

import (
	"time"
)

type SecretRef struct {
	Kind string `json:"kind,omitempty" toml:"kind"`
	Name string `json:"name,omitempty" toml:"name"`
}

func (r SecretRef) Empty() bool {
	return r.Kind == "" && r.Name == ""
}

type Runtime struct {
	OperationBuffer  int `json:"operation_buffer" toml:"operation_buffer"`
	EventHistory     int `json:"event_history" toml:"event_history"`
	SubscriberBuffer int `json:"subscriber_buffer" toml:"subscriber_buffer"`
}

type State struct {
	DataDir        string        `json:"data_dir" toml:"data_dir"`
	BusyTimeout    time.Duration `json:"busy_timeout" toml:"-"`
	EventRetention int           `json:"event_retention" toml:"event_retention"`
}

type Memory struct {
	Enabled        bool   `json:"enabled" toml:"enabled"`
	Path           string `json:"path" toml:"path"`
	MaxCandidates  int    `json:"max_candidates" toml:"max_candidates"`
	MaxPromptBytes int    `json:"max_prompt_bytes" toml:"max_prompt_bytes"`
	SemanticRerank bool   `json:"semantic_rerank" toml:"semantic_rerank"`
}

type Telemetry struct {
	LogLevel string `json:"log_level" toml:"log_level"`
}

// Context configures what the agent knows about the repository before it starts
// searching.
type Context struct {
	Index        Index        `json:"index" toml:"index"`
	LSP          LSP          `json:"lsp" toml:"lsp"`
	RepoMap      RepoMap      `json:"repo_map" toml:"repo_map"`
	WorkingSet   WorkingSet   `json:"working_set" toml:"working_set"`
	Evidence     Evidence     `json:"evidence" toml:"evidence"`
	CodingPolicy CodingPolicy `json:"coding_policy" toml:"coding_policy"`
	View         View         `json:"view" toml:"view"`
	Compact      Compact      `json:"compact" toml:"compact"`
}

// View is the public model-visible working-set contract. Defaults come from
// remaining hard input, explicit operator values, or this protocol. There is
// no hidden window percent.
type View struct {
	RecentTailTurns int `json:"recent_tail_turns" toml:"recent_tail_turns"`
	// KeepRecentToolResults is retained on the public view contract and
	// snapshots. Model-visible tool results are finalized at first Admit
	// and are not rewritten on later samples.
	KeepRecentToolResults int    `json:"keep_recent_tool_results" toml:"keep_recent_tool_results"`
	HistoryTokenCeiling   int    `json:"history_token_ceiling" toml:"history_token_ceiling"`
	Digest                string `json:"digest" toml:"digest"`
	NarrativeMode         string `json:"narrative_mode" toml:"narrative_mode"`
	// CheckpointMaxBytes bounds one write-once closed-turn Dynamic block.
	// Zero inherits summary_max_bytes, then semantic_narrative_item_max_bytes.
	CheckpointMaxBytes int `json:"checkpoint_max_bytes" toml:"checkpoint_max_bytes"`
}

// Compact configures overflow replacement and digest generation limits.
// Zero token thresholds mean no early compact ceiling: the working set is
// bounded by context.view, and replacement runs only when a request cannot
// fit hard input. Scope is total or body_after_prefix.
type Compact struct {
	PrepareTokens                    int           `json:"prepare_tokens" toml:"prepare_tokens"`
	AutoCompactTokens                int           `json:"auto_compact_tokens" toml:"auto_compact_tokens"`
	EmergencyTokens                  int           `json:"emergency_tokens" toml:"emergency_tokens"`
	Scope                            string        `json:"scope" toml:"scope"`
	SummaryMaxBytes                  int           `json:"summary_max_bytes" toml:"summary_max_bytes"`
	MaxDigestEntries                 int           `json:"max_digest_entries" toml:"max_digest_entries"`
	TruthMaxBytes                    int           `json:"truth_max_bytes" toml:"truth_max_bytes"`
	TruthMaxEntities                 int           `json:"truth_max_entities" toml:"truth_max_entities"`
	MandatoryMaxEntities             int           `json:"mandatory_max_entities" toml:"mandatory_max_entities"`
	FactMaxEntities                  int           `json:"fact_max_entities" toml:"fact_max_entities"`
	VerifiedChangeRetentionTurns     int           `json:"verified_change_retention_turns" toml:"verified_change_retention_turns"`
	FailureMaxEntities               int           `json:"failure_max_entities" toml:"failure_max_entities"`
	HandleMaxEntities                int           `json:"handle_max_entities" toml:"handle_max_entities"`
	OmissionSampleMaxEntities        int           `json:"omission_sample_max_entities" toml:"omission_sample_max_entities"`
	SemanticNarrativeMaxInputTokens  int           `json:"semantic_narrative_max_input_tokens" toml:"semantic_narrative_max_input_tokens"`
	SemanticNarrativeMaxOutputTokens int           `json:"semantic_narrative_max_output_tokens" toml:"semantic_narrative_max_output_tokens"`
	SemanticNarrativeMaxItems        int           `json:"semantic_narrative_max_items" toml:"semantic_narrative_max_items"`
	SemanticNarrativeItemMaxBytes    int           `json:"semantic_narrative_item_max_bytes" toml:"semantic_narrative_item_max_bytes"`
	SemanticNarrativeTimeout         time.Duration `json:"semantic_narrative_timeout" toml:"-"`
	SemanticNarrativeRetryLimit      int           `json:"semantic_narrative_retry_limit" toml:"semantic_narrative_retry_limit"`
	OwnerDeltaMaxSegments            int           `json:"owner_delta_max_segments" toml:"owner_delta_max_segments"`
	OwnerDeltaMaxBytes               int           `json:"owner_delta_max_bytes" toml:"owner_delta_max_bytes"`
}

// RepoMap configures the repository overview appended to every request: which
// directories hold code, how the project is built, where it starts, and what the
// files in play declare. MaxBytes is the ceiling per request, so it is the knob
// that decides what the map costs on a long session.
type RepoMap struct {
	Enabled        bool `json:"enabled" toml:"enabled"`
	MaxBytes       int  `json:"max_bytes" toml:"max_bytes"`
	MaxDirectories int  `json:"max_directories" toml:"max_directories"`
}

// WorkingSet configures the ledger of paths the session has touched. MaxEntries
// bounds the paths the runtime discovered on its own; paths the user pinned are
// always reported.
type WorkingSet struct {
	Enabled    bool `json:"enabled" toml:"enabled"`
	MaxEntries int  `json:"max_entries" toml:"max_entries"`
	MaxBytes   int  `json:"max_bytes" toml:"max_bytes"`
}

// Evidence configures the section reporting what lookups established, which
// changes nothing has verified, and which calls were wasted. MaxEntries bounds
// only the facts: risks and reminders are the point of the section and are always
// reported in full.
type Evidence struct {
	Enabled    bool `json:"enabled" toml:"enabled"`
	MaxEntries int  `json:"max_entries" toml:"max_entries"`
	MaxBytes   int  `json:"max_bytes" toml:"max_bytes"`
}

// CodingPolicy configures the working method carried in the stable prefix. It has
// no ceiling because the text is a constant; disabling it removes the instruction
// but not the mechanisms behind it, which the runtime enforces regardless.
type CodingPolicy struct {
	Enabled bool `json:"enabled" toml:"enabled"`
}

// Index configures the repository symbol index. The ceilings bound a first
// build on a large repository: a file over MaxFileBytes is recorded without
// symbols, and a repository over MaxFiles is indexed only that far, which the
// symbol tools report as a truncated index rather than as complete results.
// LSP configures resident language-server sessions. ResidentEnabled is off
// by default: a resident server is a host process, and the session must say
// so explicitly before one is kept alive.
type LSP struct {
	ResidentEnabled bool          `json:"resident_enabled" toml:"resident_enabled"`
	IdleTimeout     time.Duration `json:"idle_timeout" toml:"-"`
	MaxServers      int           `json:"max_servers" toml:"max_servers"`
	CacheCapacity   int           `json:"cache_capacity" toml:"cache_capacity"`
}

type Index struct {
	Enabled      bool  `json:"enabled" toml:"enabled"`
	MaxFileBytes int64 `json:"max_file_bytes" toml:"max_file_bytes"`
	MaxFiles     int   `json:"max_files" toml:"max_files"`
	// SignatureMaxBytes and DocstringMaxBytes bound the declaration text and
	// comment block recorded per symbol; ReferenceMaxCount bounds how many
	// distinct identifiers one file contributes. They keep the index from
	// becoming a copy of pathological source files.
	SignatureMaxBytes int64 `json:"signature_max_bytes" toml:"signature_max_bytes"`
	DocstringMaxBytes int64 `json:"docstring_max_bytes" toml:"docstring_max_bytes"`
	ReferenceMaxCount int   `json:"reference_max_count" toml:"reference_max_count"`
	// Rank bounds the file PageRank behind the repository map's directory
	// ranking. The damping default is the standard value from the original
	// PageRank paper (Brin & Page, 1998); the iteration limit and convergence
	// threshold bound the refinement loop.
	RankDamping     float64 `json:"rank_damping_factor" toml:"rank_damping_factor"`
	RankIterations  int     `json:"rank_iteration_limit" toml:"rank_iteration_limit"`
	RankConvergence float64 `json:"rank_convergence_threshold" toml:"rank_convergence_threshold"`
	// Impact bounds the reverse dependency walk behind affected-test
	// answers: how many hops a change reaches through and how many files one
	// answer may name.
	ImpactMaxDepth   int `json:"impact_max_depth" toml:"impact_max_depth"`
	ImpactMaxResults int `json:"impact_max_results" toml:"impact_max_results"`
}

// Execution configures the main agent loop. MaxOutputTokens is an optional
// operator ceiling; zero uses an adaptive ceiling bounded by the active model.
type Execution struct {
	Provider        string `json:"provider" toml:"provider"`
	Model           string `json:"model" toml:"model"`
	Protocol        string `json:"protocol" toml:"protocol"`
	Mode            string `json:"mode" toml:"mode"`
	Workspace       string `json:"workspace" toml:"workspace"`
	Tools           bool   `json:"tools" toml:"tools"`
	MaxOutputTokens uint64 `json:"max_output_tokens" toml:"max_output_tokens"`
	MaxSteps        int    `json:"max_steps" toml:"max_steps"`
	// ImplementNoProgressSamples is the finish-only lease for consecutive
	// Samples that repeat the same work-state observation with the same
	// tool-call identity. Zero inherits the MaxSteps-derived 2/3
	// finish-only lease. A new workspace version or tool-result digest
	// renews both clocks; a seen observation with a different identity
	// consumes the MaxSteps-derived lease instead.
	ImplementNoProgressSamples int `json:"implement_no_progress_samples" toml:"implement_no_progress_samples"`
	// Timeout covers connection establishment, TLS negotiation, and response
	// headers. Streaming body lifetime is governed by the caller Context and
	// IdleTimeout, so active streams do not inherit a fixed wall-clock limit.
	Timeout time.Duration `json:"timeout" toml:"-"`
	// LeaseTimeout bounds the authorization-to-execution handoff. It does not
	// terminate work after a Broker has consumed the Lease.
	LeaseTimeout time.Duration `json:"lease_timeout" toml:"-"`
	// ApprovalTimeout optionally bounds a pending human approval. Zero keeps
	// the request alive until the Turn or Session is canceled.
	ApprovalTimeout time.Duration `json:"approval_timeout" toml:"-"`
	// The phase-specific values override Timeout when non-zero.
	ConnectionTimeout     time.Duration `json:"connection_timeout" toml:"-"`
	TLSHandshakeTimeout   time.Duration `json:"tls_handshake_timeout" toml:"-"`
	ResponseHeaderTimeout time.Duration `json:"response_header_timeout" toml:"-"`
	// IdleTimeout is renewed by every provider stream event.
	IdleTimeout   time.Duration `json:"idle_timeout" toml:"-"`
	MaxConcurrent int           `json:"max_concurrent" toml:"max_concurrent"`
	RateLimit     float64       `json:"rate_limit" toml:"rate_limit"`
	// ProviderRetryLimit bounds non-rate-limit transient Provider retries.
	ProviderRetryLimit int `json:"provider_retry_limit" toml:"provider_retry_limit"`
	// RateLimitRetryLimit bounds 429 recoveries per Model Sample.
	// Zero leaves the attempt count unbounded; the wait budget still applies.
	RateLimitRetryLimit int `json:"rate_limit_retry_limit" toml:"rate_limit_retry_limit"`
	// RateLimitWait is the maximum cumulative 429 wait per Model Sample.
	// Zero inherits Timeout when the engine is constructed. The default is a
	// distinct public ceiling from connection Timeout because Retry-After
	// recovery is a different plane.
	RateLimitWait time.Duration `json:"rate_limit_wait" toml:"-"`
	// TokensPerMinute is the Operator TPM contract for Provider Throughput
	// Admission. Zero means unknown; Runtime does not invent a model default.
	TokensPerMinute uint64 `json:"tokens_per_minute" toml:"tokens_per_minute"`
	BudgetTokens    uint64 `json:"budget_tokens" toml:"budget_tokens"`
	// TurnBudgetTokens is an optional cumulative operator ceiling. Zero leaves
	// the Turn uncapped; each request remains bounded by model capacity.
	TurnBudgetTokens uint64   `json:"turn_budget_tokens" toml:"turn_budget_tokens"`
	BudgetUSD        float64  `json:"budget_usd" toml:"budget_usd"`
	ReasoningEffort  string   `json:"reasoning_effort" toml:"reasoning_effort"`
	NativeSearch     bool     `json:"native_search" toml:"native_search"`
	Verify           Verify   `json:"verify" toml:"verify"`
	Subagent         Subagent `json:"subagent" toml:"subagent"`
	Journal          Journal  `json:"journal" toml:"journal"`
}

// RateLimitWaitBudget is the cumulative 429 wait bound used by the engine.
// A zero RateLimitWait inherits Timeout; a non-zero value is the explicit
// public ceiling used by Defaults.
func (e Execution) RateLimitWaitBudget() time.Duration {
	if e.RateLimitWait > 0 {
		return e.RateLimitWait
	}
	return e.Timeout
}

// Journal configures the edit-transaction journal.
//
// Durable puts the turn ledger and before-images under the workspace state
// directory so a process killed mid-turn can be undone by the next one. Turning
// it off keeps atomicity inside the live process only, which is what a throwaway
// workspace wants and what a workspace holding real work does not.
type Journal struct {
	Durable bool `json:"durable" toml:"durable"`
	// RecoverOnStart undoes interrupted turns found at startup. Off leaves them
	// in the ledger for a later, explicit recovery.
	RecoverOnStart bool `json:"recover_on_start" toml:"recover_on_start"`
}

// Subagent bounds what a spawned child agent may spend and where it may write.
// Child budgets are deliberately separate from the session budget: a runaway
// child must not be able to consume the parent's whole allowance, and a child
// that writes must not be able to write into the parent's workspace.
type Subagent struct {
	// Delegation selects whether model-visible spawning is disabled,
	// explicit-only, or adaptively available.
	Delegation  string `json:"delegation" toml:"delegation"`
	MaxDepth    int    `json:"max_depth" toml:"max_depth"`
	MaxParallel int    `json:"max_parallel" toml:"max_parallel"`
	MaxResident int    `json:"max_resident" toml:"max_resident"`
	MaxTotal    int    `json:"max_total" toml:"max_total"`
	// MaxSteps is an optional explicit child quota. Zero uses progress
	// convergence and spend budgets without an implicit step cap.
	MaxSteps int `json:"max_steps" toml:"max_steps"`
	// MaxTokens and MaxCostUSD bound all child agents in the session together.
	// Zero MaxTokens derives the tree ceiling from the active model's context
	// window and MaxParallel instead of installing a model-independent default.
	MaxTokens  uint64  `json:"max_tokens" toml:"max_tokens"`
	MaxCostUSD float64 `json:"max_cost_usd" toml:"max_cost_usd"`
	// WallTime is an optional renewable child execution lease. Runtime progress
	// renews it; zero disables it.
	WallTime time.Duration `json:"wall_time" toml:"-"`
	// Workspace selects the isolation strategy: auto picks per stance,
	// read_only shares the parent workspace without a journal, worktree
	// requires a git worktree per writing child.
	Workspace string `json:"workspace" toml:"workspace"`
}

// Subagent workspace isolation strategies.
const (
	SubagentDelegationDisabled = "disabled"
	SubagentDelegationExplicit = "explicit"
	SubagentDelegationAdaptive = "adaptive"

	SubagentWorkspaceAuto       = "auto"
	SubagentWorkspaceReadOnly   = "read_only"
	SubagentWorkspaceWorktree   = "worktree"
	SubagentWorkspaceSerialized = "same_workspace_serialized"
)

// Verify configures the gate that runs before a turn commits its edits.
//
// Mode off skips the gate; soft reports the verdict without changing the turn
// outcome; hard applies OnFailure once the repair budget is spent.
type Verify struct {
	Mode      string `json:"mode" toml:"mode"`
	Scope     string `json:"scope" toml:"scope"`
	OnFailure string `json:"on_failure" toml:"on_failure"`
	// Command overrides the repository scope's detected commands with one shell
	// command, for workspaces whose entry point cannot be inferred.
	Command        string        `json:"command,omitempty" toml:"command"`
	MaxRepairSteps int           `json:"max_repair_steps" toml:"max_repair_steps"`
	Timeout        time.Duration `json:"timeout" toml:"-"`
}

type Vision struct {
	Enabled  bool   `json:"enabled" toml:"enabled"`
	Provider string `json:"provider" toml:"provider"`
	Model    string `json:"model" toml:"model"`
}

// RouteSlot is the provider and model one purpose samples on.
type RouteSlot struct {
	Provider string `json:"provider" toml:"provider"`
	Model    string `json:"model" toml:"model"`
}

// Empty reports a slot nobody configured.
func (s RouteSlot) Empty() bool { return s.Provider == "" && s.Model == "" }

// Route is per-purpose model selection. Slots is keyed by purpose name and holds
// only what configuration named; the act route stays in Execution, because
// execution.provider and execution.model already are it.
//
// Lock forbids falling back to act, which is what a reproducible run wants: with
// it on, a purpose without a slot is an error rather than a silent substitution.
type Route struct {
	Lock  bool                 `json:"lock" toml:"lock"`
	Slots map[string]RouteSlot `json:"slots,omitempty" toml:"-"`
}

type Web struct {
	SearchBackend string `json:"search_backend" toml:"search_backend"`
}

// DiagnosticCommand maps one file extension to a bounded post-edit checker.
// Name is resolved through PATH; Args must include {path} so the checker stays
// scoped to the file the guarded edit changed.
type DiagnosticCommand struct {
	Name string   `json:"name" toml:"name"`
	Args []string `json:"args" toml:"args"`
}

type Diagnostics struct {
	Commands map[string]DiagnosticCommand `json:"commands,omitempty" toml:"commands"`
}

type Config struct {
	Runtime     Runtime     `json:"runtime" toml:"runtime"`
	State       State       `json:"state" toml:"state"`
	Memory      Memory      `json:"memory" toml:"memory"`
	Context     Context     `json:"context" toml:"context"`
	Telemetry   Telemetry   `json:"telemetry" toml:"telemetry"`
	Credential  SecretRef   `json:"credential" toml:"credential"`
	Execution   Execution   `json:"execution" toml:"execution"`
	Route       Route       `json:"route" toml:"route"`
	Vision      Vision      `json:"vision" toml:"vision"`
	Web         Web         `json:"web" toml:"web"`
	Diagnostics Diagnostics `json:"diagnostics" toml:"diagnostics"`
}

type Overrides struct {
	OperationBuffer      *int
	EventHistory         *int
	SubscriberBuffer     *int
	StateDataDir         *string
	StateBusyTimeout     *time.Duration
	StateRetention       *int
	MemoryEnabled        *bool
	MemoryPath           *string
	MemoryMaxCandidates  *int
	MemoryMaxPromptBytes *int
	MemorySemanticRerank *bool
	IndexEnabled         *bool
	IndexMaxBytes        *int64
	IndexMaxFiles        *int
	IndexSignatureMax    *int64
	IndexDocstringMax    *int64
	IndexReferenceMax    *int
	IndexRankDamping     *float64
	IndexRankIterations  *int
	IndexRankConvergence *float64
	IndexImpactDepth     *int
	IndexImpactResults   *int
	LSPResidentEnabled   *bool
	LSPIdleTimeout       *time.Duration
	LSPMaxServers        *int
	LSPCacheCapacity     *int

	RepoMapEnabled                          *bool
	RepoMapMaxBytes                         *int
	RepoMapMaxDirectories                   *int
	WorkingSetEnabled                       *bool
	WorkingSetMaxEntries                    *int
	WorkingSetMaxBytes                      *int
	EvidenceEnabled                         *bool
	EvidenceMaxEntries                      *int
	EvidenceMaxBytes                        *int
	CodingPolicyEnabled                     *bool
	CompactAutoTokens                       *int
	CompactPrepareTokens                    *int
	CompactEmergencyTokens                  *int
	CompactScope                            *string
	CompactSummaryMax                       *int
	CompactMaxDigest                        *int
	CompactTruthMaxBytes                    *int
	CompactTruthMaxEntities                 *int
	CompactMandatoryMaxEntities             *int
	CompactFactMaxEntities                  *int
	CompactVerifiedChangeRetentionTurns     *int
	CompactFailureMaxEntities               *int
	CompactHandleMaxEntities                *int
	CompactOmissionSampleMaxEntities        *int
	ViewRecentTailTurns                     *int
	ViewKeepRecentToolResults               *int
	ViewHistoryTokenCeiling                 *int
	ViewDigest                              *string
	ViewNarrativeMode                       *string
	ViewCheckpointMaxBytes                  *int
	CompactSemanticNarrativeMaxInputTokens  *int
	CompactSemanticNarrativeMaxOutputTokens *int
	CompactSemanticNarrativeMaxItems        *int
	CompactSemanticNarrativeItemMaxBytes    *int
	CompactSemanticNarrativeTimeout         *time.Duration
	CompactSemanticNarrativeRetryLimit      *int
	CompactOwnerDeltaMaxSegments            *int
	CompactOwnerDeltaMaxBytes               *int

	LogLevel                   *string
	CredentialKind             *string
	CredentialName             *string
	Provider                   *string
	Model                      *string
	Protocol                   *string
	Mode                       *string
	Workspace                  *string
	Tools                      *bool
	MaxOutputTokens            *uint64
	MaxSteps                   *int
	ImplementNoProgressSamples *int
	Timeout                    *time.Duration
	LeaseTimeout               *time.Duration
	ApprovalTimeout            *time.Duration
	ConnectionTimeout          *time.Duration
	TLSHandshakeTimeout        *time.Duration
	ResponseHeaderTimeout      *time.Duration
	IdleTimeout                *time.Duration
	MaxConcurrent              *int
	RateLimit                  *float64
	ProviderRetryLimit         *int
	RateLimitRetryLimit        *int
	RateLimitWait              *time.Duration
	TokensPerMinute            *uint64
	BudgetTokens               *uint64
	TurnBudgetTokens           *uint64
	BudgetUSD                  *float64
	ReasoningEffort            *string
	NativeSearch               *bool
	VerifyMode                 *string
	VerifyScope                *string
	VerifyOnFailure            *string
	VerifyCommand              *string
	VerifyRepair               *int
	VerifyTimeout              *time.Duration
	JournalDurable             *bool
	JournalRecoverOnStart      *bool

	SubagentMaxDepth    *int
	SubagentDelegation  *string
	SubagentMaxParallel *int
	SubagentMaxResident *int
	SubagentMaxTotal    *int
	SubagentMaxSteps    *int
	SubagentMaxTokens   *uint64
	SubagentMaxCostUSD  *float64
	SubagentWallTime    *time.Duration
	SubagentWorkspace   *string

	VisionEnabled    *bool
	VisionProvider   *string
	VisionModel      *string
	WebSearchBackend *string

	// RouteLock forbids falling back to the act route. Slots themselves are
	// configuration-only: six purposes worth of provider/model flags would crowd
	// every command's help for something a reproducible run states in a file.
	RouteLock *bool
}
