package skill

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectionThousandSkillsIsDeterministicBoundedAndSavesTokens(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	for index := range 1000 {
		name := fmt.Sprintf("skill-%04d", index)
		description := fmt.Sprintf(
			"Handle deterministic workflow category %04d and related diagnostics.",
			index,
		)
		if index == 731 {
			name = "terraform-diagnostics"
			description = "Diagnose terraform deployment plans and infrastructure failures."
		}
		writeSkill(t, root, name, description, "SECRET BODY "+name)
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := SelectionRequest{
		Query: "please diagnose the terraform infrastructure deployment failure",
		Mode:  SelectionCandidate, Limit: 20,
	}
	first, err := catalog.Select(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Select(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Candidates, second.Candidates) {
		t.Fatal("selector output changed for identical input")
	}
	if len(first.Visible) == 0 || len(first.Visible) > 20 {
		t.Fatalf("visible candidate count = %d", len(first.Visible))
	}
	if first.Visible[0].Name != "terraform-diagnostics" {
		t.Fatalf("top candidate = %+v", first.Visible[0])
	}
	if first.Metrics.TokenSavings < 0.80 {
		t.Fatalf("token savings = %.4f", first.Metrics.TokenSavings)
	}
	if !second.Metrics.CacheHit {
		t.Fatal("second deterministic selection did not hit bounded cache")
	}
	for _, summary := range first.Visible {
		if strings.Contains(summary.Description, "SECRET BODY") {
			t.Fatal("skill body entered candidate metadata")
		}
	}
}

func TestSelectionExplicitMentionAndRequiredSkillHavePerfectRecall(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeSkill(t, root, "code-review", "Review code for defects.", "review body")
	writeSkill(t, root, "release", "Publish a release.", "release body")
	writeSkill(t, root, "unrelated", "Format prose.", "unrelated body")
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	golden := []struct {
		query    string
		required []string
		critical string
	}{
		{query: "use @code-review now", critical: "code-review"},
		{query: "publish the release", critical: "release"},
		{query: "format this", required: []string{"code-review"}, critical: "code-review"},
	}
	for _, item := range golden {
		selection, selectErr := catalog.Select(
			context.Background(),
			SelectionRequest{
				Query: item.query, Required: item.required,
				Mode: SelectionCandidate, Limit: 2,
			},
		)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		if !summaryNamed(selection.Candidates, item.critical) {
			t.Fatalf(
				"critical skill %q missing from %+v",
				item.critical, selection.Candidates,
			)
		}
	}
}

func TestSelectionMatchesASCIINameAdjacentToCJKText(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeSkill(t, root, "ubomcli", "Query release operations.", "ubom body")
	writeSkill(t, root, "unrelated", "Format prose.", "unrelated body")
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	selection, err := catalog.Select(t.Context(), SelectionRequest{
		Query: "你能使用ubomcli这个工具吗", Mode: SelectionCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) == 0 ||
		selection.Candidates[0].Name != "ubomcli" {
		t.Fatalf("mixed-script selection = %+v", selection.Candidates)
	}
}

func TestSelectionMatchesContainedChineseTriggerPhrase(t *testing.T) {
	selection, err := selectSummaries([]Summary{
		{
			Name:           "code-review",
			Description:    "Reviews changes. 代码审查 代码评审",
			ModelInvocable: true,
		},
		{
			Name:           "debugging",
			Description:    "Diagnoses failures. 复杂调试 故障排查",
			ModelInvocable: true,
		},
	}, SelectionRequest{
		Query: "代码审查一下这些变更", Mode: SelectionCandidate,
		Limit: DefaultSelectionLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) == 0 ||
		selection.Candidates[0].Name != "code-review" {
		t.Fatalf("Chinese trigger selection = %+v", selection.Candidates)
	}
}

func TestSelectionNameBoundaryRejectsEmbeddedASCIIWord(t *testing.T) {
	if containsSelectionPhrase("use myubomclitool now", "ubomcli") {
		t.Fatal("embedded ASCII name was treated as a skill-name boundary")
	}
}

func TestSelectionPreservesExplicitNameBeyondCandidateLimit(t *testing.T) {
	summaries := make([]Summary, 0, MaxSelectionCandidates+1)
	for index := range MaxSelectionCandidates {
		summaries = append(summaries, Summary{
			Name: fmt.Sprintf("skill-%04d", index), ModelInvocable: true,
		})
	}
	summaries = append(summaries, Summary{
		Name: "zzzz-target", ModelInvocable: true,
	})

	selection, err := selectSummaries(summaries, SelectionRequest{
		Query: "请使用zzzz-target处理", Mode: SelectionCandidate,
		Limit: DefaultSelectionLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summaryNamed(selection.Candidates, "zzzz-target") ||
		selection.Metrics.ExplicitMatches != 1 ||
		!selection.Metrics.CandidateSetTruncated {
		t.Fatalf("bounded explicit selection = %+v", selection)
	}
}

func TestSelectionExactNameOutranksLexicalCompetitors(t *testing.T) {
	const query = "use target-skill alpha beta gamma delta epsilon zeta eta theta iota kappa lambda"
	summaries := []Summary{{
		Name: "target-skill", Description: "Load the named skill.", ModelInvocable: true,
	}}
	for index := range DefaultSelectionLimit {
		summaries = append(summaries, Summary{
			Name:           fmt.Sprintf("competitor-%02d", index),
			Description:    "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda",
			ModelInvocable: true,
		})
	}

	selection, err := selectSummaries(summaries, SelectionRequest{
		Query: query, Mode: SelectionCandidate, Limit: DefaultSelectionLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summaryNamed(selection.Candidates, "target-skill") ||
		selection.Metrics.ExplicitMatches != 1 {
		t.Fatalf("exact-name selection = %+v", selection.Candidates)
	}
}

func TestSelectionShadowRecordsCandidatesWithoutChangingVisibleCatalog(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	for _, name := range []string{"alpha", "beta", "gamma"} {
		writeSkill(t, root, name, "Operate "+name, name+" body")
	}
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := catalog.Select(t.Context(), SelectionRequest{
		Query: "operate beta", Mode: SelectionShadow, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 1 ||
		selection.Candidates[0].Name != "beta" ||
		len(selection.Visible) != 3 {
		t.Fatalf("shadow selection = %+v", selection)
	}
}

func TestSkillHandleRejectsDigestDrift(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeSkill(t, root, "review", "Review code.", "original body")
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := catalog.HandleForName(t.Context(), "review")
	if err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root, "review", "Review code.", "changed body")
	if _, err := catalog.LoadHandle(t.Context(), handle); err == nil {
		t.Fatal("digest-drifted skill handle was accepted")
	}
}

func TestSkillHandleVariantsResolveOneCanonicalSkill(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agents", "skills")
	writeSkill(t, root, "review", "Review code.", "review body")
	catalog, err := Discover(DiscoveryOptions{
		Workspace: workspace, UserHome: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := catalog.ListHandles(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %+v", summaries)
	}
	expected := summaries[0]
	for name, handle := range map[string]string{
		"skill": expected.Handle, "package": expected.PackageHandle,
		"resource": expected.ResourceHandle,
	} {
		t.Run(name, func(t *testing.T) {
			summary, summaryErr := catalog.SummaryForHandle(t.Context(), handle)
			if summaryErr != nil {
				t.Fatal(summaryErr)
			}
			plan, loadErr := catalog.LoadHandle(t.Context(), handle)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if summary.Handle != expected.Handle ||
				len(plan) != 1 || plan[0].Name != expected.Name {
				t.Fatalf("summary=%+v plan=%+v", summary, plan)
			}
		})
	}
}

func summaryNamed(values []Summary, name string) bool {
	for _, value := range values {
		if value.Name == name {
			return true
		}
	}
	return false
}
