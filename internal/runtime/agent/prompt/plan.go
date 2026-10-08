package prompt

import (
	"crypto/sha256"
	"fmt"
	"strings"

	agentcontext "github.com/fwtllh-png/QCode/internal/runtime/agent/context"
)

// FormatPlan renders a plan partition for WorldState projection.
func FormatPlan(plan agentcontext.Plan) string {
	var b strings.Builder
	b.WriteString("<plan")
	if plan.Title != "" {
		b.WriteString(` title="`)
		b.WriteString(plan.Title)
		b.WriteString(`"`)
	}
	b.WriteString(">\n")
	writePlanField(&b, "objective", plan.Objective)
	writePlanField(&b, "context_summary", plan.ContextSummary)
	writePlanList(&b, "sources_used", plan.SourcesUsed)
	writePlanList(&b, "critical_files", plan.CriticalFiles)
	writePlanList(&b, "constraints", plan.Constraints)
	writePlanField(&b, "recommended_approach", plan.RecommendedApproach)
	writePlanField(&b, "verification_plan", plan.VerificationPlan)
	writePlanField(&b, "risks_and_unknowns", plan.RisksAndUnknowns)
	writePlanField(&b, "handoff_packet", plan.HandoffPacket)
	for index, step := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s", index+1, step.Title)
		if step.ID != "" {
			fmt.Fprintf(&b, " [id=%s]", step.ID)
		}
		if len(step.ReferenceItemIDs) != 0 {
			fmt.Fprintf(&b, " [references=%s]", strings.Join(step.ReferenceItemIDs, ","))
		}
		// A pending step needs no marker: it is the default, and marking every
		// line would cost bytes to say nothing.
		if step.Status != "" && step.Status != agentcontext.StepPending {
			fmt.Fprintf(&b, " [%s]", step.Status)
		}
		b.WriteByte('\n')
	}
	if plan.Notes != "" {
		b.WriteString("Notes: ")
		b.WriteString(plan.Notes)
		b.WriteByte('\n')
	}
	b.WriteString("</plan>")
	return b.String()
}

func writePlanField(b *strings.Builder, name, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteByte('\n')
}

func writePlanList(b *strings.Builder, name string, values []string) {
	if len(values) == 0 {
		return
	}
	b.WriteString(name)
	b.WriteString(":\n")
	for _, value := range values {
		b.WriteString("- ")
		b.WriteString(value)
		b.WriteByte('\n')
	}
}

// PlanReceipt builds the audit receipt for an applied plan.
func PlanReceipt(plan agentcontext.Plan) Receipt {
	text := FormatPlan(plan)
	tokens := HeuristicTokenCounter{}.Count(text)
	digest := sha256.Sum256([]byte(text))
	return Receipt{
		Kind: PartitionPlan, SourcePath: "session://plan",
		OriginalBytes: len(text), RetainedBytes: len(text),
		OriginalTokens: tokens, RetainedTokens: tokens,
		Digest: fmt.Sprintf("sha256:%x", digest[:]),
	}
}
