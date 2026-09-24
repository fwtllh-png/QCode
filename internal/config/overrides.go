package config

func applyOverrides(overrides Overrides, config *Config, provenance map[string]Source) {
	applyInt(overrides.OperationBuffer, &config.Runtime.OperationBuffer, fieldOperationBuffer, SourceStartup, provenance)
	applyInt(overrides.EventHistory, &config.Runtime.EventHistory, fieldEventHistory, SourceStartup, provenance)
	applyInt(overrides.SubscriberBuffer, &config.Runtime.SubscriberBuffer, fieldSubscriberBuffer, SourceStartup, provenance)
	applyString(overrides.StateDataDir, &config.State.DataDir, fieldStateDataDir, SourceStartup, provenance)
	applyDuration(overrides.StateBusyTimeout, &config.State.BusyTimeout, fieldStateBusyTimeout, SourceStartup, provenance)
	applyInt(overrides.StateRetention, &config.State.EventRetention, fieldStateRetention, SourceStartup, provenance)
	applyBool(overrides.MemoryEnabled, &config.Memory.Enabled, fieldMemoryEnabled, SourceStartup, provenance)
	applyString(overrides.MemoryPath, &config.Memory.Path, fieldMemoryPath, SourceStartup, provenance)
	applyInt(overrides.MemoryMaxCandidates, &config.Memory.MaxCandidates, fieldMemoryMaxCandidates, SourceStartup, provenance)
	applyInt(overrides.MemoryMaxPromptBytes, &config.Memory.MaxPromptBytes, fieldMemoryMaxPromptBytes, SourceStartup, provenance)
	applyBool(overrides.MemorySemanticRerank, &config.Memory.SemanticRerank, fieldMemorySemanticRerank, SourceStartup, provenance)
	index := &config.Context.Index
	applyBool(overrides.IndexEnabled, &index.Enabled, fieldIndexEnabled, SourceStartup, provenance)
	applyInt64(overrides.IndexMaxBytes, &index.MaxFileBytes, fieldIndexMaxBytes, SourceStartup, provenance)
	applyInt(overrides.IndexMaxFiles, &index.MaxFiles, fieldIndexMaxFiles, SourceStartup, provenance)
	applyInt64(overrides.IndexSignatureMax, &index.SignatureMaxBytes, fieldIndexSignatureMax, SourceStartup, provenance)
	applyInt64(overrides.IndexDocstringMax, &index.DocstringMaxBytes, fieldIndexDocstringMax, SourceStartup, provenance)
	applyInt(overrides.IndexReferenceMax, &index.ReferenceMaxCount, fieldIndexReferenceMax, SourceStartup, provenance)
	applyFloat64(overrides.IndexRankDamping, &index.RankDamping, fieldIndexRankDamping, SourceStartup, provenance)
	applyInt(overrides.IndexRankIterations, &index.RankIterations, fieldIndexRankIterations, SourceStartup, provenance)
	applyFloat64(overrides.IndexRankConvergence, &index.RankConvergence, fieldIndexRankConvergence, SourceStartup, provenance)
	applyInt(overrides.IndexImpactDepth, &index.ImpactMaxDepth, fieldIndexImpactDepth, SourceStartup, provenance)
	applyInt(overrides.IndexImpactResults, &index.ImpactMaxResults, fieldIndexImpactResults, SourceStartup, provenance)
	applyBool(overrides.LSPResidentEnabled, &config.Context.LSP.ResidentEnabled, fieldLSPResidentEnabled, SourceStartup, provenance)
	applyDuration(overrides.LSPIdleTimeout, &config.Context.LSP.IdleTimeout, fieldLSPIdleTimeout, SourceStartup, provenance)
	applyInt(overrides.LSPMaxServers, &config.Context.LSP.MaxServers, fieldLSPMaxServers, SourceStartup, provenance)
	applyInt(overrides.LSPCacheCapacity, &config.Context.LSP.CacheCapacity, fieldLSPCacheCapacity, SourceStartup, provenance)
	repoMap := &config.Context.RepoMap
	applyBool(overrides.RepoMapEnabled, &repoMap.Enabled, fieldRepoMapEnabled, SourceStartup, provenance)
	applyInt(overrides.RepoMapMaxBytes, &repoMap.MaxBytes, fieldRepoMapMaxBytes, SourceStartup, provenance)
	applyInt(
		overrides.RepoMapMaxDirectories, &repoMap.MaxDirectories,
		fieldRepoMapMaxDirectories, SourceStartup, provenance,
	)
	workingSet := &config.Context.WorkingSet
	applyBool(
		overrides.WorkingSetEnabled, &workingSet.Enabled,
		fieldWorkingSetEnabled, SourceStartup, provenance,
	)
	applyInt(
		overrides.WorkingSetMaxEntries, &workingSet.MaxEntries,
		fieldWorkingSetMaxEntries, SourceStartup, provenance,
	)
	applyInt(
		overrides.WorkingSetMaxBytes, &workingSet.MaxBytes,
		fieldWorkingSetMaxBytes, SourceStartup, provenance,
	)
	evidence := &config.Context.Evidence
	applyBool(overrides.EvidenceEnabled, &evidence.Enabled, fieldEvidenceEnabled, SourceStartup, provenance)
	applyInt(
		overrides.EvidenceMaxEntries, &evidence.MaxEntries,
		fieldEvidenceMaxEntries, SourceStartup, provenance,
	)
	applyInt(overrides.EvidenceMaxBytes, &evidence.MaxBytes, fieldEvidenceMaxBytes, SourceStartup, provenance)
	applyBool(
		overrides.CodingPolicyEnabled, &config.Context.CodingPolicy.Enabled,
		fieldCodingPolicyEnabled, SourceStartup, provenance,
	)
	view := &config.Context.View
	applyInt(
		overrides.ViewRecentTailTurns, &view.RecentTailTurns,
		fieldViewRecentTailTurns, SourceStartup, provenance,
	)
	applyInt(
		overrides.ViewKeepRecentToolResults, &view.KeepRecentToolResults,
		fieldViewKeepRecentToolResults, SourceStartup, provenance,
	)
	applyInt(
		overrides.ViewHistoryTokenCeiling, &view.HistoryTokenCeiling,
		fieldViewHistoryTokenCeiling, SourceStartup, provenance,
	)
	applyString(
		overrides.ViewDigest, &view.Digest,
		fieldViewDigest, SourceStartup, provenance,
	)
	applyString(
		overrides.ViewNarrativeMode, &view.NarrativeMode,
		fieldViewNarrativeMode, SourceStartup, provenance,
	)
	applyInt(
		overrides.ViewCheckpointMaxBytes, &view.CheckpointMaxBytes,
		fieldViewCheckpointMaxBytes, SourceStartup, provenance,
	)
	compaction := &config.Context.Compact
	applyInt(
		overrides.CompactPrepareTokens, &compaction.PrepareTokens,
		fieldCompactPrepareTokens, SourceStartup, provenance,
	)
	applyInt(
		overrides.CompactAutoTokens, &compaction.AutoCompactTokens,
		fieldCompactAutoTokens, SourceStartup, provenance,
	)
	applyInt(
		overrides.CompactEmergencyTokens, &compaction.EmergencyTokens,
		fieldCompactEmergencyTokens, SourceStartup, provenance,
	)
	applyString(
		overrides.CompactScope, &compaction.Scope,
		fieldCompactScope, SourceStartup, provenance,
	)
	applyInt(
		overrides.CompactSummaryMax, &compaction.SummaryMaxBytes,
		fieldCompactSummaryMax, SourceStartup, provenance,
	)
	applyInt(
		overrides.CompactMaxDigest, &compaction.MaxDigestEntries,
		fieldCompactMaxDigest, SourceStartup, provenance,
	)
	for _, value := range []struct {
		source *int
		target *int
		field  string
	}{
		{overrides.CompactTruthMaxBytes, &compaction.TruthMaxBytes, fieldCompactTruthMaxBytes},
		{overrides.CompactTruthMaxEntities, &compaction.TruthMaxEntities, fieldCompactTruthMaxEntities},
		{overrides.CompactMandatoryMaxEntities, &compaction.MandatoryMaxEntities, fieldCompactMandatoryMaxEntities},
		{overrides.CompactFactMaxEntities, &compaction.FactMaxEntities, fieldCompactFactMaxEntities},
		{overrides.CompactVerifiedChangeRetentionTurns, &compaction.VerifiedChangeRetentionTurns, fieldCompactVerifiedChangeRetentionTurns},
		{overrides.CompactFailureMaxEntities, &compaction.FailureMaxEntities, fieldCompactFailureMaxEntities},
		{overrides.CompactHandleMaxEntities, &compaction.HandleMaxEntities, fieldCompactHandleMaxEntities},
		{overrides.CompactOmissionSampleMaxEntities, &compaction.OmissionSampleMaxEntities, fieldCompactOmissionSampleMaxEntities},
		{overrides.CompactSemanticNarrativeMaxInputTokens, &compaction.SemanticNarrativeMaxInputTokens, fieldCompactSemanticNarrativeMaxInputTokens},
		{overrides.CompactSemanticNarrativeMaxOutputTokens, &compaction.SemanticNarrativeMaxOutputTokens, fieldCompactSemanticNarrativeMaxOutputTokens},
		{overrides.CompactSemanticNarrativeMaxItems, &compaction.SemanticNarrativeMaxItems, fieldCompactSemanticNarrativeMaxItems},
		{overrides.CompactSemanticNarrativeItemMaxBytes, &compaction.SemanticNarrativeItemMaxBytes, fieldCompactSemanticNarrativeItemMaxBytes},
		{overrides.CompactSemanticNarrativeRetryLimit, &compaction.SemanticNarrativeRetryLimit, fieldCompactSemanticNarrativeRetryLimit},
		{overrides.CompactOwnerDeltaMaxSegments, &compaction.OwnerDeltaMaxSegments, fieldCompactOwnerDeltaMaxSegments},
		{overrides.CompactOwnerDeltaMaxBytes, &compaction.OwnerDeltaMaxBytes, fieldCompactOwnerDeltaMaxBytes},
	} {
		applyInt(value.source, value.target, value.field, SourceStartup, provenance)
	}
	applyDuration(
		overrides.CompactSemanticNarrativeTimeout,
		&compaction.SemanticNarrativeTimeout,
		fieldCompactSemanticNarrativeTimeout,
		SourceStartup,
		provenance,
	)
	applyString(overrides.LogLevel, &config.Telemetry.LogLevel, fieldLogLevel, SourceStartup, provenance)
	applyString(overrides.CredentialKind, &config.Credential.Kind, fieldCredentialKind, SourceStartup, provenance)
	applyString(overrides.CredentialName, &config.Credential.Name, fieldCredentialName, SourceStartup, provenance)
	execution := &config.Execution
	applyString(overrides.Provider, &execution.Provider, fieldProvider, SourceStartup, provenance)
	applyString(overrides.Model, &execution.Model, fieldModel, SourceStartup, provenance)
	applyString(overrides.Protocol, &execution.Protocol, fieldProtocol, SourceStartup, provenance)
	applyString(overrides.BaseURL, &execution.BaseURL, fieldBaseURL, SourceStartup, provenance)
	applyString(overrides.ModelMetadata, &execution.ModelMetadata, fieldModelMetadata, SourceStartup, provenance)
	applyString(overrides.Mode, &execution.Mode, fieldMode, SourceStartup, provenance)
	applyString(overrides.Workspace, &execution.Workspace, fieldWorkspace, SourceStartup, provenance)
	applyBool(overrides.Tools, &execution.Tools, fieldTools, SourceStartup, provenance)
	applyUint64(overrides.MaxOutputTokens, &execution.MaxOutputTokens, fieldMaxOutputTokens, SourceStartup, provenance)
	applyInt(overrides.MaxSteps, &execution.MaxSteps, fieldMaxSteps, SourceStartup, provenance)
	applyInt(
		overrides.ImplementNoProgressSamples,
		&execution.ImplementNoProgressSamples,
		fieldImplementNoProgressSamples,
		SourceStartup,
		provenance,
	)
	applyDuration(overrides.Timeout, &execution.Timeout, fieldTimeout, SourceStartup, provenance)
	applyDuration(overrides.LeaseTimeout, &execution.LeaseTimeout, fieldLeaseTimeout, SourceStartup, provenance)
	applyDuration(overrides.ApprovalTimeout, &execution.ApprovalTimeout, fieldApprovalTimeout, SourceStartup, provenance)
	applyDuration(overrides.ConnectionTimeout, &execution.ConnectionTimeout, fieldConnectionTimeout, SourceStartup, provenance)
	applyDuration(overrides.TLSHandshakeTimeout, &execution.TLSHandshakeTimeout, fieldTLSHandshakeTimeout, SourceStartup, provenance)
	applyDuration(overrides.ResponseHeaderTimeout, &execution.ResponseHeaderTimeout, fieldResponseHeaderTimeout, SourceStartup, provenance)
	applyDuration(overrides.IdleTimeout, &execution.IdleTimeout, fieldIdleTimeout, SourceStartup, provenance)
	applyInt(overrides.MaxConcurrent, &execution.MaxConcurrent, fieldMaxConcurrent, SourceStartup, provenance)
	applyFloat64(overrides.RateLimit, &execution.RateLimit, fieldRateLimit, SourceStartup, provenance)
	applyInt(overrides.ProviderRetryLimit, &execution.ProviderRetryLimit, fieldProviderRetryLimit, SourceStartup, provenance)
	applyInt(overrides.RateLimitRetryLimit, &execution.RateLimitRetryLimit, fieldRateLimitRetryLimit, SourceStartup, provenance)
	applyDuration(overrides.RateLimitWait, &execution.RateLimitWait, fieldRateLimitWait, SourceStartup, provenance)
	applyUint64(overrides.TokensPerMinute, &execution.TokensPerMinute, fieldTokensPerMinute, SourceStartup, provenance)
	applyUint64(overrides.BudgetTokens, &execution.BudgetTokens, fieldBudgetTokens, SourceStartup, provenance)
	applyUint64(overrides.TurnBudgetTokens, &execution.TurnBudgetTokens, fieldTurnBudgetTokens, SourceStartup, provenance)
	applyFloat64(overrides.BudgetUSD, &execution.BudgetUSD, fieldBudgetUSD, SourceStartup, provenance)
	applyString(overrides.ReasoningEffort, &execution.ReasoningEffort, fieldReasoning, SourceStartup, provenance)
	applyBool(overrides.NativeSearch, &execution.NativeSearch, fieldNativeSearch, SourceStartup, provenance)
	verify := &execution.Verify
	applyString(overrides.VerifyMode, &verify.Mode, fieldVerifyMode, SourceStartup, provenance)
	applyString(overrides.VerifyScope, &verify.Scope, fieldVerifyScope, SourceStartup, provenance)
	applyString(overrides.VerifyOnFailure, &verify.OnFailure, fieldVerifyOnFailure, SourceStartup, provenance)
	applyString(overrides.VerifyCommand, &verify.Command, fieldVerifyCommand, SourceStartup, provenance)
	applyInt(overrides.VerifyRepair, &verify.MaxRepairSteps, fieldVerifyRepair, SourceStartup, provenance)
	applyDuration(overrides.VerifyTimeout, &verify.Timeout, fieldVerifyTimeout, SourceStartup, provenance)
	applyBool(overrides.JournalDurable, &execution.Journal.Durable, fieldJournalDurable, SourceStartup, provenance)
	applyBool(
		overrides.JournalRecoverOnStart,
		&execution.Journal.RecoverOnStart,
		fieldJournalRecoverOnStart,
		SourceStartup,
		provenance,
	)
	child := &execution.Subagent
	applyString(overrides.SubagentDelegation, &child.Delegation, fieldSubagentDelegation, SourceStartup, provenance)
	applyInt(overrides.SubagentMaxDepth, &child.MaxDepth, fieldSubagentMaxDepth, SourceStartup, provenance)
	applyInt(overrides.SubagentMaxParallel, &child.MaxParallel, fieldSubagentMaxParallel, SourceStartup, provenance)
	applyInt(overrides.SubagentMaxResident, &child.MaxResident, fieldSubagentMaxResident, SourceStartup, provenance)
	applyInt(overrides.SubagentMaxTotal, &child.MaxTotal, fieldSubagentMaxTotal, SourceStartup, provenance)
	applyInt(overrides.SubagentMaxSteps, &child.MaxSteps, fieldSubagentMaxSteps, SourceStartup, provenance)
	applyUint64(overrides.SubagentMaxTokens, &child.MaxTokens, fieldSubagentMaxTokens, SourceStartup, provenance)
	applyFloat64(overrides.SubagentMaxCostUSD, &child.MaxCostUSD, fieldSubagentMaxCostUSD, SourceStartup, provenance)
	applyDuration(overrides.SubagentWallTime, &child.WallTime, fieldSubagentWallTime, SourceStartup, provenance)
	applyString(overrides.SubagentWorkspace, &child.Workspace, fieldSubagentWorkspace, SourceStartup, provenance)
	applyBool(overrides.VisionEnabled, &config.Vision.Enabled, fieldVisionEnabled, SourceStartup, provenance)
	applyString(overrides.VisionProvider, &config.Vision.Provider, fieldVisionProvider, SourceStartup, provenance)
	applyString(overrides.VisionModel, &config.Vision.Model, fieldVisionModel, SourceStartup, provenance)
	applyString(overrides.WebSearchBackend, &config.Web.SearchBackend, fieldWebSearchBackend, SourceStartup, provenance)
	applyString(
		overrides.EnvironmentContract, &execution.Environment.Contract,
		fieldEnvironmentContract, SourceStartup, provenance,
	)
	applyString(
		overrides.EnvironmentProfile, &execution.Environment.Profile,
		fieldEnvironmentProfile, SourceStartup, provenance,
	)
	applyBool(
		overrides.EnvironmentSharedUserTemp, &execution.Environment.SharedUserTemp,
		fieldEnvironmentSharedUserTemp, SourceStartup, provenance,
	)
	applyString(
		overrides.EnvironmentSource, &execution.Environment.Source,
		fieldEnvironmentSource, SourceStartup, provenance,
	)
	applyBool(overrides.RouteLock, &config.Route.Lock, fieldRouteLock, SourceStartup, provenance)
}
