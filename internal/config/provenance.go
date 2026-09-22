package config

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceRepo    Source = "repo"
	SourceEnv     Source = "env"
	SourceStartup Source = "startup"
)

const (
	fieldOperationBuffer       = "runtime.operation_buffer"
	fieldEventHistory          = "runtime.event_history"
	fieldSubscriberBuffer      = "runtime.subscriber_buffer"
	fieldStateDataDir          = "state.data_dir"
	fieldStateBusyTimeout      = "state.busy_timeout"
	fieldStateRetention        = "state.event_retention"
	fieldDeletedEventRetention = "state.deleted_event_retention"
	fieldArchiveDeletedEvents  = "state.archive_deleted_events"
	fieldMemoryEnabled         = "memory.enabled"
	fieldMemoryPath            = "memory.path"
	fieldMemoryMaxCandidates   = "memory.max_candidates"
	fieldMemoryMaxPromptBytes  = "memory.max_prompt_bytes"
	fieldMemorySemanticRerank  = "memory.semantic_rerank"
	fieldIndexEnabled          = "context.index.enabled"
	fieldIndexMaxBytes         = "context.index.max_file_bytes"
	fieldIndexMaxFiles         = "context.index.max_files"
	fieldIndexSignatureMax     = "context.index.signature_max_bytes"
	fieldIndexDocstringMax     = "context.index.docstring_max_bytes"
	fieldIndexReferenceMax     = "context.index.reference_max_count"
	fieldIndexRankDamping      = "context.index.rank_damping_factor"
	fieldIndexRankIterations   = "context.index.rank_iteration_limit"
	fieldIndexRankConvergence  = "context.index.rank_convergence_threshold"
	fieldIndexImpactDepth      = "context.index.impact_max_depth"
	fieldIndexImpactResults    = "context.index.impact_max_results"
	fieldLSPResidentEnabled    = "context.lsp.resident_enabled"
	fieldLSPIdleTimeout        = "context.lsp.idle_timeout"
	fieldLSPMaxServers         = "context.lsp.max_servers"
	fieldLSPCacheCapacity      = "context.lsp.cache_capacity"

	fieldRepoMapEnabled        = "context.repo_map.enabled"
	fieldRepoMapMaxBytes       = "context.repo_map.max_bytes"
	fieldRepoMapMaxDirectories = "context.repo_map.max_directories"
	fieldWorkingSetEnabled     = "context.working_set.enabled"
	fieldWorkingSetMaxEntries  = "context.working_set.max_entries"
	fieldWorkingSetMaxBytes    = "context.working_set.max_bytes"
	fieldEvidenceEnabled       = "context.evidence.enabled"
	fieldEvidenceMaxEntries    = "context.evidence.max_entries"
	fieldEvidenceMaxBytes      = "context.evidence.max_bytes"
	fieldCodingPolicyEnabled   = "context.coding_policy.enabled"

	fieldViewRecentTailTurns       = "context.view.recent_tail_turns"
	fieldViewKeepRecentToolResults = "context.view.keep_recent_tool_results"
	fieldViewHistoryTokenCeiling   = "context.view.history_token_ceiling"
	fieldViewDigest                = "context.view.digest"
	fieldViewNarrativeMode         = "context.view.narrative_mode"
	fieldViewCheckpointMaxBytes    = "context.view.checkpoint_max_bytes"

	fieldCompactAutoTokens                       = "context.compact.auto_compact_tokens"
	fieldCompactPrepareTokens                    = "context.compact.prepare_tokens"
	fieldCompactEmergencyTokens                  = "context.compact.emergency_tokens"
	fieldCompactScope                            = "context.compact.scope"
	fieldCompactSummaryMax                       = "context.compact.summary_max_bytes"
	fieldCompactMaxDigest                        = "context.compact.max_digest_entries"
	fieldCompactTruthMaxBytes                    = "context.compact.truth_max_bytes"
	fieldCompactTruthMaxEntities                 = "context.compact.truth_max_entities"
	fieldCompactMandatoryMaxEntities             = "context.compact.mandatory_max_entities"
	fieldCompactFactMaxEntities                  = "context.compact.fact_max_entities"
	fieldCompactVerifiedChangeRetentionTurns     = "context.compact.verified_change_retention_turns"
	fieldCompactFailureMaxEntities               = "context.compact.failure_max_entities"
	fieldCompactHandleMaxEntities                = "context.compact.handle_max_entities"
	fieldCompactOmissionSampleMaxEntities        = "context.compact.omission_sample_max_entities"
	fieldCompactSemanticNarrativeMaxInputTokens  = "context.compact.semantic_narrative_max_input_tokens"
	fieldCompactSemanticNarrativeMaxOutputTokens = "context.compact.semantic_narrative_max_output_tokens"
	fieldCompactSemanticNarrativeMaxItems        = "context.compact.semantic_narrative_max_items"
	fieldCompactSemanticNarrativeItemMaxBytes    = "context.compact.semantic_narrative_item_max_bytes"
	fieldCompactSemanticNarrativeTimeout         = "context.compact.semantic_narrative_timeout"
	fieldCompactSemanticNarrativeRetryLimit      = "context.compact.semantic_narrative_retry_limit"
	fieldCompactOwnerDeltaMaxSegments            = "context.compact.owner_delta_max_segments"
	fieldCompactOwnerDeltaMaxBytes               = "context.compact.owner_delta_max_bytes"

	fieldLogLevel                   = "telemetry.log_level"
	fieldCredentialKind             = "credential.kind"
	fieldCredentialName             = "credential.name"
	fieldProvider                   = "execution.provider"
	fieldModel                      = "execution.model"
	fieldProtocol                   = "execution.protocol"
	fieldMode                       = "execution.mode"
	fieldWorkspace                  = "execution.workspace"
	fieldTools                      = "execution.tools"
	fieldMaxOutputTokens            = "execution.max_output_tokens"
	fieldMaxSteps                   = "execution.max_steps"
	fieldImplementNoProgressSamples = "execution.implement_no_progress_samples"
	fieldTimeout                    = "execution.timeout"
	fieldLeaseTimeout               = "execution.lease_timeout"
	fieldApprovalTimeout            = "execution.approval_timeout"
	fieldConnectionTimeout          = "execution.connection_timeout"
	fieldTLSHandshakeTimeout        = "execution.tls_handshake_timeout"
	fieldResponseHeaderTimeout      = "execution.response_header_timeout"
	fieldIdleTimeout                = "execution.idle_timeout"
	fieldMaxConcurrent              = "execution.max_concurrent"
	fieldRateLimit                  = "execution.rate_limit"
	fieldProviderRetryLimit         = "execution.provider_retry_limit"
	fieldRateLimitRetryLimit        = "execution.rate_limit_retry_limit"
	fieldRateLimitWait              = "execution.rate_limit_wait"
	fieldTokensPerMinute            = "execution.tokens_per_minute"
	fieldBudgetTokens               = "execution.budget_tokens"
	fieldTurnBudgetTokens           = "execution.turn_budget_tokens"
	fieldBudgetUSD                  = "execution.budget_usd"
	fieldReasoning                  = "execution.reasoning_effort"
	fieldNativeSearch               = "execution.native_search"
	fieldVerifyMode                 = "execution.verify.mode"
	fieldVerifyScope                = "execution.verify.scope"
	fieldVerifyOnFailure            = "execution.verify.on_failure"
	fieldVerifyCommand              = "execution.verify.command"
	fieldVerifyRepair               = "execution.verify.max_repair_steps"
	fieldVerifyTimeout              = "execution.verify.timeout"

	fieldSubagentDelegation  = "execution.subagent.delegation"
	fieldSubagentMaxDepth    = "execution.subagent.max_depth"
	fieldSubagentMaxParallel = "execution.subagent.max_parallel"
	fieldSubagentMaxResident = "execution.subagent.max_resident"
	fieldSubagentMaxTotal    = "execution.subagent.max_total"
	fieldSubagentMaxSteps    = "execution.subagent.max_steps"
	fieldSubagentMaxTokens   = "execution.subagent.max_tokens"
	fieldSubagentMaxCostUSD  = "execution.subagent.max_cost_usd"
	fieldSubagentWallTime    = "execution.subagent.wall_time"
	fieldSubagentWorkspace   = "execution.subagent.workspace"

	fieldJournalDurable        = "execution.journal.durable"
	fieldJournalRecoverOnStart = "execution.journal.recover_on_start"

	fieldEnvironmentContract       = "execution.environment.contract"
	fieldEnvironmentProfile        = "execution.environment.profile"
	fieldEnvironmentSharedUserTemp = "execution.environment.shared_user_temp"
	fieldEnvironmentSource         = "execution.environment.source"
	fieldEnvironmentResources      = "execution.environment.resources"
	fieldEnvironmentAuthServices   = "execution.environment.auth_services"

	fieldVisionEnabled    = "vision.enabled"
	fieldVisionProvider   = "vision.provider"
	fieldVisionModel      = "vision.model"
	fieldWebSearchBackend = "web.search_backend"

	fieldRouteLock = "route.lock"
)

