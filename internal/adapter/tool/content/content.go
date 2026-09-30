package content

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/orchestration/workspacebroker"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/filebroker"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

const contentOutputLimit = 4 << 20
const contentInputLimit = 64 << 20

type Tool struct {
	typed.Contract[input, tool.Result]
	root      string
	kind      string
	workspace *sandbox.Workspace
	backend   sandbox.Backend
	broker    *workspacebroker.Runtime
}

type input struct {
	Path       string `json:"path"`
	OutputPath string `json:"output_path"`
	Language   string `json:"language"`
	From       string `json:"from"`
	To         string `json:"to"`
	Format     string `json:"format"`
}

func (t *Tool) bindContract() error {
	contract, err := typed.NewResultContract(typed.ResultSpec[input]{
		Name: t.kind, Disposition: tool.DispositionWaitForTeardown,
		Run: t.run,
	})
	if err != nil {
		return err
	}
	t.Contract = contract
	return nil
}

func (t *Tool) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(t.Descriptor())
	if t.kind == "document_convert" {
		binding.Effect = tool.EffectContract{
			Mode: tool.EffectFixed, Kind: tool.EffectProcessMutating,
			Risk: tool.RiskHigh, Reversibility: tool.Bounded,
			WorkspaceTransaction: tool.TransactionBrokerOwned,
			Approval:             tool.ApprovalPolicyDefault,
		}
	}
	return binding
}

func RegisterWithBackend(registry *tool.Registry, root string, backend sandbox.Backend) error {
	return RegisterWithBackendAndRuntime(registry, root, backend, nil)
}

func RegisterWithBackendAndRuntime(
	registry *tool.Registry,
	root string,
	backend sandbox.Backend,
	broker *workspacebroker.Runtime,
) error {
	if backend == nil {
		return errors.New("content tools require an injected sandbox backend")
	}
	backend, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return err
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return err
	}
	registry.SetSandboxBackend(backend)
	for _, kind := range []string{
		"content_capabilities", "image_ocr", "speech_transcribe", "document_convert", "data_validate",
	} {
		executor := &Tool{
			root: workspace.Root(), kind: kind, workspace: workspace,
			backend: backend, broker: broker,
		}
		if err := executor.bindContract(); err != nil {
			return err
		}
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tool) Descriptor() tool.Descriptor {
	properties := map[string]any{}
	required := []string{}
	description := "Probe optional content-processing dependencies"
	switch t.kind {
	case "image_ocr":
		description = "Extract text from an image using a configured OCR dependency"
		properties = map[string]any{
			"path":     map[string]any{"type": "string", "minLength": 1},
			"language": map[string]any{"type": "string"},
		}
		required = []string{"path"}
	case "speech_transcribe":
		description = "Transcribe an audio file using a configured speech dependency"
		properties = map[string]any{
			"path":     map[string]any{"type": "string", "minLength": 1},
			"language": map[string]any{"type": "string"},
		}
		required = []string{"path"}
	case "document_convert":
		description = "Convert a document with pandoc into a workspace output file"
		properties = map[string]any{
			"path":        map[string]any{"type": "string", "minLength": 1},
			"output_path": map[string]any{"type": "string", "minLength": 1},
			"from":        map[string]any{"type": "string", "minLength": 1},
			"to":          map[string]any{"type": "string", "minLength": 1},
		}
		required = []string{"path", "output_path", "from", "to"}
	case "data_validate":
		description = "Validate a JSON or CSV workspace file"
		properties = map[string]any{
			"path":   map[string]any{"type": "string", "minLength": 1},
			"format": map[string]any{"type": "string", "enum": []any{"json", "csv"}},
		}
		required = []string{"path", "format"}
	}
	capability, access := tool.CapabilityRead, tool.AccessRead
	resolver := tool.ResourceResolver{}
	requirement := tool.SandboxNone
	switch t.kind {
	case "image_ocr", "speech_transcribe":
		capability, requirement = tool.CapabilityProcess, tool.SandboxStrong
		resolver.Templates = []tool.ResourceTemplate{{
			Kind: "file", Field: "path", Access: tool.AccessRead,
		}}
	case "document_convert":
		capability, access, requirement = tool.CapabilityProcess, tool.AccessWrite, tool.SandboxStrong
		resolver.Templates = []tool.ResourceTemplate{
			{Kind: "file", Field: "path", Access: tool.AccessRead},
			{Kind: "file", Field: "output_path", Access: tool.AccessWrite},
		}
	case "data_validate":
		resolver.Templates = []tool.ResourceTemplate{{
			Kind: "file", Field: "path", Access: tool.AccessRead,
		}}
	}
	availability := tool.AvailabilityAvailable
	unavailableReason := ""
	dependencyEnvironment, dependencyFallback := "", ""
	switch t.kind {
	case "image_ocr":
		dependencyEnvironment, dependencyFallback = "QCODE_TESSERACT_BINARY", "tesseract"
	case "speech_transcribe":
		dependencyEnvironment, dependencyFallback = "QCODE_SPEECH_BINARY", "whisper"
	case "document_convert":
		dependencyEnvironment, dependencyFallback = "QCODE_PANDOC_BINARY", "pandoc"
	}
	if dependencyFallback != "" {
		binary := dependencyName(dependencyEnvironment, dependencyFallback)
		if _, err := exec.LookPath(binary); err != nil {
			availability = tool.AvailabilityUnavailable
			unavailableReason = binary + " dependency is not available"
		}
	}
	return tool.Descriptor{
		Name: t.kind, Description: description, Visibility: tool.VisibleModel,
		DiscoveryTerms: contentDiscoveryTerms(t.kind),
		Capability:     capability, AccessMode: access, ResourceResolver: resolver,
		ParallelPolicy:     tool.ParallelConcurrent,
		SandboxRequirement: requirement, Availability: availability,
		UnavailableReason: unavailableReason,
		InputSchema: map[string]any{
			"type": "object", "properties": properties, "required": required,
			"additionalProperties": false,
		},
	}
}

