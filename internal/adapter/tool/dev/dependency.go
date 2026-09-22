package dev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fwtllh-png/QCode/internal/adapter/tool"
	"github.com/fwtllh-png/QCode/internal/adapter/tool/typed"
	"github.com/fwtllh-png/QCode/internal/platform/process"
	"github.com/fwtllh-png/QCode/internal/security/sandbox"
)

type dependencyTool struct {
	typed.Contract[dependencyInput, tool.Result]
	root    string
	backend sandbox.Backend
}

type dependencyExecutor struct {
	tool.OutcomeExecutor
	binding tool.TrustedBinding
}

func (e *dependencyExecutor) TrustedBinding() tool.TrustedBinding {
	return e.binding
}

func (*dependencyExecutor) ExecutionDisposition() tool.ExecutionDisposition {
	return tool.DispositionWaitForTeardown
}

type dependencyInput struct {
	Ecosystem      string                       `json:"ecosystem"`
	NetworkTargets []tool.DeclaredNetworkTarget `json:"network_targets"`
}

type dependencyCommand struct {
	Ecosystem string
	Binary    string
	Args      []string
	// Dir is the absolute directory of the manifest this command runs in;
	// RelativeDir is the same directory workspace-relative, "" at the root.
	Dir         string
	RelativeDir string
}

// dependencyManifestSearchDepth and dependencyManifestSearchLimit bound the
// workspace walk that locates dependency manifests: repositories with deep
// build output or vendored trees must not turn dependency resolution into a
// full index. They are public contract constants; boundary tests pin them.
const (
	dependencyManifestSearchDepth = 4
	dependencyManifestSearchLimit = 16
)

// dependencyManifestSkips lists directories that never contain a meaningful
// manifest but are routinely huge.
var dependencyManifestSkips = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
}

// dependencyManifests maps an ecosystem to the manifest file names that
// declare it.
var dependencyManifests = map[string][]string{
	"go":     {"go.mod"},
	"node":   {"package.json"},
	"rust":   {"Cargo.toml"},
	"maven":  {"pom.xml"},
	"gradle": {"gradlew", "build.gradle", "build.gradle.kts"},
}

// findManifestDirs returns workspace directories (absolute, root first,
// lexical order) containing any of the named manifests. The walk is bounded
// by depth and result count and skips vendored/build trees.
func findManifestDirs(root string, names []string) []string {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	var dirs []string
	seen := make(map[string]bool)
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		depth := 0
		if relative != "." {
			depth = strings.Count(filepath.ToSlash(relative), "/") + 1
		}
		if entry.IsDir() {
			if depth >= dependencyManifestSearchDepth ||
				(depth > 0 && dependencyManifestSkips[entry.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !wanted[entry.Name()] || seen[filepath.Dir(path)] || len(seen) >= dependencyManifestSearchLimit {
			return nil
		}
		seen[filepath.Dir(path)] = true
		dirs = append(dirs, filepath.Dir(path))
		return nil
	})
	return dirs
}

func registerDependency(
	registry *tool.Registry,
	root string,
	backend sandbox.Backend,
) error {
	instance := &dependencyTool{root: root, backend: backend}
	runtime, err := typed.Define(typed.Spec[dependencyInput, tool.Result]{
		Descriptor:  instance.Descriptor(),
		Disposition: tool.DispositionWaitForTeardown,
		Validate: func(input dependencyInput) error {
			return tool.ValidateDeclaredNetworkTargets(input.NetworkTargets)
		},
		Run:    instance.run,
		Encode: func(result tool.Result) (tool.Result, error) { return result, nil },
	})
	if err != nil {
		return err
	}
	outcome, ok := runtime.(tool.OutcomeExecutor)
	if !ok {
		return errors.New("dependency resolver typed runtime is incomplete")
	}
	return registry.Register(&dependencyExecutor{
		OutcomeExecutor: outcome, binding: instance.TrustedBinding(),
	})
}

func (t *dependencyTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{
		Name: "dependency_resolve",
		Description: "Resolve or download declared project dependencies with scripts disabled " +
			"and the workspace read-only. Declare required registry hosts in network_targets.",
		DiscoveryTerms: []string{
			"dependencies", "package install", "resolve packages", "依赖", "安装依赖", "包管理",
		},
		Visibility: tool.VisibleModel, Capability: tool.CapabilityProcess,
		AccessMode: tool.AccessRead,
		ResourceResolver: tool.ResourceResolver{
			Templates: []tool.ResourceTemplate{
				{Kind: "repo", ID: ".", Access: tool.AccessRead, Tree: true},
				{Kind: "process", ID: "dependency-manager", Access: tool.AccessWrite, Tree: true},
			},
			NetworkTargetsField: "network_targets",
		},
		ParallelPolicy: tool.ParallelSerial, RepeatPolicy: tool.RepeatExecute,
		SandboxRequirement: tool.SandboxStrong, Availability: tool.AvailabilityAvailable,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ecosystem": map[string]any{
					"type": "string",
					"enum": []any{"auto", "go", "node", "rust", "maven", "gradle"},
				},
				"network_targets": tool.NetworkTargetsInputSchema(),
			},
			"additionalProperties": false,
		},
	}
}

func (t *dependencyTool) TrustedBinding() tool.TrustedBinding {
	binding := tool.TrustedBindingFromDescriptor(t.Descriptor())
	binding.Effect = tool.EffectContract{
		Mode: tool.EffectFixed, Kind: tool.EffectNetworkRead,
		Risk: tool.RiskMedium, Reversibility: tool.Reversible,
		WorkspaceTransaction: tool.TransactionNone,
		Approval:             tool.ApprovalPolicyDefault,
	}
	return binding
}

