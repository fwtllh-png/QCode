package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Defaults() Config {
	return Config{
		Runtime: Runtime{
			OperationBuffer:  64,
			EventHistory:     256,
			SubscriberBuffer: 64,
		},
		State: State{
			DataDir:        defaultDataDir(),
			BusyTimeout:    5 * time.Second,
			EventRetention: 1_000_000,
		},
		Memory: Memory{
			Path: defaultMemoryPath(), MaxCandidates: 32,
			MaxPromptBytes: 16 << 10,
		},

		Context: Context{
			Index: Index{
				Enabled: true, MaxFileBytes: 1 << 20, MaxFiles: 20000,
				// The detail bounds keep one pathological file from turning
				// the index into a copy of its own source; the defaults hold a
				// long parameter list, a full comment block, and the distinct
				// identifiers of a large generated file respectively.
				SignatureMaxBytes: 512, DocstringMaxBytes: 2048,
				ReferenceMaxCount: 4096,
				// Damping 0.85 is the PageRank standard (Brin & Page,
				// 1998); 100 iterations and a 1e-6 total-movement stop
				// converge long before the limit on repository-scale
				// graphs and bound the loop on pathological ones.
				RankDamping: 0.85, RankIterations: 100,
				RankConvergence: 1e-6,
				// Three hops cover a direct dependent, its dependents and
				// one more layer; beyond that a lexical graph's
				// name-coincidence error grows faster than its recall.
				ImpactMaxDepth: 3, ImpactMaxResults: 200,
			},

			LSP: LSP{
				// Off by default: a resident language server is a host
				// process and must be asked for explicitly. The bounds,
				// when it is on: a ten-minute idle window, two servers,
				// 256 remembered answers.
				IdleTimeout: 10 * time.Minute, MaxServers: 2,
				CacheCapacity: 256,
			},

			RepoMap:    RepoMap{Enabled: true, MaxBytes: 8 << 10, MaxDirectories: 24},
			WorkingSet: WorkingSet{Enabled: true, MaxEntries: 16, MaxBytes: 8 << 10},

			Evidence:     Evidence{Enabled: true, MaxEntries: 24, MaxBytes: 4 << 10},
			CodingPolicy: CodingPolicy{Enabled: true},

			View: View{
				RecentTailTurns:       2,
				KeepRecentToolResults: 0,
				HistoryTokenCeiling:   0,
				Digest:                "ledger",
				NarrativeMode:         "post_turn",
				CheckpointMaxBytes:    0,
			},

			Compact: Compact{
				Scope: "total", SummaryMaxBytes: 0, MaxDigestEntries: 120,
				TruthMaxBytes: 0, TruthMaxEntities: 256,
				MandatoryMaxEntities: 128, FactMaxEntities: 96,
				VerifiedChangeRetentionTurns:     32,
				FailureMaxEntities:               24,
				HandleMaxEntities:                32,
				OmissionSampleMaxEntities:        8,
				SemanticNarrativeMaxInputTokens:  4096,
				SemanticNarrativeMaxOutputTokens: 0,
				SemanticNarrativeMaxItems:        32,
				SemanticNarrativeItemMaxBytes:    512,
				SemanticNarrativeTimeout:         30 * time.Second,
				SemanticNarrativeRetryLimit:      1,
				OwnerDeltaMaxSegments:            16,
				OwnerDeltaMaxBytes:               64 << 10,
			},
		},
		Telemetry: Telemetry{LogLevel: "info"},
		Execution: Execution{
			Protocol: "openai_chat", Mode: "act", Workspace: ".",
			MaxSteps:                   64,
			ImplementNoProgressSamples: 6,
			Timeout:                    2 * time.Minute,
			LeaseTimeout:               2 * time.Minute,
			IdleTimeout:                60 * time.Second, MaxConcurrent: 8,
			ProviderRetryLimit:  3,
			RateLimitRetryLimit: 0,
			RateLimitWait:       10 * time.Minute,
			TokensPerMinute:     0,

			Verify: Verify{
				Mode: "soft", Scope: "diagnostics", OnFailure: "fail",
				MaxRepairSteps: 1, Timeout: 2 * time.Minute,
			},

			Subagent: Subagent{
				Delegation: SubagentDelegationAdaptive,
				MaxDepth:   5, MaxParallel: 4, MaxResident: 8, MaxTotal: 16,
				Workspace: SubagentWorkspaceAuto,
			},

			Journal: Journal{Durable: true, RecoverOnStart: true},

			Environment: ExecutionEnvironment{
				Contract: EnvironmentContractV1,
				Profile:  EnvironmentProfileNative,
			},
		},
	}
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".qcode", "v1")
	}
	return filepath.Join(home, ".qcode", "v1")
}

func defaultMemoryPath() string {
	return filepath.Join(defaultDataDir(), "memory")
}
