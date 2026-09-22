package config

import (
	"fmt"
	"strconv"
	"time"
)

func applyEnvironment(lookup func(string) (string, bool), config *Config, provenance map[string]Source) error {
	integerFields := []struct {
		env    string
		field  string
		target *int
	}{
		{"QCODE_RUNTIME_OPERATION_BUFFER", fieldOperationBuffer, &config.Runtime.OperationBuffer},
		{"QCODE_RUNTIME_EVENT_HISTORY", fieldEventHistory, &config.Runtime.EventHistory},
		{"QCODE_RUNTIME_SUBSCRIBER_BUFFER", fieldSubscriberBuffer, &config.Runtime.SubscriberBuffer},
	}
	for _, item := range integerFields {
		value, exists := lookup(item.env)
		if !exists {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return &FieldError{Field: item.field, Source: SourceEnv, Reason: fmt.Sprintf("%s must be an integer", item.env)}
		}
		*item.target = parsed
		provenance[item.field] = SourceEnv
	}
	applyEnvString(lookup, "QCODE_LOG_LEVEL", fieldLogLevel, &config.Telemetry.LogLevel, provenance)
	applyEnvString(lookup, "QCODE_CREDENTIAL_KIND", fieldCredentialKind, &config.Credential.Kind, provenance)
	applyEnvString(lookup, "QCODE_CREDENTIAL_NAME", fieldCredentialName, &config.Credential.Name, provenance)
	applyEnvString(lookup, "QCODE_STATE_DATA_DIR", fieldStateDataDir, &config.State.DataDir, provenance)
	if err := applyEnvDuration(lookup, "QCODE_STATE_BUSY_TIMEOUT", fieldStateBusyTimeout, &config.State.BusyTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(lookup, "QCODE_STATE_EVENT_RETENTION", fieldStateRetention, &config.State.EventRetention, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_STATE_DELETED_EVENT_RETENTION", fieldDeletedEventRetention, &config.State.DeletedEventRetention, provenance); err != nil {
		return err
	}
	if err := applyEnvBool(lookup, "QCODE_STATE_ARCHIVE_DELETED_EVENTS", fieldArchiveDeletedEvents, &config.State.ArchiveDeletedEvents, provenance); err != nil {
		return err
	}
	if err := applyEnvBool(lookup, "QCODE_MEMORY_ENABLED", fieldMemoryEnabled, &config.Memory.Enabled, provenance); err != nil {
		return err
	}
	applyEnvString(lookup, "QCODE_MEMORY_PATH", fieldMemoryPath, &config.Memory.Path, provenance)
	if err := applyEnvInt(lookup, "QCODE_MEMORY_MAX_CANDIDATES", fieldMemoryMaxCandidates, &config.Memory.MaxCandidates, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(lookup, "QCODE_MEMORY_MAX_PROMPT_BYTES", fieldMemoryMaxPromptBytes, &config.Memory.MaxPromptBytes, provenance); err != nil {
		return err
	}
	if err := applyEnvBool(lookup, "QCODE_MEMORY_SEMANTIC_RERANK", fieldMemorySemanticRerank, &config.Memory.SemanticRerank, provenance); err != nil {
		return err
	}
	index := &config.Context.Index
	if err := applyEnvBool(lookup, "QCODE_INDEX_ENABLED", fieldIndexEnabled, &index.Enabled, provenance); err != nil {
		return err
	}
	if err := applyEnvInt64(
		lookup, "QCODE_INDEX_MAX_FILE_BYTES", fieldIndexMaxBytes, &index.MaxFileBytes, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_INDEX_MAX_FILES", fieldIndexMaxFiles, &index.MaxFiles, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt64(
		lookup, "QCODE_INDEX_SIGNATURE_MAX_BYTES", fieldIndexSignatureMax,
		&index.SignatureMaxBytes, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt64(
		lookup, "QCODE_INDEX_DOCSTRING_MAX_BYTES", fieldIndexDocstringMax,
		&index.DocstringMaxBytes, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_INDEX_REFERENCE_MAX_COUNT", fieldIndexReferenceMax,
		&index.ReferenceMaxCount, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvFloat64(
		lookup, "QCODE_INDEX_RANK_DAMPING_FACTOR", fieldIndexRankDamping,
		&index.RankDamping, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_INDEX_RANK_ITERATION_LIMIT", fieldIndexRankIterations,
		&index.RankIterations, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvFloat64(
		lookup, "QCODE_INDEX_RANK_CONVERGENCE_THRESHOLD", fieldIndexRankConvergence,
		&index.RankConvergence, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_INDEX_IMPACT_MAX_DEPTH", fieldIndexImpactDepth,
		&index.ImpactMaxDepth, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_INDEX_IMPACT_MAX_RESULTS", fieldIndexImpactResults,
		&index.ImpactMaxResults, provenance,
	); err != nil {
		return err
	}
	lspConfig := &config.Context.LSP
	if err := applyEnvBool(
		lookup, "QCODE_LSP_RESIDENT_ENABLED", fieldLSPResidentEnabled,
		&lspConfig.ResidentEnabled, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvDuration(
		lookup, "QCODE_LSP_IDLE_TIMEOUT", fieldLSPIdleTimeout,
		&lspConfig.IdleTimeout, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_LSP_MAX_SERVERS", fieldLSPMaxServers,
		&lspConfig.MaxServers, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_LSP_CACHE_CAPACITY", fieldLSPCacheCapacity,
		&lspConfig.CacheCapacity, provenance,
	); err != nil {
		return err
	}
	repoMap := &config.Context.RepoMap
	if err := applyEnvBool(
		lookup, "QCODE_REPO_MAP_ENABLED", fieldRepoMapEnabled, &repoMap.Enabled, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_REPO_MAP_MAX_BYTES", fieldRepoMapMaxBytes, &repoMap.MaxBytes, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_REPO_MAP_MAX_DIRECTORIES", fieldRepoMapMaxDirectories,
		&repoMap.MaxDirectories, provenance,
	); err != nil {
		return err
	}
	workingSet := &config.Context.WorkingSet
	if err := applyEnvBool(
		lookup, "QCODE_WORKING_SET_ENABLED", fieldWorkingSetEnabled, &workingSet.Enabled, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_WORKING_SET_MAX_ENTRIES", fieldWorkingSetMaxEntries,
		&workingSet.MaxEntries, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_WORKING_SET_MAX_BYTES", fieldWorkingSetMaxBytes,
		&workingSet.MaxBytes, provenance,
	); err != nil {
		return err
	}
	evidence := &config.Context.Evidence
	if err := applyEnvBool(
		lookup, "QCODE_EVIDENCE_ENABLED", fieldEvidenceEnabled, &evidence.Enabled, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_EVIDENCE_MAX_ENTRIES", fieldEvidenceMaxEntries,
		&evidence.MaxEntries, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_EVIDENCE_MAX_BYTES", fieldEvidenceMaxBytes,
		&evidence.MaxBytes, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvBool(
		lookup, "QCODE_CODING_POLICY_ENABLED", fieldCodingPolicyEnabled,
		&config.Context.CodingPolicy.Enabled, provenance,
	); err != nil {
		return err
	}
	compaction := &config.Context.Compact
	if err := applyEnvInt(
		lookup, "QCODE_COMPACT_PREPARE_TOKENS", fieldCompactPrepareTokens,
		&compaction.PrepareTokens, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_COMPACT_AUTO_TOKENS", fieldCompactAutoTokens,
		&compaction.AutoCompactTokens, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_COMPACT_EMERGENCY_TOKENS", fieldCompactEmergencyTokens,
		&compaction.EmergencyTokens, provenance,
	); err != nil {
		return err
	}
	applyEnvString(
		lookup, "QCODE_COMPACT_SCOPE", fieldCompactScope,
		&compaction.Scope, provenance,
	)
	if err := applyEnvInt(
		lookup, "QCODE_COMPACT_SUMMARY_MAX_BYTES", fieldCompactSummaryMax,
		&compaction.SummaryMaxBytes, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_COMPACT_MAX_DIGEST_ENTRIES", fieldCompactMaxDigest,
		&compaction.MaxDigestEntries, provenance,
	); err != nil {
		return err
	}
	for _, value := range []struct {
		env    string
		field  string
		target *int
	}{
		{"QCODE_COMPACT_TRUTH_MAX_BYTES", fieldCompactTruthMaxBytes, &compaction.TruthMaxBytes},
		{"QCODE_COMPACT_TRUTH_MAX_ENTITIES", fieldCompactTruthMaxEntities, &compaction.TruthMaxEntities},
		{"QCODE_COMPACT_MANDATORY_MAX_ENTITIES", fieldCompactMandatoryMaxEntities, &compaction.MandatoryMaxEntities},
		{"QCODE_COMPACT_FACT_MAX_ENTITIES", fieldCompactFactMaxEntities, &compaction.FactMaxEntities},
		{"QCODE_COMPACT_VERIFIED_CHANGE_RETENTION_TURNS", fieldCompactVerifiedChangeRetentionTurns, &compaction.VerifiedChangeRetentionTurns},
		{"QCODE_COMPACT_FAILURE_MAX_ENTITIES", fieldCompactFailureMaxEntities, &compaction.FailureMaxEntities},
		{"QCODE_COMPACT_HANDLE_MAX_ENTITIES", fieldCompactHandleMaxEntities, &compaction.HandleMaxEntities},
		{"QCODE_COMPACT_OMISSION_SAMPLE_MAX_ENTITIES", fieldCompactOmissionSampleMaxEntities, &compaction.OmissionSampleMaxEntities},
		{"QCODE_VIEW_RECENT_TAIL_TURNS", fieldViewRecentTailTurns, &config.Context.View.RecentTailTurns},
		{"QCODE_VIEW_KEEP_RECENT_TOOL_RESULTS", fieldViewKeepRecentToolResults, &config.Context.View.KeepRecentToolResults},
		{"QCODE_VIEW_HISTORY_TOKEN_CEILING", fieldViewHistoryTokenCeiling, &config.Context.View.HistoryTokenCeiling},
		{"QCODE_VIEW_CHECKPOINT_MAX_BYTES", fieldViewCheckpointMaxBytes, &config.Context.View.CheckpointMaxBytes},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_MAX_INPUT_TOKENS", fieldCompactSemanticNarrativeMaxInputTokens, &compaction.SemanticNarrativeMaxInputTokens},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_MAX_OUTPUT_TOKENS", fieldCompactSemanticNarrativeMaxOutputTokens, &compaction.SemanticNarrativeMaxOutputTokens},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_MAX_ITEMS", fieldCompactSemanticNarrativeMaxItems, &compaction.SemanticNarrativeMaxItems},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_ITEM_MAX_BYTES", fieldCompactSemanticNarrativeItemMaxBytes, &compaction.SemanticNarrativeItemMaxBytes},
		{"QCODE_COMPACT_SEMANTIC_NARRATIVE_RETRY_LIMIT", fieldCompactSemanticNarrativeRetryLimit, &compaction.SemanticNarrativeRetryLimit},
		{"QCODE_COMPACT_OWNER_DELTA_MAX_SEGMENTS", fieldCompactOwnerDeltaMaxSegments, &compaction.OwnerDeltaMaxSegments},
		{"QCODE_COMPACT_OWNER_DELTA_MAX_BYTES", fieldCompactOwnerDeltaMaxBytes, &compaction.OwnerDeltaMaxBytes},
	} {
		if err := applyEnvInt(lookup, value.env, value.field, value.target, provenance); err != nil {
			return err
		}
	}
	applyEnvString(
		lookup,
		"QCODE_VIEW_DIGEST",
		fieldViewDigest,
		&config.Context.View.Digest,
		provenance,
	)
	applyEnvString(
		lookup,
		"QCODE_VIEW_NARRATIVE_MODE",
		fieldViewNarrativeMode,
		&config.Context.View.NarrativeMode,
		provenance,
	)
	if err := applyEnvDuration(
		lookup,
		"QCODE_COMPACT_SEMANTIC_NARRATIVE_TIMEOUT",
		fieldCompactSemanticNarrativeTimeout,
		&compaction.SemanticNarrativeTimeout,
		provenance,
	); err != nil {
		return err
	}
	execution := &config.Execution
	applyEnvString(lookup, "QCODE_PROVIDER", fieldProvider, &execution.Provider, provenance)
	applyEnvString(lookup, "QCODE_MODEL", fieldModel, &execution.Model, provenance)
	applyEnvString(lookup, "QCODE_PROTOCOL", fieldProtocol, &execution.Protocol, provenance)
	applyEnvString(lookup, "QCODE_MODE", fieldMode, &execution.Mode, provenance)
	applyEnvString(lookup, "QCODE_WORKSPACE", fieldWorkspace, &execution.Workspace, provenance)
	if err := applyEnvBool(lookup, "QCODE_TOOLS", fieldTools, &execution.Tools, provenance); err != nil {
		return err
	}
	if err := applyEnvUint64(lookup, "QCODE_MAX_OUTPUT_TOKENS", fieldMaxOutputTokens, &execution.MaxOutputTokens, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(lookup, "QCODE_MAX_STEPS", fieldMaxSteps, &execution.MaxSteps, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup,
		"QCODE_IMPLEMENT_NO_PROGRESS_SAMPLES",
		fieldImplementNoProgressSamples,
		&execution.ImplementNoProgressSamples,
		provenance,
	); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_TIMEOUT", fieldTimeout, &execution.Timeout, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_LEASE_TIMEOUT", fieldLeaseTimeout, &execution.LeaseTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_APPROVAL_TIMEOUT", fieldApprovalTimeout, &execution.ApprovalTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_CONNECTION_TIMEOUT", fieldConnectionTimeout, &execution.ConnectionTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_TLS_HANDSHAKE_TIMEOUT", fieldTLSHandshakeTimeout, &execution.TLSHandshakeTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_RESPONSE_HEADER_TIMEOUT", fieldResponseHeaderTimeout, &execution.ResponseHeaderTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_IDLE_TIMEOUT", fieldIdleTimeout, &execution.IdleTimeout, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(lookup, "QCODE_MAX_CONCURRENT", fieldMaxConcurrent, &execution.MaxConcurrent, provenance); err != nil {
		return err
	}
	if err := applyEnvFloat64(lookup, "QCODE_RATE_LIMIT", fieldRateLimit, &execution.RateLimit, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(lookup, "QCODE_PROVIDER_RETRY_LIMIT", fieldProviderRetryLimit, &execution.ProviderRetryLimit, provenance); err != nil {
		return err
	}
	if err := applyEnvInt(lookup, "QCODE_RATE_LIMIT_RETRY_LIMIT", fieldRateLimitRetryLimit, &execution.RateLimitRetryLimit, provenance); err != nil {
		return err
	}
	if err := applyEnvDuration(lookup, "QCODE_RATE_LIMIT_WAIT", fieldRateLimitWait, &execution.RateLimitWait, provenance); err != nil {
		return err
	}
	if err := applyEnvUint64(lookup, "QCODE_TOKENS_PER_MINUTE", fieldTokensPerMinute, &execution.TokensPerMinute, provenance); err != nil {
		return err
	}
	if err := applyEnvUint64(lookup, "QCODE_BUDGET_TOKENS", fieldBudgetTokens, &execution.BudgetTokens, provenance); err != nil {
		return err
	}
	if err := applyEnvUint64(lookup, "QCODE_TURN_BUDGET_TOKENS", fieldTurnBudgetTokens, &execution.TurnBudgetTokens, provenance); err != nil {
		return err
	}
	if err := applyEnvFloat64(lookup, "QCODE_BUDGET_USD", fieldBudgetUSD, &execution.BudgetUSD, provenance); err != nil {
		return err
	}
	applyEnvString(lookup, "QCODE_REASONING_EFFORT", fieldReasoning, &execution.ReasoningEffort, provenance)
	if err := applyEnvBool(lookup, "QCODE_NATIVE_SEARCH", fieldNativeSearch, &execution.NativeSearch, provenance); err != nil {
		return err
	}
	applyEnvString(
		lookup,
		"QCODE_SUBAGENT_DELEGATION",
		fieldSubagentDelegation,
		&execution.Subagent.Delegation,
		provenance,
	)
	if err := applyEnvInt(
		lookup, "QCODE_SUBAGENT_MAX_DEPTH",
		fieldSubagentMaxDepth, &execution.Subagent.MaxDepth, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_SUBAGENT_MAX_PARALLEL",
		fieldSubagentMaxParallel, &execution.Subagent.MaxParallel, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_SUBAGENT_MAX_RESIDENT",
		fieldSubagentMaxResident, &execution.Subagent.MaxResident, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_SUBAGENT_MAX_TOTAL",
		fieldSubagentMaxTotal, &execution.Subagent.MaxTotal, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvInt(
		lookup, "QCODE_SUBAGENT_MAX_STEPS",
		fieldSubagentMaxSteps, &execution.Subagent.MaxSteps, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvUint64(
		lookup, "QCODE_SUBAGENT_MAX_TOKENS",
		fieldSubagentMaxTokens, &execution.Subagent.MaxTokens, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvFloat64(
		lookup, "QCODE_SUBAGENT_MAX_COST_USD",
		fieldSubagentMaxCostUSD, &execution.Subagent.MaxCostUSD, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvDuration(
		lookup, "QCODE_SUBAGENT_WALL_TIME",
		fieldSubagentWallTime, &execution.Subagent.WallTime, provenance,
	); err != nil {
		return err
	}
	applyEnvString(
		lookup, "QCODE_SUBAGENT_WORKSPACE",
		fieldSubagentWorkspace, &execution.Subagent.Workspace, provenance,
	)
	verify := &execution.Verify
	applyEnvString(lookup, "QCODE_VERIFY_MODE", fieldVerifyMode, &verify.Mode, provenance)
	applyEnvString(lookup, "QCODE_VERIFY_SCOPE", fieldVerifyScope, &verify.Scope, provenance)
	applyEnvString(lookup, "QCODE_VERIFY_ON_FAILURE", fieldVerifyOnFailure, &verify.OnFailure, provenance)
	applyEnvString(lookup, "QCODE_VERIFY_COMMAND", fieldVerifyCommand, &verify.Command, provenance)
	if err := applyEnvInt(
		lookup, "QCODE_VERIFY_MAX_REPAIR_STEPS", fieldVerifyRepair, &verify.MaxRepairSteps, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvDuration(
		lookup, "QCODE_VERIFY_TIMEOUT", fieldVerifyTimeout, &verify.Timeout, provenance,
	); err != nil {
		return err
	}
	if err := applyEnvBool(lookup, "QCODE_VISION_ENABLED", fieldVisionEnabled, &config.Vision.Enabled, provenance); err != nil {
		return err
	}
	applyEnvString(lookup, "QCODE_VISION_PROVIDER", fieldVisionProvider, &config.Vision.Provider, provenance)
	applyEnvString(lookup, "QCODE_VISION_MODEL", fieldVisionModel, &config.Vision.Model, provenance)
	applyEnvString(lookup, "QCODE_WEB_SEARCH_BACKEND", fieldWebSearchBackend, &config.Web.SearchBackend, provenance)
	applyEnvString(
		lookup, "QCODE_ENVIRONMENT_CONTRACT",
		fieldEnvironmentContract, &execution.Environment.Contract, provenance,
	)
	applyEnvString(
		lookup, "QCODE_ENVIRONMENT_PROFILE",
		fieldEnvironmentProfile, &execution.Environment.Profile, provenance,
	)
	if err := applyEnvBool(
		lookup, "QCODE_ENVIRONMENT_SHARED_USER_TEMP",
		fieldEnvironmentSharedUserTemp, &execution.Environment.SharedUserTemp, provenance,
	); err != nil {
		return err
	}
	applyEnvString(
		lookup, "QCODE_ENVIRONMENT_SOURCE",
		fieldEnvironmentSource, &execution.Environment.Source, provenance,
	)
	return nil
}

func applyEnvString(lookup func(string) (string, bool), env, field string, target *string, provenance map[string]Source) {
	if value, exists := lookup(env); exists {
		*target = value
		provenance[field] = SourceEnv
	}
}

func applyEnvInt(lookup func(string) (string, bool), env, field string, target *int, provenance map[string]Source) error {
	value, exists := lookup(env)
	if !exists {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return &FieldError{Field: field, Source: SourceEnv, Reason: env + " must be an integer"}
	}
	*target = parsed
	provenance[field] = SourceEnv
	return nil
}

func applyEnvInt64(lookup func(string) (string, bool), env, field string, target *int64, provenance map[string]Source) error {
	value, exists := lookup(env)
	if !exists {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return &FieldError{Field: field, Source: SourceEnv, Reason: env + " must be an integer"}
	}
	*target = parsed
	provenance[field] = SourceEnv
	return nil
}

func applyEnvUint64(lookup func(string) (string, bool), env, field string, target *uint64, provenance map[string]Source) error {
	value, exists := lookup(env)
	if !exists {
		return nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return &FieldError{Field: field, Source: SourceEnv, Reason: env + " must be an unsigned integer"}
	}
	*target = parsed
	provenance[field] = SourceEnv
	return nil
}

func applyEnvFloat64(lookup func(string) (string, bool), env, field string, target *float64, provenance map[string]Source) error {
	value, exists := lookup(env)
	if !exists {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return &FieldError{Field: field, Source: SourceEnv, Reason: env + " must be a number"}
	}
	*target = parsed
	provenance[field] = SourceEnv
	return nil
}

func applyEnvBool(lookup func(string) (string, bool), env, field string, target *bool, provenance map[string]Source) error {
	value, exists := lookup(env)
	if !exists {
		return nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return &FieldError{Field: field, Source: SourceEnv, Reason: env + " must be a boolean"}
	}
	*target = parsed
	provenance[field] = SourceEnv
	return nil
}

func applyEnvDuration(lookup func(string) (string, bool), env, field string, target *time.Duration, provenance map[string]Source) error {
	value, exists := lookup(env)
	if !exists {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return &FieldError{Field: field, Source: SourceEnv, Reason: env + " must be a duration"}
	}
	*target = parsed
	provenance[field] = SourceEnv
	return nil
}