func (t *dependencyTool) run(
	ctx context.Context,
	input dependencyInput,
) (tool.Result, error) {
	// Walk output, the pinned directory, and the sandbox policy must share
	// one root form: a lexical root on symlinked platforms (/var vs
	// /private/var) makes workspace-relative resolution escape.
	root := t.root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	commands, err := detectDependencyCommands(root, input.Ecosystem)
	if err != nil {
		return tool.Result{}, err
	}
	receipts := make([]map[string]any, 0, len(commands))
	for _, command := range commands {
		// The pinned descriptor is the child's real cwd (fd 3), so each
		// manifest directory gets its own pin.
		directory, err := process.OpenPinnedDirectory(t.backend, command.Dir)
		if err != nil {
			return tool.Result{}, err
		}
		result, err := process.Run(ctx, process.Options{
			Path: command.Binary, Args: command.Args, Dir: command.Dir,
			DirFile: directory,
			Sandbox: t.backend, RequireSandbox: true,
			WorkspaceReadOnly: true, DenyNetwork: len(input.NetworkTargets) == 0,
			OutputLimitBytes: process.ModelOutputLimitBytes,
		})
		closeErr := directory.Close()
		if err != nil {
			return tool.Result{}, err
		}
		if closeErr != nil {
			return tool.Result{}, closeErr
		}
		receipt := map[string]any{
			"ecosystem": command.Ecosystem, "exit_code": result.ExitCode,
			"dir":    command.RelativeDir,
			"stdout": strings.TrimSpace(result.Stdout),
			"stderr": strings.TrimSpace(result.Stderr),
		}
		receipts = append(receipts, receipt)
		if result.ExitCode != 0 {
			content, _ := json.Marshal(map[string]any{"checks": receipts})
			// The manager ran and failed on its own terms (for example a
			// missing module or an upstream answer); surface structured
			// attribution so the UI does not fall back to a generic
			// "no structured error details" line.
			return tool.Result{
				Content: string(content), IsError: true,
				Metadata: map[string]any{
					"error_category": "dependency_resolve_failed",
					"ecosystem":      command.Ecosystem,
					"dir":            command.RelativeDir,
				},
			}, nil
		}
	}
	content, err := json.Marshal(map[string]any{"checks": receipts})
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{
		Content:  string(content),
		Metadata: map[string]any{"ecosystems": len(commands)},
	}, nil
}

func detectDependencyCommands(root, requested string) ([]dependencyCommand, error) {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		requested = "auto"
	}
	var ecosystems []string
	if requested != "auto" {
		ecosystems = []string{requested}
	} else {
		for ecosystem := range dependencyManifests {
			ecosystems = append(ecosystems, ecosystem)
		}
		sort.Strings(ecosystems)
	}
	commands := make([]dependencyCommand, 0, len(ecosystems))
	for _, ecosystem := range ecosystems {
		names, supported := dependencyManifests[ecosystem]
		if !supported {
			return nil, fmt.Errorf("unsupported dependency ecosystem %q", ecosystem)
		}
		dirs := findManifestDirs(root, names)
		if len(dirs) == 0 {
			if requested != "auto" {
				return nil, fmt.Errorf(
					"no %s dependency manifest was found in the workspace",
					ecosystem,
				)
			}
			continue
		}
		for _, dir := range dirs {
			exists := func(name string) bool {
				info, err := os.Stat(filepath.Join(dir, name))
				return err == nil && !info.IsDir()
			}
			name, args, err := dependencyInvocation(dir, ecosystem, exists)
			if err != nil {
				return nil, err
			}
			binary := name
			if !filepath.IsAbs(name) {
				binary, err = exec.LookPath(name)
				if err != nil {
					return nil, fmt.Errorf("%s dependency manager %q is unavailable", ecosystem, name)
				}
			}
			relative := ""
			if dir != root {
				if value, relErr := filepath.Rel(root, dir); relErr == nil {
					relative = filepath.ToSlash(value)
				}
			}
			commands = append(commands, dependencyCommand{
				Ecosystem: ecosystem, Binary: binary, Args: args,
				Dir: dir, RelativeDir: relative,
			})
		}
	}
	if len(commands) == 0 {
		return nil, errors.New("no supported dependency manifest was found")
	}
	return commands, nil
}

func dependencyInvocation(
	root, ecosystem string,
	exists func(string) bool,
) (string, []string, error) {
	switch ecosystem {
	case "go":
		return "go", []string{"mod", "download"}, nil
	case "node":
		if exists("pnpm-lock.yaml") {
			return "pnpm", []string{"fetch", "--frozen-lockfile", "--ignore-scripts"}, nil
		}
		return "npm", []string{
			"install", "--dry-run", "--ignore-scripts", "--package-lock=false",
		}, nil
	case "rust":
		return "cargo", []string{"fetch", "--locked"}, nil
	case "maven":
		return "mvn", []string{"dependency:go-offline", "-DskipTests"}, nil
	case "gradle":
		if exists("gradlew") {
			return filepath.Join(root, "gradlew"), []string{
				"--no-daemon", "dependencies",
			}, nil
		}
		return "gradle", []string{"--no-daemon", "dependencies"}, nil
	default:
		return "", nil, fmt.Errorf("unsupported dependency ecosystem %q", ecosystem)
	}
}
