package engine

import (
	"context"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	toolguard "github.com/fwtllh-png/QCode/internal/adapter/tool/guard"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/interact"
	"github.com/fwtllh-png/QCode/internal/observability/trace"
	"github.com/fwtllh-png/QCode/internal/observability/verify"
	"github.com/fwtllh-png/QCode/internal/persist/workspacejournal"
	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
	promptcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/prompt"
	"github.com/fwtllh-png/QCode/internal/runtime/agent/turnkernel"
	"github.com/fwtllh-png/QCode/internal/security/policy"
)

type ProviderConfig struct {
	Provider         provider.Provider
	Route            model.ReadyRoute
	Routes           model.RouteSet
	SelectableRoutes map[string]model.ReadyRoute

	MaxOutputTokens            uint64
	MaxSteps                   int
	ImplementNoProgressSamples int
	MaxRetries                 int
	MaxRetryDelay              time.Duration
	InfrastructureRetryLimit   int
	RateLimitMaxRetries        int
	RateLimitMaxWait           time.Duration
	SharedRateLimit            *SharedRateLimit
	TokensPerMinute            uint64
	ReasoningEffort            string
	NativeSearch               bool
	TokenEstimator             TokenEstimator
}

type ContextConfig struct {
	StaticContext         []provider.Message
	ContextBudgets        map[string]promptcontext.Budget
	CodingPolicy          bool
	SummaryMaxBytes       int
	MaxDigestEntries      int
	Context               ContextPolicy
	Budget                Budget
	WorkingSet            []string
	CriticalPaths         []string
	StaticContextReceipts []promptcontext.Receipt
	PromptCacheKey        string
	TurnSnapshots         TurnSnapshotSources
	RepoContext           RepoContext
	WorkingSetLimit       int
	EvidenceLimit         int
}

type ToolConfig struct {
	Tools          *tool.Registry
	Authorize      func(provider.ToolCall) bool
	Guard          *toolguard.Guard
	GuardFactory   func(context.Context) (*toolguard.Guard, error)
	OnNetworkAllow toolguard.NetworkAllow
	Diagnostics    verify.DiagnosticRunner
	Verify         VerifyOptions
	// VerificationOnly restricts verifier-role process launches to declared,
	// workspace-read-only verification commands.
	VerificationOnly bool

	RequireCompletionDeclaration bool
	MaxToolConcurrent            int
	MaxToolStreamBytes           int
	MaxToolDefinitions           int
	MaxToolSchemaBytes           int
	ToolCatalogSync              func() error
}

type SecurityConfig struct {
	Security                 *policy.Runtime
	ProfilePermissionCeiling policy.Permission
	Workspace                string
	WorkspaceIdentity        string
	WorkspaceIsolation       string
	Journal                  *workspacejournal.Manager
	ReadTracker              *workspacejournal.ReadTracker
	WorkspaceTurnGate        *WorkspaceTurnGate
}

type TelemetryConfig struct {
	Metrics            Metrics
	Observability      trace.Runtime
	TurnKernelObserver func(turnkernel.TransitionRecord)
}

type LifecycleConfig struct {
	TurnCoordinatorRuntime turnkernel.CoordinatorRuntime
	ReleaseTurnResources   func(TurnIdentity)
	SessionID              string
	DelegationMode         string
	InputHost              *interact.Host
	ProfileRevision        uint64
	// SessionForTurn reports the living session that owns a turn. The
	// boolean is false when the turn or its session no longer exists.
	SessionForTurn func(context.Context, string) (string, bool)
	// TurnTranscriptArchive recovers the durable transcript of a closed turn
	// whose raw history was removed from memory by compaction or
	// replacement. turn_history consults it when the in-memory history no
	// longer contains the turn.
	TurnTranscriptArchive TurnTranscriptArchive
	// TurnContinuations stores the running Turn's accepted conversation so a
	// restarted process can rebuild the in-turn context that never reached a
	// terminal SessionDelta. Nil disables durable continuation.
	TurnContinuations agentcontext.BlobStore
	TurnContexts      agentcontext.TurnContextStore
}

type Options struct {
	ProviderConfig
	ContextConfig
	ToolConfig
	SecurityConfig
	TelemetryConfig
	LifecycleConfig
}
