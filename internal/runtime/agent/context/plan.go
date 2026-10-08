package agentcontext

import (
	"encoding/json"
	"strings"
)

const (
	StepPending    = "pending"
	StepInProgress = "in_progress"
	StepDone       = "done"
)

type PlanStep struct {
	ID               string   `json:"id,omitempty"`
	ReferenceItemIDs []string `json:"reference_item_ids,omitempty"`
	Title            string   `json:"title"`
	Status           string   `json:"status,omitempty"`
}

func (s *PlanStep) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, `"`) {
		var title string
		if err := json.Unmarshal(data, &title); err != nil {
			return err
		}
		*s = PlanStep{Title: title, Status: StepPending}
		return nil
	}
	var object struct {
		ID               string   `json:"id"`
		ReferenceItemIDs []string `json:"reference_item_ids"`
		Title            string   `json:"title"`
		Status           string   `json:"status"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	status := strings.TrimSpace(strings.ToLower(object.Status))
	switch status {
	case StepPending, StepInProgress, StepDone:
	default:
		status = StepPending
	}
	*s = PlanStep{ID: object.ID, ReferenceItemIDs: append([]string(nil), object.ReferenceItemIDs...), Title: object.Title, Status: status}
	return nil
}

func (s PlanStep) Done() bool { return s.Status == StepDone }

type Plan struct {
	Title               string     `json:"title,omitempty"`
	Steps               []PlanStep `json:"steps"`
	Notes               string     `json:"notes,omitempty"`
	Objective           string     `json:"objective,omitempty"`
	ContextSummary      string     `json:"context_summary,omitempty"`
	SourcesUsed         []string   `json:"sources_used,omitempty"`
	CriticalFiles       []string   `json:"critical_files,omitempty"`
	Constraints         []string   `json:"constraints,omitempty"`
	RecommendedApproach string     `json:"recommended_approach,omitempty"`
	VerificationPlan    string     `json:"verification_plan,omitempty"`
	RisksAndUnknowns    string     `json:"risks_and_unknowns,omitempty"`
	HandoffPacket       string     `json:"handoff_packet,omitempty"`
}

func (p Plan) Clone() Plan {
	clone := p
	clone.Steps = append([]PlanStep(nil), p.Steps...)
	for i := range clone.Steps {
		clone.Steps[i].ReferenceItemIDs = append([]string(nil), p.Steps[i].ReferenceItemIDs...)
	}
	clone.SourcesUsed = append([]string(nil), p.SourcesUsed...)
	clone.CriticalFiles = append([]string(nil), p.CriticalFiles...)
	clone.Constraints = append([]string(nil), p.Constraints...)
	return clone
}

// ProgressSignature names the executable Plan step vector. Title and status
// changes count; prose fields such as notes do not.
func (p Plan) ProgressSignature() string {
	parts := make([]string, 0, len(p.Steps))
	for _, step := range p.Steps {
		parts = append(parts, step.ID+"\x00"+step.Title+"\x00"+step.Status+"\x00"+strings.Join(step.ReferenceItemIDs, "\x00"))
	}
	return strings.Join(parts, "\n")
}

func (p Plan) OutstandingSteps() ([]PlanStep, int) {
	var open []PlanStep
	done := 0
	for _, step := range p.Steps {
		if step.Done() {
			done++
			continue
		}
		open = append(open, step)
	}
	return open, done
}