func contentDiscoveryTerms(kind string) []string {
	switch kind {
	case "content_capabilities":
		return []string{"content tools", "media capabilities", "内容工具", "媒体能力"}
	case "image_ocr":
		return []string{"ocr", "image text", "图片文字", "识别图片"}
	case "speech_transcribe":
		return []string{"transcribe", "speech to text", "音频转写", "语音识别"}
	case "document_convert":
		return []string{"convert document", "pandoc", "文档转换", "格式转换"}
	case "data_validate":
		return []string{"validate json", "validate csv", "校验数据", "验证 json", "验证 csv"}
	default:
		return nil
	}
}

func (t *Tool) run(ctx context.Context, value input) (tool.Result, error) {
	switch t.kind {
	case "content_capabilities":
		return t.capabilities()
	case "image_ocr":
		return t.ocr(ctx, value)
	case "speech_transcribe":
		return t.transcribe(ctx, value)
	case "document_convert":
		return t.convert(ctx, value)
	case "data_validate":
		return t.validate(value)
	default:
		return tool.Result{}, errors.New("unknown content tool")
	}
}

// Probe reports whether optional content binaries are resolvable via LookPath
// (honoring QCODE_*_BINARY overrides). Keys: ocr, speech, pandoc, ffmpeg.
func Probe() map[string]bool {
	dependencies := map[string]struct{ environment, fallback string }{
		"ocr":    {"QCODE_TESSERACT_BINARY", "tesseract"},
		"speech": {"QCODE_SPEECH_BINARY", "whisper"},
		"pandoc": {"QCODE_PANDOC_BINARY", "pandoc"},
		"ffmpeg": {"QCODE_FFMPEG_BINARY", "ffmpeg"},
	}
	available := make(map[string]bool, len(dependencies))
	for name, dependency := range dependencies {
		binary := strings.TrimSpace(os.Getenv(dependency.environment))
		if binary == "" {
			binary = dependency.fallback
		}
		_, err := exec.LookPath(binary)
		available[name] = err == nil
	}
	return available
}

func (t *Tool) capabilities() (tool.Result, error) {
	available := Probe()
	content, err := json.Marshal(map[string]any{"schema_version": 1, "available": available})
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: string(content), Metadata: map[string]any{"available": available}}, nil
}

func (t *Tool) ocr(ctx context.Context, value input) (tool.Result, error) {
	path, cleanup, err := t.snapshotWorkspaceFile(value.Path)
	if err != nil {
		return tool.Result{}, err
	}
	defer cleanup()
	binary, failure := dependency("QCODE_TESSERACT_BINARY", "tesseract", "ocr")
	if failure != nil {
		return *failure, nil
	}
	arguments := []string{path, "stdout"}
	if value.Language != "" {
		arguments = append(arguments, "-l", value.Language)
	}
	stdout, stderr, exitCode, err := t.runBounded(ctx, binary, arguments...)
	if err != nil {
		return tool.Result{}, err
	}
	return externalResult("ocr", stdout, stderr, exitCode), nil
}

