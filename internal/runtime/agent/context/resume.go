package agentcontext

import (
	"fmt"
	"strings"
)

const ResumeSource = "runtime.resume"

func ReadPathsFromWorkingSet(entries []WorkingSetEntry) []string {
	var paths []string
	for _, entry := range entries {
		path := strings.TrimSpace(entry.Path)
		if path == "" || !workingSetHasSource(entry, SourceRead) {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

func FormatResumeHint(plan Plan, readPaths []string, locatedSites ...[]string) string {
	return FormatResumeHintBudgeted(plan, readPaths, 0, resumeSites(locatedSites))
}

// FormatResumeHintBudgeted lists every already-read path that fits the existing
// session-state or checkpoint byte budget. Paths beyond that budget stay in the
// working-set ledger; the hint records how many were omitted. budget <= 0 keeps
// the full list.
func FormatResumeHintBudgeted(
	plan Plan, readPaths []string, budget int, locatedSites []string,
) string {
	kept := append([]string(nil), readPaths...)
	omitted := 0
	for {
		hint := formatResumeHint(plan, kept, omitted, locatedSites)
		if budget <= 0 || len(hint) <= budget || len(kept) == 0 {
			return hint
		}
		kept = kept[:len(kept)-1]
		omitted++
	}
}

func formatResumeHint(
	plan Plan, readPaths []string, omitted int, sites []string,
) string {
	open, done := plan.OutstandingSteps()
	if done == 0 && len(readPaths) == 0 && omitted == 0 && len(sites) == 0 {
		return ""
	}
	var parts []string
	if done > 0 {
		parts = append(parts, "Do not repeat completed plan steps.")
	}
	if len(open) > 0 {
		title := strings.TrimSpace(open[0].Title)
		if title != "" {
			parts = append(parts, "Next open work: "+title+".")
		}
	}
	if len(readPaths) > 0 || omitted > 0 {
		if len(readPaths) > 0 {
			parts = append(parts, "Already-read paths: "+strings.Join(readPaths, ", ")+".")
		}
		if omitted > 0 {
			parts = append(parts, fmt.Sprintf(
				"(%d more already-read paths omitted).", omitted,
			))
		}
		parts = append(parts,
			"Do not file_read those paths again unless you are about to edit a specific window. A dirty git status or git_diff is not a reason to file_read. Canceled or failed turns without edits are already recorded; do not re-verify that with git_diff. Absence from the visible tail is not a reason to file_read. Use "+TurnHistoryToolName+" or result_get for prior read text; if that output is truncated, call result_get before file_read. After search_text returns line hits, file_read only that window and edit; do not page the rest of the file.",
		)
	} else if done > 0 {
		parts = append(parts,
			"Do not re-audit completed work. Do not call git_status or git_diff on Continue. Canceled or failed turns without edits are already recorded. After search_text returns line hits, file_read only that window and edit.",
		)
	}
	if len(sites) > 0 {
		parts = append(parts, "Located sites: "+strings.Join(sites, ", ")+".")
		parts = append(parts,
			"file_read those paths only at a listed line and edit; do not page the rest of the file.",
		)
	}
	return strings.Join(parts, " ")
}

func ResumeRetrievalEntity(plan Plan, readPaths []string, locatedSites ...[]string) (TruthEntity, bool) {
	return ResumeRetrievalEntityBudgeted(plan, readPaths, 0, resumeSites(locatedSites))
}

func ResumeRetrievalEntityBudgeted(
	plan Plan, readPaths []string, budget int, locatedSites []string,
) (TruthEntity, bool) {
	hint := FormatResumeHintBudgeted(plan, readPaths, budget, locatedSites)
	if hint == "" {
		return TruthEntity{}, false
	}
	entity := NewTruthEntity(EntityFact, "resume", hint, ResumeSource)
	entity.normalizeLifecycle()
	return entity, true
}

func resumeSites(locatedSites [][]string) []string {
	if len(locatedSites) == 0 {
		return nil
	}
	return locatedSites[0]
}

func SessionStateResumeHint(capsule TruthCapsule) string {
	for _, entity := range capsule.Entities {
		if entity.Kind == EntityFact && entity.Source == ResumeSource {
			return entity.Value
		}
	}
	return ""
}

func workingSetHasSource(entry WorkingSetEntry, source WorkingSetSource) bool {
	for _, value := range entry.Sources {
		if value == source {
			return true
		}
	}
	return false
}

func FirstOutstandingPlanTitle(plan Plan) string {
	open, _ := plan.OutstandingSteps()
	if len(open) == 0 {
		return ""
	}
	return strings.TrimSpace(open[0].Title)
}

func FormatCanceledCheckpointNext(plan Plan) string {
	title := FirstOutstandingPlanTitle(plan)
	if title == "" {
		return ""
	}
	return fmt.Sprintf("next: %s", title)
}
