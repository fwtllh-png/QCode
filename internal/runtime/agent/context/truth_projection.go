package agentcontext

import (
	"fmt"
	"strconv"
	"strings"
)

type TruthProjection struct {
	Compatibility    Compatibility
	ModelID          string
	ContextTokens    uint64
	Summary          Summary
	Plan             Plan
	Turn             uint64
	Evidence         EvidenceDelta
	WorkspaceDigests map[string]string
	CriticalPaths    []string
	ExtraEntities    []TruthEntity
}

func BuildTruthCapsule(input TruthProjection) TruthCapsule {
	capsule := TruthCapsule{
		SchemaVersion:     TruthSchemaVersion,
		Generation:        1,
		CompatibilityHash: input.Compatibility.Hash(),
		ModelID:           input.ModelID,
		ContextTokens:     input.ContextTokens,
		DownshiftPolicy:   DownshiftRuntimeTruthOnly,
	}
	capsule.Entities = append(
		capsule.Entities,
		PlanTruthEntities(input.Plan, input.Turn)...,
	)
	if len(capsule.Entities) == 0 &&
		strings.TrimSpace(input.Summary.Goal) != "" {
		capsule.Entities = append(capsule.Entities, NewTruthEntity(
			EntityGoal,
			"active",
			strings.TrimSpace(input.Summary.Goal),
			"runtime.user_input",
		))
	}
	for _, failure := range input.Summary.Failures {
		key := strings.Join(
			[]string{failure.Kind, failure.Name, failure.Reason},
			"\x00",
		)
		entity := NewTruthEntity(
			EntityFailure,
			key,
			FailureLine(failure),
			"runtime.failure_ledger",
		)
		entity.Turn, entity.Count = failure.Turn, failure.Count
		capsule.Entities = append(capsule.Entities, entity)
	}
	capsule.Entities = append(
		capsule.Entities,
		EvidenceTruthEntities(
			input.Summary,
			input.Evidence,
			input.WorkspaceDigests,
		)...,
	)
	for _, path := range input.CriticalPaths {
		capsule.Entities = append(capsule.Entities, NewTruthEntity(
			EntityCriticalPath,
			path,
			path,
			"runtime.working_set",
		))
	}
	capsule.Entities = append(capsule.Entities, input.ExtraEntities...)
	capsule.Seal()
	return capsule
}

func PlanTruthEntities(plan Plan, turn uint64) []TruthEntity {
	var entities []TruthEntity
	goal := strings.TrimSpace(plan.Objective)
	if goal == "" {
		goal = strings.TrimSpace(plan.Title)
	}
	if goal != "" {
		entities = append(entities, NewTruthEntity(
			EntityGoal,
			"active",
			goal,
			"runtime.plan",
		))
	}
	for index, step := range plan.Steps {
		key := step.ID
		if key == "" {
			key = fmt.Sprintf("unidentified:%d:%s", index, step.Title)
		}
		entity := NewTruthEntity(
			EntityTodo,
			key,
			step.Title,
			"runtime.plan",
		)
		entity.Status = step.Status
		entity.Turn = turn
		entities = append(entities, entity)
	}
	return entities
}

func EvidenceTruthEntities(
	summary Summary,
	delta EvidenceDelta,
	workspaceDigests map[string]string,
) []TruthEntity {
	result := make(
		[]TruthEntity,
		0,
		len(summary.Changes)+len(delta.Facts)+len(delta.Handles),
	)
	for _, change := range summary.Changes {
		entity := NewTruthEntity(
			EntityChange,
			change.Path,
			change.Path,
			"runtime.evidence",
		)
		entity.Turn, entity.Read = change.Turn, change.Read
		entity.Verified, entity.Diagnostics =
			change.Verified, change.Diagnostics
		if change.Verified {
			entity.VerificationSource = "runtime.evidence"
		}
		if digest := workspaceDigests[change.Path]; digest != "" {
			entity.WorkspacePath = change.Path
			entity.WorkspaceDigest = digest
			if change.Stale {
				entity.WorkspaceClaimStatus = WorkspaceClaimStale
			} else {
				entity.WorkspaceClaimStatus = WorkspaceClaimCurrent
			}
		} else if entity.Verified {
			entity.Verified = false
			entity.VerificationSource = ""
		}
		result = append(result, entity)
	}
	for _, fact := range delta.Facts {
		entity := NewTruthEntity(
			EntityFact,
			fmt.Sprintf("%s\x00%s\x00%d", fact.Kind, fact.Path, fact.Line),
			fact.Describe(),
			"runtime.evidence",
		)
		if digest := workspaceDigests[fact.Path]; digest != "" {
			entity.WorkspacePath = fact.Path
			entity.WorkspaceDigest = digest
			if fact.Stale {
				entity.WorkspaceClaimStatus = WorkspaceClaimStale
			} else {
				entity.WorkspaceClaimStatus = WorkspaceClaimCurrent
			}
		}
		result = append(result, entity)
	}
	for _, handle := range delta.Handles {
		entity := NewTruthEntity(
			EntityContentHandle,
			handle.ID,
			fmt.Sprintf("%s result handle %s", handle.Tool, handle.ID),
			"runtime.evidence",
		)
		entity.Turn, entity.Consumed = handle.Turn, handle.Consumed
		result = append(result, entity)
	}
	return result
}

func FailureLine(value Failure) string {
	line := value.Kind + " " + value.Name
	if value.Reason != "" {
		line += ": " + value.Reason
	}
	if value.Count > 1 {
		line += " (" + strconv.Itoa(value.Count) + " times)"
	}
	return line
}