func (t *Tool) transcribe(ctx context.Context, value input) (tool.Result, error) {
	path, cleanup, err := t.snapshotWorkspaceFile(value.Path)
	if err != nil {
		return tool.Result{}, err
	}
	defer cleanup()
	binary, failure := dependency("QCODE_SPEECH_BINARY", "whisper", "speech")
	if failure != nil {
		return *failure, nil
	}
	outputDir, err := os.MkdirTemp("", "qcode-speech-*")
	if err != nil {
		return tool.Result{}, err
	}
	defer os.RemoveAll(outputDir)
	arguments := []string{path, "--output_format", "txt", "--output_dir", outputDir}
	if value.Language != "" {
		arguments = append(arguments, "--language", value.Language)
	}
	_, stderr, exitCode, err := t.runBounded(ctx, binary, arguments...)
	if err != nil {
		return tool.Result{}, err
	}
	if exitCode != 0 {
		return externalResult("speech", "", stderr, exitCode), nil
	}
	transcriptPath := filepath.Join(outputDir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".txt")
	transcript, err := os.ReadFile(transcriptPath)
	if err != nil {
		return tool.Result{}, fmt.Errorf("read speech transcript: %w", err)
	}
	if len(transcript) > contentOutputLimit {
		return contentFailure("output_too_large", "speech transcript exceeds 4 MiB"), nil
	}
	return externalResult("speech", string(transcript), stderr, exitCode), nil
}

func (t *Tool) convert(ctx context.Context, value input) (tool.Result, error) {
	inputPath, cleanup, err := t.snapshotWorkspaceFile(value.Path)
	if err != nil {
		return tool.Result{}, err
	}
	defer cleanup()
	if err := t.validateWorkspaceOutput(value.OutputPath); err != nil {
		return tool.Result{}, err
	}
	outputFile, err := os.CreateTemp("", "qcode-convert-*")
	if err != nil {
		return tool.Result{}, err
	}
	outputPath := outputFile.Name()
	if err := outputFile.Close(); err != nil {
		return tool.Result{}, err
	}
	defer os.Remove(outputPath)
	binary, failure := dependency("QCODE_PANDOC_BINARY", "pandoc", "pandoc")
	if failure != nil {
		return *failure, nil
	}
	_, stderr, exitCode, err := t.runBounded(
		ctx, binary, inputPath, "-f", value.From, "-t", value.To, "-o", outputPath,
	)
	if err != nil {
		return tool.Result{}, err
	}
	if exitCode != 0 {
		return externalResult("pandoc", "", stderr, exitCode), nil
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		return tool.Result{}, fmt.Errorf("read converted document: %w", err)
	}
	if len(output) > contentOutputLimit {
		return contentFailure("output_too_large", "converted document exceeds 4 MiB"), nil
	}
	if t.broker == nil {
		return tool.Result{}, errors.New(
			"document conversion requires the Workspace File Broker",
		)
	}
	plan, err := filebroker.PlanWrite(
		t.workspace, value.OutputPath, output, 0o644,
	)
	if err != nil {
		return tool.Result{}, fmt.Errorf("plan converted document: %w", err)
	}
	if _, err := t.broker.CommitFiles(
		ctx, "document_convert", plan, nil,
	); err != nil {
		return tool.Result{}, fmt.Errorf("commit converted document: %w", err)
	}
	content, err := json.Marshal(map[string]any{
		"output_path": value.OutputPath, "bytes": len(output), "from": value.From, "to": value.To,
	})
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content:  string(content),
		Metadata: map[string]any{"kind": "pandoc", "status": "completed", "exit_code": 0},
	}, nil
}

