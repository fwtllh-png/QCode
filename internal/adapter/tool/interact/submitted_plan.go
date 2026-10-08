package interact

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/google/uuid"
)

type SubmittedPlanStep struct {
	ReferenceItemIDs []string `json:"reference_item_ids,omitempty"`
	ID               string   `json:"id,omitempty"`
	Title            string   `json:"title"`
	Status           string   `json:"status,omitempty"`
	Dependencies     []string `json:"dependencies,omitempty"`
	ExpectedEvidence string   `json:"expected_evidence,omitempty"`
	AffectedFiles    []string `json:"affected_files,omitempty"`
}

type PlanFileBaseline struct {
	Path      string `json:"path"`
	Digest    string `json:"digest,omitempty"`
	Missing   bool   `json:"missing,omitempty"`
	Directory bool   `json:"directory,omitempty"`
}

type SubmittedPlan struct {
	ContextSelection    *ContextSelection    `json:"context_selection,omitempty"`
	Version             int                  `json:"version"`
	Purpose             protocol.PlanPurpose `json:"purpose,omitempty"`
	Revision            uint64               `json:"revision,omitempty"`
	SupersedesID        string               `json:"supersedes_id,omitempty"`
	Title               string               `json:"title,omitempty"`
	Objective           string               `json:"objective,omitempty"`
	ContextSummary      string               `json:"context_summary,omitempty"`
	Steps               []SubmittedPlanStep  `json:"steps"`
	SourcesUsed         []string             `json:"sources_used,omitempty"`
	CriticalFiles       []string             `json:"critical_files,omitempty"`
	Constraints         []string             `json:"constraints,omitempty"`
	RecommendedApproach string               `json:"recommended_approach,omitempty"`
	VerificationPlan    string               `json:"verification_plan,omitempty"`
	RisksAndUnknowns    string               `json:"risks_and_unknowns,omitempty"`
	HandoffPacket       string               `json:"handoff_packet,omitempty"`
	FileBaseline        []PlanFileBaseline   `json:"file_baseline,omitempty"`
}

func ParseSubmittedPlan(raw []byte) (SubmittedPlan, error) {
	var plan SubmittedPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return SubmittedPlan{}, err
	}
	if err := plan.NormalizeAndValidate(); err != nil {
		return SubmittedPlan{}, err
	}
	return plan, nil
}

func (p *SubmittedPlan) NormalizeAndValidate() error {
	if p == nil || len(p.Steps) == 0 || len(p.Steps) > 128 {
		return errors.New("plan steps are required")
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 {
		return fmt.Errorf("unsupported plan version %d", p.Version)
	}
	if !p.Purpose.Valid() {
		return fmt.Errorf("unsupported plan purpose %q", p.Purpose)
	}
	p.Purpose = p.Purpose.Normalize()
	seen := make(map[string]struct{}, len(p.Steps))
	for index := range p.Steps {
		step := &p.Steps[index]
		step.Title = strings.TrimSpace(step.Title)
		if step.Title == "" {
			return errors.New("plan steps must have a title")
		}
		if step.ID == "" {
			step.ID = "step:" + uuid.NewString()
		}
		if strings.ContainsAny(step.ID, " \t\r\n\x00") {
			return fmt.Errorf("plan step id %q is invalid", step.ID)
		}
		if _, exists := seen[step.ID]; exists {
			return fmt.Errorf("duplicate plan step id %q", step.ID)
		}
		seen[step.ID] = struct{}{}
		if step.Status == "" {
			step.Status = StepPending
		}
		switch step.Status {
		case StepPending, StepInProgress, StepDone:
		default:
			return fmt.Errorf("plan step %q has invalid status %q", step.ID, step.Status)
		}
	}
	for _, step := range p.Steps {
		for _, dependency := range step.Dependencies {
			if dependency == step.ID {
				return fmt.Errorf("plan step %q depends on itself", step.ID)
			}
			if _, exists := seen[dependency]; !exists {
				return fmt.Errorf("plan step %q has unknown dependency %q", step.ID, dependency)
			}
		}
	}
	return nil
}

func (p SubmittedPlan) executionPlan() Plan {
	steps := make([]PlanStep, len(p.Steps))
	for index, step := range p.Steps {
		steps[index] = PlanStep{
			ID: step.ID, ReferenceItemIDs: append([]string(nil), step.ReferenceItemIDs...),
			Title: step.Title, Status: step.Status,
		}
	}
	return Plan{
		Title: p.Title, Steps: steps, Objective: p.Objective, ContextSelection: p.ContextSelection,
		ContextSummary:      p.ContextSummary,
		SourcesUsed:         append([]string(nil), p.SourcesUsed...),
		CriticalFiles:       append([]string(nil), p.CriticalFiles...),
		Constraints:         append([]string(nil), p.Constraints...),
		RecommendedApproach: p.RecommendedApproach,
		VerificationPlan:    p.VerificationPlan,
		RisksAndUnknowns:    p.RisksAndUnknowns,
		HandoffPacket:       p.HandoffPacket,
	}
}
