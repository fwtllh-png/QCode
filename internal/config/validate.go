package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	envcontract "github.com/fwtllh-png/QCode/internal/environment"
)

var secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./:@-]*$`)
var diagnosticExtensionPattern = regexp.MustCompile(`^\.[A-Za-z0-9][A-Za-z0-9._+-]{0,15}$`)
var diagnosticCommandPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

type FieldError struct {
	Field  string
	Source Source
	Reason string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("invalid config field %s from %s: %s", e.Field, e.Source, e.Reason)
}

func (s Snapshot) Validate() error {
	checkRange := func(field string, value, maximum int) error {
		if value < 1 || value > maximum {
			return fieldError(field, s.Provenance, fmt.Sprintf("must be between 1 and %d", maximum))
		}
		return nil
	}
	if err := checkRange(fieldOperationBuffer, s.Config.Runtime.OperationBuffer, 65536); err != nil {
		return err
	}
	if err := checkRange(fieldEventHistory, s.Config.Runtime.EventHistory, 1_000_000); err != nil {
		return err
	}
	if err := checkRange(fieldSubscriberBuffer, s.Config.Runtime.SubscriberBuffer, 65536); err != nil {
		return err
	}
	if strings.TrimSpace(s.Config.State.DataDir) == "" {
		return fieldError(fieldStateDataDir, s.Provenance, "must not be empty")
	}
	if s.Config.State.BusyTimeout <= 0 || s.Config.State.BusyTimeout > 5*time.Minute {
		return fieldError(fieldStateBusyTimeout, s.Provenance, "must be between 1ns and 5m")
	}
	if err := checkRange(fieldStateRetention, s.Config.State.EventRetention, 100_000_000); err != nil {
		return err
	}
	if s.Config.State.DeletedEventRetention < 0 {
		return fieldError(fieldDeletedEventRetention, s.Provenance, "must be non-negative")
	}
	if strings.TrimSpace(s.Config.Memory.Path) == "" {
		return fieldError(fieldMemoryPath, s.Provenance, "must not be empty")
	}
	if err := checkRange(fieldMemoryMaxCandidates, s.Config.Memory.MaxCandidates, 1024); err != nil {
		return err
	}
	if s.Config.Memory.MaxPromptBytes < 256 ||
		s.Config.Memory.MaxPromptBytes > 100<<10 {
		return fieldError(fieldMemoryMaxPromptBytes, s.Provenance,
			"must be between 256 and 102400")
	}
	if s.Config.Memory.SemanticRerank {
		return fieldError(fieldMemorySemanticRerank, s.Provenance,
			"is not available; use deterministic lexical retrieval")
	}
	if index := s.Config.Context.Index; index.Enabled {

		if index.MaxFileBytes < 1024 || index.MaxFileBytes > 64<<20 {
			return fieldError(fieldIndexMaxBytes, s.Provenance,
				"must be between 1024 and 67108864")
		}
		if err := checkRange(fieldIndexMaxFiles, index.MaxFiles, 1_000_000); err != nil {
			return err
		}
		if index.SignatureMaxBytes < 64 || index.SignatureMaxBytes > 65536 {
			return fieldError(fieldIndexSignatureMax, s.Provenance,
				"must be between 64 and 65536")
		}
		if index.DocstringMaxBytes < 256 || index.DocstringMaxBytes > 1<<20 {
			return fieldError(fieldIndexDocstringMax, s.Provenance,
				"must be between 256 and 1048576")
		}
		if err := checkRange(fieldIndexReferenceMax, index.ReferenceMaxCount, 65536); err != nil {
			return err
		}
		if index.RankDamping <= 0 || index.RankDamping >= 1 {
			return fieldError(fieldIndexRankDamping, s.Provenance,
				"must be between 0 and 1 (exclusive)")
		}
		if err := checkRange(fieldIndexRankIterations, index.RankIterations, 10000); err != nil {
			return err
		}
		if index.RankConvergence <= 0 || index.RankConvergence >= 1 {
			return fieldError(fieldIndexRankConvergence, s.Provenance,
				"must be between 0 and 1 (exclusive)")
		}
		if err := checkRange(fieldIndexImpactDepth, index.ImpactMaxDepth, 16); err != nil {
			return err
		}
		if err := checkRange(fieldIndexImpactResults, index.ImpactMaxResults, 10000); err != nil {
			return err
		}
	}
	if lspConfig := s.Config.Context.LSP; lspConfig.ResidentEnabled {
		if lspConfig.IdleTimeout < time.Second || lspConfig.IdleTimeout > time.Hour {
			return fieldError(fieldLSPIdleTimeout, s.Provenance,
				"must be between 1s and 1h")
		}
		if err := checkRange(fieldLSPMaxServers, lspConfig.MaxServers, 8); err != nil {
			return err
		}
		if err := checkRange(fieldLSPCacheCapacity, lspConfig.CacheCapacity, 4096); err != nil {
			return err
		}
	}
	if repoMap := s.Config.Context.RepoMap; repoMap.Enabled {

		if repoMap.MaxBytes < 256 || repoMap.MaxBytes > 1<<20 {
			return fieldError(fieldRepoMapMaxBytes, s.Provenance,
				"must be between 256 and 1048576")
		}
		if err := checkRange(fieldRepoMapMaxDirectories, repoMap.MaxDirectories, 512); err != nil {
			return err
		}
	}
	if workingSet := s.Config.Context.WorkingSet; workingSet.Enabled {
		if err := checkRange(fieldWorkingSetMaxEntries, workingSet.MaxEntries, 256); err != nil {
			return err
		}
		if workingSet.MaxBytes < 256 || workingSet.MaxBytes > 1<<20 {
			return fieldError(fieldWorkingSetMaxBytes, s.Provenance,
				"must be between 256 and 1048576")
		}
	}
	if evidence := s.Config.Context.Evidence; evidence.Enabled {
		if err := checkRange(fieldEvidenceMaxEntries, evidence.MaxEntries, 256); err != nil {
			return err
		}
		if evidence.MaxBytes < 256 || evidence.MaxBytes > 1<<20 {
			return fieldError(fieldEvidenceMaxBytes, s.Provenance,
				"must be between 256 and 1048576")
		}
	}

	compaction := s.Config.Context.Compact
	for _, threshold := range []struct {
		field string
		value int
	}{
		{fieldCompactPrepareTokens, compaction.PrepareTokens},
		{fieldCompactAutoTokens, compaction.AutoCompactTokens},
		{fieldCompactEmergencyTokens, compaction.EmergencyTokens},
	} {
		if threshold.value != 0 &&
			(threshold.value < 256 || threshold.value > 64<<20) {
			return fieldError(threshold.field, s.Provenance,
				"must be zero or between 256 and 67108864")
		}
	}
	if compaction.PrepareTokens != 0 && compaction.AutoCompactTokens != 0 &&
		compaction.PrepareTokens >= compaction.AutoCompactTokens {
		return fieldError(fieldCompactPrepareTokens, s.Provenance,
			"must be smaller than auto_compact_tokens")
	}
	if compaction.AutoCompactTokens != 0 && compaction.EmergencyTokens != 0 &&
		compaction.AutoCompactTokens >= compaction.EmergencyTokens {
		return fieldError(fieldCompactEmergencyTokens, s.Provenance,
			"must be greater than auto_compact_tokens")
	}
	if compaction.Scope != "total" && compaction.Scope != "body_after_prefix" {
		return fieldError(fieldCompactScope, s.Provenance,
			"must be total or body_after_prefix")
	}

	if compaction.SummaryMaxBytes != 0 &&
		(compaction.SummaryMaxBytes < 256 || compaction.SummaryMaxBytes > 1<<20) {
		return fieldError(fieldCompactSummaryMax, s.Provenance,
			"must be zero or between 256 and 1048576")
	}
	if err := checkRange(fieldCompactMaxDigest, compaction.MaxDigestEntries, 4096); err != nil {
		return err
	}
	if compaction.TruthMaxBytes != 0 && compaction.TruthMaxBytes < 256 ||
		compaction.SummaryMaxBytes != 0 &&
			compaction.TruthMaxBytes != 0 &&
			compaction.TruthMaxBytes >= compaction.SummaryMaxBytes-256 {
		return fieldError(fieldCompactTruthMaxBytes, s.Provenance,
			"must be zero or leave at least 256 bytes inside summary_max_bytes")
	}
	if err := checkRange(fieldCompactTruthMaxEntities, compaction.TruthMaxEntities, 4096); err != nil {
		return err
	}
	for _, quota := range []struct {
		field string
		value int
	}{
		{fieldCompactMandatoryMaxEntities, compaction.MandatoryMaxEntities},
		{fieldCompactFactMaxEntities, compaction.FactMaxEntities},
		{fieldCompactFailureMaxEntities, compaction.FailureMaxEntities},
		{fieldCompactHandleMaxEntities, compaction.HandleMaxEntities},
		{fieldCompactOmissionSampleMaxEntities, compaction.OmissionSampleMaxEntities},
	} {
		if quota.value < 1 || quota.value > compaction.TruthMaxEntities {
			return fieldError(quota.field, s.Provenance,
				"must be between 1 and truth_max_entities")
		}
	}
	if compaction.VerifiedChangeRetentionTurns < 1 {
		return fieldError(fieldCompactVerifiedChangeRetentionTurns, s.Provenance,
			"must be positive")
	}
	view := s.Config.Context.View
	if err := checkRange(fieldViewRecentTailTurns, view.RecentTailTurns, 128); err != nil {
		return err
	}
	if view.KeepRecentToolResults < 0 || view.KeepRecentToolResults > 128 {
		return fieldError(fieldViewKeepRecentToolResults, s.Provenance,
			"must be between 0 and 128")
	}
	if view.HistoryTokenCeiling != 0 &&
		view.HistoryTokenCeiling < 256 ||
		view.HistoryTokenCeiling > 64<<20 {
		return fieldError(fieldViewHistoryTokenCeiling, s.Provenance,
			"must be zero or between 256 and 67108864")
	}
	if view.CheckpointMaxBytes != 0 &&
		(view.CheckpointMaxBytes < 256 || view.CheckpointMaxBytes > 1<<20) {
		return fieldError(fieldViewCheckpointMaxBytes, s.Provenance,
			"must be zero or between 256 and 1048576")
	}
	switch view.Digest {
	case "ledger", "ledger+narrative":
	default:
		return fieldError(fieldViewDigest, s.Provenance,
			"must be ledger or ledger+narrative")
	}
	switch view.NarrativeMode {
	case "off", "post_turn":
	default:
		return fieldError(fieldViewNarrativeMode, s.Provenance,
			"must be off or post_turn")
	}
	if view.Digest == "ledger+narrative" && view.NarrativeMode != "post_turn" {
		return fieldError(fieldViewNarrativeMode, s.Provenance,
			"must be post_turn when digest is ledger+narrative")
	}
	for _, limit := range []struct {
		field   string
		value   int
		maximum int
	}{
		{fieldCompactSemanticNarrativeMaxInputTokens, compaction.SemanticNarrativeMaxInputTokens, 64 << 20},
		{fieldCompactSemanticNarrativeMaxItems, compaction.SemanticNarrativeMaxItems, 1024},
		{fieldCompactSemanticNarrativeItemMaxBytes, compaction.SemanticNarrativeItemMaxBytes, 64 << 10},
		{fieldCompactOwnerDeltaMaxSegments, compaction.OwnerDeltaMaxSegments, 1024},
		{fieldCompactOwnerDeltaMaxBytes, compaction.OwnerDeltaMaxBytes, 16 << 20},
	} {
		if err := checkRange(limit.field, limit.value, limit.maximum); err != nil {
			return err
		}
	}
	if compaction.SemanticNarrativeMaxOutputTokens < 0 ||
		compaction.SemanticNarrativeMaxOutputTokens > 1<<20 {
		return fieldError(fieldCompactSemanticNarrativeMaxOutputTokens, s.Provenance,
			"must be zero or at most 1048576")
	}
	if compaction.SemanticNarrativeTimeout <= 0 ||
		compaction.SemanticNarrativeTimeout > 10*time.Minute {
		return fieldError(fieldCompactSemanticNarrativeTimeout, s.Provenance,
			"must be between 1ns and 10m")
	}
	if compaction.SemanticNarrativeRetryLimit < 0 ||
		compaction.SemanticNarrativeRetryLimit > 10 {
		return fieldError(fieldCompactSemanticNarrativeRetryLimit, s.Provenance,
			"must be between 0 and 10")
	}
	switch s.Config.Telemetry.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fieldError(fieldLogLevel, s.Provenance, "must be debug, info, warn, or error")
	}
	execution := s.Config.Execution
	switch execution.Protocol {
	case "openai_chat", "openai_responses":
	default:
		return fieldError(fieldProtocol, s.Provenance, "unsupported provider protocol")
	}
	if execution.BaseURL != "" {
		parsed, parseErr := url.Parse(strings.TrimSpace(execution.BaseURL))
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fieldError(fieldBaseURL, s.Provenance, "must be an absolute http(s) URL")
		}
	}
	if execution.Mode != "act" {
		return fieldError(fieldMode, s.Provenance, "must be act")
	}
	if execution.Workspace == "" {
		return fieldError(fieldWorkspace, s.Provenance, "must not be empty")
	}
	if execution.MaxSteps < 0 {
		return fieldError(fieldMaxSteps, s.Provenance, "must be non-negative")
	}
	if execution.ImplementNoProgressSamples < 0 {
		return fieldError(
			fieldImplementNoProgressSamples,
			s.Provenance,
			"must be non-negative",
		)
	}
	if execution.Timeout <= 0 {
		return fieldError(fieldTimeout, s.Provenance, "must be positive")
	}
	if execution.LeaseTimeout <= 0 {
		return fieldError(fieldLeaseTimeout, s.Provenance, "must be positive")
	}
	if execution.ApprovalTimeout < 0 {
		return fieldError(fieldApprovalTimeout, s.Provenance, "must be non-negative")
	}
	switch execution.Environment.Contract {
	case EnvironmentContractV1:
	default:
		return fieldError(fieldEnvironmentContract, s.Provenance, "must be v1")
	}
	switch execution.Environment.Profile {
	case EnvironmentProfileIsolated, EnvironmentProfileNative:
	default:
		return fieldError(fieldEnvironmentProfile, s.Provenance, "must be isolated or native")
	}
	if execution.Environment.SharedUserTemp &&
		execution.Environment.Profile != EnvironmentProfileNative {
		return fieldError(
			fieldEnvironmentSharedUserTemp,
			s.Provenance,
			"requires profile native",
		)
	}
	if strings.IndexByte(execution.Environment.Source, 0) >= 0 {
		return fieldError(fieldEnvironmentSource, s.Provenance, "must not contain NUL")
	}
	if err := validateDeclaredEnvironmentResources(execution.Environment, s.Provenance); err != nil {
		return err
	}
	if err := validateDeclaredAuthServices(execution.Environment, s.Provenance); err != nil {
		return err
	}
	for _, value := range []struct {
		field string
		value time.Duration
	}{
		{fieldConnectionTimeout, execution.ConnectionTimeout},
		{fieldTLSHandshakeTimeout, execution.TLSHandshakeTimeout},
		{fieldResponseHeaderTimeout, execution.ResponseHeaderTimeout},
	} {
		if value.value < 0 {
			return fieldError(value.field, s.Provenance, "must be non-negative")
		}
	}
	if execution.IdleTimeout <= 0 {
		return fieldError(fieldIdleTimeout, s.Provenance, "must be positive")
	}
	if execution.MaxConcurrent < 1 {
		return fieldError(fieldMaxConcurrent, s.Provenance, "must be positive")
	}
	if execution.RateLimit < 0 {
		return fieldError(fieldRateLimit, s.Provenance, "must be non-negative")
	}
	if execution.ProviderRetryLimit < 0 {
		return fieldError(
			fieldProviderRetryLimit,
			s.Provenance,
			"must be non-negative",
		)
	}
	if execution.RateLimitRetryLimit < 0 {
		return fieldError(
			fieldRateLimitRetryLimit,
			s.Provenance,
			"must be non-negative",
		)
	}
	if execution.RateLimitWait < 0 {
		return fieldError(
			fieldRateLimitWait,
			s.Provenance,
			"must be non-negative",
		)
	}
	if execution.BudgetUSD < 0 {
		return fieldError(fieldBudgetUSD, s.Provenance, "must be non-negative")
	}
	ref := s.Config.Credential
	if !ref.Empty() {
		switch ref.Kind {
		case "env", "file", "keyring":
		default:
			return fieldError(fieldCredentialKind, s.Provenance, "must be env, file, or keyring")
		}
		if !secretNamePattern.MatchString(ref.Name) {
			return fieldError(fieldCredentialName, s.Provenance, "must be a non-secret reference name")
		}
	}
	if err := s.validateVerify(); err != nil {
		return err
	}
	if err := s.validateSubagent(); err != nil {
		return err
	}
	if err := s.validateVision(); err != nil {
		return err
	}
	if err := s.validateRoute(); err != nil {
		return err
	}
	if err := s.validateDiagnostics(); err != nil {
		return err
	}
	return s.validateWeb()
}

func (s Snapshot) validateDiagnostics() error {
	extensions := make([]string, 0, len(s.Config.Diagnostics.Commands))
	for extension := range s.Config.Diagnostics.Commands {
		extensions = append(extensions, extension)
	}
	slices.Sort(extensions)
	if len(extensions) > 32 {
		return fieldError(
			"diagnostics.commands",
			s.Provenance,
			"must contain at most 32 file-extension commands",
		)
	}
	for _, extension := range extensions {
		command := s.Config.Diagnostics.Commands[extension]
		nameField := fieldDiagnosticCommandName(extension)
		argsField := fieldDiagnosticCommandArgs(extension)
		if extension != strings.ToLower(extension) ||
			!diagnosticExtensionPattern.MatchString(extension) {
			return fieldError(
				nameField,
				s.Provenance,
				"extension must be a lowercase suffix such as .md",
			)
		}
		if !diagnosticCommandPattern.MatchString(command.Name) ||
			strings.ContainsAny(command.Name, `/\`) {
			return fieldError(
				nameField,
				s.Provenance,
				"name must be a PATH-resolved executable without directory separators",
			)
		}
		if len(command.Args) == 0 || len(command.Args) > 32 {
			return fieldError(argsField, s.Provenance, "args must contain between 1 and 32 entries")
		}
		hasPath := false
		totalBytes := 0
		for _, argument := range command.Args {
			totalBytes += len(argument)
			if strings.ContainsRune(argument, 0) || len(argument) > 4096 {
				return fieldError(argsField, s.Provenance, "arguments must be bounded text without NUL")
			}
			if strings.Contains(argument, "{path}") {
				hasPath = true
			}
		}
		if totalBytes > 16<<10 {
			return fieldError(argsField, s.Provenance, "arguments exceed 16384 bytes")
		}
		if !hasPath {
			return fieldError(argsField, s.Provenance, "one argument must contain {path}")
		}
	}
	return nil
}

// validateSubagent rejects a child configuration that cannot be honored, rather
// than letting a writing child discover at spawn time that it has nowhere
// isolated to write.
func (s Snapshot) validateSubagent() error {
	child := s.Config.Execution.Subagent
	switch child.Delegation {
	case SubagentDelegationDisabled, SubagentDelegationExplicit, SubagentDelegationAdaptive:
	default:
		return fieldError(fieldSubagentDelegation, s.Provenance,
			"must be disabled, explicit, or adaptive")
	}
	switch child.Workspace {
	case SubagentWorkspaceAuto, SubagentWorkspaceReadOnly, SubagentWorkspaceWorktree,
		SubagentWorkspaceSerialized:
	default:
		return fieldError(fieldSubagentWorkspace, s.Provenance,
			"must be auto, read_only, worktree, or same_workspace_serialized")
	}
	if child.MaxDepth < 1 {
		return fieldError(fieldSubagentMaxDepth, s.Provenance, "must be positive")
	}
	if child.MaxParallel < 1 {
		return fieldError(fieldSubagentMaxParallel, s.Provenance, "must be positive")
	}
	if child.MaxResident < child.MaxParallel {
		return fieldError(
			fieldSubagentMaxResident, s.Provenance,
			"must be at least execution.subagent.max_parallel",
		)
	}
	if child.MaxTotal < child.MaxResident {
		return fieldError(
			fieldSubagentMaxTotal, s.Provenance,
			"must be at least execution.subagent.max_resident",
		)
	}
	if child.MaxSteps < 0 {
		return fieldError(fieldSubagentMaxSteps, s.Provenance, "must be non-negative")
	}
	if child.MaxCostUSD < 0 {
		return fieldError(fieldSubagentMaxCostUSD, s.Provenance, "must be non-negative")
	}
	if child.WallTime < 0 {
		return fieldError(fieldSubagentWallTime, s.Provenance, "must be non-negative")
	}
	return nil
}

// validateVerify is fail-closed: the two values the roadmap names but that have
// no implementation yet (affected scope, ask on failure) are rejected at load
// time with a pointer at the missing work, instead of silently degrading into a
// different meaning.
func (s Snapshot) validateVerify() error {
	verify := s.Config.Execution.Verify
	switch verify.Mode {
	case "off", "soft", "hard":
	default:
		return fieldError(fieldVerifyMode, s.Provenance, "must be off, soft, or hard")
	}
	switch verify.Scope {
	case "diagnostics", "repository", "affected":
	default:
		return fieldError(fieldVerifyScope, s.Provenance,
			"must be diagnostics, repository, or affected")
	}
	switch verify.OnFailure {
	case "fail", "revert":
	case "ask":
		return fieldError(fieldVerifyOnFailure, s.Provenance,
			"ask needs an interactive input request every host can render; use fail or revert")
	default:
		return fieldError(fieldVerifyOnFailure, s.Provenance, "must be fail or revert")
	}
	if strings.TrimSpace(verify.Command) != "" &&
		verify.Scope != "repository" && verify.Scope != "affected" {
		return fieldError(fieldVerifyCommand, s.Provenance,
			"only the repository and affected scopes run commands; set scope = \"repository\"")
	}
	if verify.MaxRepairSteps < 0 || verify.MaxRepairSteps > 8 {
		return fieldError(fieldVerifyRepair, s.Provenance, "must be between 0 and 8")
	}
	if verify.Timeout <= 0 {
		return fieldError(fieldVerifyTimeout, s.Provenance, "must be positive")
	}
	return nil
}

func (s Snapshot) validateVision() error {
	vision := s.Config.Vision
	if !vision.Enabled {
		return nil
	}
	if strings.TrimSpace(vision.Provider) == "" {
		return fieldError(fieldVisionProvider, s.Provenance, "required when vision.enabled is true")
	}
	if strings.TrimSpace(vision.Model) == "" {
		return fieldError(fieldVisionModel, s.Provenance, "required when vision.enabled is true")
	}
	return nil
}

// routeSlotPurposes are the wired purposes a slot may be configured for, in the
// order they are reported. It matches routeFileConfig.
var routeSlotPurposes = []string{"vision", "summary"}

// validateRoute checks the slots configuration named. A half-named slot is the
// error worth catching here: a provider without a model resolves to nothing, and
// falling back to act silently would look like the slot had been honored.
func (s Snapshot) validateRoute() error {
	for purpose := range s.Config.Route.Slots {
		if !slices.Contains(routeSlotPurposes, purpose) {
			return fieldError("route."+purpose, s.Provenance, "unsupported route purpose")
		}
	}
	for _, purpose := range routeSlotPurposes {
		slot, configured := s.Config.Route.Slots[purpose]
		if !configured {
			continue
		}
		if strings.TrimSpace(slot.Provider) == "" {
			return fieldError(
				fieldRouteProvider(purpose), s.Provenance,
				"required when the "+purpose+" route is configured",
			)
		}
		if strings.TrimSpace(slot.Model) == "" {
			return fieldError(
				fieldRouteModel(purpose), s.Provenance,
				"required when the "+purpose+" route is configured",
			)
		}
	}
	return nil
}

func (s Snapshot) validateWeb() error {
	backend := strings.ToLower(strings.TrimSpace(s.Config.Web.SearchBackend))
	switch backend {
	case "", "duckduckgo", "bing", "tavily", "searxng", "bocha", "custom":
		return nil
	default:
		return fieldError(fieldWebSearchBackend, s.Provenance, "must be duckduckgo, bing, tavily, searxng, bocha, custom, or empty")
	}
}

func validateDeclaredEnvironmentResources(
	environment ExecutionEnvironment,
	provenance map[string]Source,
) error {
	if len(environment.Resources) > MaxDeclaredEnvironmentResources {
		return fieldError(
			fieldEnvironmentResources,
			provenance,
			fmt.Sprintf("accepts at most %d resources", MaxDeclaredEnvironmentResources),
		)
	}
	seen := make(map[string]bool, len(environment.Resources))
	for _, resource := range environment.Resources {
		request := resource.Request()
		if request.Namespace == envcontract.NamespaceWorkspace {
			return fieldError(
				fieldEnvironmentResources,
				provenance,
				"workspace resources stay command-scoped; use write_paths",
			)
		}
		if request.Namespace == envcontract.NamespaceSharedUserTemp &&
			!environment.SharedUserTemp {
			return fieldError(
				fieldEnvironmentResources,
				provenance,
				"shared_user_temp resources require shared_user_temp=true",
			)
		}
		if err := envcontract.ValidateRequest(request); err != nil {
			return fieldError(fieldEnvironmentResources, provenance, err.Error())
		}
		if seen[request.Name] {
			return fieldError(
				fieldEnvironmentResources,
				provenance,
				fmt.Sprintf("duplicate resource %q", request.Name),
			)
		}
		seen[request.Name] = true
	}
	return nil
}

func validateDeclaredAuthServices(
	environment ExecutionEnvironment,
	provenance map[string]Source,
) error {
	if len(environment.AuthServices) > MaxDeclaredAuthServices {
		return fieldError(
			fieldEnvironmentAuthServices,
			provenance,
			fmt.Sprintf("accepts at most %d auth services", MaxDeclaredAuthServices),
		)
	}
	for _, service := range environment.AuthServices {
		if strings.TrimSpace(service.Protocol) != "goproxy" {
			return fieldError(
				fieldEnvironmentAuthServices,
				provenance,
				"protocol must be goproxy",
			)
		}
		if len(service.Prefixes) == 0 {
			return fieldError(
				fieldEnvironmentAuthServices,
				provenance,
				"goproxy prefixes are required",
			)
		}
		if len(service.Prefixes) > MaxAuthServicePrefixes {
			return fieldError(
				fieldEnvironmentAuthServices,
				provenance,
				fmt.Sprintf("accepts at most %d prefixes", MaxAuthServicePrefixes),
			)
		}
		if err := validateGoproxyBinding(service); err != nil {
			return fieldError(fieldEnvironmentAuthServices, provenance, err.Error())
		}
		if service.UpstreamTimeoutMS < 0 {
			return fieldError(
				fieldEnvironmentAuthServices,
				provenance,
				"upstream_timeout_ms must not be negative",
			)
		}
	}
	return nil
}

func validateGoproxyBinding(service EnvironmentAuthService) error {
	if strings.TrimSpace(service.Upstream) == "" {
		return fmt.Errorf("goproxy upstream is required")
	}
	if strings.Contains(service.Upstream, "@") {
		return fmt.Errorf("goproxy upstream must not embed credentials")
	}
	if strings.TrimSpace(service.Credential.Kind) == "" ||
		strings.TrimSpace(service.Credential.Name) == "" {
		return fmt.Errorf("goproxy credential reference is required")
	}
	switch service.Credential.Kind {
	case "env", "file", "keyring":
	default:
		return fmt.Errorf("goproxy credential kind %q is not available", service.Credential.Kind)
	}
	for _, prefix := range service.Prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || prefix == "/" || strings.Contains(prefix, "..") {
			return fmt.Errorf("goproxy prefix %q is invalid", prefix)
		}
	}
	return nil
}

func fieldError(field string, provenance map[string]Source, reason string) error {
	source, exists := provenance[field]
	if !exists {
		source = SourceDefault
	}
	return &FieldError{Field: field, Source: source, Reason: reason}
}