func (t *Tool) validate(value input) (tool.Result, error) {
	if t.workspace == nil {
		return tool.Result{}, errors.New("content workspace is required")
	}
	file, err := t.workspace.OpenFile(value.Path)
	if err != nil {
		return tool.Result{}, err
	}
	defer file.Close()
	rows := 0
	switch value.Format {
	case "json":
		decoder := json.NewDecoder(io.LimitReader(file, contentOutputLimit+1))
		var document any
		err = decoder.Decode(&document)
		if err == nil {
			var extra any
			if decoder.Decode(&extra) != io.EOF {
				err = errors.New("JSON contains multiple values")
			}
		}
	case "csv":
		reader := csv.NewReader(io.LimitReader(file, contentOutputLimit+1))
		reader.FieldsPerRecord = 0
		for {
			_, readErr := reader.Read()
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				err = readErr
				break
			}
			rows++
		}
	default:
		return tool.Result{}, errors.New("data format must be json or csv")
	}
	valid := err == nil
	payload := map[string]any{
		"schema_version": 1, "format": value.Format, "valid": valid, "rows": rows,
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	content, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return tool.Result{}, marshalErr
	}
	return tool.Result{
		Content: string(content), IsError: !valid,
		Metadata: map[string]any{"format": value.Format, "valid": valid, "rows": rows},
	}, nil
}

func (t *Tool) validateWorkspaceOutput(name string) error {
	if name == "" || filepath.IsAbs(name) {
		return errors.New("content path must be a non-empty relative path")
	}
	if t.workspace == nil {
		return errors.New("content workspace is required")
	}
	_, err := t.workspace.Resolve(name, sandbox.AllowMissing)
	return err
}

func (t *Tool) snapshotWorkspaceFile(name string) (string, func(), error) {
	if t.workspace == nil {
		return "", nil, errors.New("content workspace is required")
	}
	source, err := t.workspace.OpenFile(name)
	if err != nil {
		return "", nil, err
	}
	defer source.Close()
	snapshot, err := os.CreateTemp("", "qcode-content-*"+filepath.Ext(name))
	if err != nil {
		return "", nil, err
	}
	path := snapshot.Name()
	cleanup := func() { _ = os.Remove(path) }
	written, copyErr := io.Copy(snapshot, io.LimitReader(source, contentInputLimit+1))
	closeErr := snapshot.Close()
	if copyErr != nil {
		cleanup()
		return "", nil, copyErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	if written > contentInputLimit {
		cleanup()
		return "", nil, errors.New("content input exceeds 64 MiB")
	}
	return path, cleanup, nil
}

func dependencyName(environment, fallback string) string {
	if configured := os.Getenv(environment); configured != "" {
		return configured
	}
	return fallback
}

func dependency(environment, fallback, capability string) (string, *tool.Result) {
	binary := dependencyName(environment, fallback)
	path, err := exec.LookPath(binary)
	if err == nil {
		return path, nil
	}
	failure := contentFailure("unavailable", capability+" dependency is not available")
	failure.Metadata["dependency"] = capability
	return "", &failure
}

func externalResult(kind, stdout, stderr string, exitCode int) tool.Result {
	status := "completed"
	if exitCode != 0 {
		status = "failed"
	}
	return tool.Result{
		Content: stdout, IsError: exitCode != 0,
		Metadata: map[string]any{
			"kind": kind, "status": status, "exit_code": exitCode, "stderr": stderr,
		},
	}
}

func contentFailure(category, message string) tool.Result {
	return tool.Result{
		Content: message, IsError: true,
		Metadata: map[string]any{"error_category": category},
	}
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	if b.buffer.Len()+len(value) > b.limit {
		remaining := b.limit - b.buffer.Len()
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		return len(value), nil
	}
	return b.buffer.Write(value)
}

func (t *Tool) runBounded(
	ctx context.Context,
	binary string,
	arguments ...string,
) (string, string, int, error) {
	directory, err := process.OpenPinnedDirectory(t.backend, t.root)
	if err != nil {
		return "", "", -1, err
	}
	defer directory.Close()
	command, err := process.NewCommand(ctx, process.Options{
		Path: binary, Args: arguments, Dir: t.root,
		DirFile: directory, Sandbox: t.backend, RequireSandbox: true,
	})
	if err != nil {
		return "", "", -1, err
	}
	stdout := &limitedBuffer{limit: contentOutputLimit}
	stderr := &limitedBuffer{limit: contentOutputLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	err = command.Run()
	if ctx.Err() != nil {
		return "", "", -1, ctx.Err()
	}
	exitCode := process.ExitCode(err)
	if exitCode == -1 {
		return "", "", -1, err
	}
	return stdout.buffer.String(), stderr.buffer.String(), exitCode, nil
}
