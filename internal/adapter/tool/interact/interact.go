package interact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/platform/repowalk"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Options struct {
	Host      *Host
	Workspace string
	Backend   sandbox.Backend
	Vision    VisionClient
	OnPlan    func(Plan) error
}

type Tools struct {
	host      *Host
	workspace string
	backend   sandbox.Backend
	vision    VisionClient
	onPlan    func(Plan) error
	planMu    sync.Mutex
	plan      Plan
}

type executor struct {
	typed.Contract[operationInput, tool.Result]
	tools *Tools
	name  string
}

type operationInput struct {
	SubmittedPlan
	Prompt   string   `json:"prompt"`
	Options  []string `json:"options"`
	Path     string   `json:"path"`
	MaxDepth int      `json:"max_depth"`
	Limit    int      `json:"limit"`
	Notes    string   `json:"notes"`
}

func newExecutor(tools *Tools, name string) (*executor, error) {
	instance := &executor{tools: tools, name: name}
	contract, err := typed.NewResultContract(typed.ResultSpec[operationInput]{
		Name: name, Disposition: tool.DispositionWaitForTeardown,
		Decode: instance.decode, Run: instance.run,
	})
	if err != nil {
		return nil, err
	}
	instance.Contract = contract
	return instance, nil
}

func (e *executor) decode(raw json.RawMessage) (operationInput, error) {
	if e.name == "image_analyze" && e.tools.vision == nil {
		return operationInput{}, nil
	}
	return typed.DecodeStrict[operationInput](raw)
}

