package repoindex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

type evaluationRelation struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Target      string `json:"target"`
}
type evaluationCase struct {
	Name     string               `json:"name"`
	Files    map[string]string    `json:"files"`
	Expected []evaluationRelation `json:"expected"`
}
type evaluationMetrics struct {
	TruePositive  int      `json:"true_positive"`
	FalsePositive int      `json:"false_positive"`
	FalseNegative int      `json:"false_negative"`
	Precision     *float64 `json:"precision"`
	Recall        *float64 `json:"recall"`
}

func measureRelations(got, want map[evaluationRelation]bool) evaluationMetrics {
	m := evaluationMetrics{}
	for r := range got {
		if want[r] {
			m.TruePositive++
		} else {
			m.FalsePositive++
		}
	}
	for r := range want {
		if !got[r] {
			m.FalseNegative++
		}
	}
	return finishMetrics(m)
}

func finishMetrics(m evaluationMetrics) evaluationMetrics {
	if total := m.TruePositive + m.FalsePositive; total > 0 {
		v := float64(m.TruePositive) / float64(total)
		m.Precision = &v
	}
	if total := m.TruePositive + m.FalseNegative; total > 0 {
		v := float64(m.TruePositive) / float64(total)
		m.Recall = &v
	}
	return m
}

func TestRepositoryUnderstandingEvaluation(t *testing.T) {
	data, err := os.ReadFile("../../../testdata/repository-understanding/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Version int              `json:"version"`
		Cases   []evaluationCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.Version != 1 || len(corpus.Cases) == 0 {
		t.Fatal("invalid evaluation corpus")
	}
	type result struct {
		Name        string               `json:"name"`
		Scoped      evaluationMetrics    `json:"scoped"`
		NameOnly    evaluationMetrics    `json:"name_only"`
		Missing     []evaluationRelation `json:"missing,omitempty"`
		Unexpected  []evaluationRelation `json:"unexpected,omitempty"`
		IndexMillis float64              `json:"index_ms"`
	}
	report := struct {
		Version        int               `json:"version"`
		IndexerVersion int               `json:"indexer_version"`
		CorpusSHA256   string            `json:"corpus_sha256"`
		Scoped         evaluationMetrics `json:"scoped_total"`
		NameOnly       evaluationMetrics `json:"name_only_total"`
		Results        []result          `json:"results"`
	}{Version: 1, IndexerVersion: IndexerVersion, CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			for name, source := range c.Files {
				writeFile(t, root, name, source)
			}
			index, store := newIndex(t, root, Options{})
			start := time.Now()
			snapshot, err := ensureSettled(t, index)
			elapsed := time.Since(start)
			if err != nil || !snapshot.Ready() {
				t.Fatalf("index=%+v err=%v", snapshot, err)
			}
			files, err := store.Files(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for name, f := range files {
				if !f.ScopeAware || f.ReferenceSitesTruncated {
					t.Fatalf("%s fell back or truncated; corpus requires complete scoped analysis", name)
				}
			}
			relations, err := store.ReferenceRelations(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got, want := map[evaluationRelation]bool{}, map[evaluationRelation]bool{}
			for _, r := range relations {
				got[evaluationRelation{r.Source, r.Destination, r.Site.Target}] = true
				source := c.Files[r.Source]
				if r.Site.StartByte < 0 || r.Site.EndByte > len(source) || source[r.Site.StartByte:r.Site.EndByte] != r.Site.Name || r.SourceDigest != files[r.Source].Digest {
					t.Fatalf("invalid evidence: %+v", r)
				}
			}
			for _, r := range c.Expected {
				if _, ok := c.Files[r.Source]; !ok {
					t.Fatal("missing expected source")
				}
				if _, ok := c.Files[r.Destination]; !ok {
					t.Fatal("missing expected destination")
				}
				want[r] = true
			}
			// Reproduce the previous name-only rule from the same persisted symbols
			// and identifier counts, with no knowledge of the new scoped decisions.
			rows, err := store.db.QueryContext(t.Context(), `SELECT DISTINCT r.path,s.path,s.name FROM repo_index_references r JOIN repo_index_symbols s ON s.root_path=r.root_path AND s.name=r.name WHERE r.root_path=? AND r.path<>s.path`, store.root)
			if err != nil {
				t.Fatal(err)
			}
			old := map[evaluationRelation]bool{}
			for rows.Next() {
				var r evaluationRelation
				if err := rows.Scan(&r.Source, &r.Destination, &r.Target); err != nil {
					t.Fatal(err)
				}
				old[r] = true
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			out := result{Name: c.Name, Scoped: measureRelations(got, want), NameOnly: measureRelations(old, want), IndexMillis: float64(elapsed.Microseconds()) / 1000}
			for r := range want {
				if !got[r] {
					out.Missing = append(out.Missing, r)
				}
			}
			for r := range got {
				if !want[r] {
					out.Unexpected = append(out.Unexpected, r)
				}
			}
			less := func(values []evaluationRelation) {
				sort.Slice(values, func(i, j int) bool { return fmt.Sprint(values[i]) < fmt.Sprint(values[j]) })
			}
			less(out.Missing)
			less(out.Unexpected)
			report.Results = append(report.Results, out)
			t.Logf("scoped TP/FP/FN=%d/%d/%d; name-only=%d/%d/%d", out.Scoped.TruePositive, out.Scoped.FalsePositive, out.Scoped.FalseNegative, out.NameOnly.TruePositive, out.NameOnly.FalsePositive, out.NameOnly.FalseNegative)
			if len(out.Missing) > 0 || len(out.Unexpected) > 0 {
				t.Errorf("missing=%+v unexpected=%+v", out.Missing, out.Unexpected)
			}
		})
	}
	for _, r := range report.Results {
		report.Scoped.TruePositive += r.Scoped.TruePositive
		report.Scoped.FalsePositive += r.Scoped.FalsePositive
		report.Scoped.FalseNegative += r.Scoped.FalseNegative
		report.NameOnly.TruePositive += r.NameOnly.TruePositive
		report.NameOnly.FalsePositive += r.NameOnly.FalsePositive
		report.NameOnly.FalseNegative += r.NameOnly.FalseNegative
	}
	report.Scoped = finishMetrics(report.Scoped)
	report.NameOnly = finishMetrics(report.NameOnly)
	t.Logf("total scoped TP/FP/FN=%d/%d/%d; name-only=%d/%d/%d", report.Scoped.TruePositive, report.Scoped.FalsePositive, report.Scoped.FalseNegative, report.NameOnly.TruePositive, report.NameOnly.FalsePositive, report.NameOnly.FalseNegative)
	if target := os.Getenv("QCODE_REPO_EVAL_REPORT"); target != "" {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
