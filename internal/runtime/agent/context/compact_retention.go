package agentcontext

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

var ErrMandatoryCapacity = errors.New("mandatory truth exceeds context capacity")

type RetentionClass string

const (
	RetentionMandatory   RetentionClass = "mandatory"
	RetentionProtected   RetentionClass = "protected"
	RetentionRefreshable RetentionClass = "refreshable"
	RetentionAuditOnly   RetentionClass = "audit_only"
)

type WorkspaceClaimStatus string

const (
	WorkspaceClaimCurrent WorkspaceClaimStatus = "current"
	WorkspaceClaimStale   WorkspaceClaimStatus = "stale"
)

func (r RetentionClass) Valid() bool {
	switch r {
	case RetentionMandatory, RetentionProtected,
		RetentionRefreshable, RetentionAuditOnly:
		return true
	default:
		return false
	}
}

func (e *TruthEntity) normalizeLifecycle() {
	if e == nil {
		return
	}
	e.Source = strings.TrimSpace(e.Source)
	e.Owner = ownerForSource(e.Source)
	e.Retention = classifyEntity(*e)
	if e.WorkspacePath != "" && e.WorkspaceClaimStatus == "" {
		e.WorkspaceClaimStatus = WorkspaceClaimCurrent
	}
	if e.WorkspaceClaimStatus == WorkspaceClaimStale {
		e.Verified = false
		e.VerificationSource = ""
	}
}

func ownerForSource(source string) string {
	switch {
	case strings.HasPrefix(source, "runtime.plan"):
		return "plan"
	case strings.HasPrefix(source, "runtime.failure"):
		return "failures"
	case strings.HasPrefix(source, "runtime.evidence"):
		return "evidence"
	case strings.HasPrefix(source, "runtime.working_set"):
		return "working_set"
	case strings.HasPrefix(source, "runtime.input"):
		return "interaction"
	default:
		return "runtime"
	}
}

func classifyEntity(entity TruthEntity) RetentionClass {
	switch entity.Kind {
	case EntityGoal, EntityPendingInput:
		return RetentionMandatory
	case EntityTodo:
		switch strings.ToLower(strings.TrimSpace(entity.Status)) {
		case "completed", "done", "canceled", "cancelled", "skipped":
			return RetentionAuditOnly
		default:
			return RetentionMandatory
		}
	case EntityChange:
		if !entity.Verified || entity.Diagnostics ||
			entity.WorkspaceClaimStatus == WorkspaceClaimStale {
			return RetentionMandatory
		}
		return RetentionRefreshable
	case EntityFailure, EntityCriticalPath:
		return RetentionProtected
	case EntityContentHandle:
		if entity.Consumed {
			return RetentionAuditOnly
		}
		return RetentionProtected
	case EntityFact:
		if entity.Source == TurnHistorySource ||
			entity.Source == ResumeSource ||
			entity.Source == ContinuitySource {
			return RetentionMandatory
		}
		return RetentionRefreshable
	default:
		return RetentionAuditOnly
	}
}

type RetentionPolicy struct {
	TruthMaxBytes                int    `json:"truth_max_bytes"`
	TruthMaxEntities             int    `json:"truth_max_entities"`
	MandatoryMaxEntities         int    `json:"mandatory_max_entities"`
	FactMaxEntities              int    `json:"fact_max_entities"`
	VerifiedChangeRetentionTurns uint64 `json:"verified_change_retention_turns"`
	FailureMaxEntities           int    `json:"failure_max_entities"`
	HandleMaxEntities            int    `json:"handle_max_entities"`
	OmissionSampleMaxEntities    int    `json:"omission_sample_max_entities"`
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		TruthMaxBytes:                5632,
		TruthMaxEntities:             256,
		MandatoryMaxEntities:         128,
		FactMaxEntities:              96,
		VerifiedChangeRetentionTurns: 32,
		FailureMaxEntities:           24,
		HandleMaxEntities:            32,
		OmissionSampleMaxEntities:    8,
	}
}

func (p RetentionPolicy) Normalized() RetentionPolicy {
	defaults := DefaultRetentionPolicy()
	if p.TruthMaxBytes <= 0 {
		p.TruthMaxBytes = defaults.TruthMaxBytes
	}
	if p.TruthMaxEntities <= 0 {
		p.TruthMaxEntities = defaults.TruthMaxEntities
	}
	if p.MandatoryMaxEntities <= 0 {
		p.MandatoryMaxEntities = defaults.MandatoryMaxEntities
	}
	if p.FactMaxEntities <= 0 {
		p.FactMaxEntities = defaults.FactMaxEntities
	}
	if p.VerifiedChangeRetentionTurns == 0 {
		p.VerifiedChangeRetentionTurns = defaults.VerifiedChangeRetentionTurns
	}
	if p.FailureMaxEntities <= 0 {
		p.FailureMaxEntities = defaults.FailureMaxEntities
	}
	if p.HandleMaxEntities <= 0 {
		p.HandleMaxEntities = defaults.HandleMaxEntities
	}
	if p.OmissionSampleMaxEntities <= 0 {
		p.OmissionSampleMaxEntities = defaults.OmissionSampleMaxEntities
	}
	return p
}

