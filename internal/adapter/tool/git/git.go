package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type Tool struct {
	typed.Contract[input, tool.Result]
	root    string
	kind    string
	backend sandbox.Backend
}

type input struct {
	Staged   bool   `json:"staged"`
	Limit    int    `json:"limit"`
	Revision string `json:"revision"`
	Path     string `json:"path"`
}

func RegisterWithBackend(registry *tool.Registry, root string, backend sandbox.Backend) error {
	return RegisterWithBackendAndRuntime(registry, root, backend, nil)
}

func RegisterWithBackendAndRuntime(
	registry *tool.Registry,
	root string,
	backend sandbox.Backend,
	runtime MutationRuntime,
) error {
	if backend == nil {
		return errors.New("Git tools require an injected sandbox backend")
	}
	backend, err := sandbox.BindPolicy(backend, sandbox.Options{WorkspaceRoot: root})
	if err != nil {
		return err
	}
	workspace, err := sandbox.NewWorkspace(root)
	if err != nil {
		return err
	}
	absolute := workspace.Root()
	registry.SetSandboxBackend(backend)
	for _, kind := range []string{
		"git_status", "git_diff", "git_log", "git_remote", "git_branch", "git_show", "git_blame",
	} {
		executor := &Tool{root: absolute, kind: kind, backend: backend}
		contract, err := typed.NewResultContract(typed.ResultSpec[input]{
			Name: kind, Disposition: tool.DispositionWaitForTeardown,
			Run: executor.run,
		})
		if err != nil {
			return err
		}
		executor.Contract = contract
		if err := registry.Register(executor); err != nil {
			return err
		}
	}
	return RegisterMutations(registry, absolute, runtime)
}

func (t *Tool) Descriptor() tool.Descriptor {
	properties := map[string]any{
		"staged":   map[string]any{"type": "boolean"},
		"limit":    map[string]any{"type": "integer"},
		"revision": map[string]any{"type": "string"},
		"path": map[string]any{
			"type": "string",
			"description": "Workspace-relative path. It locates the enclosing " +
				"Git repository in multi-repo workspaces and limits git_show / " +
				"git_blame output to it.",
		},
	}
	required := []string{}
	if t.kind == "git_blame" {
		required = []string{"path"}
	}
	return tool.Descriptor{
		Name: t.kind, Description: gitReadDescription(t.kind), Visibility: tool.VisibleModel,
		DiscoveryTerms: gitReadDiscoveryTerms(t.kind),
		Capability:     tool.CapabilityRead, AccessMode: tool.AccessTree,
		ResourceResolver: tool.ResourceResolver{Templates: []tool.ResourceTemplate{{
			Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true,
		}}},
		ParallelPolicy:     tool.ParallelConcurrent,
		SandboxRequirement: tool.SandboxStrong, Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type":       "object",
			"properties": properties, "required": required, "additionalProperties": false,
		},
	}
}

func gitReadDescription(kind string) string {
	switch kind {
	case "git_status":
		return "Show concise staged, unstaged, and untracked workspace changes"
	case "git_diff":
		return "Show the workspace Git diff; set staged to inspect the index"
	case "git_log":
		return "Show recent commits in concise one-line form"
	case "git_remote":
		return "List configured Git remotes and fetch/push URLs"
	case "git_branch":
		return "List local and remote Git branches"
	case "git_show":
		return "Show one revision, optionally limited to one workspace-relative path"
	case "git_blame":
		return "Show line attribution for one workspace-relative file at a revision"
	default:
		return "Inspect the workspace Git repository"
	}
}

func gitReadDiscoveryTerms(kind string) []string {
	common := []string{"git", "repository", "仓库", "版本库"}
	switch kind {
	case "git_status":
		return append(common, "status", "changes", "状态", "变更", "未提交")
	case "git_diff":
		return append(common, "diff", "patch", "差异", "补丁")
	case "git_log":
		return append(common, "log", "history", "commit", "历史", "提交记录")
	case "git_remote":
		return append(common, "remote", "origin", "远端")
	case "git_branch":
		return append(common, "branch", "分支")
	case "git_show":
		return append(common, "show commit", "revision", "查看提交", "版本详情")
	case "git_blame":
		return append(common, "blame", "line author", "逐行", "作者")
	default:
		return common
	}
}

// gitHintMaxRepositories bounds the repository hint appended when Git
// reports that no repository was found: multi-repo workspaces list their
// immediate repositories instead of leaving the model to probe.
const gitHintMaxRepositories = 8