// fieldRouteProvider and fieldRouteModel name a purpose's slot for provenance
// and for errors. The purpose is part of the field name because "route.provider"
// would report the wrong thing once a second slot is configured.
func fieldRouteProvider(purpose string) string { return "route." + purpose + ".provider" }

func fieldRouteModel(purpose string) string { return "route." + purpose + ".model" }

func fieldDiagnosticCommandName(extension string) string {
	return "diagnostics.commands." + extension + ".name"
}

func fieldDiagnosticCommandArgs(extension string) string {
	return "diagnostics.commands." + extension + ".args"
}

type Source string

type Snapshot struct {
	Config     Config            `json:"config"`
	Provenance map[string]Source `json:"provenance"`
}

func defaultProvenance() map[string]Source {
	return map[string]Source{
		fieldOperationBuffer:       SourceDefault,
		fieldEventHistory:          SourceDefault,
		fieldSubscriberBuffer:      SourceDefault,
		fieldStateDataDir:          SourceDefault,
		fieldStateBusyTimeout:      SourceDefault,
		fieldStateRetention:        SourceDefault,
		fieldDeletedEventRetention: SourceDefault,
		fieldArchiveDeletedEvents:  SourceDefault,
		fieldMemoryEnabled:         SourceDefault,
		fieldMemoryPath:            SourceDefault,
		fieldMemoryMaxCandidates:   SourceDefault,
		fieldMemoryMaxPromptBytes:  SourceDefault,
		fieldMemorySemanticRerank:  SourceDefault,
		fieldIndexEnabled:          SourceDefault,
		fieldIndexMaxBytes:         SourceDefault,
		fieldIndexMaxFiles:         SourceDefault,
		fieldIndexSignatureMax:     SourceDefault,
		fieldIndexDocstringMax:     SourceDefault,
		fieldIndexReferenceMax:     SourceDefault,
		fieldIndexRankDamping:      SourceDefault,
		fieldIndexRankIterations:   SourceDefault,
		fieldIndexRankConvergence:  SourceDefault,
		fieldIndexImpactDepth:      SourceDefault,
		fieldIndexImpactResults:    SourceDefault,
		fieldLSPResidentEnabled:    SourceDefault,
		fieldLSPIdleTimeout:        SourceDefault,
		fieldLSPMaxServers:         SourceDefault,
		fieldLSPCacheCapacity:      SourceDefault,

		fieldRepoMapEnabled:                          SourceDefault,
		fieldRepoMapMaxBytes:                         SourceDefault,
		fieldRepoMapMaxDirectories:                   SourceDefault,
		fieldWorkingSetEnabled:                       SourceDefault,
		fieldWorkingSetMaxEntries:                    SourceDefault,
		fieldWorkingSetMaxBytes:                      SourceDefault,
		fieldEvidenceEnabled:                         SourceDefault,
		fieldEvidenceMaxEntries:                      SourceDefault,
		fieldEvidenceMaxBytes:                        SourceDefault,
		fieldCodingPolicyEnabled:                     SourceDefault,
		fieldViewRecentTailTurns:                     SourceDefault,
		fieldViewKeepRecentToolResults:               SourceDefault,
		fieldViewHistoryTokenCeiling:                 SourceDefault,
		fieldViewDigest:                              SourceDefault,
		fieldViewNarrativeMode:                       SourceDefault,
		fieldViewCheckpointMaxBytes:                  SourceDefault,
		fieldCompactAutoTokens:                       SourceDefault,
		fieldCompactPrepareTokens:                    SourceDefault,
		fieldCompactEmergencyTokens:                  SourceDefault,
		fieldCompactScope:                            SourceDefault,
		fieldCompactSummaryMax:                       SourceDefault,
		fieldCompactMaxDigest:                        SourceDefault,
		fieldCompactTruthMaxBytes:                    SourceDefault,
		fieldCompactTruthMaxEntities:                 SourceDefault,
		fieldCompactMandatoryMaxEntities:             SourceDefault,
		fieldCompactFactMaxEntities:                  SourceDefault,
		fieldCompactVerifiedChangeRetentionTurns:     SourceDefault,
		fieldCompactFailureMaxEntities:               SourceDefault,
		fieldCompactHandleMaxEntities:                SourceDefault,
		fieldCompactOmissionSampleMaxEntities:        SourceDefault,
		fieldCompactSemanticNarrativeMaxInputTokens:  SourceDefault,
		fieldCompactSemanticNarrativeMaxOutputTokens: SourceDefault,
		fieldCompactSemanticNarrativeMaxItems:        SourceDefault,
		fieldCompactSemanticNarrativeItemMaxBytes:    SourceDefault,
		fieldCompactSemanticNarrativeTimeout:         SourceDefault,
		fieldCompactSemanticNarrativeRetryLimit:      SourceDefault,
		fieldCompactOwnerDeltaMaxSegments:            SourceDefault,
		fieldCompactOwnerDeltaMaxBytes:               SourceDefault,

		fieldLogLevel:                   SourceDefault,
		fieldCredentialKind:             SourceDefault,
		fieldCredentialName:             SourceDefault,
		fieldProvider:                   SourceDefault,
		fieldModel:                      SourceDefault,
		fieldProtocol:                   SourceDefault,
		fieldMode:                       SourceDefault,
		fieldWorkspace:                  SourceDefault,
		fieldTools:                      SourceDefault,
		fieldMaxOutputTokens:            SourceDefault,
		fieldMaxSteps:                   SourceDefault,
		fieldImplementNoProgressSamples: SourceDefault,
		fieldTimeout:                    SourceDefault,
		fieldLeaseTimeout:               SourceDefault,
		fieldApprovalTimeout:            SourceDefault,
		fieldConnectionTimeout:          SourceDefault,
		fieldTLSHandshakeTimeout:        SourceDefault,
		fieldResponseHeaderTimeout:      SourceDefault,
		fieldIdleTimeout:                SourceDefault,
		fieldMaxConcurrent:              SourceDefault,
		fieldRateLimit:                  SourceDefault,
		fieldProviderRetryLimit:         SourceDefault,
		fieldRateLimitRetryLimit:        SourceDefault,
		fieldRateLimitWait:              SourceDefault,
		fieldTokensPerMinute:            SourceDefault,
		fieldBudgetTokens:               SourceDefault,
		fieldTurnBudgetTokens:           SourceDefault,
		fieldBudgetUSD:                  SourceDefault,
		fieldReasoning:                  SourceDefault,
		fieldNativeSearch:               SourceDefault,
		fieldVerifyMode:                 SourceDefault,
		fieldVerifyScope:                SourceDefault,
		fieldVerifyOnFailure:            SourceDefault,
		fieldVerifyCommand:              SourceDefault,
		fieldVerifyRepair:               SourceDefault,
		fieldVerifyTimeout:              SourceDefault,

		fieldSubagentDelegation:  SourceDefault,
		fieldSubagentMaxDepth:    SourceDefault,
		fieldSubagentMaxParallel: SourceDefault,
		fieldSubagentMaxResident: SourceDefault,
		fieldSubagentMaxTotal:    SourceDefault,
		fieldSubagentMaxSteps:    SourceDefault,
		fieldSubagentMaxTokens:   SourceDefault,
		fieldSubagentMaxCostUSD:  SourceDefault,
		fieldSubagentWallTime:    SourceDefault,
		fieldSubagentWorkspace:   SourceDefault,

		fieldJournalDurable:        SourceDefault,
		fieldJournalRecoverOnStart: SourceDefault,

		fieldEnvironmentContract:       SourceDefault,
		fieldEnvironmentProfile:        SourceDefault,
		fieldEnvironmentSharedUserTemp: SourceDefault,
		fieldEnvironmentSource:         SourceDefault,
		fieldEnvironmentResources:      SourceDefault,
		fieldEnvironmentAuthServices:   SourceDefault,

		fieldVisionEnabled:    SourceDefault,
		fieldVisionProvider:   SourceDefault,
		fieldVisionModel:      SourceDefault,
		fieldWebSearchBackend: SourceDefault,
	}
}