func (p RetentionPolicy) Validate(summaryMaxBytes int) error {
	p = p.Normalized()
	if p.TruthMaxBytes < 256 || p.TruthMaxBytes > 1<<20 {
		return errors.New("truth_max_bytes must be between 256 and 1048576")
	}
	if summaryMaxBytes > 0 && p.TruthMaxBytes >= summaryMaxBytes {
		return errors.New("truth_max_bytes must be smaller than summary_max_bytes")
	}
	if p.TruthMaxEntities < 1 || p.TruthMaxEntities > 4096 {
		return errors.New("truth_max_entities must be between 1 and 4096")
	}
	if p.MandatoryMaxEntities < 1 ||
		p.MandatoryMaxEntities > p.TruthMaxEntities {
		return errors.New("mandatory_max_entities must not exceed truth_max_entities")
	}
	for name, value := range map[string]int{
		"fact_max_entities":            p.FactMaxEntities,
		"failure_max_entities":         p.FailureMaxEntities,
		"handle_max_entities":          p.HandleMaxEntities,
		"omission_sample_max_entities": p.OmissionSampleMaxEntities,
	} {
		if value < 1 || value > p.TruthMaxEntities {
			return fmt.Errorf("%s must be between 1 and truth_max_entities", name)
		}
	}
	return nil
}

type Omission struct {
	Class     RetentionClass `json:"class"`
	Kind      string         `json:"kind"`
	Reason    string         `json:"reason"`
	Count     int            `json:"count"`
	SampleIDs []string       `json:"sample_ids,omitempty"`
}

func (o Omission) Validate() error {
	if !o.Class.Valid() || o.Kind == "" || o.Reason == "" || o.Count < 1 {
		return errors.New("truth omission is invalid")
	}
	if len(o.SampleIDs) > 4096 {
		return errors.New("truth omission sample is unbounded")
	}
	return nil
}

type RetentionCount struct {
	Class      RetentionClass `json:"class"`
	Candidates int            `json:"candidates"`
	Retained   int            `json:"retained"`
	Omitted    int            `json:"omitted"`
}

type RetentionReceipt struct {
	CandidateEntities int              `json:"candidate_entities"`
	RetainedEntities  int              `json:"retained_entities"`
	MandatoryEntities int              `json:"mandatory_entities"`
	MandatoryBytes    int              `json:"mandatory_bytes"`
	TruthBytes        int              `json:"truth_bytes"`
	OmissionCount     int              `json:"omission_count"`
	ByClass           []RetentionCount `json:"by_class"`
}