func Register(registry *tool.Registry, options Options) error {
	if registry == nil {
		return errors.New("interact tool registry is required")
	}
	root := strings.TrimSpace(options.Workspace)
	if root == "" {
		root = "."
	}
	backend := options.Backend
	if backend != nil {
		bound, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
		if err != nil {
			return err
		}
		backend = bound
	}
	host := options.Host
	if host == nil {
		host = NewHost(0)
	}
	tools := &Tools{
		host: host, workspace: root, backend: backend,
		vision: options.Vision, onPlan: options.OnPlan,
	}
	for _, name := range []string{
		"request_user_input", "update_plan", "submit_plan", "project_map",
		"image_analyze",
	} {
		executor, err := newExecutor(tools, name)
		if err != nil {
			return err
		}
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

func (e *executor) Descriptor() tool.Descriptor {
	switch e.name {
	case "request_user_input":
		return tool.Descriptor{
			Name: e.name,
			Description: "Request required user input and block the current Turn until the host replies. " +
				"Resolve discoverable facts first, include options for finite choices, and never ask " +
				"for required input in ordinary final text.",
			DiscoveryTerms: []string{"ask user", "clarify", "询问用户", "澄清"},
			Visibility:     tool.VisibleModel, Capability: tool.CapabilityRead,
			AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelSerial,
			SandboxRequirement: tool.SandboxNone, Availability: tool.AvailabilityAvailable,
			ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
				Kind: "user_input", ID: "pending", Access: tool.AccessRead,
			}}},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string", "minLength": float64(1)},
					"options": map[string]any{
						"type": "array", "maxItems": float64(12), "uniqueItems": true,
						"items": map[string]any{"type": "string", "minLength": float64(1)},
					},
				},
				"required":             []string{"prompt"},
				"additionalProperties": false,
			},
		}
	case "update_plan", "submit_plan":
		description := "Replace the structured working plan projected through ContextLedger. " +
			"Call this before starting a planned step and immediately after its " +
			"evidence is complete; do not defer status updates until Turn completion."
		if e.name == "submit_plan" {
			description = "Submit a structured, user-reviewable implementation plan. " +
				"Use purpose=deliverable when the user only requested a plan for later " +
				"implementation; it does not replace the working plan or authorize execution. " +
				"Use purpose=execution (default) for work to execute in the current Turn."
		}
		return tool.Descriptor{
			Name: e.name, Description: description,
			DiscoveryTerms: []string{"plan", "task plan", "计划", "任务计划"},
			Visibility:     tool.VisibleModel, Capability: tool.CapabilityWrite,
			AccessMode: tool.AccessWrite, ParallelPolicy: tool.ParallelSerial,
			SandboxRequirement: tool.SandboxNone, Availability: tool.AvailabilityAvailable,
			ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
				Kind: "plan", ID: "session", Access: tool.AccessWrite,
			}}},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"version": map[string]any{"type": "integer", "enum": []any{float64(1)}},
					"purpose": map[string]any{
						"type": "string", "enum": planPurposes(e.name),
						"description": "execution (default) tracks current work; submit_plan also accepts deliverable for a future plan artifact.",
					},
					"title": map[string]any{"type": "string"},
					"steps": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":    map[string]any{"type": "string"},
								"title": map[string]any{"type": "string", "minLength": float64(1)},
								"status": map[string]any{
									"type": "string",
									"enum": []string{StepPending, StepInProgress, StepDone},
								},
								"dependencies": map[string]any{
									"type": "array", "items": map[string]any{"type": "string"},
								},
								"expected_evidence": map[string]any{"type": "string"},
								"affected_files": map[string]any{
									"type":        "array",
									"description": "Workspace files or directories affected by the step.",
									"items":       map[string]any{"type": "string"},
								},
							},
							"required":             []string{"title"},
							"additionalProperties": false,
						},
						"minItems": float64(1),
					},
					"notes":           map[string]any{"type": "string"},
					"objective":       map[string]any{"type": "string"},
					"context_summary": map[string]any{"type": "string"},
					"sources_used":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"critical_files": map[string]any{
						"type":        "array",
						"description": "Existing workspace files or directories that anchor the plan.",
						"items":       map[string]any{"type": "string"},
					},
					"constraints":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"recommended_approach": map[string]any{"type": "string"},
					"verification_plan":    map[string]any{"type": "string"},
					"risks_and_unknowns":   map[string]any{"type": "string"},
					"handoff_packet":       map[string]any{"type": "string"},
				},
				"required":             []string{"steps"},
				"additionalProperties": false,
			},
		}
	case "project_map":
		return tool.Descriptor{
			Name: e.name, Description: "Summarize workspace structure as a bounded directory map.",
			DiscoveryTerms: []string{"project map", "repository structure", "项目结构", "仓库结构"},
			Visibility:     tool.VisibleModel, Capability: tool.CapabilityRead,
			AccessMode: tool.AccessTree, ParallelPolicy: tool.ParallelConcurrent,
			SandboxRequirement: tool.SandboxNone, Availability: tool.AvailabilityAvailable,
			ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
				Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true,
			}}},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      map[string]any{"type": "string"},
					"max_depth": map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(8)},
					"limit":     map[string]any{"type": "integer", "minimum": float64(1), "maximum": float64(500)},
				},
				"additionalProperties": false,
			},
		}
	case "image_analyze":
		available := tool.AvailabilityUnavailable
		reason := VisionUnavailableReason
		if e.tools.vision != nil {
			available = tool.AvailabilityAvailable
			reason = ""
		}
		return tool.Descriptor{
			Name: e.name, Description: "Analyze an image with a vision-capable model when [vision] is configured.",
			DiscoveryTerms: []string{"analyze image", "screenshot", "图片分析", "截图"},
			Visibility:     tool.VisibleModel, Capability: tool.CapabilityRead,
			AccessMode: tool.AccessRead, ParallelPolicy: tool.ParallelConcurrent,
			SandboxRequirement: tool.SandboxNone,
			Availability:       available,
			UnavailableReason:  reason,
			ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
				Kind: "image", Field: "path", Access: tool.AccessRead,
			}}},
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "minLength": float64(1)},
					"prompt": map[string]any{"type": "string"},
				},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		}
	default:
		return tool.Descriptor{Name: e.name, Availability: tool.AvailabilityUnavailable}
	}
}

func (e *executor) run(ctx context.Context, input operationInput) (tool.Result, error) {
	switch e.name {
	case "request_user_input":
		return e.tools.requestInput(ctx, input)
	case "update_plan":
		return e.tools.updatePlan(input, false)
	case "submit_plan":
		return e.tools.updatePlan(input, true)
	case "project_map":
		return e.tools.projectMap(ctx, input)
	case "image_analyze":
		return e.tools.imageAnalyze(ctx, input)
	default:
		return tool.Result{}, fmt.Errorf("unknown interact tool %q", e.name)
	}
}

func (t *Tools) requestInput(ctx context.Context, input operationInput) (tool.Result, error) {
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" {
		return tool.Result{}, errors.New("prompt is required")
	}
	options, err := normalizeInputOptions(input.Options)
	if err != nil {
		return tool.Result{}, err
	}
	reply, err := t.host.Wait(ctx, "tool", input.Prompt, options)
	if err != nil {
		return tool.Result{}, err
	}
	body := map[string]any{"answer": reply.Answer, "request_id": reply.RequestID}
	if len(reply.Values) > 0 {
		body["values"] = reply.Values
	}
	content, err := json.Marshal(body)
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content:  string(content),
		Metadata: map[string]any{"request_id": reply.RequestID},
	}, nil
}

