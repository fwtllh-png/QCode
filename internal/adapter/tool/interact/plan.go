package interact

const (
	StepPending    = "pending"
	StepInProgress = "in_progress"
	StepDone       = "done"
)

type PlanStep struct {
	Title  string `json:"title"`
	Status string `json:"status,omitempty"`
}

// Plan is the tool callback payload. Runtime owns admission, persistence and projection.
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