func PlanRetention(
	input TruthCapsule,
	policy RetentionPolicy,
	currentTurn uint64,
) (TruthCapsule, RetentionReceipt, error) {
	policy = policy.Normalized()
	if err := policy.Validate(0); err != nil {
		return TruthCapsule{}, RetentionReceipt{}, err
	}
	input.Entities = append([]TruthEntity(nil), input.Entities...)
	input.Omissions = nil
	input.Seal()
	if err := input.Validate(); err != nil {
		return TruthCapsule{}, RetentionReceipt{}, err
	}

	classes := map[RetentionClass][]TruthEntity{
		RetentionMandatory:   {},
		RetentionProtected:   {},
		RetentionRefreshable: {},
		RetentionAuditOnly:   {},
	}
	for _, entity := range input.Entities {
		entity.normalizeLifecycle()
		classes[entity.Retention] = append(classes[entity.Retention], entity)
	}
	for class := range classes {
		sortEntities(classes[class], currentTurn, policy)
	}

	receipt := RetentionReceipt{CandidateEntities: len(input.Entities)}
	counts := make(map[RetentionClass]*RetentionCount)
	for _, class := range []RetentionClass{
		RetentionMandatory, RetentionProtected,
		RetentionRefreshable, RetentionAuditOnly,
	} {
		value := &RetentionCount{
			Class: class, Candidates: len(classes[class]),
		}
		counts[class] = value
		receipt.ByClass = append(receipt.ByClass, *value)
	}

	mandatory := classes[RetentionMandatory]
	if len(mandatory) > policy.MandatoryMaxEntities ||
		len(mandatory) > policy.TruthMaxEntities {
		return TruthCapsule{}, receipt, fmt.Errorf(
			"%w: %d entities exceed limit %d",
			ErrMandatoryCapacity,
			len(mandatory),
			min(policy.MandatoryMaxEntities, policy.TruthMaxEntities),
		)
	}
	result := input
	result.Entities = append([]TruthEntity(nil), mandatory...)
	result.Seal()
	mandatoryBytes, err := canonicalCapsuleBytes(result)
	if err != nil {
		return TruthCapsule{}, receipt, err
	}
	if mandatoryBytes > policy.TruthMaxBytes {
		return TruthCapsule{}, receipt, fmt.Errorf(
			"%w: %d bytes exceed limit %d",
			ErrMandatoryCapacity,
			mandatoryBytes,
			policy.TruthMaxBytes,
		)
	}

	retainedByKind := make(map[string]int)
	for _, entity := range result.Entities {
		retainedByKind[entity.Kind]++
	}
	var omitted []TruthEntity
	for _, class := range []RetentionClass{
		RetentionProtected, RetentionRefreshable,
	} {
		for _, entity := range classes[class] {
			if len(result.Entities) >= policy.TruthMaxEntities ||
				kindQuotaReached(entity.Kind, retainedByKind, policy) {
				omitted = append(omitted, entity)
				continue
			}
			candidate := result
			candidate.Entities = append(
				append([]TruthEntity(nil), result.Entities...),
				entity,
			)
			candidate.Seal()
			size, marshalErr := canonicalCapsuleBytes(candidate)
			if marshalErr != nil {
				return TruthCapsule{}, receipt, marshalErr
			}
			if size > policy.TruthMaxBytes {
				omitted = append(omitted, entity)
				continue
			}
			result = candidate
			retainedByKind[entity.Kind]++
		}
	}
	omitted = append(omitted, classes[RetentionAuditOnly]...)
	result.Omissions = aggregateOmissions(
		omitted,
		policy.OmissionSampleMaxEntities,
	)
	for len(result.Omissions) != 0 {
		result.Seal()
		size, marshalErr := canonicalCapsuleBytes(result)
		if marshalErr != nil {
			return TruthCapsule{}, receipt, marshalErr
		}
		if size <= policy.TruthMaxBytes {
			break
		}
		last := len(result.Omissions) - 1
		if len(result.Omissions[last].SampleIDs) != 0 {
			result.Omissions[last].SampleIDs =
				result.Omissions[last].SampleIDs[:len(result.Omissions[last].SampleIDs)-1]
			continue
		}
		result.Omissions = result.Omissions[:last]
	}
	result.Seal()
	size, err := canonicalCapsuleBytes(result)
	if err != nil {
		return TruthCapsule{}, receipt, err
	}
	receipt.RetainedEntities = len(result.Entities)
	receipt.MandatoryEntities = len(mandatory)
	receipt.MandatoryBytes = mandatoryBytes
	receipt.TruthBytes = size
	receipt.OmissionCount = len(omitted)
	for index := range receipt.ByClass {
		value := &receipt.ByClass[index]
		for _, entity := range result.Entities {
			if entity.Retention == value.Class {
				value.Retained++
			}
		}
		value.Omitted = value.Candidates - value.Retained
	}
	return result, receipt, nil
}

func MandatoryCapsule(input TruthCapsule) TruthCapsule {
	result := input
	result.Entities = nil
	result.Omissions = nil
	for _, entity := range input.Entities {
		entity.normalizeLifecycle()
		if entity.Retention == RetentionMandatory {
			result.Entities = append(result.Entities, entity)
		}
	}
	result.Seal()
	return result
}

func canonicalCapsuleBytes(capsule TruthCapsule) (int, error) {
	encoded, err := json.Marshal(capsule)
	return len(encoded), err
}

func sortEntities(
	entities []TruthEntity,
	currentTurn uint64,
	policy RetentionPolicy,
) {
	sort.SliceStable(entities, func(i, j int) bool {
		left, right := entities[i], entities[j]
		leftRank := entityPriority(left, currentTurn, policy)
		rightRank := entityPriority(right, currentTurn, policy)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Turn != right.Turn {
			return left.Turn > right.Turn
		}
		if left.Source != right.Source {
			return left.Source < right.Source
		}
		return left.ID < right.ID
	})
}

