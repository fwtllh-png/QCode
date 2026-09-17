package search

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/persist/repoindex"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/testutil/tooltest"
)

type taskEvalCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}
type taskEvalAnchor struct {
	File string `json:"file"`
	Text string `json:"text"`
}
type taskEvalCase struct {
	ID            string           `json:"id"`
	Kind          string           `json:"kind"`
	Prompt        string           `json:"prompt"`
	ExpectedFiles []string         `json:"expected_files"`
	RequiredTests []string         `json:"required_tests"`
	Anchors       []taskEvalAnchor `json:"anchors"`
	Text          []taskEvalCall   `json:"text"`
	Structured    []taskEvalCall   `json:"structured"`
	Rationale     string           `json:"annotation_rationale"`
}
type taskEvalMetrics struct {
	TP         int      `json:"true_positive"`
	FP         int      `json:"false_positive"`
	FN         int      `json:"false_negative"`
	Precision  *float64 `json:"precision"`
	Recall     *float64 `json:"recall"`
	Missing    []string `json:"missing"`
	Unexpected []string `json:"unexpected"`
}

func taskEvalScore(got map[string]bool, want []string) taskEvalMetrics {
	m := taskEvalMetrics{Missing: []string{}, Unexpected: []string{}}
	expected := map[string]bool{}
	for _, f := range want {
		expected[f] = true
	}
	for f := range got {
		if expected[f] {
			m.TP++
		} else {
			m.FP++
			m.Unexpected = append(m.Unexpected, f)
		}
	}
	for f := range expected {
		if !got[f] {
			m.FN++
			m.Missing = append(m.Missing, f)
		}
	}
	if n := m.TP + m.FP; n > 0 {
		v := float64(m.TP) / float64(n)
		m.Precision = &v
	}
	if n := m.TP + m.FN; n > 0 {
		v := float64(m.TP) / float64(n)
		m.Recall = &v
	}
	sort.Strings(m.Missing)
	sort.Strings(m.Unexpected)
	return m
}