func normalizeInputOptions(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, errors.New("input options must be non-empty")
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate input option %q", trimmed)
		}
		seen[key] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

func (t *Tools) updatePlan(input operationInput, submitted bool) (tool.Result, error) {
	plan := input.SubmittedPlan
	if err := plan.NormalizeAndValidate(); err != nil {
		return tool.Result{}, err
	}
	if !submitted && plan.Purpose != protocol.PlanPurposeExecution {
		return tool.Result{}, errors.New("update_plan only accepts execution plans; use submit_plan for deliverables")
	}
	next := plan.executionPlan()
	if !submitted && t.samePlanProgress(next) {
		return unchangedPlanResult(), nil
	}
	var err error
	if submitted {
		plan.FileBaseline, err = t.capturePlanBaseline(plan)
		if err != nil {
			return tool.Result{}, err
		}
	}
	if plan.Purpose == protocol.PlanPurposeExecution {
		if err := t.applyPlan(next); err != nil {
			return tool.Result{}, err
		}
	}
	content, err := json.Marshal(plan)
	metadata := map[string]any{
		"steps":         len(plan.Steps),
		"plan_delta":    plan.Purpose == protocol.PlanPurposeExecution,
		"plan_artifact": submitted,
	}
	if submitted && plan.Purpose == protocol.PlanPurposeExecution {
		metadata["submitted_plan"] = true
	}
	return tool.Result{
		Content: string(content), Metadata: metadata,
	}, err
}

func planPurposes(name string) []string {
	if name == "submit_plan" {
		return []string{string(protocol.PlanPurposeExecution), string(protocol.PlanPurposeDeliverable)}
	}
	return []string{string(protocol.PlanPurposeExecution)}
}

func (t *Tools) applyPlan(plan Plan) error {
	t.planMu.Lock()
	t.plan = plan
	t.planMu.Unlock()
	if t.onPlan != nil {
		if err := t.onPlan(plan); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tools) samePlanProgress(plan Plan) bool {
	t.planMu.Lock()
	current := t.plan
	t.planMu.Unlock()
	return len(current.Steps) > 0 &&
		slices.Equal(current.Steps, plan.Steps)
}

const requiredActionFinishOrDeclareIncomplete = "finish_open_plan_steps_or_declare_incomplete"

func unchangedPlanResult() tool.Result {
	return tool.Result{
		Content: `{"accepted":false,"reason":"plan_progress_unchanged",` +
			`"required_action":"` + requiredActionFinishOrDeclareIncomplete + `"}`,
		IsError: true,
		Metadata: map[string]any{
			"reason":          "plan_progress_unchanged",
			"required_action": requiredActionFinishOrDeclareIncomplete,
			"retry_original":  false,
			"plan_delta":      false,
		},
	}
}

func (t *Tools) capturePlanBaseline(plan SubmittedPlan) ([]PlanFileBaseline, error) {
	workspace, err := sandbox.NewWorkspace(t.workspace)
	if err != nil {
		return nil, err
	}
	paths := append([]string(nil), plan.CriticalFiles...)
	for _, step := range plan.Steps {
		paths = append(paths, step.AffectedFiles...)
	}
	seen := make(map[string]struct{}, len(paths))
	baseline := make([]PlanFileBaseline, 0, len(paths))
	for _, name := range paths {
		name = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(name), "./"))
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		if len(seen) >= 128 {
			return nil, errors.New("plan file baseline accepts at most 128 paths")
		}
		seen[name] = struct{}{}
		file, err := workspace.OpenFile(name)
		if errors.Is(err, os.ErrNotExist) {
			baseline = append(baseline, PlanFileBaseline{
				Path: name, Missing: true,
			})
			continue
		}
		if err != nil {
			directory, directoryErr := workspace.OpenDirectory(name)
			if directoryErr == nil {
				if closeErr := directory.Close(); closeErr != nil {
					return nil, fmt.Errorf(
						"capture plan baseline directory %q: %w",
						name,
						closeErr,
					)
				}
				baseline = append(baseline, PlanFileBaseline{
					Path: name, Directory: true,
				})
				continue
			}
			return nil, fmt.Errorf("capture plan baseline %q: %w", name, err)
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return nil, fmt.Errorf(
				"capture plan baseline %q: %w",
				name,
				errors.Join(copyErr, closeErr),
			)
		}
		baseline = append(baseline, PlanFileBaseline{
			Path: name, Digest: hex.EncodeToString(digest.Sum(nil)),
		})
	}
	sort.Slice(baseline, func(i, j int) bool {
		return baseline[i].Path < baseline[j].Path
	})
	return baseline, nil
}