// repositoryRoot returns the closest directory containing a .git entry for
// the target path, walking up without leaving the workspace. Multi-repo
// workspaces keep each subproject repository below the workspace root;
// commands must run from that directory or Git fails with "not a git
// repository" at the workspace root. A .git file (worktrees) counts.
func (t *Tool) repositoryRoot(target string) string {
	directory := filepath.Clean(target)
	for {
		if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
			return directory
		}
		if directory == t.root {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return t.root
}

// workspaceRepositories lists immediate workspace subdirectories holding a
// .git entry, for the not-a-repository hint.
func workspaceRepositories(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || len(names) >= gitHintMaxRepositories {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, entry.Name(), ".git")); err == nil {
			names = append(names, entry.Name())
		}
	}
	return names
}

func (t *Tool) run(ctx context.Context, input input) (tool.Result, error) {
	// The repository is located from the path (or the workspace root): a
	// path both selects the enclosing repository in multi-repo workspaces
	// and, for show/blame, limits the output.
	target := t.root
	if input.Path != "" {
		if err := safePath(input.Path); err != nil {
			return tool.Result{}, err
		}
		target = filepath.Dir(
			filepath.Clean(filepath.Join(t.root, filepath.FromSlash(input.Path))),
		)
	}
	repo := t.repositoryRoot(target)
	if repo != t.root && (t.kind == "git_show" || t.kind == "git_blame") &&
		input.Path != "" {
		absolute := filepath.Clean(
			filepath.Join(t.root, filepath.FromSlash(input.Path)),
		)
		if relative, relErr := filepath.Rel(repo, absolute); relErr == nil &&
			!strings.HasPrefix(relative, "..") {
			input.Path = filepath.ToSlash(relative)
		}
	}
	arguments := []string{"status", "--short"}
	switch t.kind {
	case "git_diff":
		arguments = []string{"diff", "--no-ext-diff"}
		if input.Staged {
			arguments = append(arguments, "--cached")
		}
	case "git_log":
		if input.Limit <= 0 {
			input.Limit = 20
		}
		arguments = []string{"log", "--oneline", "-n", strconv.Itoa(input.Limit)}
	case "git_remote":
		arguments = []string{"remote", "-v"}
	case "git_branch":
		arguments = []string{"branch", "--all", "--no-color"}
	case "git_show":
		revision, err := safeRevision(input.Revision)
		if err != nil {
			return tool.Result{}, err
		}
		arguments = []string{"show", "--no-ext-diff", "--format=fuller", revision}
		if input.Path != "" {
			if err := safePath(input.Path); err != nil {
				return tool.Result{}, err
			}
			arguments = append(arguments, "--", input.Path)
		}
	case "git_blame":
		revision, err := safeRevision(input.Revision)
		if err != nil {
			return tool.Result{}, err
		}
		if err := safePath(input.Path); err != nil {
			return tool.Result{}, err
		}
		arguments = []string{"blame", "--line-porcelain", revision, "--", input.Path}
	}
	directory, err := process.OpenPinnedDirectory(t.backend, repo)
	if err != nil {
		return tool.Result{}, err
	}
	defer directory.Close()
	command, err := process.NewCommand(ctx, process.Options{
		Path: gitExecutable(), Args: arguments, Dir: repo,
		DirFile: directory, Sandbox: t.backend, RequireSandbox: true,
		WorkspaceReadOnly: true, DenyNetwork: true,
		Env: gitConfigEnv(t.backend),
	})
	if err != nil {
		return tool.Result{}, err
	}
	output, runErr := command.CombinedOutput()
	exitCode := process.ExitCode(runErr)
	content := string(output)
	if exitCode != 0 &&
		strings.Contains(strings.ToLower(content), "not a git repository") {
		if repos := workspaceRepositories(t.root); len(repos) != 0 {
			content += "\n[qcode] repositories in this workspace: " +
				strings.Join(repos, ", ") +
				" (pass a path inside one of them)"
		}
	}
	return tool.Result{
		Content: content, IsError: exitCode != 0,
		Metadata: map[string]any{"exit_code": exitCode, "repository": repo},
	}, nil
}

func gitConfigEnv(backend sandbox.Backend) []string {
	policy, ok := sandbox.BackendPolicy(backend)
	if ok && policy.EnvironmentProfile == "native" {
		return nil
	}
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
}

// gitExecutable prefers the real Command Line Tools / Xcode binary so Apple's
// /usr/bin/git shim does not need xcrun cache writes inside the seatbelt.
func gitExecutable() string {
	for _, candidate := range []string{
		"/Library/Developer/CommandLineTools/usr/bin/git",
		"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
	} {
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return "git"
}

func safeRevision(revision string) (string, error) {
	if revision == "" {
		return "HEAD", nil
	}
	if strings.HasPrefix(revision, "-") || strings.ContainsAny(revision, "\x00\n\r") {
		return "", errors.New("invalid Git revision")
	}
	return revision, nil
}

func safePath(name string) error {
	if name == "" || filepath.IsAbs(name) {
		return errors.New("Git path must be a non-empty relative path")
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("Git path is outside workspace")
	}
	return nil
}