func entityPriority(
	entity TruthEntity,
	currentTurn uint64,
	policy RetentionPolicy,
) int {
	switch entity.Kind {
	case EntityGoal:
		return 0
	case EntityTodo:
		return 1
	case EntityPendingInput:
		return 2
	case EntityChange:
		if !entity.Verified || entity.Diagnostics {
			return 3
		}
		if currentTurn >= entity.Turn &&
			currentTurn-entity.Turn <= policy.VerifiedChangeRetentionTurns {
			return 7
		}
		return 11
	case EntityCriticalPath:
		return 4
	case EntityFailure:
		return 5
	case EntityContentHandle:
		return 6
	case EntityFact:
		return 8
	default:
		return 12
	}
}

func kindQuotaReached(
	kind string,
	retained map[string]int,
	policy RetentionPolicy,
) bool {
	var limit int
	switch kind {
	case EntityFact:
		limit = policy.FactMaxEntities
	case EntityFailure:
		limit = policy.FailureMaxEntities
	case EntityContentHandle:
		limit = policy.HandleMaxEntities
	default:
		return false
	}
	return retained[kind] >= limit
}

func aggregateOmissions(
	entities []TruthEntity,
	sampleLimit int,
) []Omission {
	type key struct {
		class RetentionClass
		kind  string
	}
	aggregated := make(map[key]*Omission)
	for _, entity := range entities {
		reason := "retention_budget"
		if entity.Retention == RetentionAuditOnly {
			reason = "audit_only"
		}
		k := key{class: entity.Retention, kind: entity.Kind}
		value := aggregated[k]
		if value == nil {
			value = &Omission{
				Class:  entity.Retention,
				Kind:   entity.Kind,
				Reason: reason,
			}
			aggregated[k] = value
		}
		value.Count++
		if len(value.SampleIDs) < sampleLimit {
			value.SampleIDs = append(value.SampleIDs, entity.ID)
		}
	}
	result := make([]Omission, 0, len(aggregated))
	for _, value := range aggregated {
		sort.Strings(value.SampleIDs)
		result = append(result, *value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Class != result[j].Class {
			return result[i].Class < result[j].Class
		}
		return result[i].Kind < result[j].Kind
	})
	return result
}

type AdmissionRequest struct {
	ThreadID              protocol.ThreadID `json:"thread_id"`
	BaseContextRevision   uint64            `json:"base_context_revision"`
	RouteCompatibility    string            `json:"route_compatibility"`
	AddedMandatory        []TruthEntity     `json:"added_mandatory,omitempty"`
	ResolvedMandatoryIDs  []string          `json:"resolved_mandatory_ids,omitempty"`
	ProjectedStableTokens uint64            `json:"projected_stable_tokens,omitempty"`
	ProjectedToolTokens   uint64            `json:"projected_tool_tokens,omitempty"`
}

type AdmissionDecision struct {
	Allowed             bool     `json:"allowed"`
	Reason              string   `json:"reason,omitempty"`
	ProjectedTruthBytes int      `json:"projected_truth_bytes"`
	ProjectedEntities   int      `json:"projected_entities"`
	RequiredActions     []string `json:"required_actions,omitempty"`
}

type ContextAdmissionController struct {
	Policy RetentionPolicy
}

func (c ContextAdmissionController) Decide(
	current TruthCapsule,
	request AdmissionRequest,
) AdmissionDecision {
	policy := c.Policy.Normalized()
	resolved := make(map[string]struct{}, len(request.ResolvedMandatoryIDs))
	for _, id := range request.ResolvedMandatoryIDs {
		resolved[strings.TrimSpace(id)] = struct{}{}
	}
	projected := current
	projected.Entities = nil
	projected.Omissions = nil
	for _, entity := range current.Entities {
		entity.normalizeLifecycle()
		if entity.Retention != RetentionMandatory {
			continue
		}
		if _, ok := resolved[entity.ID]; !ok {
			projected.Entities = append(projected.Entities, entity)
		}
	}
	for _, entity := range request.AddedMandatory {
		entity.normalizeLifecycle()
		entity.Retention = RetentionMandatory
		projected.Entities = append(projected.Entities, entity)
	}
	if request.RouteCompatibility != "" {
		projected.CompatibilityHash = request.RouteCompatibility
	}
	projected.Seal()
	bytes, _ := canonicalCapsuleBytes(projected)
	decision := AdmissionDecision{
		Allowed:             true,
		ProjectedTruthBytes: bytes,
		ProjectedEntities:   len(projected.Entities),
	}
	if _, _, err := PlanRetention(projected, policy, 0); err != nil {
		decision.Allowed = false
		decision.Reason = err.Error()
		decision.RequiredActions = []string{
			"verify existing changes",
			"close completed plan steps",
			"split work into a new thread",
			"select a route with a larger context window",
		}
	}
	return decision
}
