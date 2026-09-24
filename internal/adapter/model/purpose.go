package model

import "fmt"

// Purpose names one routed model use in the closed session routing table.
type Purpose string

const (
	// PurposeAct is the main route and fallback.
	PurposeAct Purpose = "act"
	// PurposeVision is image analysis.
	PurposeVision Purpose = "vision"
	// PurposeSummary is semantic context maintenance.
	PurposeSummary Purpose = "summary"
	// PurposeJudge is registered but not wired.
	PurposeJudge Purpose = "judge"
)

// Purposes reports every purpose in stable order.
func Purposes() []Purpose {
	return []Purpose{
		PurposeAct, PurposeVision,
		PurposeSummary, PurposeJudge,
	}
}

// Wired reports whether the runtime currently samples for this purpose.
func (p Purpose) Wired() bool {
	switch p {
	case PurposeAct, PurposeVision, PurposeSummary:
		return true
	default:
		return false
	}
}

// ParsePurpose validates a purpose name.
func ParsePurpose(value string) (Purpose, error) {
	purpose := Purpose(value)
	for _, candidate := range Purposes() {
		if candidate == purpose {
			return purpose, nil
		}
	}
	return "", fmt.Errorf("unknown route purpose %q", value)
}
