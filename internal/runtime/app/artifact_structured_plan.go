package app

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func validateStructuredPlan(body string, requireRevision bool) error {
	var document struct {
		Version  int                  `json:"version"`
		Revision uint64               `json:"revision"`
		Purpose  protocol.PlanPurpose `json:"purpose"`
		Steps    []struct {
			ID, Title, Status string
		} `json:"steps"`
	}
	if json.Unmarshal([]byte(body), &document) != nil ||
		document.Version != 1 || !document.Purpose.Valid() || len(document.Steps) == 0 ||
		len(document.Steps) > 128 ||
		(requireRevision && document.Revision == 0) {
		return errors.New("structured Plan Document is invalid")
	}
	seen := make(map[string]struct{}, len(document.Steps))
	for _, step := range document.Steps {
		if strings.TrimSpace(step.ID) == "" ||
			strings.TrimSpace(step.Title) == "" {
			return errors.New("structured Plan step is invalid")
		}
		switch step.Status {
		case "pending", "in_progress", "done":
		default:
			return errors.New("structured Plan step status is invalid")
		}
		if _, exists := seen[step.ID]; exists {
			return errors.New("structured Plan step ids must be unique")
		}
		seen[step.ID] = struct{}{}
	}
	return nil
}