func (t *Tools) projectMap(ctx context.Context, input operationInput) (tool.Result, error) {
	if input.MaxDepth <= 0 {
		input.MaxDepth = 3
	}
	if input.Limit <= 0 {
		input.Limit = 120
	}
	rel := strings.TrimSpace(input.Path)
	if rel == "" {
		rel = "."
	}
	prefix, err := t.mapPrefix(rel)
	if err != nil {
		return tool.Result{}, err
	}
	// The map is drawn from the same enumeration the search tools use, so it shows
	// the files a reader would find and not the build output an ignore rule hides.
	walker, err := repowalk.New(t.workspace, t.backend)
	if err != nil {
		return tool.Result{}, err
	}
	listing, err := walker.List(ctx)
	if err != nil {
		return tool.Result{}, err
	}
	names := make(map[string]struct{})
	for _, entry := range listing.Files {
		if prefix != "" && !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		segments := strings.Split(entry.Path, "/")
		for depth := 1; depth <= len(segments) && depth <= input.MaxDepth; depth++ {
			name := strings.Join(segments[:depth], "/")
			if depth < len(segments) {
				name += "/"
			}
			names[name] = struct{}{}
		}
	}
	entries := make([]string, 0, len(names))
	for name := range names {
		entries = append(entries, name)
	}
	sort.Strings(entries)
	truncated := len(entries) > input.Limit
	if truncated {
		entries = entries[:input.Limit]
	}
	content, err := json.Marshal(map[string]any{
		"root": rel, "entries": entries, "count": len(entries), "truncated": truncated,
	})
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content: string(content),
		Metadata: map[string]any{
			"count": len(entries), "truncated": truncated, "max_depth": input.MaxDepth,
		},
	}, nil
}

// mapPrefix turns a requested subdirectory into the workspace-relative prefix
// its entries share, refusing anything that leaves the workspace.
func (t *Tools) mapPrefix(rel string) (string, error) {
	rootAbs, err := filepath.Abs(t.workspace)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if abs != rootAbs &&
		!strings.HasPrefix(abs+string(os.PathSeparator), rootAbs+string(os.PathSeparator)) {
		return "", errors.New("path escapes workspace")
	}
	if abs == rootAbs {
		return "", nil
	}
	relative, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative) + "/", nil
}

func (t *Tools) imageAnalyze(ctx context.Context, input operationInput) (tool.Result, error) {
	if t.vision == nil {
		return tool.Result{
			Content: VisionUnavailableReason, IsError: true,
			Metadata: map[string]any{"error_category": "unavailable"},
		}, nil
	}
	path := strings.TrimSpace(input.Path)
	if path == "" {
		return tool.Result{}, errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return tool.Result{
			Content: "image path must be workspace-relative", IsError: true,
			Metadata: map[string]any{"error_category": "invalid_path"},
		}, nil
	}
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return tool.Result{
			Content: "image path escapes workspace", IsError: true,
			Metadata: map[string]any{"error_category": "invalid_path"},
		}, nil
	}
	abs := filepath.Join(t.workspace, cleaned)
	info, err := os.Stat(abs)
	if err != nil {
		return tool.Result{
			Content: err.Error(), IsError: true,
			Metadata: map[string]any{"error_category": "not_found"},
		}, nil
	}
	if info.IsDir() {
		return tool.Result{
			Content: "image path is a directory", IsError: true,
			Metadata: map[string]any{"error_category": "invalid_path"},
		}, nil
	}
	text, err := t.vision.Analyze(ctx, abs, input.Prompt)
	if err != nil {
		return tool.Result{
			Content: err.Error(), IsError: true,
			Metadata: map[string]any{"error_category": "vision_error"},
		}, nil
	}
	return tool.Result{
		Content:  text,
		Metadata: map[string]any{"path": cleaned, "bytes": info.Size()},
	}, nil
}

// Plan returns a copy of the last applied plan, if any.
func (t *Tools) Plan() (Plan, bool) {
	if t == nil {
		return Plan{}, false
	}
	t.planMu.Lock()
	defer t.planMu.Unlock()
	if len(t.plan.Steps) == 0 {
		return Plan{}, false
	}
	return t.plan, true
}