type taskEvalRoute struct {
	Files       taskEvalMetrics `json:"files"`
	Tests       taskEvalMetrics `json:"required_tests"`
	Anchors     taskEvalMetrics `json:"evidence_anchors"`
	Calls       []taskEvalTrace `json:"calls"`
	ToolCalls   int             `json:"tool_calls"`
	OutputBytes int             `json:"output_utf8_bytes"`
	InputBytes  int             `json:"arguments_utf8_bytes"`
	ModelTokens *int            `json:"model_tokens"`
}
type taskEvalTrace struct {
	Tool      string          `json:"tool"`
	Arguments map[string]any  `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Millis    float64         `json:"elapsed_ms"`
}

func TestRepositoryTaskEvaluation(t *testing.T) {
	reportPath := os.Getenv("QCODE_REPO_TASK_REPORT")
	if reportPath == "" {
		t.Skip("observational evaluation: run make repository-task-eval")
	}
	data, err := os.ReadFile("../../../../testdata/repository-understanding/tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Version          int            `json:"version"`
		AnnotationStatus string         `json:"annotation_status"`
		Scope            string         `json:"scope"`
		Budget           uint64         `json:"result_token_budget"`
		MaxResults       int            `json:"max_results"`
		BudgetProvenance string         `json:"budget_provenance"`
		Files            []string       `json:"files"`
		Tasks            []taskEvalCase `json:"tasks"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.Version != 1 || corpus.Budget == 0 || corpus.MaxResults < 1 || len(corpus.Tasks) == 0 {
		t.Fatal("invalid corpus configuration")
	}
	files := map[string]string{}
	digests := map[string]string{}
	for _, path := range corpus.Files {
		if !strings.HasPrefix(path, "internal/platform/") || filepath.ToSlash(filepath.Clean(path)) != path || strings.Contains(path, "..") || !strings.HasSuffix(path, ".go") {
			t.Fatalf("not an allowed source path: %s", path)
		}
		full := filepath.Join("../../../..", path)
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("not a regular source: %s: %v", path, err)
		}
		source, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(source)
		digests[path] = repowalk.Digest(source)
	}
	type result struct {
		Task       taskEvalCase  `json:"task"`
		Text       taskEvalRoute `json:"text"`
		Structured taskEvalRoute `json:"structured"`
	}
	report := struct {
		Version          int               `json:"version"`
		Scope            string            `json:"scope"`
		AnnotationStatus string            `json:"annotation_status"`
		Method           string            `json:"method"`
		ModelExecuted    bool              `json:"model_executed"`
		TaskSuccess      *bool             `json:"autonomous_task_success"`
		CorpusDigest     string            `json:"corpus_digest"`
		SourceDigests    map[string]string `json:"source_digests"`
		IndexerVersion   int               `json:"indexer_version"`
		Budget           uint64            `json:"result_token_budget"`
		MaxResults       int               `json:"max_results"`
		BudgetProvenance string            `json:"budget_provenance"`
		Results          []result          `json:"results"`
	}{Version: 1, Scope: corpus.Scope, AnnotationStatus: corpus.AnnotationStatus, Method: "fixed_tool_workflows_no_model_no_patch_execution", CorpusDigest: repowalk.Digest(data), SourceDigests: digests, IndexerVersion: repoindex.IndexerVersion, Budget: corpus.Budget, MaxResults: corpus.MaxResults, BudgetProvenance: corpus.BudgetProvenance}
	seen := map[string]bool{}
	for _, task := range corpus.Tasks {
		if task.ID == "" || seen[task.ID] || task.Prompt == "" || task.Rationale == "" || len(task.Text) == 0 || len(task.Structured) == 0 || len(task.ExpectedFiles) == 0 {
			t.Fatal("invalid task identity/annotation")
		}
		seen[task.ID] = true
		for _, path := range append(append([]string{}, task.ExpectedFiles...), task.RequiredTests...) {
			if _, ok := files[path]; !ok {
				t.Fatalf("unavailable gold file: %s", path)
			}
		}
		for _, anchor := range task.Anchors {
			if !strings.Contains(files[anchor.File], anchor.Text) {
				t.Fatalf("stale annotation: %+v", anchor)
			}
		}
		// Independent registries and temporary workspaces prevent cross-route caches.
		textRegistry := indexedRegistry(t, files)
		structuredRegistry := indexedRegistry(t, files)
		baseline := runTaskRoute(t, textRegistry, task, task.Text, files, corpus.Budget, corpus.MaxResults)
		structured := runTaskRoute(t, structuredRegistry, task, task.Structured, files, corpus.Budget, corpus.MaxResults)
		report.Results = append(report.Results, result{task, baseline, structured})
		t.Logf("%s file TP/FP/FN text=%d/%d/%d structured=%d/%d/%d; missing tests=%d/%d; bytes=%d/%d", task.ID, baseline.Files.TP, baseline.Files.FP, baseline.Files.FN, structured.Files.TP, structured.Files.FP, structured.Files.FN, baseline.Tests.FN, structured.Tests.FN, baseline.OutputBytes, structured.OutputBytes)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	var summary strings.Builder
	summary.WriteString("# QCode 真实源码任务评测\n\n这是固定工具流程的证据检索评测，未调用模型、未修改源码、未运行任务所推荐的测试。标注由代理核查源码后编写，尚未经独立人工复核；范围限于语料列出的源码子集。结果不能代表 Agent 自主修复成功率。\n\n")
	fmt.Fprintf(&summary, "语料 digest：`%s`。索引版本：%d。每次调用结果预算：%d；查询 max_results：%d。原始 JSON 保存每次调用参数、完整工具输出、文件 digest 与耗时。\n\n", report.CorpusDigest, report.IndexerVersion, report.Budget, report.MaxResults)
	summary.WriteString("| 任务 | 文本文件 TP/FP/FN | 结构化文件 TP/FP/FN | 必要测试漏项（文本/结构化） | 输出字节（文本/结构化） | 调用数（文本/结构化） |\n| --- | --- | --- | --- | --- | --- |\n")
	var textBytes, structuredBytes, textCalls, structuredCalls, textMisses, structuredMisses int
	for _, r := range report.Results {
		a, b := r.Text, r.Structured
		fmt.Fprintf(&summary, "| %s | %d/%d/%d | %d/%d/%d | %d/%d | %d/%d | %d/%d |\n", r.Task.ID, a.Files.TP, a.Files.FP, a.Files.FN, b.Files.TP, b.Files.FP, b.Files.FN, a.Tests.FN, b.Tests.FN, a.OutputBytes, b.OutputBytes, a.ToolCalls, b.ToolCalls)
		textBytes += a.OutputBytes
		structuredBytes += b.OutputBytes
		textCalls += a.ToolCalls
		structuredCalls += b.ToolCalls
		textMisses += a.Tests.FN
		structuredMisses += b.Tests.FN
	}
	fmt.Fprintf(&summary, "\n总输出字节：%d / %d；固定流程调用数：%d / %d；必要测试漏项：%d / %d（均为文本 / 结构化）。按任务计数，同一测试文件可在不同任务重复计入。字节数不是 token 数，调用数由预设流程决定，不能据此推断 Agent 自主调用次数。\n", textBytes, structuredBytes, textCalls, structuredCalls, textMisses, structuredMisses)
	summary.WriteString("\n## 未命中与多余结果\n")
	for _, r := range report.Results {
		fmt.Fprintf(&summary, "\n- `%s`：文本缺失文件 `%v`，多余文件 `%v`；结构化缺失文件 `%v`，多余文件 `%v`。文本缺失证据锚点 `%v`；结构化缺失证据锚点 `%v`。\n", r.Task.ID, r.Text.Files.Missing, r.Text.Files.Unexpected, r.Structured.Files.Missing, r.Structured.Files.Unexpected, r.Text.Anchors.Missing, r.Structured.Anchors.Missing)
	}
	summary.WriteString("\n评分变化仅记录，不设置准确率门禁；工具执行失败、语料过期或报告写入失败才使命令失败。下一轮应人工复核多余结果及必要测试标注，再按失败样例优化。\n")
	if err := os.WriteFile(reportPath+".summary.md", []byte(summary.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func runTaskRoute(t *testing.T, registry *tool.Registry, task taskEvalCase, calls []taskEvalCall, source map[string]string, budget uint64, maxResults int) taskEvalRoute {
	t.Helper()
	found, tests, anchors := map[string]bool{}, map[string]bool{}, map[string]bool{}
	expectedAnchors := []string{}
	for _, a := range task.Anchors {
		expectedAnchors = append(expectedAnchors, a.File+":"+a.Text)
	}
	out := taskEvalRoute{Calls: []taskEvalTrace{}}
	location := func(file string, line int) {
		if _, ok := source[file]; !ok {
			t.Fatalf("tool returned unlisted file %q", file)
		}
		if task.Kind != "verify" || repoindex.IsTestPath(file) {
			found[file] = true
		}
		if repoindex.IsTestPath(file) {
			tests[file] = true
		}
		lines := strings.Split(source[file], "\n")
		if line > 0 && line <= len(lines) {
			for _, a := range task.Anchors {
				if file == a.File && strings.Contains(lines[line-1], a.Text) {
					anchors[a.File+":"+a.Text] = true
				}
			}
		}
	}
	for _, call := range calls {
		switch call.Tool {
		case "search_text", KindDefinition, KindReferences, KindRelatedTests:
		default:
			t.Fatalf("unsupported evaluation tool %s", call.Tool)
		}
		args := map[string]any{}
		for k, v := range call.Arguments {
			args[k] = v
		}
		if call.Tool != KindRelatedTests {
			args["max_results"] = maxResults
		}
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		r, err := tooltest.Execute(tool.WithResultTokenBudget(t.Context(), budget), registry, tool.Call{Name: call.Tool, Arguments: data})
		if err != nil {
			t.Fatalf("%s %s: %v", task.ID, call.Tool, err)
		}
		if !json.Valid([]byte(r.Content)) {
			t.Fatalf("non JSON tool result: %s", call.Tool)
		}
		out.Calls = append(out.Calls, taskEvalTrace{call.Tool, args, json.RawMessage(r.Content), float64(time.Since(start).Microseconds()) / 1000})
		out.ToolCalls++
		out.OutputBytes += len(r.Content)
		out.InputBytes += len(data)
		var payload struct {
			Status  string `json:"status"`
			Matches []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"matches"`
			Coverage []struct {
				Tests []relatedTestEvidence `json:"tests"`
			} `json:"coverage"`
		}
		if err := json.Unmarshal([]byte(r.Content), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Status == "unavailable" {
			t.Fatalf("%s unavailable", call.Tool)
		}
		for _, m := range payload.Matches {
			location(m.File, m.Line)
		}
		for _, c := range payload.Coverage {
			for _, test := range c.Tests {
				location(test.Path, 0)
				for _, step := range test.Chain {
					if step.Evidence != nil {
						location(step.Evidence.Source, step.Evidence.Site.Line)
					}
				}
			}
		}
	}
	out.Files = taskEvalScore(found, task.ExpectedFiles)
	out.Tests = taskEvalScore(tests, task.RequiredTests)
	out.Anchors = taskEvalScore(anchors, expectedAnchors)
	return out
}

func TestTaskEvaluationMetricsDoNotHideMisses(t *testing.T) {
	m := taskEvalScore(map[string]bool{"a": true, "extra": true}, []string{"a", "missing"})
	if m.TP != 1 || m.FP != 1 || m.FN != 1 || *m.Precision != 0.5 || *m.Recall != 0.5 {
		t.Fatalf("metrics=%+v", m)
	}
	empty := taskEvalScore(map[string]bool{}, nil)
	if empty.Precision != nil || empty.Recall != nil {
		t.Fatalf("empty=%+v", empty)
	}
}
